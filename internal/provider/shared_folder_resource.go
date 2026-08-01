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

	fwvalidators "github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// This resource manages OMV's built-in "ShareMgmt" RPC service and is meant
// as the worked example / template for the rest of the provider's
// resources. Verified against the OMV 8.5.5 source
// (usr/share/openmediavault/engined/rpc/sharemgmt.inc and
// usr/share/openmediavault/datamodels/{rpc.sharemgmt,conf.system.sharedfolder}.json
// in https://github.com/openmediavault/openmediavault, commit 96cd9aa),
// specifically:
//   - service/method names are "ShareMgmt" / "get" / "set" / "delete" (NOT
//     "getSharedFolder" etc. -- ShareMgmt reuses the generic get/set/delete
//     names for its primary CRUD methods and has separate, differently
//     named methods for its other features like ACLs and snapshots).
//   - the "please generate a UUID for me" sentinel is the fixed literal
//     omvclient.NewObjectUUID, not a word like "new"/"newuuid".
//   - "comment" is a required string param on `set` (send "" if unset, it
//     is NOT optional at the RPC layer even though it's optional in the UI).
//   - "mode" is an optional string param restricted to one of
//     "700"/"750"/"755"/"770"/"775"/"777" (octal permission strings).
//   - "name" is validated against the MS-FSCC share name spec: no control
//     characters or `" \ / [ ] : | < > + = ; , * ?`, no leading/trailing
//     spaces, max 80 characters.
//   - creating/updating/deleting a shared folder marks the "sharedfolders"
//     and "systemd" engine modules dirty (see
//     usr/share/openmediavault/engined/module/sharedfolders.inc), even
//     though ShareMgmt itself applies directory changes immediately; a
//     Config.applyChanges call is still needed for any *other* config that
//     references the shared folder (e.g. NFS/SMB exports) to actually be
//     (re)deployed.
//
// Other resources will have different modules to mark dirty when scoping
// their own applyOrHandleApplyFailure calls -- check each RPC service's
// corresponding engined/module/*.inc file for its bindListeners() to find
// the right module name(s).
var (
	_ resource.Resource                = &SharedFolderResource{}
	_ resource.ResourceWithConfigure   = &SharedFolderResource{}
	_ resource.ResourceWithImportState = &SharedFolderResource{}
)

// dirtiedBySharedFolderChanges are the engine modules that OMV marks dirty
// whenever a shared folder is created, modified, or deleted (see
// Sharedfolders::bindListeners() in engined/module/sharedfolders.inc).
var dirtiedBySharedFolderChanges = []string{"sharedfolders", "systemd"}

// shareNameRegexp mirrors the "sharename" format validator in
// datamodel/schema.inc: no control characters (0x00-0x1F) or
// " \ / [ ] : | < > + = ; , * ?, and no leading or trailing space.
var shareNameRegexp = regexp.MustCompile(`^[^\x00-\x1F"\\/\[\]:|<>+=;,*? ](.*[^\x00-\x1F"\\/\[\]:|<>+=;,*? ])?$|^[^\x00-\x1F"\\/\[\]:|<>+=;,*? ]$`)

func NewSharedFolderResource() resource.Resource {
	return &SharedFolderResource{}
}

// SharedFolderResource implements the omv_shared_folder resource.
type SharedFolderResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// sharedFolderResourceModel maps omv_shared_folder schema <-> Go.
type sharedFolderResourceModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	MountPointID types.String `tfsdk:"mount_point_id"`
	RelativePath types.String `tfsdk:"relative_path"`
	Comment      types.String `tfsdk:"comment"`
	Mode         types.String `tfsdk:"mode"`
}

func (r *SharedFolderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_shared_folder"
}

func (r *SharedFolderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages an OpenMediaVault shared folder via the ShareMgmt RPC service. " +
			"Verified against OMV 8.5.5's source; re-check against your target version if it " +
			"diverges significantly.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "UUID assigned by OMV to this shared folder.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Name of the shared folder. Must be unique across the whole OMV instance (it's reused as the default NFS/SMB export name).",
				Validators: []validator.String{
					fwvalidators.LengthAtMost(80),
					// Mirrors the MS-FSCC share name validator in
					// datamodel/schema.inc: no control chars, none of
					// " \ / [ ] : | < > + = ; , * ?, and no leading/trailing
					// spaces. Terraform-side validation here is a
					// convenience for fast feedback; OMV re-validates
					// server-side regardless.
					fwvalidators.RegexMatches(
						shareNameRegexp,
						`must not contain control characters or " \ / [ ] : | < > + = ; , * ? and must not start or end with a space`,
					),
				},
			},
			"mount_point_id": schema.StringAttribute{
				Required:    true,
				Description: "UUID of the filesystem/mount point configuration object (conf.system.filesystem.mountpoint) the shared folder lives on. Corresponds to the RPC field \"mntentref\".",
			},
			"relative_path": schema.StringAttribute{
				Required:    true,
				Description: "Path of the shared folder relative to the mount point. Must not contain \"..\" path segments. Corresponds to the RPC field \"reldirpath\".",
			},
			"comment": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
				Description: "Free-form description shown in the OMV UI. Required by the RPC layer (defaults to an empty string here since it's optional in practice).",
			},
			"mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("775"),
				Description: "Octal directory permissions for the shared folder, applied only the first time its " +
					"directory is created. Must be one of \"700\", \"750\", \"755\", \"770\", \"775\" (OMV's default), or \"777\".",
				Validators: []validator.String{
					fwvalidators.OneOf("700", "750", "755", "770", "775", "777"),
				},
			},
		},
	}
}

func (r *SharedFolderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// sharedFolderRPCObject is the shape of a shared folder object as consumed/
// returned by ShareMgmt's get/set methods (see the field docs on `set` in
// sharemgmt.inc and conf.system.sharedfolder.json). "Mode" is intentionally
// only ever sent, never trusted from a response: it's absent from `get`'s
// result and is only consulted by `set` the first time the directory is
// created, so it must never be blindly overwritten from a Read.
type sharedFolderRPCObject struct {
	UUID         string `json:"uuid"`
	Name         string `json:"name"`
	MountPointID string `json:"mntentref"`
	RelativePath string `json:"reldirpath"`
	Comment      string `json:"comment"`
	Mode         string `json:"mode,omitempty"`
}

func (r *SharedFolderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sharedFolderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := sharedFolderRPCObject{
		UUID:         omvclient.NewObjectUUID,
		Name:         plan.Name.ValueString(),
		MountPointID: plan.MountPointID.ValueString(),
		RelativePath: plan.RelativePath.ValueString(),
		Comment:      plan.Comment.ValueString(),
		Mode:         plan.Mode.ValueString(),
	}

	var created sharedFolderRPCObject
	if err := r.client.Call(ctx, "ShareMgmt", "set", obj, &created); err != nil {
		resp.Diagnostics.AddError("Error Creating Shared Folder", err.Error())
		return
	}

	plan.ID = types.StringValue(created.UUID)
	plan.Comment = types.StringValue(created.Comment)
	// `set`'s response doesn't echo "mode" back (see sharedFolderRPCObject's
	// doc comment), so leave plan.Mode as-is: it's already the value we
	// requested, and Read will never touch it either.

	// The object now genuinely exists in OMV's config database (and its
	// directory was created on disk), regardless of what happens next, so
	// resp.State is populated before we do anything that might fail. If
	// applyOrHandleApplyFailure adds an error diagnostic below, Terraform
	// will still persist this state and mark the resource tainted rather
	// than losing track of it.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *SharedFolderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sharedFolderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found sharedFolderRPCObject
	err := r.client.Call(ctx, "ShareMgmt", "get", map[string]string{
		"uuid": state.ID.ValueString(),
	}, &found)
	if err != nil {
		// TODO: ShareMgmt.get() calls Database::get(), which throws an
		// \OMV\Config\ObjectDoesNotExistException wrapped into an RPC
		// error when the UUID is unknown. Match on that error's code/text
		// here (rather than treating every error as fatal) and call
		// resp.State.RemoveResource(ctx) instead, so `terraform apply` can
		// recreate shared folders that were deleted out of band.
		resp.Diagnostics.AddError("Error Reading Shared Folder", err.Error())
		return
	}

	state.Name = types.StringValue(found.Name)
	state.MountPointID = types.StringValue(found.MountPointID)
	state.RelativePath = types.StringValue(found.RelativePath)
	state.Comment = types.StringValue(found.Comment)
	// state.Mode is intentionally left untouched: ShareMgmt.get() does not
	// return the directory's current mode, only `set` ever consults it
	// (and only on first creation), so there is nothing meaningful to
	// refresh it from.

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *SharedFolderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sharedFolderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := sharedFolderRPCObject{
		UUID:         plan.ID.ValueString(),
		Name:         plan.Name.ValueString(),
		MountPointID: plan.MountPointID.ValueString(),
		RelativePath: plan.RelativePath.ValueString(),
		Comment:      plan.Comment.ValueString(),
		Mode:         plan.Mode.ValueString(),
	}

	var updated sharedFolderRPCObject
	if err := r.client.Call(ctx, "ShareMgmt", "set", obj, &updated); err != nil {
		resp.Diagnostics.AddError("Error Updating Shared Folder", err.Error())
		return
	}

	plan.Comment = types.StringValue(updated.Comment)

	// As in Create: persist state before the apply step so a deploy
	// failure doesn't lose track of the (already-written) config change.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *SharedFolderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sharedFolderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.Call(ctx, "ShareMgmt", "delete", map[string]interface{}{
		"uuid":      state.ID.ValueString(),
		"recursive": true,
	}, nil)
	if err != nil {
		// Unlike Create/Update, nothing was written on failure here (delete()
		// validates -- e.g. assertIsNotReferenced -- before it mutates
		// anything), so the object is still fully present: leave state as-is
		// (the framework keeps prior state on a Delete error automatically)
		// and report a normal blocking error.
		resp.Diagnostics.AddError("Error Deleting Shared Folder", err.Error())
		return
	}

	// The config object (and, since recursive=true, its directory) is now
	// genuinely gone from OMV -- ShareMgmt.delete() mutates the config
	// database and filesystem directly, it doesn't stage the removal. So
	// unlike Create/Update, a failure to deploy dependent config (e.g. an
	// NFS export that referenced this folder) doesn't leave anything
	// "pending" to protect by keeping the resource in state: retrying
	// Delete would just fail with "not found". Report it as a warning
	// instead of a blocking error so Terraform still removes the resource
	// from state, but the operator is told dependent services may need a
	// manual Apply/reconfigure in the OMV web UI.
	if _, err := r.client.ApplyChanges(ctx, dirtiedBySharedFolderChanges, false); err != nil {
		resp.Diagnostics.AddWarning(
			"Shared Folder Deleted, but Deploying Dependent Configuration Failed",
			fmt.Sprintf(
				"The shared folder was removed from OMV's configuration, but applying the resulting "+
					"configuration changes (e.g. updating NFS/SMB exports that referenced it) failed: %s. "+
					"You may need to trigger \"Apply\" manually in the OMV web UI.",
				err,
			),
		)
	}
}

func (r *SharedFolderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyOrHandleApplyFailure calls Config.applyChanges scoped to the modules
// a shared folder change dirties. On failure it either:
//   - (default) adds a blocking error diagnostic and leaves the change
//     queued in OMV, matching what the OMV web UI itself does when its
//     "Apply" button's RPC call fails -- it does NOT auto-revert, only its
//     separate, explicit "Undo" button does (see apply-config-panel.
//     component.ts). The object this resource manages was already written
//     to OMV's config database (the caller must resp.State.Set before
//     calling this), so it stays present -- just "dirty" -- in both OMV and
//     Terraform state, consistent with the web UI leaving it in the pending
//     changes list for the operator to retry or undo by hand.
//   - (opt-in via the provider's revert_on_apply_failure) also calls
//     Config.revertChanges, mirroring a manual click of "Undo". Since that
//     RPC discards the ENTIRE pending-changes queue instance-wide, not just
//     this resource's change, the error message says so explicitly.
func (r *SharedFolderResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	if _, err := r.client.ApplyChanges(ctx, dirtiedBySharedFolderChanges, false); err != nil {
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
						"recorded in Terraform state pointing at an object that may no longer reflect what "+
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
					"automatically undone -- it remains queued as a pending change. Fix the underlying issue "+
					"and run `terraform apply` again, or resolve it manually (Apply/Undo) in the OMV web UI. "+
					"Set the provider's revert_on_apply_failure = true to have Terraform automatically call "+
					"Config.revertChanges instead, noting that discards ALL pending changes instance-wide, "+
					"not just this resource's.",
				err,
			),
		)
	}
}
