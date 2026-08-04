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

// This resource manages OMV's "Services > SMB/CIFS > Shares" page.
// Verified against the OMV 8.5.5 source (engined/rpc/smb.inc,
// engined/module/samba.inc, and the rpc.smb/conf.service.smb.share
// datamodels):
//   - service "Smb", methods "getShare"/"setShare"/"deleteShare" for
//     basic CRUD. Unlike Rsync, getShare()/setShare() are both safe to
//     decode with the same struct: both simply return
//     $object->getAssoc() (or $db->getAssoc(...)) with no post-
//     processing divergence.
//   - "rpc.smb.setshare" marks every field required EXCEPT
//     "timemachinemaxsize", "transportencryption", "followsymlinks", and
//     "widelinks" (present in the conf.service.smb.share config
//     datamodel but simply absent from the narrower RPC params schema).
//     Checked OMV's actual JSON schema validator
//     (usr/share/php/openmediavault/json/schema.inc's checkProperties()):
//     it only iterates the schema's OWN declared properties against the
//     input and never rejects extra/undeclared ones, and the
//     ConfigObject constructed inside setShare() is validated against
//     the full conf.service.smb.share datamodel (which does declare
//     these 4 fields), not the narrower RPC schema -- so sending them is
//     both safe and effective, and they're modeled here as ordinary
//     Optional+Computed attributes like everything else.
//   - setShare() calls db->assertIsUnique(object, "sharedfolderref") --
//     at most one SMB share per shared folder. Creating (or updating) a
//     second share pointing at a shared folder that's already exported
//     via SMB fails with a clear RPC error.
//   - "recyclemaxsize" is in raw BYTES and "recyclemaxage" is in DAYS
//     (confirmed from the web UI form component, since the datamodel
//     itself just says "integer" with no unit) -- named
//     recycle_bin_max_size_bytes/recycle_bin_retention_days here rather
//     than the RPC's bare names, to avoid the exact kind of unit
//     ambiguity that's bitten other fields in this provider before.
//   - creating/modifying/deleting a share marks the "samba" engine module
//     dirty (engined/module/samba.inc's getName() returns "samba", NOT
//     "smb" -- same naming mismatch pattern as nginx's module being
//     called "webserver").
var (
	_ resource.Resource                = &SMBShareResource{}
	_ resource.ResourceWithConfigure   = &SMBShareResource{}
	_ resource.ResourceWithImportState = &SMBShareResource{}
)

// dirtiedBySMBShareChanges is the engine module OMV marks dirty whenever
// a SMB share is created, modified, or deleted (Samba::bindListeners()
// in engined/module/samba.inc).
var dirtiedBySMBShareChanges = []string{"samba"}

// timeMachineMaxSizeRegexp mirrors conf.service.smb.share.json's
// "timemachinemaxsize" pattern verbatim: empty (unrestricted), or a
// number optionally followed by a single K/M/G/T/P size suffix.
var timeMachineMaxSizeRegexp = regexp.MustCompile(`^(\d+[KMGTP]?)?$`)

func NewSMBShareResource() resource.Resource {
	return &SMBShareResource{}
}

// SMBShareResource implements the omv_smb_share resource.
type SMBShareResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// smbShareResourceModel maps omv_smb_share schema <-> Go.
type smbShareResourceModel struct {
	ID                      types.String `tfsdk:"id"`
	Enabled                 types.Bool   `tfsdk:"enabled"`
	SharedFolderID          types.String `tfsdk:"shared_folder_id"`
	Comment                 types.String `tfsdk:"comment"`
	Guest                   types.String `tfsdk:"guest"`
	ReadOnly                types.Bool   `tfsdk:"read_only"`
	Browseable              types.Bool   `tfsdk:"browseable"`
	RecycleBin              types.Bool   `tfsdk:"recycle_bin"`
	RecycleBinMaxSizeBytes  types.Int64  `tfsdk:"recycle_bin_max_size_bytes"`
	RecycleBinRetentionDays types.Int64  `tfsdk:"recycle_bin_retention_days"`
	HideDotFiles            types.Bool   `tfsdk:"hide_dot_files"`
	InheritACLs             types.Bool   `tfsdk:"inherit_acls"`
	InheritPermissions      types.Bool   `tfsdk:"inherit_permissions"`
	EASupport               types.Bool   `tfsdk:"extended_attributes"`
	StoreDOSAttributes      types.Bool   `tfsdk:"store_dos_attributes"`
	HostsAllow              types.String `tfsdk:"hosts_allow"`
	HostsDeny               types.String `tfsdk:"hosts_deny"`
	Audit                   types.Bool   `tfsdk:"audit"`
	TimeMachine             types.Bool   `tfsdk:"time_machine"`
	TimeMachineMaxSize      types.String `tfsdk:"time_machine_max_size"`
	TransportEncryption     types.Bool   `tfsdk:"transport_encryption"`
	FollowSymlinks          types.Bool   `tfsdk:"follow_symlinks"`
	WideLinks               types.Bool   `tfsdk:"wide_links"`
	ExtraOptions            types.String `tfsdk:"extra_options"`
}

func (r *SMBShareResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_smb_share"
}

func (r *SMBShareResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages an OpenMediaVault SMB/CIFS share (Services > SMB/CIFS > Shares). " +
			"Verified against OMV 8.5.5's source. At most one omv_smb_share may reference a given " +
			"shared_folder_id -- OMV enforces this (db->assertIsUnique) and rejects a second one with " +
			"a clear error.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "UUID assigned by OMV to this share.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Whether the share is active. Corresponds to RPC field \"enable\".",
			},
			"shared_folder_id": schema.StringAttribute{
				Required:    true,
				Description: "UUID of the shared folder to export. Corresponds to RPC field \"sharedfolderref\". At most one share may reference a given shared folder.",
			},
			"comment": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Free-form description shown in the OMV UI and as the share's SMB comment.",
			},
			"guest": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("no"),
				Description: "Guest (unauthenticated) access: \"no\", \"allow\" (permitted alongside authenticated access), or \"only\" (guest-only, no authentication accepted).",
				Validators:  []validator.String{fwstringvalidator.OneOf("no", "allow", "only")},
			},
			"read_only": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Export the share read-only. Corresponds to RPC field \"readonly\".",
			},
			"browseable": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				Description: "Whether the share appears when browsing available shares on the server.",
			},
			"recycle_bin": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Move deleted files to a recycle bin instead of deleting them immediately. Corresponds to RPC field \"recyclebin\".",
			},
			"recycle_bin_max_size_bytes": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(0),
				Description: "Files larger than this (in bytes) are deleted immediately rather than moved " +
					"to the recycle bin. 0 means unrestricted. Corresponds to RPC field \"recyclemaxsize\", " +
					"which is a raw byte count despite having no unit in its own name -- confirmed against " +
					"the web UI's form definition, not just the datamodel.",
				Validators: []validator.Int64{fwint64validator.AtLeast(0)},
			},
			"recycle_bin_retention_days": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(0),
				Description: "Files in the recycle bin are automatically deleted after this many days. " +
					"0 means manual deletion only. Corresponds to RPC field \"recyclemaxage\", which is in " +
					"days despite having no unit in its own name.",
				Validators: []validator.Int64{fwint64validator.AtLeast(0)},
			},
			"hide_dot_files": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				Description: "Treat files starting with a dot as hidden. Corresponds to RPC field \"hidedotfiles\".",
			},
			"inherit_acls": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				Description: "Corresponds to RPC field \"inheritacls\" (Samba's \"inherit acls\").",
			},
			"inherit_permissions": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Corresponds to RPC field \"inheritpermissions\" (Samba's \"inherit permissions\").",
			},
			"extended_attributes": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				Description: "Support filesystem extended attributes. Corresponds to RPC field \"easupport\".",
			},
			"store_dos_attributes": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Corresponds to RPC field \"storedosattributes\" (Samba's \"store dos attributes\").",
			},
			"hosts_allow": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Space-separated list of hosts/networks permitted to access the share (Samba's \"hosts allow\"). Empty means no restriction.",
			},
			"hosts_deny": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Space-separated list of hosts/networks denied access to the share (Samba's \"hosts deny\").",
			},
			"audit": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Enable file operation audit logging for this share.",
			},
			"time_machine": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Advertise this share as a macOS Time Machine backup target.",
			},
			"time_machine_max_size": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Maximum Time Machine backup size, e.g. \"500G\" or \"2T\". Empty means unrestricted. Only meaningful when time_machine is true. NOT sent as a required RPC field (see this file's top doc comment) but still fully settable.",
				Validators: []validator.String{
					fwstringvalidator.RegexMatches(timeMachineMaxSizeRegexp, "must be empty or a number optionally followed by a single K/M/G/T/P suffix, e.g. \"500G\""),
				},
			},
			"transport_encryption": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Require SMB transport encryption for this share. Not sent as a required RPC field (see this file's top doc comment) but still fully settable.",
			},
			"follow_symlinks": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				Description: "Follow symbolic links that point outside the share. Not sent as a required RPC field (see this file's top doc comment) but still fully settable.",
			},
			"wide_links": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Allow symbolic links that point outside the share's directory tree (Samba's \"wide links\"; has no effect unless follow_symlinks is also true). Not sent as a required RPC field (see this file's top doc comment) but still fully settable.",
			},
			"extra_options": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Additional raw smb.conf options for this share's section.",
			},
		},
	}
}

func (r *SMBShareResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// smbShareRPCObject is the shape of a SMB share object as consumed/
// returned by Smb's getShare/setShare methods. Both are safe to decode
// with this same struct -- see this file's top doc comment.
type smbShareRPCObject struct {
	UUID                string `json:"uuid"`
	Enable              bool   `json:"enable"`
	SharedFolderRef     string `json:"sharedfolderref"`
	Comment             string `json:"comment"`
	Guest               string `json:"guest"`
	ReadOnly            bool   `json:"readonly"`
	Browseable          bool   `json:"browseable"`
	RecycleBin          bool   `json:"recyclebin"`
	RecycleMaxSize      int64  `json:"recyclemaxsize"`
	RecycleMaxAge       int64  `json:"recyclemaxage"`
	HideDotFiles        bool   `json:"hidedotfiles"`
	InheritACLs         bool   `json:"inheritacls"`
	InheritPermissions  bool   `json:"inheritpermissions"`
	EASupport           bool   `json:"easupport"`
	StoreDOSAttributes  bool   `json:"storedosattributes"`
	HostsAllow          string `json:"hostsallow"`
	HostsDeny           string `json:"hostsdeny"`
	Audit               bool   `json:"audit"`
	TimeMachine         bool   `json:"timemachine"`
	TimeMachineMaxSize  string `json:"timemachinemaxsize"`
	TransportEncryption bool   `json:"transportencryption"`
	FollowSymlinks      bool   `json:"followsymlinks"`
	WideLinks           bool   `json:"widelinks"`
	ExtraOptions        string `json:"extraoptions"`
}

func (r *SMBShareResource) toRPCObject(uuid string, m *smbShareResourceModel) smbShareRPCObject {
	return smbShareRPCObject{
		UUID:                uuid,
		Enable:              m.Enabled.ValueBool(),
		SharedFolderRef:     m.SharedFolderID.ValueString(),
		Comment:             m.Comment.ValueString(),
		Guest:               m.Guest.ValueString(),
		ReadOnly:            m.ReadOnly.ValueBool(),
		Browseable:          m.Browseable.ValueBool(),
		RecycleBin:          m.RecycleBin.ValueBool(),
		RecycleMaxSize:      m.RecycleBinMaxSizeBytes.ValueInt64(),
		RecycleMaxAge:       m.RecycleBinRetentionDays.ValueInt64(),
		HideDotFiles:        m.HideDotFiles.ValueBool(),
		InheritACLs:         m.InheritACLs.ValueBool(),
		InheritPermissions:  m.InheritPermissions.ValueBool(),
		EASupport:           m.EASupport.ValueBool(),
		StoreDOSAttributes:  m.StoreDOSAttributes.ValueBool(),
		HostsAllow:          m.HostsAllow.ValueString(),
		HostsDeny:           m.HostsDeny.ValueString(),
		Audit:               m.Audit.ValueBool(),
		TimeMachine:         m.TimeMachine.ValueBool(),
		TimeMachineMaxSize:  m.TimeMachineMaxSize.ValueString(),
		TransportEncryption: m.TransportEncryption.ValueBool(),
		FollowSymlinks:      m.FollowSymlinks.ValueBool(),
		WideLinks:           m.WideLinks.ValueBool(),
		ExtraOptions:        m.ExtraOptions.ValueString(),
	}
}

func fromSMBShareRPCObject(obj *smbShareRPCObject, m *smbShareResourceModel) {
	m.ID = types.StringValue(obj.UUID)
	m.Enabled = types.BoolValue(obj.Enable)
	m.SharedFolderID = types.StringValue(obj.SharedFolderRef)
	m.Comment = types.StringValue(obj.Comment)
	m.Guest = types.StringValue(obj.Guest)
	m.ReadOnly = types.BoolValue(obj.ReadOnly)
	m.Browseable = types.BoolValue(obj.Browseable)
	m.RecycleBin = types.BoolValue(obj.RecycleBin)
	m.RecycleBinMaxSizeBytes = types.Int64Value(obj.RecycleMaxSize)
	m.RecycleBinRetentionDays = types.Int64Value(obj.RecycleMaxAge)
	m.HideDotFiles = types.BoolValue(obj.HideDotFiles)
	m.InheritACLs = types.BoolValue(obj.InheritACLs)
	m.InheritPermissions = types.BoolValue(obj.InheritPermissions)
	m.EASupport = types.BoolValue(obj.EASupport)
	m.StoreDOSAttributes = types.BoolValue(obj.StoreDOSAttributes)
	m.HostsAllow = types.StringValue(obj.HostsAllow)
	m.HostsDeny = types.StringValue(obj.HostsDeny)
	m.Audit = types.BoolValue(obj.Audit)
	m.TimeMachine = types.BoolValue(obj.TimeMachine)
	m.TimeMachineMaxSize = types.StringValue(obj.TimeMachineMaxSize)
	m.TransportEncryption = types.BoolValue(obj.TransportEncryption)
	m.FollowSymlinks = types.BoolValue(obj.FollowSymlinks)
	m.WideLinks = types.BoolValue(obj.WideLinks)
	m.ExtraOptions = types.StringValue(obj.ExtraOptions)
}

func (r *SMBShareResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan smbShareResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := r.toRPCObject(omvclient.NewObjectUUID, &plan)

	var created smbShareRPCObject
	if err := r.client.Call(ctx, "Smb", "setShare", obj, &created); err != nil {
		resp.Diagnostics.AddError("Error Creating SMB Share", err.Error())
		return
	}
	fromSMBShareRPCObject(&created, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *SMBShareResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state smbShareResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found smbShareRPCObject
	err := r.client.Call(ctx, "Smb", "getShare", map[string]string{
		"uuid": state.ID.ValueString(),
	}, &found)
	if err != nil {
		// TODO: as elsewhere in this provider, match the specific "object
		// does not exist" RPC error and call resp.State.RemoveResource(ctx)
		// instead, so shares deleted out of band can be recreated.
		resp.Diagnostics.AddError("Error Reading SMB Share", err.Error())
		return
	}
	fromSMBShareRPCObject(&found, &state)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *SMBShareResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan smbShareResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := r.toRPCObject(plan.ID.ValueString(), &plan)

	var updated smbShareRPCObject
	if err := r.client.Call(ctx, "Smb", "setShare", obj, &updated); err != nil {
		resp.Diagnostics.AddError("Error Updating SMB Share", err.Error())
		return
	}
	fromSMBShareRPCObject(&updated, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *SMBShareResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state smbShareResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.Call(ctx, "Smb", "deleteShare", map[string]string{
		"uuid": state.ID.ValueString(),
	}, nil)
	if err != nil {
		resp.Diagnostics.AddError("Error Deleting SMB Share", err.Error())
		return
	}

	if _, err := r.client.ApplyChanges(ctx, dirtiedBySMBShareChanges, false); err != nil {
		resp.Diagnostics.AddWarning(
			"SMB Share Deleted, but Deploying the Change Failed",
			fmt.Sprintf(
				"The share was removed from OMV's configuration, but applying the resulting smb.conf "+
					"regeneration (Config.applyChanges) failed: %s. The share may still be accessible over "+
					"the network on its old configuration until this is resolved (retry from the OMV web "+
					"UI's pending changes panel).",
				err,
			),
		)
	}
}

func (r *SMBShareResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyOrHandleApplyFailure delegates to the shared implementation in
// apply_helper.go (see its doc comment for the full rationale, including
// the client-side-timeout-vs-real-failure distinction), scoped to the
// modules a SMB share change dirties.
func (r *SMBShareResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	applyOrHandleApplyFailure(ctx, r.client, r.revertOnApplyFailure, dirtiedBySMBShareChanges, diags)
}
