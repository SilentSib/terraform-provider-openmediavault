package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

var (
	_ datasource.DataSource              = &SharedFolderDataSource{}
	_ datasource.DataSourceWithConfigure = &SharedFolderDataSource{}
)

func NewSharedFolderDataSource() datasource.DataSource {
	return &SharedFolderDataSource{}
}

// SharedFolderDataSource implements the omv_shared_folder data source,
// looking a shared folder up by name via ShareMgmt.enumerateSharedFolders.
// Same verification caveats as shared_folder_resource.go apply.
type SharedFolderDataSource struct {
	client *omvclient.Client
}

type sharedFolderDataSourceModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	MountPointID types.String `tfsdk:"mount_point_id"`
	RelativePath types.String `tfsdk:"relative_path"`
	Comment      types.String `tfsdk:"comment"`
}

func (d *SharedFolderDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_shared_folder"
}

func (d *SharedFolderDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an existing OpenMediaVault shared folder by name. " +
			"TEMPLATE: verify RPC field names against your OMV 8 instance before use.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "UUID of the shared folder.",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Name of the shared folder to look up.",
			},
			"mount_point_id": schema.StringAttribute{
				Computed:    true,
				Description: "UUID of the filesystem/mount point the shared folder lives on.",
			},
			"relative_path": schema.StringAttribute{
				Computed:    true,
				Description: "Path of the shared folder relative to the mount point.",
			},
			"comment": schema.StringAttribute{
				Computed:    true,
				Description: "Free-form description shown in the OMV UI.",
			},
		},
	}
}

func (d *SharedFolderDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T. This is a provider bug, please report it.", req.ProviderData),
		)
		return
	}
	d.client = pd.Client
}

func (d *SharedFolderDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config sharedFolderDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var all []sharedFolderRPCObject
	err := d.client.Call(ctx, "ShareMgmt", "enumerateSharedFolders", nil, &all)
	if err != nil {
		resp.Diagnostics.AddError("Error Listing Shared Folders", err.Error())
		return
	}

	name := config.Name.ValueString()
	var match *sharedFolderRPCObject
	for i := range all {
		if all[i].Name == name {
			match = &all[i]
			break
		}
	}
	if match == nil {
		resp.Diagnostics.AddError(
			"Shared Folder Not Found",
			fmt.Sprintf("No shared folder named %q was found on this OMV instance.", name),
		)
		return
	}

	config.ID = types.StringValue(match.UUID)
	config.MountPointID = types.StringValue(match.MountPointID)
	config.RelativePath = types.StringValue(match.RelativePath)
	config.Comment = types.StringValue(match.Comment)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
