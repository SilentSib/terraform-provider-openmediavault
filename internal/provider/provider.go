package provider

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// minSupportedOMVVersion is the lowest OpenMediaVault major version this
// provider is validated against. Configure() refuses to proceed against
// older instances so that resources can rely on OMV 8's RPC schemas.
const minSupportedOMVVersion = 8

// Ensure the implementation satisfies the expected interfaces.
var (
	_ provider.Provider = &OMVProvider{}
)

// OMVProvider is the top level implementation of the openmediavault
// Terraform provider.
type OMVProvider struct {
	// version is set at build time via -ldflags, see main.go.
	version string
}

// omvProviderModel maps the provider block's configuration to Go types.
type omvProviderModel struct {
	Host                 types.String `tfsdk:"host"`
	Port                 types.Int64  `tfsdk:"port"`
	Scheme               types.String `tfsdk:"scheme"`
	Username             types.String `tfsdk:"username"`
	Password             types.String `tfsdk:"password"`
	InsecureSkipVerify   types.Bool   `tfsdk:"insecure_skip_verify"`
	RevertOnApplyFailure types.Bool   `tfsdk:"revert_on_apply_failure"`
}

// providerData is what's stashed in resp.ResourceData / resp.DataSourceData
// for resources/data sources to read back out in their Configure methods.
type providerData struct {
	Client *omvclient.Client
	// RevertOnApplyFailure controls what resources do when a mutating RPC
	// succeeds (the config database was updated) but the follow-up
	// Config.applyChanges deploy step fails.
	//
	// OMV's pending-changes queue and its "Undo" action (Config.
	// revertChanges) are INSTANCE-WIDE, not scoped to whatever object a
	// single resource just touched -- reverting discards every pending
	// change on the system, including ones staged by other admins or
	// tools. Because of that this defaults to false: resources instead
	// report the apply failure as an error (so `terraform apply` fails
	// loudly, matching the OMV web UI's own behavior of leaving the
	// change queued rather than auto-undoing it) while still recording
	// the object in state, since it really was written to OMV's config.
	// Set this to true only if you want Terraform to mirror clicking
	// "Undo" in the web UI on every apply failure, understanding that it
	// can revert unrelated pending changes too.
	RevertOnApplyFailure bool
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &OMVProvider{version: version}
	}
}

func (p *OMVProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	// This becomes the prefix for every resource/data source type this
	// provider implements (via ProviderTypeName + "_shared_folder" etc. in
	// each resource's Metadata()) -- i.e. it must be "omv" for the
	// resource types to actually be "omv_shared_folder"/"omv_rsync_job",
	// matching every example, README snippet, and test in this repo.
	// This is independent of the provider's registry "source" address
	// (main.go's providerserver.ServeOpts.Address / go.mod-less path
	// "example/openmediavault") -- Terraform derives the local name it
	// looks up in required_providers from a resource's type prefix (the
	// part before the first "_"), NOT from the source address, so a
	// required_providers entry must use "omv" as its key for Terraform to
	// route "omv_*" resources to this provider at all.
	resp.TypeName = "omv"
	resp.Version = p.version
}

func (p *OMVProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages an OpenMediaVault (>= 8) NAS instance through its JSON-RPC API.",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				Optional:    true,
				Description: "Hostname or IP address of the OMV instance. May also be set via the OMV_HOST environment variable.",
			},
			"port": schema.Int64Attribute{
				Optional:    true,
				Description: "TCP port of the OMV web UI. Defaults to 443 (or 80 when scheme is \"http\"). May also be set via OMV_PORT.",
			},
			"scheme": schema.StringAttribute{
				Optional:    true,
				Description: "Either \"https\" (default) or \"http\". May also be set via OMV_SCHEME.",
			},
			"username": schema.StringAttribute{
				Optional:    true,
				Description: "OMV account used to authenticate, e.g. \"admin\". May also be set via OMV_USERNAME.",
			},
			"password": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Password for the OMV account. May also be set via OMV_PASSWORD.",
			},
			"insecure_skip_verify": schema.BoolAttribute{
				Optional:    true,
				Description: "Disable TLS certificate verification, e.g. when OMV is using its default self-signed certificate. May also be set via OMV_INSECURE_SKIP_VERIFY.",
			},
			"revert_on_apply_failure": schema.BoolAttribute{
				Optional: true,
				Description: "When a resource writes a change to OMV's config database but the follow-up " +
					"deploy step (Config.applyChanges) then fails, controls whether the provider also " +
					"calls Config.revertChanges (equivalent to the \"Undo\" button in the OMV web UI) " +
					"before returning the error. Defaults to false. IMPORTANT: OMV's pending-changes " +
					"queue is instance-wide, not scoped to a single resource -- enabling this can discard " +
					"OTHER unrelated pending changes made by other admins or tools at the same time. When " +
					"false (the default), the provider instead leaves the change queued (as the OMV web UI " +
					"itself does on a failed apply) and still records the object in Terraform state, since " +
					"it was in fact written to OMV, so a later `terraform apply` or `terraform destroy` can " +
					"reconcile it.",
			},
		},
	}
}

func (p *OMVProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data omvProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	host := firstNonEmpty(data.Host.ValueString(), os.Getenv("OMV_HOST"))
	if host == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("host"),
			"Missing OMV Host",
			"The provider requires a host, set it via the \"host\" attribute or the OMV_HOST environment variable.",
		)
	}

	username := firstNonEmpty(data.Username.ValueString(), os.Getenv("OMV_USERNAME"))
	if username == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("username"),
			"Missing OMV Username",
			"The provider requires a username, set it via the \"username\" attribute or the OMV_USERNAME environment variable.",
		)
	}

	password := firstNonEmpty(data.Password.ValueString(), os.Getenv("OMV_PASSWORD"))
	if password == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("password"),
			"Missing OMV Password",
			"The provider requires a password, set it via the \"password\" attribute or the OMV_PASSWORD environment variable.",
		)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	scheme := firstNonEmpty(data.Scheme.ValueString(), os.Getenv("OMV_SCHEME"), "https")

	var port int
	switch {
	case !data.Port.IsNull():
		port = int(data.Port.ValueInt64())
	case os.Getenv("OMV_PORT") != "":
		v, err := strconv.Atoi(os.Getenv("OMV_PORT"))
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("port"), "Invalid OMV_PORT", err.Error())
			return
		}
		port = v
	}

	insecureSkipVerify := data.InsecureSkipVerify.ValueBool()
	if !data.InsecureSkipVerify.IsNull() {
		insecureSkipVerify = data.InsecureSkipVerify.ValueBool()
	} else if v := os.Getenv("OMV_INSECURE_SKIP_VERIFY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("insecure_skip_verify"), "Invalid OMV_INSECURE_SKIP_VERIFY", err.Error())
			return
		}
		insecureSkipVerify = b
	}

	client, err := omvclient.New(omvclient.Config{
		Host:               host,
		Port:               port,
		Scheme:             scheme,
		Username:           username,
		Password:           password,
		InsecureSkipVerify: insecureSkipVerify,
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to Create OMV Client", err.Error())
		return
	}

	if err := client.Login(ctx); err != nil {
		resp.Diagnostics.AddError(
			"Unable to Authenticate to OMV",
			fmt.Sprintf("The provider could not log in to the OMV instance at %s: %s", host, err),
		)
		return
	}

	if err := client.CheckMinVersion(ctx, minSupportedOMVVersion); err != nil {
		resp.Diagnostics.AddError(
			"Unsupported OpenMediaVault Version",
			err.Error(),
		)
		return
	}

	// Make the authenticated client available to resources and data sources.
	pd := &providerData{
		Client:               client,
		RevertOnApplyFailure: data.RevertOnApplyFailure.ValueBool(),
	}
	resp.ResourceData = pd
	resp.DataSourceData = pd
}

func (p *OMVProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewSharedFolderResource,
		NewRsyncJobResource,
		NewWorkbenchSettingsResource,
		NewSSLCertificateResource,
	}
}

func (p *OMVProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewSharedFolderDataSource,
	}
}

// firstNonEmpty returns the first non-empty string among candidates.
func firstNonEmpty(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}
