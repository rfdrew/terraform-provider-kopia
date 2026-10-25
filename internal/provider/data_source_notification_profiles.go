package provider

import (
	"context"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ datasource.DataSource              = &notificationProfilesDataSource{}
	_ datasource.DataSourceWithConfigure = &notificationProfilesDataSource{}
)

// NewNotificationProfilesDataSource returns the kopia_notification_profiles
// data source.
func NewNotificationProfilesDataSource() datasource.DataSource {
	return &notificationProfilesDataSource{}
}

type notificationProfilesDataSource struct {
	client *kopiaclient.Client
}

type notificationProfileSummaryModel struct {
	Name        types.String `tfsdk:"name"`
	Method      types.String `tfsdk:"method"`
	MinSeverity types.String `tfsdk:"min_severity"`
}

type notificationProfilesDataSourceModel struct {
	Profiles []notificationProfileSummaryModel `tfsdk:"profiles"`
}

func (d *notificationProfilesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_profiles"
}

func (d *notificationProfilesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the notification profiles of the Kopia server: their names, methods and " +
			"minimum severities. Settings are never listed, so no secret reaches the state through this data " +
			"source; manage a profile with `kopia_notification_profile` to see them.\n\n" +
			"Useful to find the name of a profile set up in the Kopia UI or with the CLI, to import it as a " +
			"`kopia_notification_profile`, or in a `check` block. Kopia keeps profiles in the repository, so " +
			"the server needs one connected.",
		Attributes: map[string]schema.Attribute{
			"profiles": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Every profile, sorted by name. Kopia lists profiles by when they were last " +
					"saved, and lists a name twice should two clients have saved it at the same moment; it is " +
					"listed once here, as Kopia reads it.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Name of the profile, which is also the id to import it by as a " +
								"`kopia_notification_profile`.",
						},
						"method": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "How the profile sends: `email`, `pushover` or `webhook`, or a " +
								"method Kopia knows but `kopia_notification_profile` does not manage, such as " +
								"`testsender`.",
						},
						"min_severity": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Lowest severity of the notifications the profile receives. " +
								notificationSeverityDescription,
						},
					},
				},
			},
		},
	}
}

func (d *notificationProfilesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *notificationProfilesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	listed, err := d.client.ListNotificationProfiles(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to list Kopia notification profiles", err.Error())
		return
	}

	// Kopia lists the oldest first and does not de-duplicate names; its GET
	// returns the latest of a name's entries, so the last one wins here too.
	byName := make(map[string]kopiaclient.NotificationProfileSummary, len(listed))
	for _, p := range listed {
		byName[p.Name] = p
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	state := notificationProfilesDataSourceModel{Profiles: make([]notificationProfileSummaryModel, 0, len(names))}
	for _, name := range names {
		p := byName[name]
		state.Profiles = append(state.Profiles, notificationProfileSummaryModel{
			Name:        types.StringValue(p.Name),
			Method:      types.StringValue(p.MethodType),
			MinSeverity: types.StringValue(notificationSeverityString(p.MinSeverity)),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
