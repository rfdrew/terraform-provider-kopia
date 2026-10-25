package provider

import (
	"math"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The attributes of a Kopia policy — retention, files, scheduling and the
// other sections, and no_parent — are described once, here, and rendered
// twice: as optional attributes with validators for kopia_policy, where the
// configuration says what this one policy sets, and as computed attributes
// without validators for the `effective` policy of kopia_effective_policy.
// The two schema packages have different attribute types, so the tree is a
// small description of its own, policyAttr, rather than either of them.
//
// Every section is a single nested attribute, not a block, and none is
// computed: kopia_policy owns the whole policy of its target, and an
// attribute left out means "not set here, inherit it". Kopia's pointer fields
// map to attributes whose null, false and 0 are three different things; its
// plain fields cannot store false, 0, "" or an empty list at all, so their
// validators refuse those values, except a plain false, which is accepted and
// kept (see policy_conversions.go).

// policyAttrKind is the type of a policyAttr.
type policyAttrKind int

const (
	policyKindBool policyAttrKind = iota
	policyKindInt64
	policyKindString
	policyKindStringList
	policyKindStringSet
	policyKindObject
	policyKindObjectSet
)

// policyAttr describes one attribute of the policy tree. Validators apply only
// where the attribute is configured, in kopia_policy.
type policyAttr struct {
	name        string
	kind        policyAttrKind
	description string
	// required marks the attributes of a set element that every element must
	// set; every other attribute is optional.
	required bool
	// attributes are the nested attributes of policyKindObject and
	// policyKindObjectSet.
	attributes []policyAttr

	boolValidators   []validator.Bool
	int64Validators  []validator.Int64
	stringValidators []validator.String
	listValidators   []validator.List
	setValidators    []validator.Set
	objectValidators []validator.Object
}

// Kopia keeps several numbers in Go's int (OptionalInt, the action timeout),
// which is 32 bits wide in a 32-bit build of Kopia, where a larger number
// fails to decode; they are capped at what every build accepts.
const policyMaxInt = math.MaxInt32

// Text shared by the descriptions of Kopia's plain fields.
const (
	policyPlainBoolNote = " Kopia cannot store `false` here: `false` and leaving the attribute out both mean " +
		"that this policy does not set it, so it can turn the setting on but never off for what it inherits."
	policyFirstListNote = " The most specific policy that sets a list replaces the lists of every policy above " +
		"it; a more specific policy cannot clear it."
)

// policyExtensionPattern matches what Kopia compares compression lists with:
// filepath.Ext of a file name, which keeps the leading dot
// (snapshot/policy/compression_policy.go:44).
var policyExtensionPattern = regexp.MustCompile(`^\.[^/\\]*$`)

// policyBodyAttributes returns the policy tree: every attribute of a policy
// body, which is kopia_policy's schema without `id` and `target`.
func policyBodyAttributes() []policyAttr {
	logLevel := func(name, description string) policyAttr {
		return policyAttr{
			name: name, kind: policyKindInt64,
			description:     description + " From 0 (nothing) to 10 (everything); 5 is normal.",
			int64Validators: []validator.Int64{int64validator.Between(0, 10)},
		}
	}

	return []policyAttr{
		{
			name: "retention", kind: policyKindObject,
			description: "Which snapshots Kopia keeps when it expires old ones. Every `keep_*` count keeps the " +
				"newest snapshot of each of that many hours, days, weeks, months or years, counted back from " +
				"the newest snapshot, and `keep_latest` the newest snapshots overall; a snapshot any count " +
				"keeps is kept. `0` is an explicit count that overrides what is inherited; if the six counts " +
				"add up to 0, Kopia keeps every snapshot.",
			attributes: []policyAttr{
				policyRetentionCount("keep_latest", "Number of most recent snapshots to keep. Kopia's built-in default is `10`."),
				policyRetentionCount("keep_hourly", "Number of hours to keep the newest snapshot of. Kopia's built-in default is `48`."),
				policyRetentionCount("keep_daily", "Number of days to keep the newest snapshot of. Kopia's built-in default is `7`."),
				policyRetentionCount("keep_weekly", "Number of weeks to keep the newest snapshot of. Kopia's built-in default is `4`."),
				policyRetentionCount("keep_monthly", "Number of months to keep the newest snapshot of. Kopia's built-in default is `24`."),
				policyRetentionCount("keep_annual", "Number of years to keep the newest snapshot of. Kopia's built-in default is `3`."),
				{
					name: "ignore_identical_snapshots", kind: policyKindBool,
					description: "Whether a snapshot identical to the previous one is discarded instead of " +
						"saved. Kopia's built-in default is `false`.",
				},
			},
		},
		{
			name: "files", kind: policyKindObject,
			description: "Which files a snapshot leaves out.",
			attributes: []policyAttr{
				{
					name: "ignore", kind: policyKindStringList,
					description: "Ignore rules in `.gitignore` syntax, in order: a later rule overrides an earlier " +
						"one, `!` includes a file again and `\\` escapes the next character. Kopia compiles the " +
						"rules only when it takes a snapshot, so a malformed one fails the snapshot, not the " +
						"apply; the provider warns about a trailing `\\` and an unclosed `[`." + policyFirstListNote,
					listValidators: []validator.List{
						listvalidator.SizeAtLeast(1),
						listvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1), policyIgnoreRuleValidator{}),
					},
				},
				{
					name: "no_parent_ignore", kind: policyKindBool,
					description: "Whether ignore rules inherited from the parent directories inside a " +
						"snapshot are dropped. Once a policy sets it, more specific ones cannot clear it." +
						policyPlainBoolNote,
				},
				{
					name: "ignore_dot_files", kind: policyKindStringList,
					description: "Names of files, such as `.kopiaignore` (Kopia's built-in default) or " +
						"`.gitignore`, whose rules a snapshot applies to the directory they are in." +
						policyFirstListNote,
					listValidators: []validator.List{
						listvalidator.SizeAtLeast(1),
						listvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1)),
					},
				},
				{
					name: "no_parent_dot_files", kind: policyKindBool,
					description: "Whether the ignore files of parent directories inside a snapshot are no " +
						"longer applied. Once a policy sets it, more specific ones cannot clear it." +
						policyPlainBoolNote,
				},
				{
					name: "ignore_cache_dirs", kind: policyKindBool,
					description: "Whether directories marked as caches by a `CACHEDIR.TAG` file are left out. " +
						"Kopia treats it as `true` when no policy sets it.",
				},
				{
					name: "max_file_size", kind: policyKindInt64,
					description: "Size in bytes above which files are left out, or `-1` for no limit, which " +
						"overrides a limit inherited from above. Kopia cannot store `0`.",
					int64Validators: []validator.Int64{policySizeLimitValidator()},
				},
				{
					name: "one_file_system", kind: policyKindBool,
					description: "Whether a snapshot stays on the file system its root is on, leaving out " +
						"other file systems mounted below it. Kopia treats it as `false` when no policy sets it.",
				},
			},
		},
		{
			name: "error_handling", kind: policyKindObject,
			description: "Which read errors a snapshot tolerates instead of failing.",
			attributes: []policyAttr{
				{
					name: "ignore_file_errors", kind: policyKindBool,
					description: "Whether errors reading files are ignored. Kopia's built-in default is `false`.",
				},
				{
					name: "ignore_directory_errors", kind: policyKindBool,
					description: "Whether errors reading directories are ignored. Kopia's built-in default is " +
						"`false`. Kopia 0.22 applies `ignore_file_errors` to directories too.",
				},
				{
					name: "ignore_unknown_types", kind: policyKindBool,
					description: "Whether entries of unknown types, such as sockets, are skipped silently. " +
						"Kopia's built-in default is `true`.",
				},
			},
		},
		{
			name: "scheduling", kind: policyKindObject,
			description: "When the Kopia server takes snapshots by itself. It schedules only sources on " +
				"its own host, and the next snapshot is the earliest of the interval, the times of day and " +
				"the cron entries in effect, wherever each is set: an interval on a path does not replace a " +
				"cron schedule it inherits, both apply. Times of day and cron entries are in the server's " +
				"local time zone. The provider checks the section with Kopia at plan time, which is how an " +
				"invalid cron entry is found before the apply.",
			objectValidators: []validator.Object{policyManualSchedulingValidator{}},
			attributes: []policyAttr{
				{
					name: "interval_seconds", kind: policyKindInt64,
					description: "Interval between snapshots, in seconds. Snapshots are aligned to multiples " +
						"of the interval counted in UTC, so `86400` takes one at midnight UTC. Kopia cannot store " +
						"`0`, and a more specific policy cannot clear an inherited interval.",
					int64Validators: []validator.Int64{int64validator.AtLeast(1)},
				},
				{
					name: "time_of_day", kind: policyKindObjectSet,
					description: "Times of day at which to take a snapshot. Unlike other settings, the times " +
						"of every level are combined: a policy adds its own to those it inherits, unless " +
						"`no_parent_time_of_day` is set.",
					setValidators: []validator.Set{setvalidator.SizeAtLeast(1)},
					attributes: []policyAttr{
						{
							name: "hour", kind: policyKindInt64, required: true,
							description:     "Hour, from 0 to 23.",
							int64Validators: []validator.Int64{int64validator.Between(0, 23)},
						},
						{
							name: "minute", kind: policyKindInt64, required: true,
							description:     "Minute, from 0 to 59.",
							int64Validators: []validator.Int64{int64validator.Between(0, 59)},
						},
					},
				},
				{
					name: "no_parent_time_of_day", kind: policyKindBool,
					description: "Whether the times of day of less specific policies are dropped, leaving only " +
						"this policy's own, or none." + policyPlainBoolNote,
				},
				{
					name: "manual", kind: policyKindBool,
					description: "Whether snapshots are only taken on request, never on a schedule. Kopia " +
						"refuses it together with any other scheduling attribute, and once a policy sets it, more " +
						"specific ones cannot clear it." + policyPlainBoolNote,
				},
				{
					name: "cron", kind: policyKindStringList,
					description: "Cron expressions, such as `0 3 * * *`, at which to take a snapshot. Kopia " +
						"treats everything from a `#` on as a comment, so cron's `#` syntax for the n-th weekday " +
						"of a month cannot be used: `0 0 * * 5#3` runs every Friday. The provider warns about " +
						"a `#` that does not follow a space." + policyFirstListNote,
					listValidators: []validator.List{
						listvalidator.SizeAtLeast(1),
						listvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1), policyCronEntryValidator{}),
					},
				},
				{
					name: "run_missed", kind: policyKindBool,
					description: "Whether a snapshot at a time of day or cron time that was missed, while the " +
						"server was down, say, is taken as soon as possible, provided the next one is more than " +
						"30 minutes away. It does not apply to intervals. Kopia's built-in default is `true`.",
				},
			},
		},
		{
			name: "compression", kind: policyKindObject,
			description: "How file contents are compressed.",
			attributes: []policyAttr{
				{
					name: "compressor_name", kind: policyKindString,
					description: "Compression algorithm, such as `zstd`, `zstd-fastest` or `s2-default`, or " +
						"`none`, which overrides a compressor inherited from above. Kopia's built-in default " +
						"is `none`. The provider checks the name against the algorithms the server supports at " +
						"plan time; `lz4` no longer works from Kopia 0.23 on.",
					stringValidators: []validator.String{stringvalidator.LengthAtLeast(1)},
				},
				{
					name: "only_compress", kind: policyKindStringSet,
					description: "File extensions, with their leading dot, such as `.txt`, that are " +
						"compressed even when `never_compress` lists them. Despite its name it restricts " +
						"nothing: files with other extensions are compressed too, unless `never_compress` " +
						"lists them. Unlike other settings, the extensions of every level are combined, unless " +
						"`no_parent_only_compress` is set.",
					setValidators: []validator.Set{
						setvalidator.SizeAtLeast(1),
						setvalidator.ValueStringsAre(policyExtensionValidator()),
					},
				},
				{
					name: "no_parent_only_compress", kind: policyKindBool,
					description: "Whether the `only_compress` extensions of less specific policies are " +
						"dropped." + policyPlainBoolNote,
				},
				{
					name: "never_compress", kind: policyKindStringSet,
					description: "File extensions, with their leading dot, such as `.gz` or `.jpg`, that are " +
						"never compressed. The extensions of every level are combined, unless " +
						"`no_parent_never_compress` is set.",
					setValidators: []validator.Set{
						setvalidator.SizeAtLeast(1),
						setvalidator.ValueStringsAre(policyExtensionValidator()),
					},
				},
				{
					name: "no_parent_never_compress", kind: policyKindBool,
					description: "Whether the `never_compress` extensions of less specific policies are " +
						"dropped." + policyPlainBoolNote,
				},
				{
					name: "min_size", kind: policyKindInt64,
					description: "Size in bytes below which files are not compressed, or `-1` for no limit, " +
						"which overrides a limit inherited from above. Kopia cannot store `0`.",
					int64Validators: []validator.Int64{policySizeLimitValidator()},
				},
				{
					name: "max_size", kind: policyKindInt64,
					description: "Size in bytes above which files are not compressed, or `-1` for no limit, " +
						"which overrides a limit inherited from above. Kopia cannot store `0`.",
					int64Validators: []validator.Int64{policySizeLimitValidator()},
				},
			},
		},
		{
			name: "metadata_compression", kind: policyKindObject,
			description: "How Kopia compresses the directory listings and other metadata of snapshots.",
			attributes: []policyAttr{
				{
					name: "compressor_name", kind: policyKindString,
					description: "Compression algorithm, or `none`. Kopia's built-in default is " +
						"`zstd-fastest`. The provider checks the name at plan time, as for `compression`.",
					stringValidators: []validator.String{stringvalidator.LengthAtLeast(1)},
				},
			},
		},
		{
			name: "splitter", kind: policyKindObject,
			description: "How files are split into chunks for deduplication.",
			attributes: []policyAttr{
				{
					name: "algorithm", kind: policyKindString,
					description: "Splitter algorithm, such as `DYNAMIC-4M-BUZHASH` or `FIXED-1M`. Without " +
						"one, the repository's own splitter is used. The provider checks the name against the " +
						"algorithms the server supports at plan time; Kopia itself would fall back to the " +
						"repository's splitter silently.",
					stringValidators: []validator.String{stringvalidator.LengthAtLeast(1)},
				},
			},
		},
		{
			name: "actions", kind: policyKindObject,
			description: "Commands run around snapshots. They run only if the server's repository connection " +
				"has actions enabled (`kopia repository connect --enable-actions`); otherwise Kopia skips them. " +
				"`before_snapshot_root` and `after_snapshot_root` are inherited, while `before_folder` and " +
				"`after_folder` apply only to the directory of this very policy.",
			attributes: []policyAttr{
				policyActionAttribute("before_folder", "Runs before a snapshot enters the directory this policy is defined on."),
				policyActionAttribute("after_folder", "Runs after a snapshot has left the directory this policy is defined on."),
				policyActionAttribute("before_snapshot_root", "Runs before a snapshot starts."),
				policyActionAttribute("after_snapshot_root", "Runs after a snapshot has finished."),
			},
		},
		{
			name: "os_snapshots", kind: policyKindObject,
			description: "Operating-system snapshots taken of the source while it is read. Only Kopia " +
				"servers on Windows take them.",
			attributes: []policyAttr{
				{
					name: "volume_shadow_copy", kind: policyKindString,
					description: "Whether Windows Volume Shadow Copy is used: `never` (Kopia's built-in " +
						"default), `always`, which fails the snapshot when no shadow copy can be made, or " +
						"`when-available`, which carries on without one and warns. A number Kopia stores that " +
						"is none of these is shown as it is.",
					stringValidators: []validator.String{
						stringvalidator.OneOf(policyVolumeShadowCopyModes...),
					},
				},
			},
		},
		{
			name: "logging", kind: policyKindObject,
			description: "How much a snapshot logs about the directories and files it processes.",
			attributes: []policyAttr{
				{
					name: "directories", kind: policyKindObject,
					description: "Logging about directories.",
					attributes: []policyAttr{
						logLevel("snapshotted", "Detail logged for a directory snapshotted. Kopia's built-in default is `5`."),
						logLevel("ignored", "Detail logged for a directory left out. Kopia's built-in default is `5`."),
					},
				},
				{
					name: "entries", kind: policyKindObject,
					description: "Logging about files and other entries.",
					attributes: []policyAttr{
						logLevel("snapshotted", "Detail logged for an entry snapshotted. Kopia's built-in default is `0`."),
						logLevel("ignored", "Detail logged for an entry left out. Kopia's built-in default is `5`."),
						logLevel("cache_hit", "Detail logged for an entry found unchanged in the cache. Kopia's built-in default is `0`."),
						logLevel("cache_miss", "Detail logged for an entry not found in the cache. Kopia's built-in default is `0`."),
					},
				},
			},
		},
		{
			name: "upload", kind: policyKindObject,
			description: "How much of a snapshot runs in parallel.",
			attributes: []policyAttr{
				{
					name: "max_parallel_snapshots", kind: policyKindInt64,
					description: "Number of snapshots the server takes at the same time. Kopia's built-in " +
						"default is `1`. Kopia reads it only from the policies of the server's own `user@host` " +
						"and refuses it on a path policy, so it has no effect on other targets.",
					int64Validators: []validator.Int64{int64validator.Between(1, policyMaxInt)},
				},
				{
					name: "max_parallel_file_reads", kind: policyKindInt64,
					description: "Number of files read at the same time. Kopia uses the number of CPUs when " +
						"no policy sets it.",
					int64Validators: []validator.Int64{int64validator.Between(1, policyMaxInt)},
				},
				{
					name: "parallel_upload_above_size", kind: policyKindInt64,
					description: "Size in bytes above which a file is uploaded in parallel chunks, or a " +
						"negative number never to. Kopia's built-in default is `2147483648` (2 GiB). `0` is " +
						"refused: Kopia would divide by it.",
					int64Validators: []validator.Int64{policyNonZeroValidator()},
				},
			},
		},
		{
			name: "no_parent", kind: policyKindBool,
			description: "Whether this policy inherits nothing: neither from less specific policies nor " +
				"Kopia's built-in defaults, so that whatever it leaves out falls back to Kopia's runtime " +
				"behaviour, which, among other things, keeps every snapshot and compresses nothing. It also " +
				"keeps this policy's own `before_folder` and `after_folder` actions from running at the root " +
				"of a snapshot." +
				policyPlainBoolNote,
		},
	}
}

// policyRetentionCount is one of the keep_* counts of the retention section.
func policyRetentionCount(name, description string) policyAttr {
	return policyAttr{
		name: name, kind: policyKindInt64,
		description:     description + " Between 0 and 2147483647.",
		int64Validators: []validator.Int64{int64validator.Between(0, policyMaxInt)},
	}
}

// policyMaxScriptBytes is the CLI's limit for an action script
// (cli/command_policy_set_actions.go:17).
const policyMaxScriptBytes = 32000

// policyActionAttribute is one of the four actions of the actions section.
func policyActionAttribute(name, description string) policyAttr {
	return policyAttr{
		name: name, kind: policyKindObject,
		description: description + " Exactly one of `command` and `script` must be set.",
		attributes: []policyAttr{
			{
				name: "command", kind: policyKindString,
				description: "Path of the program to run, on the Kopia server. Kopia sends it as `path`.",
				stringValidators: []validator.String{
					stringvalidator.LengthAtLeast(1),
					stringvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("script")),
				},
			},
			{
				name: "args", kind: policyKindStringList,
				description: "Arguments to `command`. Only valid together with `command`.",
				listValidators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.AlsoRequires(path.MatchRelative().AtParent().AtName("command")),
				},
			},
			{
				name: "script", kind: policyKindString,
				description: "Script to run instead of a command, at most 32000 bytes. Kopia writes it to a " +
					"temporary file on the server and runs that directly when it starts with `#!`, with " +
					"`sh -e` otherwise, or with `%COMSPEC% /c` on Windows.",
				stringValidators: []validator.String{stringvalidator.LengthBetween(1, policyMaxScriptBytes)},
			},
			{
				name: "timeout_seconds", kind: policyKindInt64,
				description: "Time limit in seconds; Kopia allows 3 minutes when it is not set. Kopia sends " +
					"it as `timeout`.",
				int64Validators: []validator.Int64{int64validator.Between(1, policyMaxInt)},
			},
			{
				name: "mode", kind: policyKindString,
				description: "`essential`, where a non-zero exit status fails the snapshot (the default of " +
					"`kopia policy set`), `optional`, where it is only logged, or `async`, where Kopia does " +
					"not wait for the command. Kopia treats a mode it is not given as `optional`.",
				stringValidators: []validator.String{stringvalidator.OneOf("essential", "optional", "async")},
			},
		},
	}
}

// policyVolumeShadowCopyModes are the names of Kopia's OSSnapshotMode values,
// in the order of their numbers, as `kopia policy set` takes them.
var policyVolumeShadowCopyModes = []string{"never", "always", "when-available"}

// policyResourceAttributes renders attrs for kopia_policy: optional, with
// validators.
func policyResourceAttributes(attrs []policyAttr) map[string]schema.Attribute {
	rendered := make(map[string]schema.Attribute, len(attrs))
	for _, a := range attrs {
		rendered[a.name] = a.resourceAttribute()
	}
	return rendered
}

func (a policyAttr) resourceAttribute() schema.Attribute {
	optional, required := !a.required, a.required
	switch a.kind {
	case policyKindBool:
		return schema.BoolAttribute{
			Optional: optional, Required: required, MarkdownDescription: a.description, Validators: a.boolValidators,
		}
	case policyKindInt64:
		return schema.Int64Attribute{
			Optional: optional, Required: required, MarkdownDescription: a.description, Validators: a.int64Validators,
		}
	case policyKindString:
		return schema.StringAttribute{
			Optional: optional, Required: required, MarkdownDescription: a.description, Validators: a.stringValidators,
		}
	case policyKindStringList:
		return schema.ListAttribute{
			ElementType: types.StringType,
			Optional:    optional, Required: required, MarkdownDescription: a.description, Validators: a.listValidators,
		}
	case policyKindStringSet:
		return schema.SetAttribute{
			ElementType: types.StringType,
			Optional:    optional, Required: required, MarkdownDescription: a.description, Validators: a.setValidators,
		}
	case policyKindObject:
		return schema.SingleNestedAttribute{
			Optional: optional, Required: required, MarkdownDescription: a.description, Validators: a.objectValidators,
			Attributes: policyResourceAttributes(a.attributes),
		}
	case policyKindObjectSet:
		return schema.SetNestedAttribute{
			Optional: optional, Required: required, MarkdownDescription: a.description, Validators: a.setValidators,
			NestedObject: schema.NestedAttributeObject{Attributes: policyResourceAttributes(a.attributes)},
		}
	}
	panic("policy attribute " + a.name + " has no kind")
}

// policyDataSourceAttributes renders attrs for a data source: computed, and
// without validators, which only check configuration.
func policyDataSourceAttributes(attrs []policyAttr) map[string]datasourceschema.Attribute {
	rendered := make(map[string]datasourceschema.Attribute, len(attrs))
	for _, a := range attrs {
		rendered[a.name] = a.dataSourceAttribute()
	}
	return rendered
}

func (a policyAttr) dataSourceAttribute() datasourceschema.Attribute {
	switch a.kind {
	case policyKindBool:
		return datasourceschema.BoolAttribute{Computed: true, MarkdownDescription: a.description}
	case policyKindInt64:
		return datasourceschema.Int64Attribute{Computed: true, MarkdownDescription: a.description}
	case policyKindString:
		return datasourceschema.StringAttribute{Computed: true, MarkdownDescription: a.description}
	case policyKindStringList:
		return datasourceschema.ListAttribute{ElementType: types.StringType, Computed: true, MarkdownDescription: a.description}
	case policyKindStringSet:
		return datasourceschema.SetAttribute{ElementType: types.StringType, Computed: true, MarkdownDescription: a.description}
	case policyKindObject:
		return datasourceschema.SingleNestedAttribute{
			Computed: true, MarkdownDescription: a.description,
			Attributes: policyDataSourceAttributes(a.attributes),
		}
	case policyKindObjectSet:
		return datasourceschema.SetNestedAttribute{
			Computed: true, MarkdownDescription: a.description,
			NestedObject: datasourceschema.NestedAttributeObject{Attributes: policyDataSourceAttributes(a.attributes)},
		}
	}
	panic("policy attribute " + a.name + " has no kind")
}

// policyAttrTypes returns the value types of attrs, for building objects of
// the tree outside of a plan or state.
func policyAttrTypes(attrs []policyAttr) map[string]attr.Type {
	attrTypes := make(map[string]attr.Type, len(attrs))
	for _, a := range attrs {
		attrTypes[a.name] = a.attrType()
	}
	return attrTypes
}

func (a policyAttr) attrType() attr.Type {
	switch a.kind {
	case policyKindBool:
		return types.BoolType
	case policyKindInt64:
		return types.Int64Type
	case policyKindString:
		return types.StringType
	case policyKindStringList:
		return types.ListType{ElemType: types.StringType}
	case policyKindStringSet:
		return types.SetType{ElemType: types.StringType}
	case policyKindObject:
		return types.ObjectType{AttrTypes: policyAttrTypes(a.attributes)}
	case policyKindObjectSet:
		return types.SetType{ElemType: types.ObjectType{AttrTypes: policyAttrTypes(a.attributes)}}
	}
	panic("policy attribute " + a.name + " has no kind")
}

// policyBodyTypes are the value types of the policy body's attributes, and the
// attribute types of kopia_effective_policy's `effective` object.
var policyBodyTypes = policyAttrTypes(policyBodyAttributes())

// policyObjectTypes returns the attribute types of the object at the given
// attribute names below the policy body, such as ("logging", "entries").
func policyObjectTypes(names ...string) map[string]attr.Type {
	attrTypes := policyBodyTypes
	for _, name := range names {
		switch t := attrTypes[name].(type) {
		case types.ObjectType:
			attrTypes = t.AttrTypes
		case types.SetType:
			attrTypes = t.ElemType.(types.ObjectType).AttrTypes
		default:
			panic("policy attribute " + name + " is not an object")
		}
	}
	return attrTypes
}
