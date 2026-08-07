package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	fwint64validator "github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	fwstringvalidator "github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// This resource manages OMV's "System > Notifications > Settings" tab --
// the outgoing SMTP configuration used for all system email
// notifications (NOT the per-event notification toggles on that same
// page's main list, a separate "Notification" RPC service/config type
// not yet covered by this provider). Verified against the OMV 8.5.5
// source (engined/rpc/notification.inc's OMVRpcServiceEmailNotification
// class, engined/module/postfix.inc, and the
// rpc.emailnotification/conf.system.notification.email datamodels):
//   - service "EmailNotification", methods "get"/"set" (plus
//     "sendTestEmail", not wrapped here -- not a meaningful concept for a
//     declarative resource, and it refuses to run while "postfix" is
//     dirty, i.e. right after this resource's own Create/Update, so it
//     wouldn't usefully compose with `terraform apply` anyway).
//   - ANOTHER Rsync-style get()-vs-set() shape divergence, this time via
//     nested-vs-flat structure rather than array-vs-string: the
//     underlying conf.system.notification.email config object stores
//     authentication.enable/username/password NESTED, but get() flattens
//     them into top-level authenable/username/password (via
//     $object->copy(...) + $object->remove("authentication")) before
//     returning -- while set()'s response is the RAW object, still
//     nested under "authentication", with NO flat aliases at all.
//     Decoding set()'s response with the same flat struct used for
//     get() wouldn't crash (unlike Rsync's array/string mismatch) but
//     would SILENTLY zero out authenable/username/password in Terraform
//     state after every Create/Update, since those flat keys simply
//     aren't present in that response. Same fix as Rsync: Create/Update
//     discard set()'s response and immediately re-fetch via get() for
//     the canonical, correctly-flattened representation.
//   - UNLIKE the cert resources, get() DOES return the real stored
//     "password" value in plaintext ($object->copy("authentication.
//     password", "password") -- no stripping). So smtp_password is
//     refreshed normally on every Read here, with none of the "never
//     trust the read, fall back to empty after import" treatment
//     ssl/ssh certificate private keys and the rsync password need --
//     that pattern is specific to RPC services that deliberately strip
//     secrets from get(), and this one doesn't.
//   - all fields are required by rpc.emailnotification.json's "set"
//     schema, so Create/Update always send the full object, same as
//     every other resource here.
//   - modifying settings marks the "postfix" engine module dirty
//     (Postfix::bindListeners() in engined/module/postfix.inc) -- that's
//     Postfix, OMV's actual outgoing-mail MTA, not a module literally
//     named "email" or "notification".
//   - like omv_workbench_settings, the underlying config object is a
//     genuine singleton ("iterable": false) -- there is exactly one per
//     OMV instance, so this resource uses the same fixed synthetic
//     Terraform id and has no real "delete" (removing it from Terraform
//     state doesn't reset OMV's settings; there's no RPC to do that).
var (
	_ resource.Resource                   = &NotificationSettingsResource{}
	_ resource.ResourceWithConfigure      = &NotificationSettingsResource{}
	_ resource.ResourceWithImportState    = &NotificationSettingsResource{}
	_ resource.ResourceWithValidateConfig = &NotificationSettingsResource{}
)

// notificationSettingsID is the fixed, synthetic Terraform resource ID
// for this singleton -- see this file's top doc comment and
// workbench_settings_resource.go's identical pattern.
const notificationSettingsID = "settings"

// dirtiedByNotificationSettingsChanges is the engine module OMV marks
// dirty whenever conf.system.notification.email changes
// (Postfix::bindListeners() in engined/module/postfix.inc).
var dirtiedByNotificationSettingsChanges = []string{"postfix"}

// notificationEmailRegexp is a best-effort, NON-authoritative client-side
// sanity check. OMV's own "email" format validator delegates to PHP's
// FILTER_VALIDATE_EMAIL builtin (datamodel/schema.inc), which has no
// documented regex to mirror verbatim the way this provider's other
// format validators do -- so, unlike those, this one does not claim to
// exactly replicate OMV's server-side check, only to catch obviously
// malformed input early. Empty string is always allowed (these fields
// are optional at the config-object level; "required" at the RPC layer
// just means the key must be present, not non-empty -- confirmed by the
// same rpc.emailnotification.json schema allowing `"maxLength": 0` as an
// alternative to the email format).
var notificationEmailRegexp = regexp.MustCompile(`^$|^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func NewNotificationSettingsResource() resource.Resource {
	return &NotificationSettingsResource{}
}

// NotificationSettingsResource implements the omv_notification_settings
// resource.
type NotificationSettingsResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// notificationSettingsResourceModel maps omv_notification_settings
// schema <-> Go.
type notificationSettingsResourceModel struct {
	ID             types.String `tfsdk:"id"`
	Enabled        types.Bool   `tfsdk:"enabled"`
	SMTPServer     types.String `tfsdk:"smtp_server"`
	SMTPPort       types.Int64  `tfsdk:"smtp_port"`
	EncryptionMode types.String `tfsdk:"encryption_mode"`
	SenderEmail    types.String `tfsdk:"sender_email"`
	AuthEnabled    types.Bool   `tfsdk:"auth_enabled"`
	SMTPUsername   types.String `tfsdk:"smtp_username"`
	SMTPPassword   types.String `tfsdk:"smtp_password"`
	PrimaryEmail   types.String `tfsdk:"primary_email"`
	SecondaryEmail types.String `tfsdk:"secondary_email"`
}

func (r *NotificationSettingsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_settings"
}

func (r *NotificationSettingsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages OpenMediaVault's System > Notifications > Settings tab: the outgoing SMTP " +
			"configuration used for system email notifications (the \"EmailNotification\" RPC service). " +
			"Does not cover the per-event notification toggles on that same page's main list (a separate, " +
			"not-yet-implemented \"Notification\" RPC service/config type). Like omv_workbench_settings, " +
			"the underlying config object is a singleton -- there is exactly one per OMV instance, see " +
			"this resource's \"id\" attribute.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Fixed synthetic identifier (\"settings\") -- OMV's underlying config object has no UUID of its own, see this resource's top-level description.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Whether email notifications are sent at all. Corresponds to RPC field \"enable\".",
			},
			"smtp_server": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Outgoing SMTP server hostname or IP, e.g. \"smtp.example.com\". The OMV web " +
					"UI requires a valid hostname/IP here when enabled, but this is a UI-only restriction " +
					"-- the RPC layer accepts any string -- so this provider does not enforce a format " +
					"either. Corresponds to RPC field \"server\".",
			},
			"smtp_port": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(25),
				Description: "SMTP port, e.g. 25, 465, or 587.",
				Validators:  []validator.Int64{fwint64validator.Between(1, 65535)},
			},
			"encryption_mode": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("none"),
				Description: "\"none\", \"ssl\" (implicit TLS), \"starttls\", or \"auto\". Corresponds to RPC field \"tls\".",
				Validators:  []validator.String{fwstringvalidator.OneOf("none", "ssl", "starttls", "auto")},
			},
			"sender_email": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "The \"From\" address for notification emails. Required (non-empty) when enabled. Corresponds to RPC field \"sender\".",
				Validators:  []validator.String{fwstringvalidator.RegexMatches(notificationEmailRegexp, "must be empty or look like a valid email address")},
			},
			"auth_enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Whether the SMTP server requires authentication. Corresponds to RPC field \"authenable\" (nested under \"authentication.enable\" in OMV's config datamodel, but flat here as in the RPC layer).",
			},
			"smtp_username": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "SMTP authentication username. Required when auth_enabled is true. Corresponds to RPC field \"username\" (nested under \"authentication.username\" in OMV's config datamodel).",
			},
			"smtp_password": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Sensitive: true,
				Description: "SMTP authentication password. Required when auth_enabled is true. Corresponds " +
					"to RPC field \"password\" (nested under \"authentication.password\" in OMV's config " +
					"datamodel). Unlike the certificate resources' private keys, this IS returned in " +
					"plaintext by get() and so is refreshed normally on every Read -- see this file's top " +
					"doc comment.",
			},
			"primary_email": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Primary recipient for notification emails. Required when enabled. Corresponds to RPC field \"primaryemail\".",
				Validators:  []validator.String{fwstringvalidator.RegexMatches(notificationEmailRegexp, "must be empty or look like a valid email address")},
			},
			"secondary_email": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Optional additional recipient for notification emails. Corresponds to RPC field \"secondaryemail\".",
				Validators:  []validator.String{fwstringvalidator.RegexMatches(notificationEmailRegexp, "must be empty or look like a valid email address")},
			},
		},
	}
}

func (r *NotificationSettingsResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config notificationSettingsResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Best-effort mirror of the OMV web UI's own requiredIf constraints.
	// Only checked when the relevant values are explicitly known at
	// config-validation time (not null/unknown), for the same reason as
	// omv_workbench_settings.ValidateConfig: defaults haven't been
	// applied yet at this point, so a value left to its default looks
	// identical to one the user genuinely omitted, and treating that as
	// an error would produce false positives.
	boolKnown := func(v types.Bool) bool { return !v.IsNull() && !v.IsUnknown() }
	strKnown := func(v types.String) bool { return !v.IsNull() && !v.IsUnknown() }

	if boolKnown(config.Enabled) && config.Enabled.ValueBool() {
		if strKnown(config.SMTPServer) && config.SMTPServer.ValueString() == "" {
			resp.Diagnostics.AddAttributeError(path.Root("smtp_server"), "Missing SMTP Server", "smtp_server is required when enabled is true.")
		}
		if strKnown(config.SenderEmail) && config.SenderEmail.ValueString() == "" {
			resp.Diagnostics.AddAttributeError(path.Root("sender_email"), "Missing Sender Email", "sender_email is required when enabled is true.")
		}
		if strKnown(config.PrimaryEmail) && config.PrimaryEmail.ValueString() == "" {
			resp.Diagnostics.AddAttributeError(path.Root("primary_email"), "Missing Primary Email", "primary_email is required when enabled is true.")
		}
	}

	if boolKnown(config.AuthEnabled) && config.AuthEnabled.ValueBool() {
		if strKnown(config.SMTPUsername) && config.SMTPUsername.ValueString() == "" {
			resp.Diagnostics.AddAttributeError(path.Root("smtp_username"), "Missing SMTP Username", "smtp_username is required when auth_enabled is true.")
		}
		if strKnown(config.SMTPPassword) && config.SMTPPassword.ValueString() == "" {
			resp.Diagnostics.AddAttributeError(path.Root("smtp_password"), "Missing SMTP Password", "smtp_password is required when auth_enabled is true.")
		}
	}
}

func (r *NotificationSettingsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T. This is a provider bug, please report it.", req.ProviderData),
		)
		return
	}
	r.client = pd.Client
	r.revertOnApplyFailure = pd.RevertOnApplyFailure
}

// emailNotificationRPCObject is the shape of the conf.system.notification.
// email object as returned by EmailNotification.get() -- flat, matching
// the "requires a get()-refetch after set()" pattern described in this
// file's top doc comment. NOT safe to decode a set() response into this
// struct (its authenable/username/password would silently come back
// zero-valued); Create/Update never do that -- see them below.
type emailNotificationRPCObject struct {
	Enable         bool   `json:"enable"`
	Server         string `json:"server"`
	Port           int64  `json:"port"`
	TLS            string `json:"tls"`
	Sender         string `json:"sender"`
	AuthEnable     bool   `json:"authenable"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	PrimaryEmail   string `json:"primaryemail"`
	SecondaryEmail string `json:"secondaryemail"`
}

func toEmailNotificationRPCObject(m *notificationSettingsResourceModel) map[string]interface{} {
	// A plain map, not emailNotificationRPCObject, is used for the
	// request specifically because that struct's json tags describe
	// get()'s flat response shape, which happens to be identical to what
	// set() accepts too (both are flat "authenable"/"username"/"password"
	// at the request/RPC-params level -- it's only set()'s RESPONSE that
	// diverges by returning the raw nested object instead). Using the
	// same struct for both directions here would be a small trap for a
	// future edit to "help" by decoding set()'s response with it, so the
	// request is deliberately built as an unrelated type.
	return map[string]interface{}{
		"enable":         m.Enabled.ValueBool(),
		"server":         m.SMTPServer.ValueString(),
		"port":           m.SMTPPort.ValueInt64(),
		"tls":            m.EncryptionMode.ValueString(),
		"sender":         m.SenderEmail.ValueString(),
		"authenable":     m.AuthEnabled.ValueBool(),
		"username":       m.SMTPUsername.ValueString(),
		"password":       m.SMTPPassword.ValueString(),
		"primaryemail":   m.PrimaryEmail.ValueString(),
		"secondaryemail": m.SecondaryEmail.ValueString(),
	}
}

func fromEmailNotificationRPCObject(obj *emailNotificationRPCObject, m *notificationSettingsResourceModel) {
	m.ID = types.StringValue(notificationSettingsID)
	m.Enabled = types.BoolValue(obj.Enable)
	m.SMTPServer = types.StringValue(obj.Server)
	m.SMTPPort = types.Int64Value(obj.Port)
	m.EncryptionMode = types.StringValue(obj.TLS)
	m.SenderEmail = types.StringValue(obj.Sender)
	m.AuthEnabled = types.BoolValue(obj.AuthEnable)
	m.SMTPUsername = types.StringValue(obj.Username)
	m.SMTPPassword = types.StringValue(obj.Password)
	m.PrimaryEmail = types.StringValue(obj.PrimaryEmail)
	m.SecondaryEmail = types.StringValue(obj.SecondaryEmail)
}

// setThenRefetch calls EmailNotification.set() (discarding its response,
// which isn't safe to decode -- see this file's top doc comment), then
// EmailNotification.get() for the canonical, correctly-flattened result.
func (r *NotificationSettingsResource) setThenRefetch(ctx context.Context, m *notificationSettingsResourceModel) error {
	if err := r.client.Call(ctx, "EmailNotification", "set", toEmailNotificationRPCObject(m), nil); err != nil {
		return err
	}
	var full emailNotificationRPCObject
	if err := r.client.Call(ctx, "EmailNotification", "get", nil, &full); err != nil {
		return err
	}
	fromEmailNotificationRPCObject(&full, m)
	return nil
}

func (r *NotificationSettingsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan notificationSettingsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.setThenRefetch(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Error Setting Notification Settings", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *NotificationSettingsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state notificationSettingsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found emailNotificationRPCObject
	if err := r.client.Call(ctx, "EmailNotification", "get", nil, &found); err != nil {
		resp.Diagnostics.AddError("Error Reading Notification Settings", err.Error())
		return
	}
	fromEmailNotificationRPCObject(&found, &state)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *NotificationSettingsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan notificationSettingsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.setThenRefetch(ctx, &plan); err != nil {
		resp.Diagnostics.AddError(
			"Error Updating Notification Settings",
			fmt.Sprintf(
				"%s. Note this may still have partially written the config database before failing -- if "+
					"this looked like a client-side timeout, check whether the change actually applied "+
					"before retrying (see this provider's deploy_timeout_seconds).",
				err,
			),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *NotificationSettingsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Same rationale as omv_workbench_settings.Delete: conf.system.
	// notification.email is a singleton that always exists in OMV, with
	// no RPC to "delete" it, and resetting live SMTP credentials as a
	// side effect of `terraform destroy` would be a surprising,
	// operationally risky default. Delete stops Terraform from managing
	// this resource and leaves OMV's settings exactly as they are.
	resp.Diagnostics.AddWarning(
		"Notification Settings Left Unchanged",
		"omv_notification_settings has no real \"delete\" operation in OMV -- the underlying settings "+
			"object always exists. Removing this resource from Terraform state does not reset the SMTP "+
			"configuration; it remains exactly as last configured.",
	)
}

func (r *NotificationSettingsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID != notificationSettingsID {
		resp.Diagnostics.AddError(
			"Unexpected Import Identifier",
			fmt.Sprintf(
				"omv_notification_settings has exactly one instance per OMV system and must be imported "+
					"using the fixed identifier %q, e.g.:\n\n"+
					"  terraform import omv_notification_settings.<name> %s\n\ngot: %q",
				notificationSettingsID, notificationSettingsID, req.ID,
			),
		)
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyOrHandleApplyFailure delegates to the shared implementation in
// apply_helper.go (see its doc comment for the full rationale, including
// the client-side-timeout-vs-real-failure distinction), scoped to the
// modules a notification settings change dirties.
func (r *NotificationSettingsResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	applyOrHandleApplyFailure(ctx, r.client, r.revertOnApplyFailure, dirtiedByNotificationSettingsChanges, diags)
}
