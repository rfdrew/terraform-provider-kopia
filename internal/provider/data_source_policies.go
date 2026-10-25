package provider

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ datasource.DataSource              = &policiesDataSource{}
	_ datasource.DataSourceWithConfigure = &policiesDataSource{}
)

// NewPoliciesDataSource returns the kopia_policies data source.
func NewPoliciesDataSource() datasource.DataSource { return &policiesDataSource{} }

type policiesDataSource struct {
	client *kopiaclient.Client
}

type policiesDataSourceModel struct {
	Host     types.String             `tfsdk:"host"`
	UserName types.String             `tfsdk:"user_name"`
	Path     types.String             `tfsdk:"path"`
	Policies []policiesDataSourceItem `tfsdk:"policies"`
}

type policiesDataSourceItem struct {
	Target     types.String `tfsdk:"target"`
	Host       types.String `tfsdk:"host"`
	UserName   types.String `tfsdk:"user_name"`
	Path       types.String `tfsdk:"path"`
	ManifestID types.String `tfsdk:"manifest_id"`
	PolicyJSON types.String `tfsdk:"policy_json"`
}

func (d *policiesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_policies"
}

func (d *policiesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	filter := func(what string) schema.StringAttribute {
		return schema.StringAttribute{
			Optional:            true,
			MarkdownDescription: "Lists only the policies whose target has exactly this " + what + ".",
			Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
		}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the targets that have a policy of their own on the Kopia server, with " +
			"the policy each one stores — what it sets itself, not what it inherits; the " +
			"`kopia_effective_policy` data source resolves that. Unlike the `kopia_policy` resource, it " +
			"takes ownership of nothing.\n\n" +
			"The filters match exactly, and a filter left out matches everything, so the global policy " +
			"cannot be asked for on its own: filter the list in Terraform instead.",
		Attributes: map[string]schema.Attribute{
			"host":      filter("host"),
			"user_name": filter("user name"),
			"path":      filter("path"),
			"policies": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The policies, sorted by target. Kopia can hold targets the " +
					"`kopia_policy` resource refuses, such as `docker@` (no host) or `@host1:/data` (no user), " +
					"and they are listed as Kopia writes them.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"target": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Target of the policy as Kopia writes it, such as `(global)`, " +
								"`@host1`, `docker@host1` or `docker@host1:/data/app`: the `target` of a " +
								"`kopia_policy`, and its import id.",
						},
						"host": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Host of the target; null for the global policy.",
						},
						"user_name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "User of the target; null for the global policy and host policies.",
						},
						"path": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Path of the target; null unless it is a path policy.",
						},
						"manifest_id": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Identifier of the repository manifest the policy is stored " +
								"in. Kopia writes a new manifest on every change, so it changes with the policy.",
						},
						"policy_json": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "The stored policy as compact JSON with Kopia's own field " +
								"names, such as `{\"retention\":{\"keepDaily\":14}}`, for `jsondecode`. Empty " +
								"sections are left out, so the text is the same whichever Kopia release " +
								"answered; so are fields this provider does not know, which a later Kopia " +
								"release might add.",
						},
					},
				},
			},
		},
	}
}

func (d *policiesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *policiesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config policiesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entries, err := d.client.ListPolicies(ctx, kopiaclient.PolicyFilter{
		Host:     config.Host.ValueString(),
		UserName: config.UserName.ValueString(),
		Path:     config.Path.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to list Kopia policies", policyReadErrorDetail(err))
		return
	}

	// Kopia lists policies by when they last changed, so the order would
	// move with every apply; the target, then the manifest id for the rare
	// duplicate, is stable.
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].Target.String(), entries[j].Target.String()
		if a != b {
			return a < b
		}
		return entries[i].ID < entries[j].ID
	})

	config.Policies = make([]policiesDataSourceItem, 0, len(entries))
	for _, entry := range entries {
		policy := kopiaclient.Policy{}
		if entry.Policy != nil {
			policy = *entry.Policy
		}
		encoded, err := json.Marshal(policy)
		if err != nil {
			resp.Diagnostics.AddError("Unable to encode Kopia policy",
				"Encoding the policy of "+entry.Target.String()+" as JSON failed. This is a bug in the provider. "+err.Error())
			return
		}
		config.Policies = append(config.Policies, policiesDataSourceItem{
			Target:     types.StringValue(entry.Target.String()),
			Host:       stringValueOrNullIfEmpty(entry.Target.Host),
			UserName:   stringValueOrNullIfEmpty(entry.Target.UserName),
			Path:       stringValueOrNullIfEmpty(entry.Target.Path),
			ManifestID: types.StringValue(entry.ID),
			PolicyJSON: types.StringValue(string(encoded)),
		})
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
