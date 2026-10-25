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
	_ datasource.DataSource              = &sourcesDataSource{}
	_ datasource.DataSourceWithConfigure = &sourcesDataSource{}
)

// NewSourcesDataSource returns the kopia_sources data source.
func NewSourcesDataSource() datasource.DataSource { return &sourcesDataSource{} }

type sourcesDataSource struct {
	client *kopiaclient.Client
}

type sourcesDataSourceModel struct {
	Host          types.String        `tfsdk:"host"`
	UserName      types.String        `tfsdk:"user_name"`
	Path          types.String        `tfsdk:"path"`
	LocalHost     types.String        `tfsdk:"local_host"`
	LocalUsername types.String        `tfsdk:"local_username"`
	MultiUser     types.Bool          `tfsdk:"multi_user"`
	Sources       []sourcesEntryModel `tfsdk:"sources"`
}

type sourcesEntryModel struct {
	Target           types.String              `tfsdk:"target"`
	Host             types.String              `tfsdk:"host"`
	UserName         types.String              `tfsdk:"user_name"`
	Path             types.String              `tfsdk:"path"`
	Status           types.String              `tfsdk:"status"`
	Schedule         sourcesScheduleModel      `tfsdk:"schedule"`
	NextSnapshotTime types.String              `tfsdk:"next_snapshot_time"`
	LastSnapshot     *sourcesLastSnapshotModel `tfsdk:"last_snapshot"`
	Upload           *sourcesUploadModel       `tfsdk:"upload"`
	CurrentTask      types.String              `tfsdk:"current_task"`
}

type sourcesScheduleModel struct {
	IntervalSeconds   types.Int64             `tfsdk:"interval_seconds"`
	TimeOfDay         []sourcesTimeOfDayModel `tfsdk:"time_of_day"`
	NoParentTimeOfDay types.Bool              `tfsdk:"no_parent_time_of_day"`
	Manual            types.Bool              `tfsdk:"manual"`
	Cron              types.List              `tfsdk:"cron"`
	RunMissed         types.Bool              `tfsdk:"run_missed"`
}

type sourcesTimeOfDayModel struct {
	Hour   types.Int64 `tfsdk:"hour"`
	Minute types.Int64 `tfsdk:"minute"`
}

type sourcesLastSnapshotModel struct {
	ID               types.String              `tfsdk:"id"`
	Description      types.String              `tfsdk:"description"`
	StartTime        types.String              `tfsdk:"start_time"`
	EndTime          types.String              `tfsdk:"end_time"`
	IncompleteReason types.String              `tfsdk:"incomplete_reason"`
	RootID           types.String              `tfsdk:"root_id"`
	Pins             types.List                `tfsdk:"pins"`
	Stats            sourcesSnapshotStatsModel `tfsdk:"stats"`
}

type sourcesSnapshotStatsModel struct {
	TotalSize         types.Int64 `tfsdk:"total_size"`
	ExcludedTotalSize types.Int64 `tfsdk:"excluded_total_size"`
	FileCount         types.Int64 `tfsdk:"file_count"`
	CachedFiles       types.Int64 `tfsdk:"cached_files"`
	NonCachedFiles    types.Int64 `tfsdk:"non_cached_files"`
	DirCount          types.Int64 `tfsdk:"dir_count"`
	ExcludedFileCount types.Int64 `tfsdk:"excluded_file_count"`
	ExcludedDirCount  types.Int64 `tfsdk:"excluded_dir_count"`
	IgnoredErrorCount types.Int64 `tfsdk:"ignored_error_count"`
	ErrorCount        types.Int64 `tfsdk:"error_count"`
}

type sourcesUploadModel struct {
	CachedBytes    types.Int64  `tfsdk:"cached_bytes"`
	HashedBytes    types.Int64  `tfsdk:"hashed_bytes"`
	EstimatedBytes types.Int64  `tfsdk:"estimated_bytes"`
	CachedFiles    types.Int64  `tfsdk:"cached_files"`
	HashedFiles    types.Int64  `tfsdk:"hashed_files"`
	EstimatedFiles types.Int64  `tfsdk:"estimated_files"`
	Errors         types.Int64  `tfsdk:"errors"`
	IgnoredErrors  types.Int64  `tfsdk:"ignored_errors"`
	Directory      types.String `tfsdk:"directory"`
	LastErrorPath  types.String `tfsdk:"last_error_path"`
	LastError      types.String `tfsdk:"last_error"`
}

func (d *sourcesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sources"
}

// sourcesComputedInt64 is a computed integer attribute, for the counters.
func sourcesComputedInt64(description string) schema.Int64Attribute {
	return schema.Int64Attribute{Computed: true, MarkdownDescription: description}
}

// sourcesComputedString is a computed string attribute.
func sourcesComputedString(description string) schema.StringAttribute {
	return schema.StringAttribute{Computed: true, MarkdownDescription: description}
}

// sourcesFilterAttribute is one of the optional filters, which Kopia matches
// exactly. An empty filter would match everything, which leaving it out
// already says.
func sourcesFilterAttribute(description string) schema.StringAttribute {
	return schema.StringAttribute{
		Optional:            true,
		MarkdownDescription: description,
		Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
	}
}

func (d *sourcesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the snapshot sources the Kopia server knows, with the state of each: whether it " +
			"is idle, uploading or remote, its effective schedule, when its next snapshot is due and its last " +
			"snapshot. It needs a repository connected.\n\n" +
			"A source is a user, a host and a path. The list holds every source with a snapshot in the " +
			"repository, from any host, every policy target with a user, a host and a path, and every source " +
			"added in the Kopia UI. Kopia brings it up to date when a policy changes and every few hours " +
			"(`--refresh-interval`, `4h` by default), so a source can lag behind snapshots taken or deleted " +
			"elsewhere.\n\n" +
			"The state is the server's view as of its last refresh of each source. `status`, `current_task` and " +
			"`next_snapshot_time` are hints rather than a reliable state machine: a source that has just been " +
			"asked for a snapshot has no next snapshot time until that snapshot finishes, and a `FAILED` status " +
			"stays until the next snapshot.",
		Attributes: map[string]schema.Attribute{
			"host": sourcesFilterAttribute("List only the sources of this host. Leave it out for every host."),
			"user_name": sourcesFilterAttribute("List only the sources of this user. Leave it out for every " +
				"user."),
			"path": sourcesFilterAttribute("List only the sources of this path, compared as a plain string. " +
				"Leave it out for every path."),
			"local_host": sourcesComputedString("Host name the server takes snapshots as, the same as " +
				"`hostname` of `kopia_repository_status`. Sources of this host are local: the server snapshots " +
				"them."),
			"local_username": sourcesComputedString("User name the server takes snapshots as, the same as " +
				"`username` of `kopia_repository_status`."),
			"multi_user": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the server sees the sources of every user and host: true for a " +
					"repository connected directly to its storage, false for one reached through a Kopia " +
					"repository server.",
			},
			"sources": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The sources that match the filters, sorted by `target`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: sourcesEntryAttributes(),
				},
			},
		},
	}
}

// sourcesEntryAttributes are the attributes of one listed source.
func sourcesEntryAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"target": sourcesComputedString("The source as Kopia writes it, `USER@HOST:PATH`: the form " +
			"`kopia_policy` takes as the `target` of a path's policy."),
		"host":      sourcesComputedString("Host the source belongs to."),
		"user_name": sourcesComputedString("User the source belongs to."),
		"path":      sourcesComputedString("Path that is snapshotted, as the host writes it."),
		"status": sourcesComputedString("State of the source on this server: `IDLE` while a local source " +
			"waits for its next snapshot, `PENDING` while a requested snapshot waits for a free slot, " +
			"`UPLOADING` while one runs, `REMOTE` for a source of another host (or of any host on a read-only " +
			"connection), which this server never snapshots, `PAUSED` for a source paused through the server's " +
			"control API, and `FAILED` when the server last failed to read its policy or snapshots. `UNKNOWN`, " +
			"`INITIALIZING` and `STOPPED` appear briefly while a source is added or removed."),
		"schedule": schema.SingleNestedAttribute{
			Computed: true,
			MarkdownDescription: "Effective scheduling policy of the source — what its own policy and those it " +
				"inherits from add up to — as of the server's last refresh of it. Empty when that refresh " +
				"failed.",
			Attributes: map[string]schema.Attribute{
				"interval_seconds": sourcesComputedInt64("Time between snapshots, in seconds; null when " +
					"snapshots are not taken at an interval."),
				"time_of_day": schema.ListNestedAttribute{
					Computed:            true,
					MarkdownDescription: "Times of day at which a snapshot is taken, in the server's time zone.",
					NestedObject: schema.NestedAttributeObject{
						Attributes: map[string]schema.Attribute{
							"hour":   sourcesComputedInt64("Hour, from 0 to 23."),
							"minute": sourcesComputedInt64("Minute, from 0 to 59."),
						},
					},
				},
				"no_parent_time_of_day": schema.BoolAttribute{
					Computed:            true,
					MarkdownDescription: "Whether the times of day ignore those of the policies the source inherits from.",
				},
				"manual": schema.BoolAttribute{
					Computed:            true,
					MarkdownDescription: "Whether snapshots are only taken when asked for, never on a schedule.",
				},
				"cron": schema.ListAttribute{
					Computed:            true,
					ElementType:         types.StringType,
					MarkdownDescription: "Cron expressions at whose times a snapshot is taken.",
				},
				"run_missed": schema.BoolAttribute{
					Computed: true,
					MarkdownDescription: "Whether a snapshot whose time passed while the server was not running " +
						"is taken as soon as it can be; null when no policy says.",
				},
			},
		},
		"next_snapshot_time": sourcesComputedString("When the next scheduled snapshot is due, as an RFC 3339 " +
			"timestamp in UTC; it may lie in the past for an overdue source. Null for a source that is only " +
			"snapshotted manually or is paused, and normally also while a snapshot is pending or uploading and " +
			"after one failed, until the server refreshes the source."),
		"last_snapshot": schema.SingleNestedAttribute{
			Computed: true,
			MarkdownDescription: "Newest snapshot of the source, complete or not, as of the server's last refresh " +
				"of it; null when it has none. Its `id` can be out of date for a while after the snapshot is " +
				"edited or deleted.",
			Attributes: map[string]schema.Attribute{
				"id": sourcesComputedString("Identifier of the snapshot's manifest, which changes when the " +
					"snapshot is edited."),
				"description": sourcesComputedString("Description of the snapshot; empty when it has none."),
				"start_time":  sourcesComputedString("When the snapshot started, as an RFC 3339 timestamp in UTC."),
				"end_time":    sourcesComputedString("When the snapshot ended, as an RFC 3339 timestamp in UTC."),
				"incomplete_reason": sourcesComputedString("Why the snapshot is incomplete — `checkpoint`, " +
					"`canceled` or `limit reached` — or null for a complete snapshot."),
				"root_id": sourcesComputedString("Identifier of the snapshot's root object, which two snapshots " +
					"of unchanged data share; null when the snapshot has no root."),
				"pins": schema.ListAttribute{
					Computed:            true,
					ElementType:         types.StringType,
					MarkdownDescription: "Pins that keep the snapshot from being deleted by retention.",
				},
				"stats": schema.SingleNestedAttribute{
					Computed:            true,
					MarkdownDescription: "What the upload that made the snapshot saw. Sizes are in bytes.",
					Attributes: map[string]schema.Attribute{
						"total_size":          sourcesComputedInt64("Total size of the files snapshotted."),
						"excluded_total_size": sourcesComputedInt64("Total size of the files excluded by policy."),
						"file_count":          sourcesComputedInt64("Number of files snapshotted."),
						"cached_files": sourcesComputedInt64("Number of files found unchanged since the previous " +
							"snapshot, and not read again."),
						"non_cached_files":    sourcesComputedInt64("Number of files read and hashed."),
						"dir_count":           sourcesComputedInt64("Number of directories snapshotted."),
						"excluded_file_count": sourcesComputedInt64("Number of files excluded by policy."),
						"excluded_dir_count":  sourcesComputedInt64("Number of directories excluded by policy."),
						"ignored_error_count": sourcesComputedInt64("Number of errors the error-handling policy " +
							"ignored."),
						"error_count": sourcesComputedInt64("Number of errors that were not ignored."),
					},
				},
			},
		},
		"upload": schema.SingleNestedAttribute{
			Computed: true,
			MarkdownDescription: "Progress of the snapshot being taken while `status` is `UPLOADING`; null " +
				"otherwise. Sizes are in bytes.",
			Attributes: map[string]schema.Attribute{
				"cached_bytes": sourcesComputedInt64("Bytes of files found unchanged, which are not read " +
					"again."),
				"hashed_bytes":    sourcesComputedInt64("Bytes read and hashed so far."),
				"estimated_bytes": sourcesComputedInt64("Estimated total bytes of the snapshot."),
				"cached_files":    sourcesComputedInt64("Number of files found unchanged."),
				"hashed_files":    sourcesComputedInt64("Number of files read and hashed so far."),
				"estimated_files": sourcesComputedInt64("Estimated total number of files of the snapshot."),
				"errors":          sourcesComputedInt64("Number of errors so far that were not ignored."),
				"ignored_errors":  sourcesComputedInt64("Number of errors so far that were ignored."),
				"directory":       sourcesComputedString("Directory being read, within the source."),
				"last_error_path": sourcesComputedString("Path of the entry that last failed; empty when none " +
					"did."),
				"last_error": sourcesComputedString("Error of the entry that last failed; empty when none did."),
			},
		},
		"current_task": sourcesComputedString("Identifier of the server task taking the snapshot while " +
			"`status` is `UPLOADING`, as the Kopia UI's task list shows it; null otherwise."),
	}
}

func (d *sourcesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// sourcesEntryFromAPI maps one source. Lists Kopia leaves out because they
// are empty are reported as empty lists rather than null, so that their
// length can always be taken.
func sourcesEntryFromAPI(source *kopiaclient.SourceStatus) sourcesEntryModel {
	entry := sourcesEntryModel{
		Target:           types.StringValue(source.Source.String()),
		Host:             types.StringValue(source.Source.Host),
		UserName:         types.StringValue(source.Source.UserName),
		Path:             types.StringValue(source.Source.Path),
		Status:           types.StringValue(source.Status),
		NextSnapshotTime: types.StringNull(),
		CurrentTask:      stringValueOrNullIfEmpty(source.CurrentTask),
	}

	schedule := source.Schedule
	entry.Schedule = sourcesScheduleModel{
		IntervalSeconds:   int64ValueOrNullIfZero(schedule.IntervalSeconds),
		TimeOfDay:         make([]sourcesTimeOfDayModel, 0, len(schedule.TimeOfDay)),
		NoParentTimeOfDay: types.BoolValue(schedule.NoParentTimeOfDay),
		Manual:            types.BoolValue(schedule.Manual),
		Cron:              stringListValue(schedule.Cron),
		RunMissed:         boolValueOrNull(schedule.RunMissed),
	}
	for _, at := range schedule.TimeOfDay {
		entry.Schedule.TimeOfDay = append(entry.Schedule.TimeOfDay, sourcesTimeOfDayModel{
			Hour:   types.Int64Value(int64(at.Hour)),
			Minute: types.Int64Value(int64(at.Minute)),
		})
	}

	if source.NextSnapshotTime != nil {
		entry.NextSnapshotTime = repositoryTimeValue(*source.NextSnapshotTime)
	}

	if last := source.LastSnapshot; last != nil {
		stats := last.Stats
		entry.LastSnapshot = &sourcesLastSnapshotModel{
			ID:               types.StringValue(last.ID),
			Description:      types.StringValue(last.Description),
			StartTime:        repositoryTimeValue(last.StartTime),
			EndTime:          repositoryTimeValue(last.EndTime),
			IncompleteReason: stringValueOrNullIfEmpty(last.IncompleteReason),
			RootID:           types.StringNull(),
			Pins:             stringListValue(last.Pins),
			Stats: sourcesSnapshotStatsModel{
				TotalSize:         types.Int64Value(stats.TotalSize),
				ExcludedTotalSize: types.Int64Value(stats.ExcludedTotalSize),
				FileCount:         types.Int64Value(stats.FileCount),
				CachedFiles:       types.Int64Value(stats.CachedFiles),
				NonCachedFiles:    types.Int64Value(stats.NonCachedFiles),
				DirCount:          types.Int64Value(stats.DirCount),
				ExcludedFileCount: types.Int64Value(stats.ExcludedFileCount),
				ExcludedDirCount:  types.Int64Value(stats.ExcludedDirCount),
				IgnoredErrorCount: types.Int64Value(stats.IgnoredErrorCount),
				ErrorCount:        types.Int64Value(stats.ErrorCount),
			},
		}
		if last.RootEntry != nil {
			entry.LastSnapshot.RootID = stringValueOrNullIfEmpty(last.RootEntry.ObjectID)
		}
	}

	if upload := source.Upload; upload != nil {
		entry.Upload = &sourcesUploadModel{
			CachedBytes:    types.Int64Value(upload.CachedBytes),
			HashedBytes:    types.Int64Value(upload.HashedBytes),
			EstimatedBytes: types.Int64Value(upload.EstimatedBytes),
			CachedFiles:    types.Int64Value(upload.CachedFiles),
			HashedFiles:    types.Int64Value(upload.HashedFiles),
			EstimatedFiles: types.Int64Value(upload.EstimatedFiles),
			Errors:         types.Int64Value(upload.Errors),
			IgnoredErrors:  types.Int64Value(upload.IgnoredErrors),
			Directory:      types.StringValue(upload.Directory),
			LastErrorPath:  types.StringValue(upload.LastErrorPath),
			LastError:      types.StringValue(upload.LastError),
		}
	}
	return entry
}

func (d *sourcesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config sourcesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	listed, err := d.client.ListSources(ctx, kopiaclient.SourcesFilter{
		Host:     config.Host.ValueString(),
		UserName: config.UserName.ValueString(),
		Path:     config.Path.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to list Kopia sources", repositoryCallErrorDetail(err))
		return
	}

	state := sourcesDataSourceModel{
		Host:          config.Host,
		UserName:      config.UserName,
		Path:          config.Path,
		LocalHost:     types.StringValue(listed.LocalHost),
		LocalUsername: types.StringValue(listed.LocalUsername),
		MultiUser:     types.BoolValue(listed.MultiUser),
		Sources:       make([]sourcesEntryModel, 0, len(listed.Sources)),
	}
	for i := range listed.Sources {
		state.Sources = append(state.Sources, sourcesEntryFromAPI(&listed.Sources[i]))
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
