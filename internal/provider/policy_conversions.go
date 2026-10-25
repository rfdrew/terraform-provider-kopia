package provider

import (
	"sort"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// Conversions between the policy tree (policy_schema.go) and
// kopiaclient.Policy.
//
// To Kopia, every attribute maps to one field, and a null attribute leaves
// its field out. The client never sends an empty section or an empty list,
// and a plain field's false, 0 or "" is left out by omitempty, so what is sent
// is always a value Kopia stores as it is.
//
// From Kopia, state is written from what the server answered, never from the
// plan, with two exceptions, both for values Kopia cannot express:
//
//   - A section (or nested object) Kopia reports empty, or leaves out, is null
//     unless the plan or prior state holds an object there; then it is an
//     object whose attributes Kopia does not report are null. `retention = {}`
//     in a configuration therefore reads back as {} rather than null, and
//     neither drifts nor makes the apply inconsistent.
//   - A plain boolean Kopia does not report is false when the plan or prior
//     state says false, and null otherwise, because Kopia cannot store false:
//     `manual = false` in a configuration stays false.
//
// Everything else that Kopia reports empty is null, which is what an unset
// attribute is, and the validators refuse configuring the values Kopia cannot
// store (0, "", [] for plain fields), so they never have to be kept.

// policyBodyModel is the policy itself: kopia_policy's attributes other than
// `id` and `target`, and the `effective` policy of kopia_effective_policy.
type policyBodyModel struct {
	Retention           types.Object `tfsdk:"retention"`
	Files               types.Object `tfsdk:"files"`
	ErrorHandling       types.Object `tfsdk:"error_handling"`
	Scheduling          types.Object `tfsdk:"scheduling"`
	Compression         types.Object `tfsdk:"compression"`
	MetadataCompression types.Object `tfsdk:"metadata_compression"`
	Splitter            types.Object `tfsdk:"splitter"`
	Actions             types.Object `tfsdk:"actions"`
	OSSnapshots         types.Object `tfsdk:"os_snapshots"`
	Logging             types.Object `tfsdk:"logging"`
	Upload              types.Object `tfsdk:"upload"`
	NoParent            types.Bool   `tfsdk:"no_parent"`
}

// policyValues reads the attributes of an object of the policy tree. The
// values of a null object are nil, and every attribute of nil reads as null.
type policyValues map[string]attr.Value

// policyValuesOf returns the attributes of v, or nil when v is null, unknown
// or not an object.
func policyValuesOf(v attr.Value) policyValues {
	object, ok := v.(types.Object)
	if !ok || object.IsNull() || object.IsUnknown() {
		return nil
	}
	return object.Attributes()
}

func (v policyValues) object(name string) policyValues { return policyValuesOf(v[name]) }

func (v policyValues) int64Pointer(name string) *int64 {
	value, _ := v[name].(types.Int64)
	return int64PointerOrNil(value)
}

func (v policyValues) boolPointer(name string) *bool {
	value, _ := v[name].(types.Bool)
	return boolPointerOrNil(value)
}

// int64, bool and string read a plain field: null is the zero value, which
// Kopia leaves out.
func (v policyValues) int64(name string) int64 {
	value, _ := v[name].(types.Int64)
	return value.ValueInt64()
}

func (v policyValues) bool(name string) bool {
	value, _ := v[name].(types.Bool)
	return value.ValueBool()
}

func (v policyValues) string(name string) string {
	value, _ := v[name].(types.String)
	return value.ValueString()
}

func (v policyValues) list(name string) []string {
	value, _ := v[name].(types.List)
	values, _ := stringsFromList(value)
	return values
}

func (v policyValues) set(name string) []string {
	value, _ := v[name].(types.Set)
	values, _ := stringsFromSet(value)
	return values
}

// toWire converts a policy into the body of a PUT, or the updates of a
// resolve. It expects a fully known value, as in an apply; an unknown value
// converts as if it were null.
func (m policyBodyModel) toWire() *kopiaclient.Policy {
	p := &kopiaclient.Policy{NoParent: m.NoParent.ValueBool()}

	if v := policyValuesOf(m.Retention); v != nil {
		p.Retention = &kopiaclient.RetentionPolicy{
			KeepLatest:               v.int64Pointer("keep_latest"),
			KeepHourly:               v.int64Pointer("keep_hourly"),
			KeepDaily:                v.int64Pointer("keep_daily"),
			KeepWeekly:               v.int64Pointer("keep_weekly"),
			KeepMonthly:              v.int64Pointer("keep_monthly"),
			KeepAnnual:               v.int64Pointer("keep_annual"),
			IgnoreIdenticalSnapshots: v.boolPointer("ignore_identical_snapshots"),
		}
	}
	if v := policyValuesOf(m.Files); v != nil {
		p.Files = &kopiaclient.FilesPolicy{
			IgnoreRules:            v.list("ignore"),
			NoParentIgnoreRules:    v.bool("no_parent_ignore"),
			DotIgnoreFiles:         v.list("ignore_dot_files"),
			NoParentDotIgnoreFiles: v.bool("no_parent_dot_files"),
			IgnoreCacheDirectories: v.boolPointer("ignore_cache_dirs"),
			MaxFileSize:            v.int64("max_file_size"),
			OneFileSystem:          v.boolPointer("one_file_system"),
		}
	}
	if v := policyValuesOf(m.ErrorHandling); v != nil {
		p.ErrorHandling = &kopiaclient.ErrorHandlingPolicy{
			IgnoreFileErrors:      v.boolPointer("ignore_file_errors"),
			IgnoreDirectoryErrors: v.boolPointer("ignore_directory_errors"),
			IgnoreUnknownTypes:    v.boolPointer("ignore_unknown_types"),
		}
	}
	if v := policyValuesOf(m.Scheduling); v != nil {
		p.Scheduling = &kopiaclient.SchedulingPolicy{
			IntervalSeconds:   v.int64("interval_seconds"),
			TimesOfDay:        policyTimesOfDayToWire(v["time_of_day"]),
			NoParentTimeOfDay: v.bool("no_parent_time_of_day"),
			Manual:            v.bool("manual"),
			Cron:              v.list("cron"),
			RunMissed:         v.boolPointer("run_missed"),
		}
	}
	if v := policyValuesOf(m.Compression); v != nil {
		p.Compression = &kopiaclient.CompressionPolicy{
			CompressorName:        v.string("compressor_name"),
			OnlyCompress:          v.set("only_compress"),
			NoParentOnlyCompress:  v.bool("no_parent_only_compress"),
			NeverCompress:         v.set("never_compress"),
			NoParentNeverCompress: v.bool("no_parent_never_compress"),
			MinSize:               v.int64("min_size"),
			MaxSize:               v.int64("max_size"),
		}
	}
	if v := policyValuesOf(m.MetadataCompression); v != nil {
		p.MetadataCompression = &kopiaclient.MetadataCompressionPolicy{CompressorName: v.string("compressor_name")}
	}
	if v := policyValuesOf(m.Splitter); v != nil {
		p.Splitter = &kopiaclient.SplitterPolicy{Algorithm: v.string("algorithm")}
	}
	if v := policyValuesOf(m.Actions); v != nil {
		p.Actions = &kopiaclient.ActionsPolicy{
			BeforeFolder:       policyActionToWire(v.object("before_folder")),
			AfterFolder:        policyActionToWire(v.object("after_folder")),
			BeforeSnapshotRoot: policyActionToWire(v.object("before_snapshot_root")),
			AfterSnapshotRoot:  policyActionToWire(v.object("after_snapshot_root")),
		}
	}
	if v := policyValuesOf(m.OSSnapshots); v != nil {
		p.OSSnapshots = &kopiaclient.OSSnapshotPolicy{
			VolumeShadowCopy: &kopiaclient.VolumeShadowCopyPolicy{Enable: policyVolumeShadowCopyToWire(v["volume_shadow_copy"])},
		}
	}
	if v := policyValuesOf(m.Logging); v != nil {
		p.Logging = &kopiaclient.LoggingPolicy{}
		if d := v.object("directories"); d != nil {
			p.Logging.Directories = &kopiaclient.DirLoggingPolicy{
				Snapshotted: d.int64Pointer("snapshotted"),
				Ignored:     d.int64Pointer("ignored"),
			}
		}
		if e := v.object("entries"); e != nil {
			p.Logging.Entries = &kopiaclient.EntryLoggingPolicy{
				Snapshotted: e.int64Pointer("snapshotted"),
				Ignored:     e.int64Pointer("ignored"),
				CacheHit:    e.int64Pointer("cache_hit"),
				CacheMiss:   e.int64Pointer("cache_miss"),
			}
		}
	}
	if v := policyValuesOf(m.Upload); v != nil {
		p.Upload = &kopiaclient.UploadPolicy{
			MaxParallelSnapshots:    v.int64Pointer("max_parallel_snapshots"),
			MaxParallelFileReads:    v.int64Pointer("max_parallel_file_reads"),
			ParallelUploadAboveSize: v.int64Pointer("parallel_upload_above_size"),
		}
	}
	return p
}

// policyTimesOfDayToWire converts the time_of_day set, sorted, so that the
// request body does not depend on the order of a set.
func policyTimesOfDayToWire(v attr.Value) []kopiaclient.TimeOfDay {
	set, ok := v.(types.Set)
	if !ok || set.IsNull() || set.IsUnknown() {
		return nil
	}
	times := make([]kopiaclient.TimeOfDay, 0, len(set.Elements()))
	for _, element := range set.Elements() {
		if t := policyValuesOf(element); t != nil {
			times = append(times, kopiaclient.TimeOfDay{Hour: t.int64("hour"), Minute: t.int64("minute")})
		}
	}
	sort.Slice(times, func(i, j int) bool {
		if times[i].Hour != times[j].Hour {
			return times[i].Hour < times[j].Hour
		}
		return times[i].Minute < times[j].Minute
	})
	return times
}

// policyActionToWire converts one action; nil for a null one.
func policyActionToWire(v policyValues) *kopiaclient.ActionCommand {
	if v == nil {
		return nil
	}
	return &kopiaclient.ActionCommand{
		Command:        v.string("command"),
		Arguments:      v.list("args"),
		Script:         v.string("script"),
		TimeoutSeconds: v.int64("timeout_seconds"),
		Mode:           v.string("mode"),
	}
}

// policyVolumeShadowCopyToWire converts a mode name into Kopia's number. A
// number read from Kopia, which policyVolumeShadowCopyValue shows as its
// decimal text, converts back to itself.
func policyVolumeShadowCopyToWire(v attr.Value) *int64 {
	mode, ok := v.(types.String)
	if !ok || mode.IsNull() || mode.IsUnknown() {
		return nil
	}
	for i, name := range policyVolumeShadowCopyModes {
		if mode.ValueString() == name {
			n := int64(i)
			return &n
		}
	}
	if n, err := strconv.ParseInt(mode.ValueString(), 10, 64); err == nil {
		return &n
	}
	return nil
}

// policyVolumeShadowCopyValue converts Kopia's number into a mode name. On
// Windows, Kopia treats any other number as "never"; it is shown as its
// decimal text so that it shows up as a difference rather than passing for a
// valid mode.
func policyVolumeShadowCopyValue(n *int64) types.String {
	if n == nil {
		return types.StringNull()
	}
	if *n >= 0 && *n < int64(len(policyVolumeShadowCopyModes)) {
		return types.StringValue(policyVolumeShadowCopyModes[*n])
	}
	return types.StringValue(strconv.FormatInt(*n, 10))
}

// policyBodyFromWire converts a policy Kopia reported. prior is the plan, in
// Create and Update, or the prior state, in Read; a zero policyBodyModel when
// there is none, as in an import or a data source. See the top of this file
// for what prior decides.
func policyBodyFromWire(policy *kopiaclient.Policy, prior policyBodyModel) policyBodyModel {
	p := kopiaclient.Policy{}
	if policy != nil {
		p = policy.Normalised()
	}

	m := policyBodyModel{
		NoParent: policyPlainBool(p.NoParent, prior.NoParent),
	}

	m.Retention = policyObjectValue(p.Retention == nil, prior.Retention, policyObjectTypes("retention"), func(_ policyValues) map[string]attr.Value {
		s := p.Retention
		if s == nil {
			s = &kopiaclient.RetentionPolicy{}
		}
		return map[string]attr.Value{
			"keep_latest":                int64ValueOrNull(s.KeepLatest),
			"keep_hourly":                int64ValueOrNull(s.KeepHourly),
			"keep_daily":                 int64ValueOrNull(s.KeepDaily),
			"keep_weekly":                int64ValueOrNull(s.KeepWeekly),
			"keep_monthly":               int64ValueOrNull(s.KeepMonthly),
			"keep_annual":                int64ValueOrNull(s.KeepAnnual),
			"ignore_identical_snapshots": boolValueOrNull(s.IgnoreIdenticalSnapshots),
		}
	})

	m.Files = policyObjectValue(p.Files == nil, prior.Files, policyObjectTypes("files"), func(prior policyValues) map[string]attr.Value {
		s := p.Files
		if s == nil {
			s = &kopiaclient.FilesPolicy{}
		}
		return map[string]attr.Value{
			"ignore":              stringListOrNull(s.IgnoreRules),
			"no_parent_ignore":    policyPlainBool(s.NoParentIgnoreRules, prior["no_parent_ignore"]),
			"ignore_dot_files":    stringListOrNull(s.DotIgnoreFiles),
			"no_parent_dot_files": policyPlainBool(s.NoParentDotIgnoreFiles, prior["no_parent_dot_files"]),
			"ignore_cache_dirs":   boolValueOrNull(s.IgnoreCacheDirectories),
			"max_file_size":       int64ValueOrNullIfZero(s.MaxFileSize),
			"one_file_system":     boolValueOrNull(s.OneFileSystem),
		}
	})

	m.ErrorHandling = policyObjectValue(p.ErrorHandling == nil, prior.ErrorHandling, policyObjectTypes("error_handling"), func(_ policyValues) map[string]attr.Value {
		s := p.ErrorHandling
		if s == nil {
			s = &kopiaclient.ErrorHandlingPolicy{}
		}
		return map[string]attr.Value{
			"ignore_file_errors":      boolValueOrNull(s.IgnoreFileErrors),
			"ignore_directory_errors": boolValueOrNull(s.IgnoreDirectoryErrors),
			"ignore_unknown_types":    boolValueOrNull(s.IgnoreUnknownTypes),
		}
	})

	m.Scheduling = policyObjectValue(p.Scheduling == nil, prior.Scheduling, policyObjectTypes("scheduling"), func(prior policyValues) map[string]attr.Value {
		s := p.Scheduling
		if s == nil {
			s = &kopiaclient.SchedulingPolicy{}
		}
		return map[string]attr.Value{
			"interval_seconds":      int64ValueOrNullIfZero(s.IntervalSeconds),
			"time_of_day":           policyTimesOfDayValue(s.TimesOfDay),
			"no_parent_time_of_day": policyPlainBool(s.NoParentTimeOfDay, prior["no_parent_time_of_day"]),
			"manual":                policyPlainBool(s.Manual, prior["manual"]),
			"cron":                  stringListOrNull(s.Cron),
			"run_missed":            boolValueOrNull(s.RunMissed),
		}
	})

	m.Compression = policyObjectValue(p.Compression == nil, prior.Compression, policyObjectTypes("compression"), func(prior policyValues) map[string]attr.Value {
		s := p.Compression
		if s == nil {
			s = &kopiaclient.CompressionPolicy{}
		}
		return map[string]attr.Value{
			"compressor_name":          stringValueOrNullIfEmpty(s.CompressorName),
			"only_compress":            policyStringSetOrNull(s.OnlyCompress),
			"no_parent_only_compress":  policyPlainBool(s.NoParentOnlyCompress, prior["no_parent_only_compress"]),
			"never_compress":           policyStringSetOrNull(s.NeverCompress),
			"no_parent_never_compress": policyPlainBool(s.NoParentNeverCompress, prior["no_parent_never_compress"]),
			"min_size":                 int64ValueOrNullIfZero(s.MinSize),
			"max_size":                 int64ValueOrNullIfZero(s.MaxSize),
		}
	})

	m.MetadataCompression = policyObjectValue(p.MetadataCompression == nil, prior.MetadataCompression, policyObjectTypes("metadata_compression"), func(_ policyValues) map[string]attr.Value {
		name := ""
		if p.MetadataCompression != nil {
			name = p.MetadataCompression.CompressorName
		}
		return map[string]attr.Value{"compressor_name": stringValueOrNullIfEmpty(name)}
	})

	m.Splitter = policyObjectValue(p.Splitter == nil, prior.Splitter, policyObjectTypes("splitter"), func(_ policyValues) map[string]attr.Value {
		algorithm := ""
		if p.Splitter != nil {
			algorithm = p.Splitter.Algorithm
		}
		return map[string]attr.Value{"algorithm": stringValueOrNullIfEmpty(algorithm)}
	})

	m.Actions = policyObjectValue(p.Actions == nil, prior.Actions, policyObjectTypes("actions"), func(_ policyValues) map[string]attr.Value {
		s := p.Actions
		if s == nil {
			s = &kopiaclient.ActionsPolicy{}
		}
		return map[string]attr.Value{
			"before_folder":        policyActionValue(s.BeforeFolder),
			"after_folder":         policyActionValue(s.AfterFolder),
			"before_snapshot_root": policyActionValue(s.BeforeSnapshotRoot),
			"after_snapshot_root":  policyActionValue(s.AfterSnapshotRoot),
		}
	})

	m.OSSnapshots = policyObjectValue(p.OSSnapshots == nil, prior.OSSnapshots, policyObjectTypes("os_snapshots"), func(_ policyValues) map[string]attr.Value {
		var enable *int64
		if p.OSSnapshots != nil && p.OSSnapshots.VolumeShadowCopy != nil {
			enable = p.OSSnapshots.VolumeShadowCopy.Enable
		}
		return map[string]attr.Value{"volume_shadow_copy": policyVolumeShadowCopyValue(enable)}
	})

	m.Logging = policyObjectValue(p.Logging == nil, prior.Logging, policyObjectTypes("logging"), func(prior policyValues) map[string]attr.Value {
		s := p.Logging
		if s == nil {
			s = &kopiaclient.LoggingPolicy{}
		}
		directories := policyObjectValue(s.Directories == nil, prior["directories"], policyObjectTypes("logging", "directories"), func(_ policyValues) map[string]attr.Value {
			d := s.Directories
			if d == nil {
				d = &kopiaclient.DirLoggingPolicy{}
			}
			return map[string]attr.Value{
				"snapshotted": int64ValueOrNull(d.Snapshotted),
				"ignored":     int64ValueOrNull(d.Ignored),
			}
		})
		entries := policyObjectValue(s.Entries == nil, prior["entries"], policyObjectTypes("logging", "entries"), func(_ policyValues) map[string]attr.Value {
			e := s.Entries
			if e == nil {
				e = &kopiaclient.EntryLoggingPolicy{}
			}
			return map[string]attr.Value{
				"snapshotted": int64ValueOrNull(e.Snapshotted),
				"ignored":     int64ValueOrNull(e.Ignored),
				"cache_hit":   int64ValueOrNull(e.CacheHit),
				"cache_miss":  int64ValueOrNull(e.CacheMiss),
			}
		})
		return map[string]attr.Value{"directories": directories, "entries": entries}
	})

	m.Upload = policyObjectValue(p.Upload == nil, prior.Upload, policyObjectTypes("upload"), func(_ policyValues) map[string]attr.Value {
		s := p.Upload
		if s == nil {
			s = &kopiaclient.UploadPolicy{}
		}
		return map[string]attr.Value{
			"max_parallel_snapshots":     int64ValueOrNull(s.MaxParallelSnapshots),
			"max_parallel_file_reads":    int64ValueOrNull(s.MaxParallelFileReads),
			"parallel_upload_above_size": int64ValueOrNull(s.ParallelUploadAboveSize),
		}
	})

	return m
}

// policyObjectValue builds an object of the policy tree from what Kopia
// reported. empty says Kopia reported nothing for it; the object is then null,
// unless prior holds an object, which is kept as an object (of whatever build
// returns, nulls for the most part) so that a configured {} does not turn
// into null. build gets prior's attributes, nil when prior is null.
func policyObjectValue(empty bool, prior attr.Value, attrTypes map[string]attr.Type, build func(prior policyValues) map[string]attr.Value) types.Object {
	priorValues := policyValuesOf(prior)
	if empty && priorValues == nil {
		return types.ObjectNull(attrTypes)
	}
	return types.ObjectValueMust(attrTypes, build(priorValues))
}

// policyPlainBool converts one of Kopia's plain booleans. Kopia leaves false
// out and cannot store it, so a false that prior holds is kept rather than
// turned into null.
func policyPlainBool(wire bool, prior attr.Value) types.Bool {
	if wire {
		return types.BoolValue(true)
	}
	if b, ok := prior.(types.Bool); ok && !b.IsNull() && !b.IsUnknown() && !b.ValueBool() {
		return types.BoolValue(false)
	}
	return types.BoolNull()
}

// policyStringSetOrNull converts a list Kopia reports into a set, or null
// when it is empty. Kopia stores a list as it was sent, so another writer
// may have stored duplicates; a set holds each value once.
func policyStringSetOrNull(values []string) types.Set {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			unique = append(unique, v)
		}
	}
	return stringSetOrNull(unique)
}

// policyTimesOfDayValue converts Kopia's times of day into the time_of_day
// set: null for none, and each time once.
func policyTimesOfDayValue(times []kopiaclient.TimeOfDay) types.Set {
	elementTypes := policyObjectTypes("scheduling", "time_of_day")
	elementType := types.ObjectType{AttrTypes: elementTypes}
	if len(times) == 0 {
		return types.SetNull(elementType)
	}
	seen := make(map[kopiaclient.TimeOfDay]bool, len(times))
	elements := make([]attr.Value, 0, len(times))
	for _, t := range times {
		if seen[t] {
			continue
		}
		seen[t] = true
		elements = append(elements, types.ObjectValueMust(elementTypes, map[string]attr.Value{
			"hour":   types.Int64Value(t.Hour),
			"minute": types.Int64Value(t.Minute),
		}))
	}
	return types.SetValueMust(elementType, elements)
}

// policyActionValue converts one action: null when Kopia has none. An empty
// action, which Kopia stores but fails snapshots with, is an object of nulls,
// so that it shows up as a difference to a configuration that has none.
func policyActionValue(a *kopiaclient.ActionCommand) types.Object {
	attrTypes := policyObjectTypes("actions", "before_folder")
	if a == nil {
		return types.ObjectNull(attrTypes)
	}
	return types.ObjectValueMust(attrTypes, map[string]attr.Value{
		"command":         stringValueOrNullIfEmpty(a.Command),
		"args":            stringListOrNull(a.Arguments),
		"script":          stringValueOrNullIfEmpty(a.Script),
		"timeout_seconds": int64ValueOrNullIfZero(a.TimeoutSeconds),
		"mode":            stringValueOrNullIfEmpty(a.Mode),
	})
}

// policyBodyObject converts a policy body into the `effective` object of
// kopia_effective_policy.
func policyBodyObject(m policyBodyModel) types.Object {
	return types.ObjectValueMust(policyBodyTypes, map[string]attr.Value{
		"retention":            m.Retention,
		"files":                m.Files,
		"error_handling":       m.ErrorHandling,
		"scheduling":           m.Scheduling,
		"compression":          m.Compression,
		"metadata_compression": m.MetadataCompression,
		"splitter":             m.Splitter,
		"actions":              m.Actions,
		"os_snapshots":         m.OSSnapshots,
		"logging":              m.Logging,
		"upload":               m.Upload,
		"no_parent":            m.NoParent,
	})
}
