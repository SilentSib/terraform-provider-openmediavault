package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/defaults"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	fwint64validator "github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	fwlistvalidator "github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	fwstringvalidator "github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// This resource manages OMV's "Rsync" RPC service (scheduled rsync jobs --
// distinct from "Rsyncd", which configures the rsync *daemon/server*, not
// jobs). Verified against the OMV 8.5.5 source
// (usr/share/openmediavault/engined/rpc/rsync.inc,
// usr/share/openmediavault/engined/module/rsync.inc, and the
// rpc.rsync/conf.service.rsync.job datamodels), specifically:
//   - service/method names are "Rsync" / "get" / "set" / "delete" (plus a
//     separate "execute" method to trigger a job immediately -- not wired
//     up as a Terraform action here, see the note on Delete below).
//   - the "please generate a UUID for me" sentinel is omvclient.NewObjectUUID,
//     same as every other OMV config object -- see shared_folder_resource.go.
//   - ALL fields in rpc.rsync.json's "set" params are marked required by
//     the RPC schema, even ones that only make sense for a subset of
//     type/mode combinations (e.g. "srcuri" when type="local"). OMV's own
//     `set()` handler only actually *uses* the combination-appropriate
//     fields and discards the rest, but the RPC call itself still needs
//     every key present -- so Create/Update always send the full object,
//     with fields irrelevant to the chosen type/mode left at their zero
//     value, matching what the OMV web UI's form does.
//   - "minute"/"hour"/"dayofmonth"/"month"/"dayofweek" are arrays of cron
//     field strings at the RPC layer (each either "*" or a bounded number),
//     but are stored in the config database -- and sent back by
//     Rsync.set()'s response -- as a single comma-separated string. Only
//     Rsync.get() converts the stored string into an array; set()'s
//     response echoes the raw comma-joined string instead, which decodes
//     to a Go decode error if you assume it's an array (an earlier version
//     of this file did exactly that). Create/Update work around this by
//     only reading the UUID out of set()'s response and re-fetching the
//     full object via get() -- see the doc comment on rsyncJobRPCObject
//     and the comments in Create()/Update() below.
//   - similarly, Rsync.get() (not set()) is the only method that populates
//     the flat srcsharedfolderref/srcuri/destsharedfolderref/desturi
//     convenience fields this provider's model relies on; set()'s
//     response only has the type/mode-appropriate values nested under
//     "src"/"dest" objects, which this provider doesn't parse. The
//     get()-refetch above covers this too.
//   - creating/modifying/deleting a job marks the "rsync" engine module
//     dirty (engined/module/rsync.inc); unlike shared folders, this
//     module's deploy() step is NOT a no-op -- it's what actually writes
//     the cron job and its wrapper script to disk, so Config.applyChanges
//     is functionally required here, not just best-effort tidiness.
//     Rsync.execute() (running a job on demand) explicitly refuses to run
//     while its module is still dirty.
var (
	_ resource.Resource                = &RsyncJobResource{}
	_ resource.ResourceWithConfigure   = &RsyncJobResource{}
	_ resource.ResourceWithImportState = &RsyncJobResource{}
)

// dirtiedByRsyncJobChanges is the engine module OMV marks dirty whenever a
// rsync job is created, modified, or deleted (Rsync::bindListeners() in
// engined/module/rsync.inc).
var dirtiedByRsyncJobChanges = []string{"rsync"}

// cronFieldValidator builds a list validator matching one of rpc.rsync.
// json's cron field patterns: either the literal "*" or a bounded number
// (no leading zeros). Note this is intentionally STRICTER than OMV's own
// regexes, not an exact copy: e.g. the real minute pattern in
// rpc.rsync.json is "^[0-9]|[1-5][0-9]$", which -- because "|" binds
// looser than "^"/"$" -- actually means "starts with one digit" OR "ends
// with 10-59", unanchored on the other side of each alternative, and so
// technically accepts some malformed strings a naive reading wouldn't
// expect. This validator instead fully anchors both alternatives
// (`^(...)$`), accepting exactly the valid range and nothing OMV's server-
// side check wouldn't also accept, without inheriting that looseness.
func cronFieldValidator(pattern, description string) validator.List {
	return fwlistvalidator.ValueStringsAre(
		fwstringvalidator.RegexMatches(regexp.MustCompile(pattern), description),
	)
}

// singleStringListDefault builds a schema default for a List(String)
// attribute containing exactly the given literal values.
func singleStringListDefault(values ...string) defaults.List {
	elems := make([]attr.Value, len(values))
	for i, v := range values {
		elems[i] = types.StringValue(v)
	}
	return listdefault.StaticValue(types.ListValueMust(types.StringType, elems))
}

func NewRsyncJobResource() resource.Resource {
	return &RsyncJobResource{}
}

// RsyncJobResource implements the omv_rsync_job resource.
type RsyncJobResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// rsyncJobResourceModel maps omv_rsync_job schema <-> Go. Field ordering
// mirrors rpc.rsync.json for ease of cross-checking.
type rsyncJobResourceModel struct {
	ID     types.String `tfsdk:"id"`
	Enable types.Bool   `tfsdk:"enabled"`

	SendEmail types.Bool   `tfsdk:"send_email"`
	Comment   types.String `tfsdk:"comment"`

	Type types.String `tfsdk:"type"` // "local" | "remote"
	Mode types.String `tfsdk:"mode"` // "push" | "pull" (remote only)

	SrcSharedFolderID types.String `tfsdk:"src_shared_folder_id"` // local, or remote+push
	SrcURI            types.String `tfsdk:"src_uri"`              // remote+pull

	DestSharedFolderID types.String `tfsdk:"dest_shared_folder_id"` // local, or remote+pull
	DestURI            types.String `tfsdk:"dest_uri"`              // remote+push

	Minute           types.List `tfsdk:"minute"`
	EveryNMinute     types.Bool `tfsdk:"every_n_minute"`
	Hour             types.List `tfsdk:"hour"`
	EveryNHour       types.Bool `tfsdk:"every_n_hour"`
	Month            types.List `tfsdk:"month"`
	DayOfMonth       types.List `tfsdk:"day_of_month"`
	EveryNDayOfMonth types.Bool `tfsdk:"every_n_day_of_month"`
	DayOfWeek        types.List `tfsdk:"day_of_week"`

	OptionRecursive types.Bool `tfsdk:"option_recursive"`
	OptionTimes     types.Bool `tfsdk:"option_times"`
	OptionGroup     types.Bool `tfsdk:"option_group"`
	OptionOwner     types.Bool `tfsdk:"option_owner"`
	OptionCompress  types.Bool `tfsdk:"option_compress"`
	OptionArchive   types.Bool `tfsdk:"option_archive"`
	OptionDelete    types.Bool `tfsdk:"option_delete"`
	OptionQuiet     types.Bool `tfsdk:"option_quiet"`
	OptionPerms     types.Bool `tfsdk:"option_perms"`
	OptionACLs      types.Bool `tfsdk:"option_acls"`
	OptionXattrs    types.Bool `tfsdk:"option_xattrs"`
	OptionDryRun    types.Bool `tfsdk:"option_dry_run"`
	OptionPartial   types.Bool `tfsdk:"option_partial"`

	ExtraOptions types.String `tfsdk:"extra_options"`

	Authentication   types.String `tfsdk:"authentication"` // "password" | "pubkey" (remote only)
	Password         types.String `tfsdk:"password"`
	SSHCertificateID types.String `tfsdk:"ssh_certificate_id"`
	SSHPort          types.Int64  `tfsdk:"ssh_port"`
}

func (r *RsyncJobResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_rsync_job"
}

func (r *RsyncJobResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	wildcardOrNumber := func(pattern, label string) validator.List {
		return cronFieldValidator(pattern, "must be \"*\" or "+label)
	}

	resp.Schema = schema.Schema{
		Description: "Manages an OpenMediaVault scheduled rsync job (the \"Rsync\" RPC service -- " +
			"local or remote rsync transfers on a cron schedule; NOT the rsync daemon/server config, " +
			"which is a separate \"Rsyncd\" service). Verified against OMV 8.5.5's source.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "UUID assigned by OMV to this rsync job.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Whether the cron job is active. Corresponds to RPC field \"enable\".",
			},
			"send_email": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Email the job's output after each run.",
			},
			"comment": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Free-form description shown in the OMV UI.",
			},
			"type": schema.StringAttribute{
				Required:    true,
				Description: "\"local\" (both endpoints are shared folders on this OMV instance) or \"remote\".",
				Validators:  []validator.String{fwstringvalidator.OneOf("local", "remote")},
			},
			"mode": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("push"),
				Description: "Only meaningful when type = \"remote\": \"push\" (this OMV instance sends " +
					"to a remote rsync URI) or \"pull\" (this OMV instance fetches from one). Required by " +
					"the RPC layer regardless of type; ignored for type = \"local\".",
				Validators: []validator.String{fwstringvalidator.OneOf("push", "pull")},
			},
			"src_shared_folder_id": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "UUID of the source shared folder. Required when type = \"local\", or when " +
					"type = \"remote\" and mode = \"push\". Corresponds to RPC field \"srcsharedfolderref\".",
			},
			"src_uri": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Source rsync URI (e.g. \"rsync://host/module/path\" or \"user@host:/path\"). " +
					"Only used when type = \"remote\" and mode = \"pull\". Corresponds to RPC field \"srcuri\".",
			},
			"dest_shared_folder_id": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "UUID of the destination shared folder. Required when type = \"local\", or " +
					"when type = \"remote\" and mode = \"pull\". Corresponds to RPC field \"destsharedfolderref\".",
			},
			"dest_uri": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Destination rsync URI. Only used when type = \"remote\" and mode = \"push\". " +
					"Corresponds to RPC field \"desturi\".",
			},
			"minute": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true, Computed: true,
				Default:     singleStringListDefault("*"),
				Description: "Cron minute field values (each \"*\" or \"0\"-\"59\"). Multiple values create a comma-separated cron list.",
				Validators:  []validator.List{wildcardOrNumber(`^([0-9]|[1-5][0-9]|\*)$`, "an integer 0-59")},
			},
			"every_n_minute": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Treat \"minute\"'s single value as a step (cron \"*/n\") rather than a literal list.",
			},
			"hour": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true, Computed: true,
				Default:     singleStringListDefault("*"),
				Description: "Cron hour field values (each \"*\" or \"0\"-\"23\").",
				Validators:  []validator.List{wildcardOrNumber(`^([0-9]|1[0-9]|2[0-3]|\*)$`, "an integer 0-23")},
			},
			"every_n_hour": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Treat \"hour\"'s single value as a step (cron \"*/n\") rather than a literal list.",
			},
			"month": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true, Computed: true,
				Default:     singleStringListDefault("*"),
				Description: "Cron month field values (each \"*\" or \"1\"-\"12\").",
				Validators:  []validator.List{wildcardOrNumber(`^([1-9]|1[0-2]|\*)$`, "an integer 1-12")},
			},
			"day_of_month": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true, Computed: true,
				Default:     singleStringListDefault("*"),
				Description: "Cron day-of-month field values (each \"*\" or \"1\"-\"31\").",
				Validators:  []validator.List{wildcardOrNumber(`^([1-9]|[12][0-9]|3[01]|\*)$`, "an integer 1-31")},
			},
			"every_n_day_of_month": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				Description: "Treat \"day_of_month\"'s single value as a step (cron \"*/n\") rather than a literal list.",
			},
			"day_of_week": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true, Computed: true,
				Default:     singleStringListDefault("*"),
				Description: "Cron day-of-week field values (each \"*\" or \"1\"-\"7\", 1 = Monday per OMV/cron convention).",
				Validators:  []validator.List{wildcardOrNumber(`^([1-7]|\*)$`, "an integer 1-7")},
			},
			"option_recursive": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "rsync --recursive."},
			"option_times":     schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "rsync --times."},
			"option_group":     schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "rsync --group."},
			"option_owner":     schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "rsync --owner."},
			"option_compress":  schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), Description: "rsync --compress."},
			"option_archive":   schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "rsync --archive."},
			"option_delete":    schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), Description: "rsync --delete."},
			"option_quiet":     schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "rsync --quiet."},
			"option_perms":     schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), Description: "rsync --perms."},
			"option_acls":      schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), Description: "rsync --acls."},
			"option_xattrs":    schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), Description: "rsync --xattrs."},
			"option_dry_run":   schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), Description: "rsync --dry-run. Useful for validating a job before enabling it for real."},
			"option_partial":   schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), Description: "rsync --partial."},
			"extra_options": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Additional raw rsync command-line options, space-separated.",
			},
			"authentication": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("password"),
				Description: "Only meaningful when type = \"remote\": \"password\" or \"pubkey\" SSH authentication.",
				Validators:  []validator.String{fwstringvalidator.OneOf("password", "pubkey")},
			},
			"password": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Sensitive: true,
				Description: "SSH/rsync password. Only used when type = \"remote\" and authentication = " +
					"\"password\". Stored in OMV's configuration database in plain text (matching OMV's " +
					"own behavior), not just in Terraform state -- prefer authentication = \"pubkey\" where " +
					"possible. This provider deliberately never reads the stored value back from OMV into " +
					"state (so an out-of-band password change on the server doesn't fight with a plan " +
					"that's changing it here); after `terraform import`, it's set to \"\" regardless of the " +
					"real stored value -- set it explicitly in configuration after importing if the job " +
					"actually uses password authentication.",
			},
			"ssh_certificate_id": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "UUID of the SSH certificate/key configuration object to use. Only used when " +
					"type = \"remote\" and authentication = \"pubkey\". Corresponds to RPC field \"sshcertificateref\".",
			},
			"ssh_port": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(22),
				Description: "SSH port for remote jobs. Must be between 1 and 65535.",
				Validators:  []validator.Int64{fwint64validator.Between(1, 65535)},
			},
		},
	}
}

func (r *RsyncJobResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// rsyncJobRPCObject is the shape of a rsync job object as CONSUMED by
// Rsync.set() and RETURNED by Rsync.get() -- it is NOT safe to decode a
// set() response into this struct. get() explicitly post-processes its
// result (see engined/rpc/rsync.inc): it explode()s the stored
// comma-separated minute/hour/dayofmonth/month/dayofweek strings into
// arrays, and copy()s the type/mode-appropriate src/dest fields into the
// flat srcsharedfolderref/srcuri/destsharedfolderref/desturi keys this
// struct expects. set() does neither -- its response has those cron
// fields as plain (comma-joined) strings, which fails to decode into the
// []string fields below, and never includes the flat src/dest alias keys
// at all (only the nested "src"/"dest" objects, which this struct doesn't
// model). Create/Update therefore only pull the UUID out of set()'s
// response and immediately re-fetch via get() -- see the comments there.
type rsyncJobRPCObject struct {
	UUID      string `json:"uuid"`
	Enable    bool   `json:"enable"`
	SendEmail bool   `json:"sendemail"`
	Comment   string `json:"comment"`

	Type string `json:"type"`
	Mode string `json:"mode"`

	SrcSharedFolderRef  string `json:"srcsharedfolderref"`
	SrcURI              string `json:"srcuri"`
	DestSharedFolderRef string `json:"destsharedfolderref"`
	DestURI             string `json:"desturi"`

	Minute           []string `json:"minute"`
	EveryNMinute     bool     `json:"everynminute"`
	Hour             []string `json:"hour"`
	EveryNHour       bool     `json:"everynhour"`
	Month            []string `json:"month"`
	DayOfMonth       []string `json:"dayofmonth"`
	EveryNDayOfMonth bool     `json:"everyndayofmonth"`
	DayOfWeek        []string `json:"dayofweek"`

	OptionRecursive bool `json:"optionrecursive"`
	OptionTimes     bool `json:"optiontimes"`
	OptionGroup     bool `json:"optiongroup"`
	OptionOwner     bool `json:"optionowner"`
	OptionCompress  bool `json:"optioncompress"`
	OptionArchive   bool `json:"optionarchive"`
	OptionDelete    bool `json:"optiondelete"`
	OptionQuiet     bool `json:"optionquiet"`
	OptionPerms     bool `json:"optionperms"`
	OptionACLs      bool `json:"optionacls"`
	OptionXattrs    bool `json:"optionxattrs"`
	OptionDryRun    bool `json:"optiondryrun"`
	OptionPartial   bool `json:"optionpartial"`

	ExtraOptions string `json:"extraoptions"`

	Authentication    string `json:"authentication"`
	Password          string `json:"password"`
	SSHCertificateRef string `json:"sshcertificateref"`
	SSHPort           int64  `json:"sshport"`
}

func (r *RsyncJobResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan rsyncJobResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj, diags := r.toRPCObject(ctx, omvclient.NewObjectUUID, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Rsync.set()'s response is NOT safe to decode into rsyncJobRPCObject
	// directly (see the doc comment on that type): unlike get(), it
	// doesn't convert minute/hour/dayofmonth/month/dayofweek from their
	// stored comma-separated-string form into arrays, and it never
	// populates the flat srcsharedfolderref/srcuri/destsharedfolderref/
	// desturi convenience fields at all (those only exist in get()'s
	// response). Decoding it as this minimal, uuid-only shape and then
	// re-fetching via get() -- the same call Read() makes -- sidesteps
	// both problems and guarantees Create leaves state in exactly the
	// shape a subsequent Read would produce.
	var created struct {
		UUID string `json:"uuid"`
	}
	if err := r.client.Call(ctx, "Rsync", "set", obj, &created); err != nil {
		resp.Diagnostics.AddError("Error Creating Rsync Job", err.Error())
		return
	}

	var full rsyncJobRPCObject
	if err := r.client.Call(ctx, "Rsync", "get", map[string]string{"uuid": created.UUID}, &full); err != nil {
		resp.Diagnostics.AddError(
			"Rsync Job Created, but Re-Reading It Failed",
			fmt.Sprintf(
				"The job was created (uuid=%s), but the follow-up read used to populate Terraform "+
					"state failed: %s. The job exists in OMV; re-run `terraform apply` or `terraform "+
					"import omv_rsync_job.<name> %s` to bring it under management.",
				created.UUID, err, created.UUID,
			),
		)
		return
	}

	resp.Diagnostics.Append(r.fromRPCObject(ctx, &full, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// As with omv_shared_folder: persist state before the apply step, so a
	// deploy failure below doesn't lose track of a job that really was
	// written to OMV's config database.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *RsyncJobResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state rsyncJobResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found rsyncJobRPCObject
	err := r.client.Call(ctx, "Rsync", "get", map[string]string{
		"uuid": state.ID.ValueString(),
	}, &found)
	if err != nil {
		// TODO: as in shared_folder_resource.go, match the specific "object
		// does not exist" RPC error here and call
		// resp.State.RemoveResource(ctx) instead of a blocking error, so
		// jobs deleted out of band can be recreated by `terraform apply`.
		resp.Diagnostics.AddError("Error Reading Rsync Job", err.Error())
		return
	}

	resp.Diagnostics.Append(r.fromRPCObject(ctx, &found, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *RsyncJobResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan rsyncJobResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj, diags := r.toRPCObject(ctx, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// See the comment in Create(): set()'s response isn't safe to decode
	// into rsyncJobRPCObject, and we already know the UUID (it's the
	// existing one being updated), so there's nothing useful to read from
	// it here -- discard it and re-fetch via get() instead.
	if err := r.client.Call(ctx, "Rsync", "set", obj, nil); err != nil {
		resp.Diagnostics.AddError("Error Updating Rsync Job", err.Error())
		return
	}

	var full rsyncJobRPCObject
	if err := r.client.Call(ctx, "Rsync", "get", map[string]string{"uuid": plan.ID.ValueString()}, &full); err != nil {
		resp.Diagnostics.AddError(
			"Rsync Job Updated, but Re-Reading It Failed",
			fmt.Sprintf(
				"The job's configuration was updated, but the follow-up read used to refresh Terraform "+
					"state failed: %s. The update was applied in OMV; re-run `terraform apply` (or "+
					"`terraform plan`/`refresh`) to resync state.",
				err,
			),
		)
		return
	}

	resp.Diagnostics.Append(r.fromRPCObject(ctx, &full, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *RsyncJobResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state rsyncJobResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.Call(ctx, "Rsync", "delete", map[string]string{
		"uuid": state.ID.ValueString(),
	}, nil)
	if err != nil {
		resp.Diagnostics.AddError("Error Deleting Rsync Job", err.Error())
		return
	}

	// Unlike ShareMgmt.delete(), Rsync.delete() only removes the config
	// object -- the actual cron job/script on disk isn't cleaned up until
	// a deploy runs (that's the "rsync" module's deploy() rewriting the
	// whole set of cron scripts from the now-updated config). So a failed
	// apply here is more consequential than in the shared folder case: the
	// stale cron job could keep running on its old schedule until deploy
	// eventually succeeds. Still reported as a warning rather than a
	// blocking error -- the config object genuinely is gone and retrying
	// Delete would just fail with "not found" -- but the message is more
	// pointed about the residual risk.
	if _, err := r.client.ApplyChanges(ctx, dirtiedByRsyncJobChanges, false); err != nil {
		resp.Diagnostics.AddWarning(
			"Rsync Job Deleted, but Deploying the Change Failed",
			fmt.Sprintf(
				"The rsync job was removed from OMV's configuration, but regenerating the cron job/script "+
					"files (Config.applyChanges) failed: %s. Until this is resolved (retry from the OMV web "+
					"UI's pending changes panel), the job's OLD cron entry and wrapper script may still be "+
					"present on disk and could still run on its previous schedule.",
				err,
			),
		)
	}
}

func (r *RsyncJobResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// toRPCObject builds the RPC payload for set(), sending every field
// required by rpc.rsync.json regardless of type/mode (see this file's top
// doc comment).
func (r *RsyncJobResource) toRPCObject(ctx context.Context, uuid string, m *rsyncJobResourceModel) (rsyncJobRPCObject, diag.Diagnostics) {
	var diags diag.Diagnostics

	minute := stringListToSlice(ctx, m.Minute, &diags)
	hour := stringListToSlice(ctx, m.Hour, &diags)
	month := stringListToSlice(ctx, m.Month, &diags)
	dayOfMonth := stringListToSlice(ctx, m.DayOfMonth, &diags)
	dayOfWeek := stringListToSlice(ctx, m.DayOfWeek, &diags)
	if diags.HasError() {
		return rsyncJobRPCObject{}, diags
	}

	obj := rsyncJobRPCObject{
		UUID:      uuid,
		Enable:    m.Enable.ValueBool(),
		SendEmail: m.SendEmail.ValueBool(),
		Comment:   m.Comment.ValueString(),

		Type: m.Type.ValueString(),
		Mode: m.Mode.ValueString(),

		SrcSharedFolderRef:  m.SrcSharedFolderID.ValueString(),
		SrcURI:              m.SrcURI.ValueString(),
		DestSharedFolderRef: m.DestSharedFolderID.ValueString(),
		DestURI:             m.DestURI.ValueString(),

		Minute:           minute,
		EveryNMinute:     m.EveryNMinute.ValueBool(),
		Hour:             hour,
		EveryNHour:       m.EveryNHour.ValueBool(),
		Month:            month,
		DayOfMonth:       dayOfMonth,
		EveryNDayOfMonth: m.EveryNDayOfMonth.ValueBool(),
		DayOfWeek:        dayOfWeek,

		OptionRecursive: m.OptionRecursive.ValueBool(),
		OptionTimes:     m.OptionTimes.ValueBool(),
		OptionGroup:     m.OptionGroup.ValueBool(),
		OptionOwner:     m.OptionOwner.ValueBool(),
		OptionCompress:  m.OptionCompress.ValueBool(),
		OptionArchive:   m.OptionArchive.ValueBool(),
		OptionDelete:    m.OptionDelete.ValueBool(),
		OptionQuiet:     m.OptionQuiet.ValueBool(),
		OptionPerms:     m.OptionPerms.ValueBool(),
		OptionACLs:      m.OptionACLs.ValueBool(),
		OptionXattrs:    m.OptionXattrs.ValueBool(),
		OptionDryRun:    m.OptionDryRun.ValueBool(),
		OptionPartial:   m.OptionPartial.ValueBool(),

		ExtraOptions: m.ExtraOptions.ValueString(),

		Authentication:    m.Authentication.ValueString(),
		Password:          m.Password.ValueString(),
		SSHCertificateRef: m.SSHCertificateID.ValueString(),
		SSHPort:           m.SSHPort.ValueInt64(),
	}
	return obj, diags
}

// fromRPCObject copies an RPC response back into the Terraform model. It
// intentionally does NOT overwrite m.Password from obj.Password:
// Rsync.get() does return the stored password in plaintext, but echoing
// whatever OMV has back into state on every Read would fight with a plan
// that's deliberately changing it, and there's no server-side reason to
// trust the roundtrip over what was just sent, so the value the caller
// last set() is left as-is.
//
// The exception is when m.Password is null/unknown, which only happens
// right after `terraform import` (Create/Update always populate it from
// the plan before calling this). Left null forever, that would cause the
// exact same perpetual "will be set" diff as the analogous mode issue in
// shared_folder_resource.go: state has nothing to compare the
// configuration against. Falling back to the schema default ("") means an
// import followed by a plan against a job with no real password comes
// back clean; if the user's configuration specifies one, the first apply
// pushes it via Update and the diff won't recur.
func (r *RsyncJobResource) fromRPCObject(ctx context.Context, obj *rsyncJobRPCObject, m *rsyncJobResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics

	m.ID = types.StringValue(obj.UUID)
	m.Enable = types.BoolValue(obj.Enable)
	m.SendEmail = types.BoolValue(obj.SendEmail)
	m.Comment = types.StringValue(obj.Comment)
	m.Type = types.StringValue(obj.Type)
	m.Mode = types.StringValue(obj.Mode)
	m.SrcSharedFolderID = types.StringValue(obj.SrcSharedFolderRef)
	m.SrcURI = types.StringValue(obj.SrcURI)
	m.DestSharedFolderID = types.StringValue(obj.DestSharedFolderRef)
	m.DestURI = types.StringValue(obj.DestURI)

	m.Minute = sliceToStringList(ctx, obj.Minute, &diags)
	m.EveryNMinute = types.BoolValue(obj.EveryNMinute)
	m.Hour = sliceToStringList(ctx, obj.Hour, &diags)
	m.EveryNHour = types.BoolValue(obj.EveryNHour)
	m.Month = sliceToStringList(ctx, obj.Month, &diags)
	m.DayOfMonth = sliceToStringList(ctx, obj.DayOfMonth, &diags)
	m.EveryNDayOfMonth = types.BoolValue(obj.EveryNDayOfMonth)
	m.DayOfWeek = sliceToStringList(ctx, obj.DayOfWeek, &diags)

	m.OptionRecursive = types.BoolValue(obj.OptionRecursive)
	m.OptionTimes = types.BoolValue(obj.OptionTimes)
	m.OptionGroup = types.BoolValue(obj.OptionGroup)
	m.OptionOwner = types.BoolValue(obj.OptionOwner)
	m.OptionCompress = types.BoolValue(obj.OptionCompress)
	m.OptionArchive = types.BoolValue(obj.OptionArchive)
	m.OptionDelete = types.BoolValue(obj.OptionDelete)
	m.OptionQuiet = types.BoolValue(obj.OptionQuiet)
	m.OptionPerms = types.BoolValue(obj.OptionPerms)
	m.OptionACLs = types.BoolValue(obj.OptionACLs)
	m.OptionXattrs = types.BoolValue(obj.OptionXattrs)
	m.OptionDryRun = types.BoolValue(obj.OptionDryRun)
	m.OptionPartial = types.BoolValue(obj.OptionPartial)

	m.ExtraOptions = types.StringValue(obj.ExtraOptions)
	m.Authentication = types.StringValue(obj.Authentication)
	// m.Password intentionally left untouched -- see doc comment above --
	// except for the null/unknown (post-import) case, which needs some
	// concrete value to avoid a perpetual plan diff.
	if m.Password.IsNull() || m.Password.IsUnknown() {
		m.Password = types.StringValue("")
	}
	m.SSHCertificateID = types.StringValue(obj.SSHCertificateRef)
	m.SSHPort = types.Int64Value(obj.SSHPort)

	return diags
}

// applyOrHandleApplyFailure delegates to the shared implementation in
// apply_helper.go (see its doc comment for the full rationale, including
// the client-side-timeout-vs-real-failure distinction), scoped to the
// modules a RsyncJobResource change dirties.
func (r *RsyncJobResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	applyOrHandleApplyFailure(ctx, r.client, r.revertOnApplyFailure, dirtiedByRsyncJobChanges, diags)
}

// stringListToSlice converts a types.List of strings to a []string,
// appending any conversion diagnostics to diags. A null/unknown list
// (shouldn't happen given every list attribute here has a Default) yields
// an empty slice rather than a nil-dereference.
func stringListToSlice(ctx context.Context, l types.List, diags *diag.Diagnostics) []string {
	if l.IsNull() || l.IsUnknown() {
		return []string{}
	}
	var out []string
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return out
}

// sliceToStringList is stringListToSlice's inverse, for building model
// values out of an RPC response.
func sliceToStringList(ctx context.Context, s []string, diags *diag.Diagnostics) types.List {
	l, d := types.ListValueFrom(ctx, types.StringType, s)
	diags.Append(d...)
	return l
}
