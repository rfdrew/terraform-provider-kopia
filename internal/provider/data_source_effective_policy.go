package provider

import (
	"context"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ datasource.DataSource              = &effectivePolicyDataSource{}
	_ datasource.DataSourceWithConfigure = &effectivePolicyDataSource{}
)

// NewEffectivePolicyDataSource returns the kopia_effective_policy data source.
func NewEffectivePolicyDataSource() datasource.DataSource { return &effectivePolicyDataSource{} }

type effectivePolicyDataSource struct {
	client *kopiaclient.Client
}

type effectivePolicyDataSourceModel struct {
	Target                   types.String `tfsdk:"target"`
	NumUpcomingSnapshotTimes types.Int64  `tfsdk:"num_upcoming_snapshot_times"`
	Effective                types.Object `tfsdk:"effective"`
	Definition               types.Map    `tfsdk:"definition"`
	UpcomingSnapshotTimes    types.List   `tfsdk:"upcoming_snapshot_times"`
	SchedulingError          types.String `tfsdk:"scheduling_error"`
}

// policyMaxUpcomingSnapshotTimes bounds num_upcoming_snapshot_times: Kopia
// computes the times in a loop without a limit of its own.
const policyMaxUpcomingSnapshotTimes = 100

func (d *effectivePolicyDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_effective_policy"
}

func (d *effectivePolicyDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Resolves the policy Kopia applies to a target: its own policy merged with " +
			"everything it inherits, from the policies of its parent directories up to `(global)` and " +
			"Kopia's built-in defaults. It also tells which target each value comes from, and when the " +
			"server will take the next snapshots, as Kopia's UI shows them.",
		Attributes: map[string]schema.Attribute{
			"target": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Target to resolve, written as for `kopia_policy`: `(global)`, `@host`, " +
					"`user@host` or `user@host:path`, such as the path of a snapshot source. It needs no policy " +
					"of its own.",
				Validators: []validator.String{policyTargetValidator{}},
			},
			"num_upcoming_snapshot_times": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Number of upcoming snapshot times to compute, from 0 to 100. Defaults " +
					"to `0`.",
				Validators: []validator.Int64{int64validator.Between(0, policyMaxUpcomingSnapshotTimes)},
			},
			"effective": schema.SingleNestedAttribute{
				Computed: true,
				MarkdownDescription: "The effective policy, with the attributes of `kopia_policy`. An " +
					"attribute nothing sets is null, and so is a section with nothing set; `no_parent` is " +
					"always null, as Kopia reports it only on the policies themselves.",
				Attributes: policyDataSourceAttributes(policyBodyAttributes()),
			},
			"definition": schema.MapAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Where each value of `effective` comes from: the target whose policy " +
					"sets it, keyed by the attribute's path, such as `retention.keep_daily` or " +
					"`logging.entries.cache_hit`. `(global)` stands for the global policy or a built-in " +
					"default, which Kopia does not tell apart. Kopia reports no source for `no_parent`, " +
					"`scheduling.no_parent_time_of_day` and the `no_parent_*` flags of `compression`, which " +
					"are left out; `actions.before_folder` and `actions.after_folder` are never inherited, so " +
					"they come from `target` itself. `scheduling.time_of_day` names the most " +
					"general policy that has times of day, even where `no_parent_time_of_day` excludes them.",
			},
			"upcoming_snapshot_times": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The next times the server will take a snapshot of the target, as RFC " +
					"3339 timestamps in the server's time zone, computed as if a snapshot had just " +
					"finished: they ignore `run_missed` and the snapshots actually taken. Empty when the " +
					"schedule is manual or there is none, and when `num_upcoming_snapshot_times` is 0. The " +
					"server schedules only sources on its own host.",
			},
			"scheduling_error": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Why Kopia would refuse the target's own scheduling section, or null when " +
					"it accepts it.",
			},
		},
	}
}

func (d *effectivePolicyDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *effectivePolicyDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config effectivePolicyDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, err := kopiaclient.ParseTarget(config.Target.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("target"), "Invalid policy target", policyTargetHelp(err))
		return
	}

	// Without updates Kopia resolves the stored policies, which is right for
	// every target; the error in its handling of updates (spec: policy.md
	// §3.6) does not apply.
	resolved, err := d.client.ResolvePolicy(ctx, target, nil, int(config.NumUpcomingSnapshotTimes.ValueInt64()))
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve Kopia policy",
			"Resolving the effective policy of "+target.String()+" failed. "+policyReadErrorDetail(err))
		return
	}

	effective := policyBodyFromWire(resolved.Effective, policyBodyModel{})
	config.Effective = policyBodyObject(effective)

	definition := make(map[string]attr.Value)
	for key, source := range policyDefinitionSources(target, resolved.Definition, config.Effective) {
		definition[key] = types.StringValue(source.String())
	}
	config.Definition = types.MapValueMust(types.StringType, definition)

	times := make([]attr.Value, 0, len(resolved.UpcomingSnapshotTimes))
	for _, t := range resolved.UpcomingSnapshotTimes {
		times = append(times, types.StringValue(t.Format(time.RFC3339Nano)))
	}
	config.UpcomingSnapshotTimes = types.ListValueMust(types.StringType, times)
	config.SchedulingError = stringValueOrNullIfEmpty(resolved.SchedulingError)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

// policyDefinitionSources maps the attribute paths of the effective policy to
// the targets that set them, for every attribute that is not null in
// effective. Kopia reports a source for fields nothing sets as well, as the
// zero target; those are left out rather than shown as coming from
// (global).
func policyDefinitionSources(target kopiaclient.SourceInfo, d *kopiaclient.PolicyDefinition, effective types.Object) map[string]kopiaclient.SourceInfo {
	if d == nil {
		d = &kopiaclient.PolicyDefinition{}
	}
	all := map[string]kopiaclient.SourceInfo{
		"retention.keep_latest":                d.Retention.KeepLatest,
		"retention.keep_hourly":                d.Retention.KeepHourly,
		"retention.keep_daily":                 d.Retention.KeepDaily,
		"retention.keep_weekly":                d.Retention.KeepWeekly,
		"retention.keep_monthly":               d.Retention.KeepMonthly,
		"retention.keep_annual":                d.Retention.KeepAnnual,
		"retention.ignore_identical_snapshots": d.Retention.IgnoreIdenticalSnapshots,

		"files.ignore":              d.Files.IgnoreRules,
		"files.no_parent_ignore":    d.Files.NoParentIgnoreRules,
		"files.ignore_dot_files":    d.Files.DotIgnoreFiles,
		"files.no_parent_dot_files": d.Files.NoParentDotIgnoreFiles,
		"files.ignore_cache_dirs":   d.Files.IgnoreCacheDirectories,
		"files.max_file_size":       d.Files.MaxFileSize,
		"files.one_file_system":     d.Files.OneFileSystem,

		"error_handling.ignore_file_errors":      d.ErrorHandling.IgnoreFileErrors,
		"error_handling.ignore_directory_errors": d.ErrorHandling.IgnoreDirectoryErrors,
		"error_handling.ignore_unknown_types":    d.ErrorHandling.IgnoreUnknownTypes,

		"scheduling.interval_seconds": d.Scheduling.IntervalSeconds,
		"scheduling.time_of_day":      d.Scheduling.TimesOfDay,
		"scheduling.manual":           d.Scheduling.Manual,
		"scheduling.cron":             d.Scheduling.Cron,
		"scheduling.run_missed":       d.Scheduling.RunMissed,

		"compression.compressor_name": d.Compression.CompressorName,
		"compression.only_compress":   d.Compression.OnlyCompress,
		"compression.never_compress":  d.Compression.NeverCompress,
		"compression.min_size":        d.Compression.MinSize,
		"compression.max_size":        d.Compression.MaxSize,

		"metadata_compression.compressor_name": d.MetadataCompression.CompressorName,
		"splitter.algorithm":                   d.Splitter.Algorithm,

		// Folder actions are never inherited: Kopia copies them into the
		// effective policy from the target's own (policy_merge.go:51-53).
		"actions.before_folder":        target,
		"actions.after_folder":         target,
		"actions.before_snapshot_root": d.Actions.BeforeSnapshotRoot,
		"actions.after_snapshot_root":  d.Actions.AfterSnapshotRoot,

		"os_snapshots.volume_shadow_copy": d.OSSnapshots.VolumeShadowCopy.Enable,

		"logging.directories.snapshotted": d.Logging.Directories.Snapshotted,
		"logging.directories.ignored":     d.Logging.Directories.Ignored,
		"logging.entries.snapshotted":     d.Logging.Entries.Snapshotted,
		"logging.entries.ignored":         d.Logging.Entries.Ignored,
		"logging.entries.cache_hit":       d.Logging.Entries.CacheHit,
		"logging.entries.cache_miss":      d.Logging.Entries.CacheMiss,

		"upload.max_parallel_snapshots":     d.Upload.MaxParallelSnapshots,
		"upload.max_parallel_file_reads":    d.Upload.MaxParallelFileReads,
		"upload.parallel_upload_above_size": d.Upload.ParallelUploadAboveSize,
	}

	sources := make(map[string]kopiaclient.SourceInfo, len(all))
	for key, source := range all {
		if policyIsSet(effective, strings.Split(key, ".")) {
			sources[key] = source
		}
	}
	return sources
}

// policyIsSet reports whether the attribute at names below object is known and
// not null.
func policyIsSet(object types.Object, names []string) bool {
	var value attr.Value = object
	for _, name := range names {
		values := policyValuesOf(value)
		if values == nil {
			return false
		}
		value = values[name]
	}
	return value != nil && !value.IsNull() && !value.IsUnknown()
}
