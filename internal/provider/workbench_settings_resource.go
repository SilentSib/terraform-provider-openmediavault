package provider

import (
	"context"
	"fmt"

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

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// This resource manages OMV's "System > Workbench > Settings" page --
// the web UI's own listen port, session auto-logout timeout, and
// SSL/TLS settings. Verified against the OMV 8.5.5 source:
//   - the frontend page (workbench-settings-form-page.component.ts) is
//     backed by RPC service "WebGui", methods "getSettings"/"setSettings".
//   - the underlying config object is "conf.webadmin", which datamodels/
//     conf.webadmin.json marks "iterable: false" -- this is a genuine
//     SINGLETON, unlike every other resource in this provider: there is
//     no UUID, no list of objects, just one implicit settings object that
//     always exists (OMV seeds it with defaults at install time). This
//     resource therefore uses a fixed, synthetic Terraform ID
//     (workbenchSettingsID) rather than one assigned by OMV.
//   - getSettings()/setSettings() are, unlike Rsync's get()/set(), safe to
//     decode with the SAME struct: both return $object->getAssoc()
//     directly with no post-processing, so there's no set()-vs-get()
//     shape divergence to work around here.
//   - all 6 fields (port, enablessl, sslport, forcesslonly,
//     sslcertificateref, timeout) are required by rpc.webgui.json's
//     "setSettings" schema, so Create/Update always send the full object,
//     same pattern as every other resource in this provider.
//   - changing these settings marks the "webserver" (nginx's actual
//     module name -- see engined/module/nginx.inc's getName(), NOT
//     "nginx") and "monit" engine modules dirty
//     (engined/module/webadmin.inc), both of which need deploying via
//     Config.applyChanges for the new port/SSL config to actually take
//     effect (regenerating nginx's config and restarting it).
//
// IMPORTANT OPERATIONAL WARNING, called out again on the resource and
// relevant attributes' schema descriptions: this resource controls how
// EVERY client -- including this provider's own connection -- reaches
// the OMV instance. Applying a change to port/enable_ssl/ssl_port/
// force_ssl_only can (a) make the RPC call that applies it look like it
// failed, even though it succeeded, if nginx restarts before the HTTP
// response finishes; and (b) require updating this provider's own
// host/port/scheme configuration to match before the NEXT `terraform
// plan`/`apply` can connect at all.
var (
	_ resource.Resource                   = &WorkbenchSettingsResource{}
	_ resource.ResourceWithConfigure      = &WorkbenchSettingsResource{}
	_ resource.ResourceWithImportState    = &WorkbenchSettingsResource{}
	_ resource.ResourceWithValidateConfig = &WorkbenchSettingsResource{}
)

// workbenchSettingsID is a fixed, synthetic Terraform resource ID: OMV's
// conf.webadmin config object has no UUID of its own (it's a singleton,
// see the doc comment above), so there's nothing real to use as an "id".
// `terraform import omv_workbench_settings.<name> settings` is this
// resource's import syntax -- the literal string "settings" is the only
// valid value, since there's only ever one of these per OMV instance.
const workbenchSettingsID = "settings"

// dirtiedByWorkbenchSettingsChanges are the engine modules OMV marks dirty
// whenever conf.webadmin changes (Webadmin::bindListeners() in
// engined/module/webadmin.inc). Note "webserver" is nginx's module name,
// not "nginx" -- see engined/module/nginx.inc's getName().
var dirtiedByWorkbenchSettingsChanges = []string{"webserver", "monit"}

func NewWorkbenchSettingsResource() resource.Resource {
	return &WorkbenchSettingsResource{}
}

// WorkbenchSettingsResource implements the omv_workbench_settings resource.
type WorkbenchSettingsResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// workbenchSettingsResourceModel maps omv_workbench_settings schema <-> Go.
type workbenchSettingsResourceModel struct {
	ID               types.String `tfsdk:"id"`
	Port             types.Int64  `tfsdk:"port"`
	Timeout          types.Int64  `tfsdk:"auto_logout_minutes"`
	EnableSSL        types.Bool   `tfsdk:"enable_ssl"`
	SSLPort          types.Int64  `tfsdk:"ssl_port"`
	ForceSSLOnly     types.Bool   `tfsdk:"force_ssl_only"`
	SSLCertificateID types.String `tfsdk:"ssl_certificate_id"`
}

func (r *WorkbenchSettingsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_workbench_settings"
}

func (r *WorkbenchSettingsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages OpenMediaVault's System > Workbench > Settings page: the web UI's own " +
			"listen port, session auto-logout timeout, and SSL/TLS settings (the \"WebGui\" RPC service, " +
			"backed by the singleton \"conf.webadmin\" config object -- there is exactly one of these per " +
			"OMV instance, see this file's top doc comment for why the \"id\" is a fixed literal rather " +
			"than an OMV-assigned UUID). " +
			"WARNING: this resource controls how every client -- including this provider's own connection " +
			"-- reaches OMV. Changing port/enable_ssl/ssl_port/force_ssl_only can require updating this " +
			"provider's own host/port/scheme configuration before the next plan/apply will connect, and " +
			"the apply that changes them may itself report a connection error even when the change " +
			"succeeded, if the web server restarts before the response finishes.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Fixed synthetic identifier (\"settings\") -- OMV's underlying config object has no UUID of its own, see this resource's top-level description.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"port": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(80),
				Description: "TCP port the web UI listens on for plain HTTP. Must differ from ssl_port " +
					"if enable_ssl is true (checked at plan time on a best-effort basis; OMV's own RPC " +
					"does not enforce this server-side, only nginx failing to start would catch it).",
				Validators: []validator.Int64{fwint64validator.Between(1, 65535)},
			},
			"auto_logout_minutes": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(5),
				Description: "Minutes of inactivity before the web UI session is automatically logged " +
					"out. 0 disables automatic logout. Corresponds to RPC field \"timeout\".",
				Validators: []validator.Int64{fwint64validator.Between(0, 1440)},
			},
			"enable_ssl": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Whether the web UI also accepts HTTPS connections on ssl_port. Corresponds to RPC field \"enablessl\".",
			},
			"ssl_port": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(443),
				Description: "TCP port the web UI listens on for HTTPS, when enable_ssl is true.",
				Validators:  []validator.Int64{fwint64validator.Between(1, 65535)},
			},
			"force_ssl_only": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "When enable_ssl is true, redirect/reject plain HTTP instead of also serving it. Corresponds to RPC field \"forcesslonly\".",
			},
			"ssl_certificate_id": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "UUID of the SSL certificate configuration object to use for HTTPS. Required " +
					"(non-empty) when enable_ssl is true. Corresponds to RPC field \"sslcertificateref\".",
			},
		},
	}
}

func (r *WorkbenchSettingsResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config workbenchSettingsResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Best-effort mirror of the web UI's own cross-field check (see
	// workbench-settings-form-page.component.ts's "custom" validator on
	// sslport). Only checked when the user has explicitly set BOTH
	// port and ssl_port in configuration -- if either is left to its
	// default, its value isn't known yet at ValidateConfig time (defaults
	// are applied later, during planning), so there's nothing reliable to
	// compare. This is a convenience check, not an authoritative one: OMV
	// itself doesn't validate this server-side either.
	if config.EnableSSL.ValueBool() &&
		!config.Port.IsNull() && !config.Port.IsUnknown() &&
		!config.SSLPort.IsNull() && !config.SSLPort.IsUnknown() &&
		config.Port.ValueInt64() == config.SSLPort.ValueInt64() {
		resp.Diagnostics.AddAttributeError(
			path.Root("ssl_port"),
			"Conflicting Port Configuration",
			"port and ssl_port must not be the same value when enable_ssl is true.",
		)
	}

	if config.EnableSSL.ValueBool() &&
		!config.SSLCertificateID.IsNull() && !config.SSLCertificateID.IsUnknown() &&
		config.SSLCertificateID.ValueString() == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("ssl_certificate_id"),
			"Missing SSL Certificate",
			"ssl_certificate_id is required when enable_ssl is true.",
		)
	}
}

func (r *WorkbenchSettingsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// webadminRPCObject is the shape of the conf.webadmin object as consumed/
// returned by WebGui.getSettings/setSettings -- both are safe to decode
// into this same struct (unlike Rsync's get()/set(), see this file's top
// doc comment).
type webadminRPCObject struct {
	Port              int64  `json:"port"`
	EnableSSL         bool   `json:"enablessl"`
	SSLPort           int64  `json:"sslport"`
	ForceSSLOnly      bool   `json:"forcesslonly"`
	SSLCertificateRef string `json:"sslcertificateref"`
	Timeout           int64  `json:"timeout"`
}

func toWebadminRPCObject(m *workbenchSettingsResourceModel) webadminRPCObject {
	return webadminRPCObject{
		Port:              m.Port.ValueInt64(),
		EnableSSL:         m.EnableSSL.ValueBool(),
		SSLPort:           m.SSLPort.ValueInt64(),
		ForceSSLOnly:      m.ForceSSLOnly.ValueBool(),
		SSLCertificateRef: m.SSLCertificateID.ValueString(),
		Timeout:           m.Timeout.ValueInt64(),
	}
}

func fromWebadminRPCObject(obj *webadminRPCObject, m *workbenchSettingsResourceModel) {
	m.ID = types.StringValue(workbenchSettingsID)
	m.Port = types.Int64Value(obj.Port)
	m.Timeout = types.Int64Value(obj.Timeout)
	m.EnableSSL = types.BoolValue(obj.EnableSSL)
	m.SSLPort = types.Int64Value(obj.SSLPort)
	m.ForceSSLOnly = types.BoolValue(obj.ForceSSLOnly)
	m.SSLCertificateID = types.StringValue(obj.SSLCertificateRef)
}

func (r *WorkbenchSettingsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan workbenchSettingsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := toWebadminRPCObject(&plan)

	var result webadminRPCObject
	if err := r.client.Call(ctx, "WebGui", "setSettings", obj, &result); err != nil {
		resp.Diagnostics.AddError("Error Setting Workbench Settings", err.Error())
		return
	}
	fromWebadminRPCObject(&result, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *WorkbenchSettingsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state workbenchSettingsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found webadminRPCObject
	if err := r.client.Call(ctx, "WebGui", "getSettings", nil, &found); err != nil {
		resp.Diagnostics.AddError("Error Reading Workbench Settings", err.Error())
		return
	}
	fromWebadminRPCObject(&found, &state)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *WorkbenchSettingsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan workbenchSettingsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := toWebadminRPCObject(&plan)

	var result webadminRPCObject
	if err := r.client.Call(ctx, "WebGui", "setSettings", obj, &result); err != nil {
		resp.Diagnostics.AddError(
			"Error Updating Workbench Settings",
			fmt.Sprintf(
				"%s. If this changed port/enable_ssl/ssl_port/force_ssl_only, note that this error "+
					"could also mean the change actually succeeded but the web server restarted before "+
					"this provider received the response -- check the OMV web UI (possibly on a new "+
					"port/scheme) and this provider's own host/port/scheme configuration before retrying.",
				err,
			),
		)
		return
	}
	fromWebadminRPCObject(&result, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *WorkbenchSettingsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// conf.webadmin is a singleton that always exists in OMV -- there is
	// no RPC method to "delete" it, and resetting the live web UI's
	// port/SSL settings to some default as part of `terraform destroy`
	// would be a surprising, operationally risky side effect (it could
	// change how the instance needs to be reached going forward). So
	// Delete simply stops Terraform from managing this resource and
	// leaves OMV's current settings exactly as they are; it does not
	// call any RPC method.
	resp.Diagnostics.AddWarning(
		"Workbench Settings Left Unchanged",
		"omv_workbench_settings has no real \"delete\" operation in OMV -- the underlying settings "+
			"object always exists. Removing this resource from Terraform state does not reset the web "+
			"UI's port/timeout/SSL settings; they remain exactly as last configured.",
	)
}

func (r *WorkbenchSettingsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID != workbenchSettingsID {
		resp.Diagnostics.AddError(
			"Unexpected Import Identifier",
			fmt.Sprintf(
				"omv_workbench_settings has exactly one instance per OMV system and must be imported "+
					"using the fixed identifier %q, e.g.:\n\n"+
					"  terraform import omv_workbench_settings.<name> %s\n\ngot: %q",
				workbenchSettingsID, workbenchSettingsID, req.ID,
			),
		)
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyOrHandleApplyFailure mirrors SharedFolderResource's method of the
// same name (see shared_folder_resource.go for the full rationale),
// scoped to the modules a workbench settings change dirties.
func (r *WorkbenchSettingsResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	if _, err := r.client.ApplyChanges(ctx, dirtiedByWorkbenchSettingsChanges, false); err != nil {
		if r.revertOnApplyFailure {
			if revertErr := r.client.RevertChanges(ctx, ""); revertErr != nil {
				diags.AddError(
					"Failed to Apply Changes, and the Automatic Revert Also Failed",
					fmt.Sprintf(
						"Applying the configuration change failed: %s. The provider then tried to revert ALL "+
							"pending configuration changes (revert_on_apply_failure = true), but that also "+
							"failed: %s. OMV's configuration database and the actual system state may now be "+
							"inconsistent -- check the OMV web UI's pending changes panel.",
						err, revertErr,
					),
				)
				return
			}
			diags.AddError(
				"Failed to Apply Changes; Reverted All Pending Changes",
				fmt.Sprintf(
					"Applying the configuration change failed: %s. Because revert_on_apply_failure = true, "+
						"the provider reverted ALL pending configuration changes on this OMV instance "+
						"(equivalent to clicking \"Undo\" in the web UI) -- including any unrelated changes "+
						"staged by other admins or tools, not just this resource's. This resource remains "+
						"recorded in Terraform state pointing at settings that may no longer reflect what "+
						"was just planned; run `terraform plan` again to reconcile.",
					err,
				),
			)
			return
		}
		diags.AddError(
			"Configuration Written, but Deploying It Failed",
			fmt.Sprintf(
				"The change was written to OMV's configuration database, but deploying it "+
					"(Config.applyChanges) failed: %s. As in the OMV web UI, the change has NOT been "+
					"automatically undone -- it remains queued. Fix the underlying issue and run "+
					"`terraform apply` again, or resolve it manually (Apply/Undo) in the OMV web UI. Set "+
					"the provider's revert_on_apply_failure = true to have Terraform automatically call "+
					"Config.revertChanges instead, noting that discards ALL pending changes instance-wide, "+
					"not just this resource's.",
				err,
			),
		)
	}
}
