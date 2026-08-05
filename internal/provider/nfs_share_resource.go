package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	fwstringvalidator "github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// This resource manages OMV's "Services > NFS > Shares" page. Verified
// against the OMV 8.5.5 source (engined/rpc/nfs.inc,
// engined/module/nfs.inc, engined/module/fstab.inc, and the
// rpc.nfs/conf.service.nfs.share datamodels), and it works meaningfully
// differently from omv_smb_share:
//   - service "Nfs", methods "getShare"/"setShare"/"deleteShare" -- same
//     clean shape as Smb (no get()-vs-set() divergence, both just return
//     $object->getAssoc()).
//   - NFS is MANY-TO-ONE where SMB is one-to-one: a NFS "share" is really
//     one (client, options) export rule, and OMV places NO uniqueness
//     constraint on sharedfolderref (no assertIsUnique call, unlike
//     ShareMgmt/Smb) -- the web UI's own hint says so explicitly ("only
//     one client can be configured per share"), meaning multiple
//     omv_nfs_share resources routinely point at the same
//     shared_folder_id, one per client/network needing different rules.
//   - creating the FIRST NFS share for a given shared folder makes OMV
//     silently create a bind mount (a conf.system.filesystem.mountpoint
//     entry binding the shared folder's directory into
//     OMV_NFSD_EXPORT_DIR) via an internal call to the "FsTab" RPC
//     service, and sets "mntentref" to point at it -- overwriting
//     whatever the caller sent for that field, but ONLY on create
//     ($object->isNew()); on update, whatever "mntentref" the caller
//     sends is persisted as-is with no re-validation. This resource
//     therefore treats mount_entry_id as purely Computed (never settable
//     by configuration): sent as omvclient.NewObjectUUID on Create
//     (discarded/overwritten by OMV either way, matching exactly what
//     the web UI's own create form does -- see its hidden "mntentref"
//     field's value, literally "{{ newconfobjuuid }}"), and always
//     resent verbatim from prior state on Update, which
//     UseStateForUnknown makes automatic.
//   - deleting a share checks whether any OTHER NFS share still
//     references the same mntentref; if not, the bind mount is deleted
//     too (again via an internal "FsTab" call). No action needed on this
//     resource's part beyond deploying afterward.
//   - creating/modifying/deleting a share marks the "nfs" engine module
//     dirty; creating or deleting one (not modifying) ALSO marks
//     "zeroconf" dirty (mDNS advertisement); and because of the bind
//     mount machinery above, the FIRST share for a shared folder also
//     dirties "fstab" (which is what actually performs the bind mount on
//     disk -- confirmed via engined/module/fstab.inc, dirtied on
//     conf.system.filesystem.mountpoint CREATE/DELETE). This resource
//     always requests deploying all three ("nfs", "fstab", "zeroconf");
//     Config.applyChanges only acts on whichever of a requested module
//     list are actually dirty (verified from source), so requesting
//     modules that happen not to be dirty in a given operation is a
//     harmless no-op, not an error.
//   - on create, if the target shared folder's relative path contains a
//     space, setShare() rejects it outright (a known SaltStack NFS
//     export limitation, per the source's own comment linking
//     https://github.com/saltstack/salt/issues/54508). Not practical to
//     check client-side without an extra lookup; documented here so the
//     resulting RPC error isn't a mystery.
var (
	_ resource.Resource                = &NFSShareResource{}
	_ resource.ResourceWithConfigure   = &NFSShareResource{}
	_ resource.ResourceWithImportState = &NFSShareResource{}
)

// dirtiedByNFSShareChanges is the superset of engine modules an NFS share
// change can dirty -- see this file's top doc comment for why it's safe
// (and necessary) to always request all three.
var dirtiedByNFSShareChanges = []string{"nfs", "fstab", "zeroconf"}

// nfsExtraOptionsRegexp mirrors conf.service.nfs.share.json's
// "extraoptions" pattern verbatim: a comma-separated list of bare or
// key=value tokens (letters/underscore keys; values limited to
// word-chars, @, :, /), no spaces. Verified empirically via `php -r`
// against OMV's actual pattern (not just read from the datamodel JSON),
// which surfaced two non-obvious constraints: the pattern requires AT
// LEAST ONE token (an empty string does NOT match, and OMV's schema
// validator runs pattern checks unconditionally with no empty-string
// exception -- see the "extra_options" schema attribute's default,
// deliberately non-empty because of this), and the value portion of a
// key=value pair cannot contain hyphens (so hyphenated values, e.g. a
// dashed UUID, are rejected by OMV itself).
var nfsExtraOptionsRegexp = regexp.MustCompile(
	`^(([a-zA-Z_]+)(=([\w@:/]+))?[,])*([a-zA-Z_]+)(=([\w@:/]+))?$`,
)

func NewNFSShareResource() resource.Resource {
	return &NFSShareResource{}
}

// NFSShareResource implements the omv_nfs_share resource.
type NFSShareResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// nfsShareResourceModel maps omv_nfs_share schema <-> Go.
type nfsShareResourceModel struct {
	ID             types.String `tfsdk:"id"`
	SharedFolderID types.String `tfsdk:"shared_folder_id"`
	MountEntryID   types.String `tfsdk:"mount_entry_id"`
	Client         types.String `tfsdk:"client"`
	Options        types.String `tfsdk:"options"`
	Comment        types.String `tfsdk:"comment"`
	ExtraOptions   types.String `tfsdk:"extra_options"`
}

func (r *NFSShareResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_nfs_share"
}

func (r *NFSShareResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a single OpenMediaVault NFS export rule (Services > NFS > Shares). Verified " +
			"against OMV 8.5.5's source. Unlike omv_smb_share, this is many-to-one: OMV places no " +
			"uniqueness constraint on shared_folder_id, since one \"share\" here is really one " +
			"(client, options) export rule -- create multiple omv_nfs_share resources against the same " +
			"shared_folder_id, one per client/network that needs different rules, matching the OMV web " +
			"UI's own \"only one client can be configured per share\" guidance.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "UUID assigned by OMV to this export rule.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"shared_folder_id": schema.StringAttribute{
				Required:    true,
				Description: "UUID of the shared folder to export. Corresponds to RPC field \"sharedfolderref\". Its relative path must not contain spaces (a SaltStack NFS export limitation OMV itself enforces on create).",
			},
			"mount_entry_id": schema.StringAttribute{
				Computed:      true,
				Description:   "UUID of the bind-mount configuration object OMV automatically creates (only for the first NFS share against a given shared folder) to expose it under the NFS export directory. Entirely managed by OMV; never settable here. Corresponds to RPC field \"mntentref\".",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"client": schema.StringAttribute{
				Required: true,
				Description: "The single host/network permitted to mount this export, e.g. \"192.168.1.0/24\", " +
					"a hostname, or \"*\" for any client -- see exports(5). Only one client pattern per " +
					"omv_nfs_share; create additional resources against the same shared_folder_id for " +
					"other clients that need different rules.",
			},
			"options": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("ro"),
				Description: "NFS export options for this rule, comma-separated, exports(5) syntax. The " +
					"OMV web UI only exposes a simple \"ro\"/\"rw\" toggle here, but the underlying field " +
					"accepts the full exports(5) option syntax (e.g. \"rw,no_root_squash,async\") -- this " +
					"provider does not restrict it to ro/rw.",
			},
			"comment": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Free-form description shown in the OMV UI.",
			},
			"extra_options": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("subtree_check,insecure"),
				Description: "Additional comma-separated exports(5) options. Defaults to " +
					"\"subtree_check,insecure\" -- the same value the OMV web UI's own new-share form " +
					"suggests -- NOT to empty, because empty genuinely fails OMV's own validation for " +
					"this field (verified directly against source: conf.service.nfs.share.json's pattern " +
					"requires at least one token, and OMV's schema validator runs pattern checks " +
					"unconditionally, with no empty-string exception). Each token is a bare word or " +
					"key=value pair, comma-separated, no spaces. Note the value portion of a key=value " +
					"pair may only contain word characters (letters/digits/underscore), \"@\", \":\", and " +
					"\"/\" -- NOT hyphens, so hyphenated values like a dashed UUID in \"fsid=uuid:...\" will " +
					"be rejected by OMV itself despite looking reasonable; this is a limitation of OMV's " +
					"own pattern, not of this provider.",
				Validators: []validator.String{
					fwstringvalidator.RegexMatches(nfsExtraOptionsRegexp, "must be a non-empty comma-separated list of bare words or key=value pairs (values: word characters, @, :, / only -- no hyphens), e.g. \"subtree_check,insecure\""),
				},
			},
		},
	}
}

func (r *NFSShareResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// nfsShareRPCObject is the shape of a NFS share object as consumed/
// returned by Nfs's getShare/setShare methods. Both are safe to decode
// with this same struct -- see this file's top doc comment.
type nfsShareRPCObject struct {
	UUID            string `json:"uuid"`
	SharedFolderRef string `json:"sharedfolderref"`
	MountEntryRef   string `json:"mntentref"`
	Client          string `json:"client"`
	Options         string `json:"options"`
	Comment         string `json:"comment"`
	ExtraOptions    string `json:"extraoptions"`
}

func (r *NFSShareResource) toRPCObject(uuid string, m *nfsShareResourceModel) nfsShareRPCObject {
	// mount_entry_id is Computed-only: on Create it's whatever the plan
	// carries (null/unknown -> we substitute the same "please assign a
	// new one" sentinel the web UI's own create form sends, discarded
	// server-side regardless per this file's top doc comment); on Update
	// it's the real value UseStateForUnknown already carried forward from
	// prior state, which MUST be resent verbatim -- setShare() does not
	// re-derive it on update, only on create.
	mountEntryRef := m.MountEntryID.ValueString()
	if m.MountEntryID.IsNull() || m.MountEntryID.IsUnknown() {
		mountEntryRef = omvclient.NewObjectUUID
	}
	return nfsShareRPCObject{
		UUID:            uuid,
		SharedFolderRef: m.SharedFolderID.ValueString(),
		MountEntryRef:   mountEntryRef,
		Client:          m.Client.ValueString(),
		Options:         m.Options.ValueString(),
		Comment:         m.Comment.ValueString(),
		ExtraOptions:    m.ExtraOptions.ValueString(),
	}
}

func fromNFSShareRPCObject(obj *nfsShareRPCObject, m *nfsShareResourceModel) {
	m.ID = types.StringValue(obj.UUID)
	m.SharedFolderID = types.StringValue(obj.SharedFolderRef)
	m.MountEntryID = types.StringValue(obj.MountEntryRef)
	m.Client = types.StringValue(obj.Client)
	m.Options = types.StringValue(obj.Options)
	m.Comment = types.StringValue(obj.Comment)
	m.ExtraOptions = types.StringValue(obj.ExtraOptions)
}

func (r *NFSShareResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan nfsShareResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := r.toRPCObject(omvclient.NewObjectUUID, &plan)

	var created nfsShareRPCObject
	if err := r.client.Call(ctx, "Nfs", "setShare", obj, &created); err != nil {
		resp.Diagnostics.AddError("Error Creating NFS Share", err.Error())
		return
	}
	fromNFSShareRPCObject(&created, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *NFSShareResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state nfsShareResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found nfsShareRPCObject
	err := r.client.Call(ctx, "Nfs", "getShare", map[string]string{
		"uuid": state.ID.ValueString(),
	}, &found)
	if err != nil {
		// TODO: as elsewhere in this provider, match the specific "object
		// does not exist" RPC error and call resp.State.RemoveResource(ctx)
		// instead, so shares deleted out of band can be recreated.
		resp.Diagnostics.AddError("Error Reading NFS Share", err.Error())
		return
	}
	fromNFSShareRPCObject(&found, &state)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *NFSShareResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan nfsShareResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := r.toRPCObject(plan.ID.ValueString(), &plan)

	var updated nfsShareRPCObject
	if err := r.client.Call(ctx, "Nfs", "setShare", obj, &updated); err != nil {
		resp.Diagnostics.AddError("Error Updating NFS Share", err.Error())
		return
	}
	fromNFSShareRPCObject(&updated, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *NFSShareResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state nfsShareResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.Call(ctx, "Nfs", "deleteShare", map[string]string{
		"uuid": state.ID.ValueString(),
	}, nil)
	if err != nil {
		resp.Diagnostics.AddError("Error Deleting NFS Share", err.Error())
		return
	}

	if _, err := r.client.ApplyChanges(ctx, dirtiedByNFSShareChanges, false); err != nil {
		resp.Diagnostics.AddWarning(
			"NFS Share Deleted, but Deploying the Change Failed",
			fmt.Sprintf(
				"The export rule was removed from OMV's configuration (and, if it was the last rule for "+
					"its shared folder, the associated bind mount was queued for removal too), but applying "+
					"that (Config.applyChanges) failed: %s. The export and/or bind mount may still be "+
					"present until this is resolved (retry from the OMV web UI's pending changes panel).",
				err,
			),
		)
	}
}

func (r *NFSShareResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyOrHandleApplyFailure delegates to the shared implementation in
// apply_helper.go (see its doc comment for the full rationale, including
// the client-side-timeout-vs-real-failure distinction), scoped to the
// modules a NFS share change dirties.
func (r *NFSShareResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	applyOrHandleApplyFailure(ctx, r.client, r.revertOnApplyFailure, dirtiedByNFSShareChanges, diags)
}
