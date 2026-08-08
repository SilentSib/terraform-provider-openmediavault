package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// This resource manages ONE entry from OMV's "System > Notifications"
// page's main list -- the per-event on/off toggles for which system
// events trigger an email notification (the outgoing SMTP configuration
// itself is the separate omv_notification_settings resource). Verified
// against the OMV 8.5.5 source (engined/rpc/notification.inc's
// Notification class, the various engined/module/*.inc files
// implementing INotification, and the
// rpc.notification/conf.system.notification.notification datamodels).
// This one works differently enough from every other resource in this
// provider that it's worth reading this comment in full before touching
// the code below.
//
//   - THE SET OF VALID event_id VALUES IS NOT FIXED OR ENUMERABLE FROM
//     THE DATAMODEL. Notification::getNotificationConfig() builds the
//     list at runtime: two hardcoded built-ins ("authentication",
//     "misc") plus whatever every engine module implementing
//     \OMV\Engine\Module\INotification contributes via its own
//     getNotificationConfig(). In stock OMV 8.5.5 (no extra plugins)
//     that's: "monitprocevents", "monitloadavg", "monitcpuusage",
//     "monitmemoryusage", "monitfilesystems" (all from Monit),
//     "apt" (APT), and "smartmontools" (S.M.A.R.T.) -- 9 total,
//     verified by grepping every engined/module/*.inc for
//     getNotificationConfig()'s "id" entries, not guessed. Installing
//     additional plugins (RAID, UPS, ZFS, Docker, etc.) can add more;
//     check the OMV web UI's Notifications list for what's available on
//     a specific instance. Setting event_id to something not recognized
//     doesn't error -- Notification.set() has no validation against the
//     known list -- it just silently creates an orphaned config object
//     no listener will ever act on.
//   - THERE IS NO "GET BY id" RPC METHOD, only "get" (by OMV-assigned
//     UUID) and "getList" (every event, including synthesized
//     not-yet-configured ones with uuid == "" -- confirmed from source:
//     ConfigObject's generic type-based default for an unspecified
//     string property, which "uuid" is here, is "", and getList()
//     explicitly does NOT set uuid when synthesizing a default entry for
//     an event with no persisted object yet). So finding the UUID for a
//     given event_id requires fetching the whole list and filtering
//     client-side -- this resource only does that in Create and
//     ImportState; Read and Update use the plain "get"/"set" by UUID
//     once Create has established a stable one, same as every other
//     resource here.
//   - Create must distinguish two cases after that lookup: no persisted
//     object exists yet for this event_id (uuid == "") -> create one
//     with the omvclient.NewObjectUUID sentinel; OR one already exists
//     (e.g. toggled via the web UI, or by a previous apply outside
//     Terraform's current state) -> reuse that UUID rather than creating
//     a second object with the same event_id. Notification.set() has NO
//     uniqueness constraint on "id" (unlike ShareMgmt/Smb's
//     assertIsUnique elsewhere in this provider) -- creating a duplicate
//     wouldn't error immediately, but getList()'s own getByFilter(...,
//     limit=1) call would then start throwing for that event_id, since
//     it explicitly rejects multiple matches. Getting the adopt-vs-create
//     branch right is the single most important correctness property of
//     this resource.
//   - There is no "delete" RPC method for individual notification
//     objects at all. Unlike the singleton settings resources in this
//     provider (where "delete" leaving OMV's settings untouched is the
//     safe choice, since there's no unambiguous safe reset value),
//     "disabled" IS the clear, safe, well-defined default state for a
//     notification toggle -- it's exactly what "getList() synthesizes
//     for a never-configured event" already means. So Delete here calls
//     set() with enable=false rather than leaving state untouched;
//     documented explicitly since it's a deliberately different choice
//     from workbench/notification *settings* Delete's rationale.
//   - creating/modifying a notification event object marks up to five
//     engine modules dirty depending on WHICH event_id: "rsyslog" (note:
//     NOT "syslog" -- module file syslog.inc, getName() "rsyslog"),
//     "monit", "apt", "apticron", "smartmontools" (all confirmed via
//     each module's own bindListeners()). This resource always requests
//     all five; Config.applyChanges only acts on whichever are actually
//     dirty (verified from source), so requesting ones that aren't dirty
//     for a given event_id is a harmless no-op.
var (
	_ resource.Resource                = &NotificationEventResource{}
	_ resource.ResourceWithConfigure   = &NotificationEventResource{}
	_ resource.ResourceWithImportState = &NotificationEventResource{}
)

// dirtiedByNotificationEventChanges is the superset of engine modules a
// notification event change can dirty -- see this file's top doc comment.
var dirtiedByNotificationEventChanges = []string{"rsyslog", "monit", "apt", "apticron", "smartmontools"}

// knownNotificationEventIDs are the event_id values verified (by reading
// every engined/module/*.inc implementing INotification, not guessed)
// to exist in stock OMV 8.5.5 with no additional plugins installed. Used
// only for error messages/documentation -- never enforced as a strict
// allow-list, since plugin-provided IDs are legitimate and can't be
// known in advance.
var knownNotificationEventIDs = []string{
	"authentication", "misc", "monitprocevents", "monitloadavg",
	"monitcpuusage", "monitmemoryusage", "monitfilesystems", "apt",
	"smartmontools",
}

func NewNotificationEventResource() resource.Resource {
	return &NotificationEventResource{}
}

// NotificationEventResource implements the omv_notification_event
// resource.
type NotificationEventResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// notificationEventResourceModel maps omv_notification_event schema <->
// Go.
type notificationEventResourceModel struct {
	ID      types.String `tfsdk:"id"`
	EventID types.String `tfsdk:"event_id"`
	Enabled types.Bool   `tfsdk:"enabled"`
}

func (r *NotificationEventResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_event"
}

func (r *NotificationEventResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a single per-event notification toggle from OpenMediaVault's System > " +
			"Notifications page (email delivery settings themselves are the separate " +
			"omv_notification_settings resource). event_id's valid values depend on which OMV modules/" +
			"plugins are installed -- see this resource's top-level source comment for the full verified " +
			"list on stock OMV 8.5.5 (\"authentication\", \"misc\", \"monitprocevents\", \"monitloadavg\", " +
			"\"monitcpuusage\", \"monitmemoryusage\", \"monitfilesystems\", \"apt\", \"smartmontools\") -- " +
			"an unrecognized event_id is NOT rejected by OMV, it just silently does nothing.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "UUID OMV assigns to the underlying notification config object.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"event_id": schema.StringAttribute{
				Required: true,
				Description: "Which event this toggle controls, e.g. \"smartmontools\" or \"apt\" -- see " +
					"this resource's description for known values. Changing this on an existing resource " +
					"replaces it (re-targets a different event rather than renaming the current one).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Whether this event triggers an email notification. Defaults to false, " +
					"matching what OMV itself treats an unconfigured event as (see getList()'s synthesized " +
					"default in this file's top comment) -- NOT the conf.system.notification.notification " +
					"datamodel's own declared default of true, which in practice is only ever used as a " +
					"fallback inside ConfigObject and is overridden by getList() before a caller ever sees " +
					"it.",
			},
		},
	}
}

func (r *NotificationEventResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// notificationEventRPCObject is the shape of a notification event object
// as consumed/returned by Notification's get/set/getList methods. Unlike
// several other resources in this provider, get()/set() have no shape
// divergence to work around here -- set()'s response is exactly
// $object->getAssoc() with no additional flattening/stripping, matching
// this same struct. (getList()'s entries additionally carry synthesized
// "title"/"type" display fields this provider has no use for and simply
// ignores via the missing struct tags.)
type notificationEventRPCObject struct {
	UUID   string `json:"uuid"`
	ID     string `json:"id"`
	Enable bool   `json:"enable"`
}

// findNotificationEventByID calls Notification.getList() and returns the
// entry matching eventID, or nil if OMV doesn't recognize that event_id
// at all (as opposed to recognizing it but not yet having a persisted
// object -- that case returns a non-nil entry with UUID == "").
func (r *NotificationEventResource) findNotificationEventByID(ctx context.Context, eventID string) (*notificationEventRPCObject, error) {
	var list []notificationEventRPCObject
	if err := r.client.Call(ctx, "Notification", "getList", nil, &list); err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == eventID {
			return &list[i], nil
		}
	}
	return nil, nil
}

func unrecognizedEventIDError(eventID string) string {
	return fmt.Sprintf(
		"OMV does not recognize event_id %q. Known built-in values on stock OMV 8.5.5 are: %s. "+
			"Additional plugins can register more -- check the OMV web UI's System > Notifications list "+
			"for what's available on this specific instance. Note OMV does not reject unrecognized "+
			"event_id values at the RPC layer; it would silently create a config object no listener acts "+
			"on, which is why this provider checks proactively instead.",
		eventID, strings.Join(knownNotificationEventIDs, ", "),
	)
}

func (r *NotificationEventResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan notificationEventResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	eventID := plan.EventID.ValueString()

	existing, err := r.findNotificationEventByID(ctx, eventID)
	if err != nil {
		resp.Diagnostics.AddError("Error Looking Up Notification Event", err.Error())
		return
	}
	if existing == nil {
		resp.Diagnostics.AddError("Unrecognized event_id", unrecognizedEventIDError(eventID))
		return
	}

	// Adopt the existing object's UUID if one is already persisted for
	// this event_id (see this file's top doc comment for why getting
	// this branch right matters); otherwise ask OMV to assign a new one.
	uuid := existing.UUID
	if uuid == "" {
		uuid = omvclient.NewObjectUUID
	}

	var result notificationEventRPCObject
	obj := notificationEventRPCObject{UUID: uuid, ID: eventID, Enable: plan.Enabled.ValueBool()}
	if err := r.client.Call(ctx, "Notification", "set", obj, &result); err != nil {
		resp.Diagnostics.AddError("Error Setting Notification Event", err.Error())
		return
	}

	plan.ID = types.StringValue(result.UUID)
	plan.EventID = types.StringValue(result.ID)
	plan.Enabled = types.BoolValue(result.Enable)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *NotificationEventResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state notificationEventResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found notificationEventRPCObject
	err := r.client.Call(ctx, "Notification", "get", map[string]string{
		"uuid": state.ID.ValueString(),
	}, &found)
	if err != nil {
		// TODO: as elsewhere in this provider, match the specific "object
		// does not exist" RPC error and call resp.State.RemoveResource(ctx)
		// instead, so events deleted out of band can be recreated.
		resp.Diagnostics.AddError("Error Reading Notification Event", err.Error())
		return
	}

	state.ID = types.StringValue(found.UUID)
	state.EventID = types.StringValue(found.ID)
	state.Enabled = types.BoolValue(found.Enable)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *NotificationEventResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan notificationEventResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// event_id has RequiresReplace, so plan.ID (UUID) is always already
	// known and real by the time Update runs -- no lookup needed here.
	var updated notificationEventRPCObject
	obj := notificationEventRPCObject{
		UUID:   plan.ID.ValueString(),
		ID:     plan.EventID.ValueString(),
		Enable: plan.Enabled.ValueBool(),
	}
	if err := r.client.Call(ctx, "Notification", "set", obj, &updated); err != nil {
		resp.Diagnostics.AddError("Error Updating Notification Event", err.Error())
		return
	}

	plan.ID = types.StringValue(updated.UUID)
	plan.EventID = types.StringValue(updated.ID)
	plan.Enabled = types.BoolValue(updated.Enable)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *NotificationEventResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state notificationEventResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// No RPC method exists to delete an individual notification object
	// (see this file's top doc comment) -- the closest, safe equivalent
	// is resetting it to disabled, which is exactly the state OMV itself
	// treats a never-configured event as.
	obj := notificationEventRPCObject{
		UUID:   state.ID.ValueString(),
		ID:     state.EventID.ValueString(),
		Enable: false,
	}
	if err := r.client.Call(ctx, "Notification", "set", obj, nil); err != nil {
		resp.Diagnostics.AddError("Error Disabling Notification Event", err.Error())
		return
	}

	if _, err := r.client.ApplyChanges(ctx, dirtiedByNotificationEventChanges, false); err != nil {
		resp.Diagnostics.AddWarning(
			"Notification Event Disabled, but Deploying the Change Failed",
			fmt.Sprintf(
				"The event was reset to disabled in OMV's configuration, but applying that "+
					"(Config.applyChanges) failed: %s. It may still be enabled in effect until this is "+
					"resolved (retry from the OMV web UI's pending changes panel).",
				err,
			),
		)
	}
}

func (r *NotificationEventResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// The import identifier is event_id (e.g. "smartmontools"), NOT
	// OMV's own UUID -- users won't typically know or want to look up
	// the latter. This means ImportState can't use the usual
	// ImportStatePassthroughID-by-"id" pattern used elsewhere in this
	// provider (that sets "id" from the import string, but here the
	// import string is event_id, and Read() needs a real "id"/UUID
	// already in state to work at all) -- so it does the getList()
	// lookup itself and populates both attributes directly.
	eventID := req.ID

	existing, err := r.findNotificationEventByID(ctx, eventID)
	if err != nil {
		resp.Diagnostics.AddError("Error Looking Up Notification Event", err.Error())
		return
	}
	if existing == nil {
		resp.Diagnostics.AddError("Unrecognized event_id", unrecognizedEventIDError(eventID))
		return
	}
	if existing.UUID == "" {
		resp.Diagnostics.AddError(
			"Nothing to Import",
			fmt.Sprintf(
				"event_id %q is a recognized notification type, but OMV has no persisted configuration "+
					"object for it yet (it's using the implicit disabled-by-default state). There is "+
					"nothing to import -- just declare an omv_notification_event resource with this "+
					"event_id and `terraform apply` it; that will create the object correctly.",
				eventID,
			),
		)
		return
	}

	imported := notificationEventResourceModel{
		ID:      types.StringValue(existing.UUID),
		EventID: types.StringValue(existing.ID),
		Enabled: types.BoolValue(existing.Enable),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &imported)...)
}

// applyOrHandleApplyFailure delegates to the shared implementation in
// apply_helper.go (see its doc comment for the full rationale, including
// the client-side-timeout-vs-real-failure distinction), scoped to the
// modules a notification event change dirties.
func (r *NotificationEventResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	applyOrHandleApplyFailure(ctx, r.client, r.revertOnApplyFailure, dirtiedByNotificationEventChanges, diags)
}
