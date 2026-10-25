package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ datasource.DataSource              = &snapshotsDataSource{}
	_ datasource.DataSourceWithConfigure = &snapshotsDataSource{}
)

// NewSnapshotsDataSource returns the kopia_snapshots data source.
func NewSnapshotsDataSource() datasource.DataSource { return &snapshotsDataSource{} }

type snapshotsDataSource struct {
	client *kopiaclient.Client
}

type snapshotsDataSourceModel struct {
	Host            types.String          `tfsdk:"host"`
	UserName        types.String          `tfsdk:"user_name"`
	Path            types.String          `tfsdk:"path"`
	All             types.Bool            `tfsdk:"all"`
	Snapshots       []snapshotsEntryModel `tfsdk:"snapshots"`
	UnfilteredCount types.Int64           `tfsdk:"unfiltered_count"`
	UniqueCount     types.Int64           `tfsdk:"unique_count"`
}

type snapshotsEntryModel struct {
	ID               types.String           `tfsdk:"id"`
	Description      types.String           `tfsdk:"description"`
	StartTime        types.String           `tfsdk:"start_time"`
	EndTime          types.String           `tfsdk:"end_time"`
	IncompleteReason types.String           `tfsdk:"incomplete_reason"`
	RootID           types.String           `tfsdk:"root_id"`
	Retention        types.List             `tfsdk:"retention"`
	Pins             types.List             `tfsdk:"pins"`
	Summary          *snapshotsSummaryModel `tfsdk:"summary"`
}

type snapshotsSummaryModel struct {
	Size             types.Int64                  `tfsdk:"size"`
	Files            types.Int64                  `tfsdk:"files"`
	Symlinks         types.Int64                  `tfsdk:"symlinks"`
	Dirs             types.Int64                  `tfsdk:"dirs"`
	MaxTime          types.String                 `tfsdk:"max_time"`
	IncompleteReason types.String                 `tfsdk:"incomplete_reason"`
	NumFailed        types.Int64                  `tfsdk:"num_failed"`
	NumIgnoredErrors types.Int64                  `tfsdk:"num_ignored_errors"`
	Errors           []snapshotsSummaryErrorModel `tfsdk:"errors"`
}

type snapshotsSummaryErrorModel struct {
	Path  types.String `tfsdk:"path"`
	Error types.String `tfsdk:"error"`
}

func (d *snapshotsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_snapshots"
}

func (d *snapshotsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computedInt64 := func(description string) schema.Int64Attribute {
		return schema.Int64Attribute{Computed: true, MarkdownDescription: description}
	}
	computedString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the snapshots in the Kopia server's repository of one host, optionally narrowed " +
			"to one user and one path, oldest first. It needs a repository connected.\n\n" +
			"Unless `all` is set, Kopia reports snapshots with identical contents once: each group of " +
			"snapshots sharing a `root_id` appears as its oldest member, with the retention reasons and pins " +
			"of the whole group. With `all`, every snapshot is listed, but the oldest of each such group still " +
			"carries the reasons and pins of its group, because Kopia works them out on the same list.\n\n" +
			"~> **Set `user_name` and `path` when `retention` matters.** Kopia works out the retention reasons " +
			"from the retention policy of the source asked for, applied to every snapshot listed, so a list " +
			"that spans several sources gets reasons that mean nothing for any of them.",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Host whose snapshots are listed. Kopia files every snapshot under its host " +
					"and has no way to list the snapshots of every host at once.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"user_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "List only the snapshots of this user. Leave it out for every user of the host.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"path": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "List only the snapshots of this path, compared as a plain string. Leave it " +
					"out for every path.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"all": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "List every snapshot, rather than one for each set of identical contents. " +
					"Defaults to `false`.",
			},
			"unfiltered_count": computedInt64("Number of snapshots found, before snapshots with identical " +
				"contents were merged; unreadable snapshots are not counted."),
			"unique_count": computedInt64("Number of distinct contents among the snapshots found: the length of " +
				"`snapshots` unless `all` is set."),
			"snapshots": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The snapshots, sorted by `start_time`, oldest first; snapshots that " +
					"started at the same moment come in no particular order.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": computedString("Identifier of the snapshot's manifest. It changes whenever the " +
							"snapshot is edited, so identify a snapshot by `start_time` across runs."),
						"description": computedString("Description of the snapshot; empty when it has none."),
						"start_time":  computedString("When the snapshot started, as an RFC 3339 timestamp in UTC."),
						"end_time":    computedString("When the snapshot ended, as an RFC 3339 timestamp in UTC."),
						"incomplete_reason": computedString("Why the snapshot is incomplete — `checkpoint`, " +
							"`canceled` or `limit reached` — or null for a complete snapshot."),
						"root_id": computedString("Identifier of the snapshot's root object, which two snapshots of " +
							"unchanged data share; null when the snapshot has no root."),
						"retention": schema.ListAttribute{
							Computed:    true,
							ElementType: types.StringType,
							MarkdownDescription: "Why the retention policy keeps the snapshot, such as `latest-1` or " +
								"`daily-2`, or `incomplete` for a recent incomplete one; empty when it does not keep " +
								"it. Runs are merged, as in `daily-1..3`, for snapshots that stand for a group.",
						},
						"pins": schema.ListAttribute{
							Computed:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "Pins that keep the snapshot from being deleted by retention.",
						},
						"summary": schema.SingleNestedAttribute{
							Computed: true,
							MarkdownDescription: "Totals over the snapshot's root directory; null for a snapshot " +
								"without one. Sizes are in bytes.",
							Attributes: map[string]schema.Attribute{
								"size":     computedInt64("Total size of the files."),
								"files":    computedInt64("Number of files."),
								"symlinks": computedInt64("Number of symbolic links."),
								"dirs":     computedInt64("Number of directories."),
								"max_time": computedString("Newest modification time in the tree, as an RFC 3339 " +
									"timestamp in UTC; the Unix epoch, `1970-01-01T00:00:00Z`, when there is none."),
								"incomplete_reason": computedString("Why the tree is incomplete, or null when it " +
									"is complete."),
								"num_failed":         computedInt64("Number of entries that could not be read."),
								"num_ignored_errors": computedInt64("Number of errors the error-handling policy ignored."),
								"errors": schema.ListNestedAttribute{
									Computed:            true,
									MarkdownDescription: "The first ten entries that could not be read.",
									NestedObject: schema.NestedAttributeObject{
										Attributes: map[string]schema.Attribute{
											"path":  computedString("Path of the entry, within the snapshot."),
											"error": computedString("What went wrong."),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (d *snapshotsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// snapshotsEntryFromAPI maps one snapshot. Kopia never sends null retention
// reasons or pins; the empty lists they map to keep it that way should a
// future release do so.
func snapshotsEntryFromAPI(snapshot *kopiaclient.Snapshot) snapshotsEntryModel {
	entry := snapshotsEntryModel{
		ID:               types.StringValue(snapshot.ID),
		Description:      types.StringValue(snapshot.Description),
		StartTime:        repositoryTimeValue(snapshot.StartTime),
		EndTime:          repositoryTimeValue(snapshot.EndTime),
		IncompleteReason: stringValueOrNullIfEmpty(snapshot.IncompleteReason),
		RootID:           stringValueOrNullIfEmpty(snapshot.RootID),
		Retention:        stringListValue(snapshot.Retention),
		Pins:             stringListValue(snapshot.Pins),
	}

	if summary := snapshot.Summary; summary != nil {
		entry.Summary = &snapshotsSummaryModel{
			Size:             types.Int64Value(summary.Size),
			Files:            types.Int64Value(summary.Files),
			Symlinks:         types.Int64Value(summary.Symlinks),
			Dirs:             types.Int64Value(summary.Dirs),
			MaxTime:          repositoryTimeValue(summary.MaxTime),
			IncompleteReason: stringValueOrNullIfEmpty(summary.IncompleteReason),
			NumFailed:        types.Int64Value(summary.NumFailed),
			NumIgnoredErrors: types.Int64Value(summary.NumIgnoredErrors),
			Errors:           make([]snapshotsSummaryErrorModel, 0, len(summary.Errors)),
		}
		for _, failed := range summary.Errors {
			entry.Summary.Errors = append(entry.Summary.Errors, snapshotsSummaryErrorModel{
				Path:  types.StringValue(failed.Path),
				Error: types.StringValue(failed.Error),
			})
		}
	}
	return entry
}

func (d *snapshotsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config snapshotsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	listed, err := d.client.ListSnapshots(ctx, config.Host.ValueString(), config.UserName.ValueString(),
		config.Path.ValueString(), config.All.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Unable to list Kopia snapshots", repositoryCallErrorDetail(err))
		return
	}

	state := snapshotsDataSourceModel{
		Host:            config.Host,
		UserName:        config.UserName,
		Path:            config.Path,
		All:             config.All,
		Snapshots:       make([]snapshotsEntryModel, 0, len(listed.Snapshots)),
		UnfilteredCount: types.Int64Value(listed.UnfilteredCount),
		UniqueCount:     types.Int64Value(listed.UniqueCount),
	}
	for i := range listed.Snapshots {
		state.Snapshots = append(state.Snapshots, snapshotsEntryFromAPI(&listed.Snapshots[i]))
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
