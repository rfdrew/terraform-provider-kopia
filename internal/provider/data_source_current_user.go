package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ datasource.DataSource              = &currentUserDataSource{}
	_ datasource.DataSourceWithConfigure = &currentUserDataSource{}
)

// NewCurrentUserDataSource returns the kopia_current_user data source.
func NewCurrentUserDataSource() datasource.DataSource { return &currentUserDataSource{} }

type currentUserDataSource struct {
	client *kopiaclient.Client
}

type currentUserDataSourceModel struct {
	Username types.String `tfsdk:"username"`
	Hostname types.String `tfsdk:"hostname"`
}

func (d *currentUserDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_current_user"
}

func (d *currentUserDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reports the operating-system user the Kopia server process runs as, and the host " +
			"name of the machine — or container — it runs on. It works without a repository connected.\n\n" +
			"~> **This is neither the UI user the provider signs in as nor the identity the server takes " +
			"snapshots as.** The latter is `username` and `hostname` of the `kopia_repository_status` data " +
			"source, which a repository connection may override, and which then differ from these.",
		Attributes: map[string]schema.Attribute{
			"username": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Name of the operating-system account running the server, without a Windows " +
					"domain: `root` for the official Docker image. `nobody` when Kopia cannot look it up.",
			},
			"hostname": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Host name of the machine running the server, lower-cased and cut at the " +
					"first dot. In a container it is the container's host name. `nohost` when Kopia cannot look " +
					"it up.",
			},
		},
	}
}

func (d *currentUserDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *currentUserDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	user, err := d.client.CurrentUser(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Kopia current user", repositoryCallErrorDetail(err))
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &currentUserDataSourceModel{
		Username: types.StringValue(user.Username),
		Hostname: types.StringValue(user.Hostname),
	})...)
}
