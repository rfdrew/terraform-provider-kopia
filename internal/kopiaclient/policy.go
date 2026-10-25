package kopiaclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Snapshot policies (snapshot/policy in Kopia) are stored per target: the
// global policy, a host, a user on a host, or a path of a user on a host.
// GET, PUT and DELETE /api/v1/policy address one target through the query
// parameters host, userName and path; PUT replaces the target's whole policy.
//
// The JSON shape of a policy depends on the Kopia release. 0.22.0 tags every
// non-slice field omitzero and leaves empty sections out; 0.22.3 and later tag
// them omitempty, which Go never applies to a struct, so every section is sent
// even when it is empty: "splitter": {}, "logging": {"directories": {},
// "entries": {}} and so on. Policy normalises both on decoding, so callers see
// the same value whichever release answered, and it never sends an empty
// section itself.
//
// Kopia's pointer fields (*OptionalBool, *OptionalInt, *LogDetail and the
// like) can say "not set here" (nil, absent) apart from an explicit false or
// 0, which overrides a parent policy. Its plain fields cannot: an empty value
// is left out (omitempty) and means "inherit", so false, 0 and "" can never be
// stored. The structs below mirror that exactly — pointers where Kopia has
// them, plain values where it has plain ones — and every field is omitempty,
// so nothing the caller did not set is sent.

// Policy is a snapshot policy as Kopia stores it (snapshot/policy/policy.go:23-37).
// Kopia's labels are left out: the server never serialises them and sets them
// itself from the target.
type Policy struct {
	Retention           *RetentionPolicy           `json:"retention,omitempty"`
	Files               *FilesPolicy               `json:"files,omitempty"`
	ErrorHandling       *ErrorHandlingPolicy       `json:"errorHandling,omitempty"`
	Scheduling          *SchedulingPolicy          `json:"scheduling,omitempty"`
	Compression         *CompressionPolicy         `json:"compression,omitempty"`
	MetadataCompression *MetadataCompressionPolicy `json:"metadataCompression,omitempty"`
	Splitter            *SplitterPolicy            `json:"splitter,omitempty"`
	Actions             *ActionsPolicy             `json:"actions,omitempty"`
	OSSnapshots         *OSSnapshotPolicy          `json:"osSnapshots,omitempty"`
	Logging             *LoggingPolicy             `json:"logging,omitempty"`
	Upload              *UploadPolicy              `json:"upload,omitempty"`
	// NoParent stops inheritance at this policy, and with it Kopia's
	// built-in defaults.
	NoParent bool `json:"noParent,omitempty"`
}

// RetentionPolicy is snapshot/policy/retention_policy.go:23-31. The counts are
// Kopia's *OptionalInt.
type RetentionPolicy struct {
	KeepLatest               *int64 `json:"keepLatest,omitempty"`
	KeepHourly               *int64 `json:"keepHourly,omitempty"`
	KeepDaily                *int64 `json:"keepDaily,omitempty"`
	KeepWeekly               *int64 `json:"keepWeekly,omitempty"`
	KeepMonthly              *int64 `json:"keepMonthly,omitempty"`
	KeepAnnual               *int64 `json:"keepAnnual,omitempty"`
	IgnoreIdenticalSnapshots *bool  `json:"ignoreIdenticalSnapshots,omitempty"`
}

// FilesPolicy is snapshot/policy/files_policy.go:6-14. MaxFileSize is in
// bytes.
type FilesPolicy struct {
	IgnoreRules            []string `json:"ignore,omitempty"`
	NoParentIgnoreRules    bool     `json:"noParentIgnore,omitempty"`
	DotIgnoreFiles         []string `json:"ignoreDotFiles,omitempty"`
	NoParentDotIgnoreFiles bool     `json:"noParentDotFiles,omitempty"`
	IgnoreCacheDirectories *bool    `json:"ignoreCacheDirs,omitempty"`
	MaxFileSize            int64    `json:"maxFileSize,omitempty"`
	OneFileSystem          *bool    `json:"oneFileSystem,omitempty"`
}

// ErrorHandlingPolicy is snapshot/policy/error_handling_policy.go:6-15.
type ErrorHandlingPolicy struct {
	IgnoreFileErrors      *bool `json:"ignoreFileErrors,omitempty"`
	IgnoreDirectoryErrors *bool `json:"ignoreDirectoryErrors,omitempty"`
	IgnoreUnknownTypes    *bool `json:"ignoreUnknownTypes,omitempty"`
}

// SchedulingPolicy is snapshot/policy/scheduling_policy.go:65-72, in the field
// order of 0.22.3 and later. IntervalSeconds is in seconds.
type SchedulingPolicy struct {
	IntervalSeconds   int64       `json:"intervalSeconds,omitempty"`
	TimesOfDay        []TimeOfDay `json:"timeOfDay,omitempty"`
	NoParentTimeOfDay bool        `json:"noParentTimeOfDay,omitempty"`
	Manual            bool        `json:"manual,omitempty"`
	Cron              []string    `json:"cron,omitempty"`
	RunMissed         *bool       `json:"runMissed,omitempty"`
}

// TimeOfDay is a time of day in the server's local time zone
// (scheduling_policy.go:22-25). The minute's key is "min": Kopia silently
// drops a "minute" key and stores the time as a full hour.
type TimeOfDay struct {
	Hour   int64 `json:"hour"`
	Minute int64 `json:"min"`
}

// CompressionPolicy is snapshot/policy/compression_policy.go:13-21. MinSize
// and MaxSize are in bytes; the extensions include their leading dot.
type CompressionPolicy struct {
	CompressorName        string   `json:"compressorName,omitempty"`
	OnlyCompress          []string `json:"onlyCompress,omitempty"`
	NoParentOnlyCompress  bool     `json:"noParentOnlyCompress,omitempty"`
	NeverCompress         []string `json:"neverCompress,omitempty"`
	NoParentNeverCompress bool     `json:"noParentNeverCompress,omitempty"`
	MinSize               int64    `json:"minSize,omitempty"`
	MaxSize               int64    `json:"maxSize,omitempty"`
}

// MetadataCompressionPolicy is snapshot/policy/compression_policy.go:24-26.
type MetadataCompressionPolicy struct {
	CompressorName string `json:"compressorName,omitempty"`
}

// SplitterPolicy is snapshot/policy/splitter_policy.go:9-11.
type SplitterPolicy struct {
	Algorithm string `json:"algorithm,omitempty"`
}

// ActionsPolicy is snapshot/policy/actions_policy.go:6-14. Unlike the
// sections, an action is a pointer in Kopia, so an empty action {} is kept as
// it is: Kopia stores it, and fails every snapshot whose before-action it is.
type ActionsPolicy struct {
	BeforeFolder       *ActionCommand `json:"beforeFolder,omitempty"`
	AfterFolder        *ActionCommand `json:"afterFolder,omitempty"`
	BeforeSnapshotRoot *ActionCommand `json:"beforeSnapshotRoot,omitempty"`
	AfterSnapshotRoot  *ActionCommand `json:"afterSnapshotRoot,omitempty"`
}

// ActionCommand is snapshot/policy/actions_policy.go:23-33. Command is sent as
// "path"; TimeoutSeconds as "timeout", in seconds.
type ActionCommand struct {
	Command        string   `json:"path,omitempty"`
	Arguments      []string `json:"args,omitempty"`
	Script         string   `json:"script,omitempty"`
	TimeoutSeconds int64    `json:"timeout,omitempty"`
	Mode           string   `json:"mode,omitempty"`
}

// OSSnapshotPolicy is snapshot/policy/os_snapshot_policy.go:6-8.
type OSSnapshotPolicy struct {
	VolumeShadowCopy *VolumeShadowCopyPolicy `json:"volumeShadowCopy,omitempty"`
}

// VolumeShadowCopyPolicy is os_snapshot_policy.go:22-24. Enable is Kopia's
// OSSnapshotMode, a byte sent as a number: 0 never, 1 always, 2 when
// available.
type VolumeShadowCopyPolicy struct {
	Enable *int64 `json:"enable,omitempty"`
}

// The OSSnapshotMode values (os_snapshot_policy.go:43-47).
const (
	OSSnapshotNever         = 0
	OSSnapshotAlways        = 1
	OSSnapshotWhenAvailable = 2
)

// LoggingPolicy is snapshot/policy/logging_policy.go:48-51. Every level is a
// LogDetail from 0 (none) to 10 (maximum).
type LoggingPolicy struct {
	Directories *DirLoggingPolicy   `json:"directories,omitempty"`
	Entries     *EntryLoggingPolicy `json:"entries,omitempty"`
}

// DirLoggingPolicy is logging_policy.go:6-9.
type DirLoggingPolicy struct {
	Snapshotted *int64 `json:"snapshotted,omitempty"`
	Ignored     *int64 `json:"ignored,omitempty"`
}

// EntryLoggingPolicy is logging_policy.go:24-29.
type EntryLoggingPolicy struct {
	Snapshotted *int64 `json:"snapshotted,omitempty"`
	Ignored     *int64 `json:"ignored,omitempty"`
	CacheHit    *int64 `json:"cacheHit,omitempty"`
	CacheMiss   *int64 `json:"cacheMiss,omitempty"`
}

// UploadPolicy is snapshot/policy/upload_policy.go:10-14.
// ParallelUploadAboveSize is in bytes.
type UploadPolicy struct {
	MaxParallelSnapshots    *int64 `json:"maxParallelSnapshots,omitempty"`
	MaxParallelFileReads    *int64 `json:"maxParallelFileReads,omitempty"`
	ParallelUploadAboveSize *int64 `json:"parallelUploadAboveSize,omitempty"`
}

// The empty methods report whether a section holds nothing. A nil section is
// empty too, so they can be called on any field of a Policy.

func (s *RetentionPolicy) empty() bool { return s == nil || *s == RetentionPolicy{} }

func (s *FilesPolicy) empty() bool {
	return s == nil || (len(s.IgnoreRules) == 0 && !s.NoParentIgnoreRules && len(s.DotIgnoreFiles) == 0 &&
		!s.NoParentDotIgnoreFiles && s.IgnoreCacheDirectories == nil && s.MaxFileSize == 0 && s.OneFileSystem == nil)
}

func (s *ErrorHandlingPolicy) empty() bool { return s == nil || *s == ErrorHandlingPolicy{} }

func (s *SchedulingPolicy) empty() bool {
	return s == nil || (s.IntervalSeconds == 0 && len(s.TimesOfDay) == 0 && !s.NoParentTimeOfDay && !s.Manual &&
		len(s.Cron) == 0 && s.RunMissed == nil)
}

func (s *CompressionPolicy) empty() bool {
	return s == nil || (s.CompressorName == "" && len(s.OnlyCompress) == 0 && !s.NoParentOnlyCompress &&
		len(s.NeverCompress) == 0 && !s.NoParentNeverCompress && s.MinSize == 0 && s.MaxSize == 0)
}

func (s *MetadataCompressionPolicy) empty() bool { return s == nil || s.CompressorName == "" }

func (s *SplitterPolicy) empty() bool { return s == nil || s.Algorithm == "" }

func (s *ActionsPolicy) empty() bool { return s == nil || *s == ActionsPolicy{} }

func (s *OSSnapshotPolicy) empty() bool {
	return s == nil || s.VolumeShadowCopy == nil || s.VolumeShadowCopy.Enable == nil
}

func (s *DirLoggingPolicy) empty() bool { return s == nil || *s == DirLoggingPolicy{} }

func (s *EntryLoggingPolicy) empty() bool { return s == nil || *s == EntryLoggingPolicy{} }

func (s *LoggingPolicy) empty() bool { return s == nil || (s.Directories.empty() && s.Entries.empty()) }

func (s *UploadPolicy) empty() bool { return s == nil || *s == UploadPolicy{} }

// Normalised returns a copy of p in the one shape this package uses whatever
// the release, and in which every Policy it decodes already is: empty
// sections, and the empty structs nested in the logging and OS snapshot
// sections, are nil, and empty lists are nil. A section that is not nil
// therefore holds something. Kopia has all of them as plain structs, so an
// empty one means exactly what an absent one does; an action is a pointer in
// Kopia, and an empty action is kept. Sections and lists that are kept are
// copied, so replacing a field of the result's sections, or an element of its
// lists, leaves p alone.
func (p Policy) Normalised() Policy {
	n := Policy{NoParent: p.NoParent}
	if !p.Retention.empty() {
		s := *p.Retention
		n.Retention = &s
	}
	if !p.Files.empty() {
		s := *p.Files
		s.IgnoreRules = policyStrings(s.IgnoreRules)
		s.DotIgnoreFiles = policyStrings(s.DotIgnoreFiles)
		n.Files = &s
	}
	if !p.ErrorHandling.empty() {
		s := *p.ErrorHandling
		n.ErrorHandling = &s
	}
	if !p.Scheduling.empty() {
		s := *p.Scheduling
		s.TimesOfDay = nil
		if len(p.Scheduling.TimesOfDay) > 0 {
			s.TimesOfDay = append([]TimeOfDay(nil), p.Scheduling.TimesOfDay...)
		}
		s.Cron = policyStrings(s.Cron)
		n.Scheduling = &s
	}
	if !p.Compression.empty() {
		s := *p.Compression
		s.OnlyCompress = policyStrings(s.OnlyCompress)
		s.NeverCompress = policyStrings(s.NeverCompress)
		n.Compression = &s
	}
	if !p.MetadataCompression.empty() {
		s := *p.MetadataCompression
		n.MetadataCompression = &s
	}
	if !p.Splitter.empty() {
		s := *p.Splitter
		n.Splitter = &s
	}
	if !p.Actions.empty() {
		s := ActionsPolicy{
			BeforeFolder:       p.Actions.BeforeFolder.normalised(),
			AfterFolder:        p.Actions.AfterFolder.normalised(),
			BeforeSnapshotRoot: p.Actions.BeforeSnapshotRoot.normalised(),
			AfterSnapshotRoot:  p.Actions.AfterSnapshotRoot.normalised(),
		}
		n.Actions = &s
	}
	if !p.OSSnapshots.empty() {
		vss := *p.OSSnapshots.VolumeShadowCopy
		n.OSSnapshots = &OSSnapshotPolicy{VolumeShadowCopy: &vss}
	}
	if !p.Logging.empty() {
		s := LoggingPolicy{}
		if !p.Logging.Directories.empty() {
			d := *p.Logging.Directories
			s.Directories = &d
		}
		if !p.Logging.Entries.empty() {
			e := *p.Logging.Entries
			s.Entries = &e
		}
		n.Logging = &s
	}
	if !p.Upload.empty() {
		s := *p.Upload
		n.Upload = &s
	}
	return n
}

// normalised copies an action, with an empty argument list as nil.
func (a *ActionCommand) normalised() *ActionCommand {
	if a == nil {
		return nil
	}
	c := *a
	c.Arguments = policyStrings(c.Arguments)
	return &c
}

// policyStrings copies a list, turning an empty one into nil.
func policyStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

// policyWire is Policy without its JSON methods, for encoding and decoding
// the normalised value.
type policyWire Policy

// MarshalJSON encodes the normalised policy: an empty section is left out
// rather than sent as {}, and no list is ever sent as [] (omitempty already
// drops those). Kopia would treat either like an absent key, but `"cron": []`
// together with "manual": true fails its validation (spec: policy.md §2.1).
func (p Policy) MarshalJSON() ([]byte, error) {
	return json.Marshal(policyWire(p.Normalised()))
}

// UnmarshalJSON decodes a policy in either release's shape and normalises it,
// so that the same stored policy decodes to the same value whether 0.22.0
// left its empty sections out or 0.22.3 and later sent them as {}.
func (p *Policy) UnmarshalJSON(data []byte) error {
	var decoded policyWire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = Policy(decoded).Normalised()
	return nil
}

// SourceInfo names a policy target or a snapshot source
// (snapshot/source.go:12-16). All three fields empty is the global policy;
// otherwise Path set is a path, then UserName set a user, then Host set a
// host, which is how Kopia itself classifies them (policy_manager.go:315-344).
type SourceInfo struct {
	Host     string `json:"host"`
	UserName string `json:"userName"`
	Path     string `json:"path"`
}

// GlobalTarget is the text form of the global policy's target.
const GlobalTarget = "(global)"

// String renders the target as Kopia does (snapshot/source.go:18-28):
// "(global)", "user@host", "@host" or "user@host:path". The server can also
// hold targets ParseTarget refuses, such as "docker@" or "@host:/data", and
// String renders those the same way.
func (s SourceInfo) String() string {
	switch {
	case s.Host == "" && s.UserName == "" && s.Path == "":
		return GlobalTarget
	case s.Path == "":
		return s.UserName + "@" + s.Host
	default:
		return s.UserName + "@" + s.Host + ":" + s.Path
	}
}

// IsGlobal reports whether s is the global policy's target.
func (s SourceInfo) IsGlobal() bool { return s == SourceInfo{} }

// query is the target as the query parameters of the policy endpoints. Kopia
// reads host, userName and path, a missing one as "" (api_policies.go:44-54);
// the user parameter is camelCase, and a lowercase "username" would be ignored
// silently, addressing a less specific target than intended.
func (s SourceInfo) query() url.Values {
	q := url.Values{}
	if s.Host != "" {
		q.Set("host", s.Host)
	}
	if s.UserName != "" {
		q.Set("userName", s.UserName)
	}
	if s.Path != "" {
		q.Set("path", s.Path)
	}
	return q
}

// ParseTarget parses the text form of a policy target strictly: exactly
// "(global)", "@HOST", "USER@HOST" or "USER@HOST:PATH" with an absolute PATH,
// with nothing to normalise, so that String gives back the same text.
//
// Kopia's own ParseSourceInfo is deliberately not mirrored: whatever it cannot
// split, it takes for a local path and makes absolute on the machine doing the
// parsing, which here would be the one running Terraform (spec: policy.md §4).
//
// The text is split at the first "@" and then at the first ":" after it, as
// Kopia splits it, so a host never contains ":" and a user never contains "@".
// A path must be absolute and may not end with a separator unless it is a
// root, which is what Kopia's PUT enforces too (policy_manager.go:382-394). It
// may not contain empty, "." or ".." components either, and a Windows path
// must use backslashes: Kopia matches a policy to a snapshot source by exact
// text, and records sources as cleaned absolute paths, so a policy on any other
// spelling would be stored but never apply.
func ParseTarget(text string) (SourceInfo, error) {
	if text == GlobalTarget {
		return SourceInfo{}, nil
	}
	invalid := func(reason string) (SourceInfo, error) {
		return SourceInfo{}, fmt.Errorf("%q is not a valid policy target: %s", text, reason)
	}
	if strings.TrimSpace(text) != text {
		return invalid("it starts or ends with whitespace")
	}

	user, rest, ok := strings.Cut(text, "@")
	if !ok {
		return invalid("write " + GlobalTarget + ", @host, user@host or user@host:path")
	}
	host, path, hasPath := strings.Cut(rest, ":")
	switch {
	case host == "":
		return invalid("the host is empty")
	case strings.Contains(host, "@"):
		return invalid("the host contains \"@\"")
	case strings.Contains(user, ":"):
		return invalid("the user name contains \":\"")
	case strings.TrimSpace(host) != host || strings.TrimSpace(user) != user:
		return invalid("the user name or host starts or ends with whitespace")
	case hasPath && user == "":
		return invalid("a path policy needs a user name: write user@host:path")
	}
	if hasPath {
		if reason := policyPathProblem(path); reason != "" {
			return invalid(reason)
		}
	}
	return SourceInfo{Host: host, UserName: user, Path: path}, nil
}

// policyPathProblem explains what is wrong with a target's path, or returns
// "" for a path ParseTarget accepts: a Unix path such as /data/app, a Windows
// path such as C:\Data, or a UNC path such as \\server\share\dir.
func policyPathProblem(path string) string {
	const notAbsolute = `the path must be absolute: /path, C:\path or \\server\share`
	var root, rest string
	var separator byte
	unc := false
	switch {
	case path == "":
		return "the path is empty"
	case path[0] == '/':
		root, rest, separator = "/", path[1:], '/'
	case len(path) >= 2 && isASCIILetter(path[0]) && path[1] == ':':
		if len(path) == 2 || (path[2] != '\\' && path[2] != '/') {
			return notAbsolute
		}
		root, rest, separator = path[:3], path[3:], '\\'
	case strings.HasPrefix(path, `\\`):
		rest, separator, unc = path[2:], '\\', true
	default:
		return notAbsolute
	}

	if separator == '\\' && strings.Contains(path, "/") {
		return "a Windows path must use backslashes, as Kopia records Windows sources with them " +
			"and matches policies to sources by exact text"
	}
	if unc {
		if server, share, ok := strings.Cut(rest, `\`); !ok || server == "" || share == "" {
			return `a UNC path needs a server and a share: \\server\share`
		}
	}
	if last := path[len(path)-1]; (last == '/' || last == '\\') && path != root {
		return "the path ends with a slash or backslash, which Kopia refuses for anything but a root " +
			`such as / or C:\`
	}
	if rest == "" {
		return ""
	}
	for _, component := range strings.Split(rest, string(separator)) {
		if component == "" || component == "." || component == ".." {
			return "the path contains an empty, \".\" or \"..\" component, so it would never match a " +
				"snapshot source, whose path Kopia records cleaned"
		}
	}
	return ""
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// ErrPolicyUnavailable means GET /api/v1/policy answered 200 with the body
// null. Kopia's handler answers that way whenever loading the policy fails for
// any reason other than its absence, dropping the error
// (internal/server/api_policies.go:56-63), so it says nothing about whether
// the policy exists; reading again later usually succeeds.
var ErrPolicyUnavailable = errors.New("kopia answered with null instead of the policy, which it does when " +
	"loading the policy fails on the server; the policy may well exist, and reading it again usually succeeds")

// GetPolicy returns the policy defined on exactly target, without anything it
// inherits. A target without a policy is an *Error that IsNotFound reports:
// Kopia answers 400 NOT_FOUND "policy not found", not 404
// (internal/server/api_policies.go:59). A 200 null is ErrPolicyUnavailable.
func (c *Client) GetPolicy(ctx context.Context, target SourceInfo) (*Policy, error) {
	var policy *Policy
	if err := c.do(ctx, http.MethodGet, "/api/v1/policy", target.query(), nil, &policy); err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, fmt.Errorf("GET /api/v1/policy for %s: %w", target, ErrPolicyUnavailable)
	}
	return policy, nil
}

// SetPolicy replaces the policy of target with policy, creating it when the
// target has none. Kopia keeps nothing of the previous policy, and has no
// version check: the last writer wins. It validates very little: manual
// scheduling combined with anything else, an unparsable cron entry,
// maxParallelSnapshots on a path and a path ending in a separator each fail
// with 500 INTERNAL, told apart from storage failures only by the message
// (spec: policy.md §2.2).
func (c *Client) SetPolicy(ctx context.Context, target SourceInfo, policy *Policy) error {
	if policy == nil {
		policy = &Policy{}
	}
	return c.do(ctx, http.MethodPut, "/api/v1/policy", target.query(), policy, nil)
}

// DeletePolicy removes the policy of target. Kopia answers 200 whether or not
// there was one. Deleting the global policy removes it for good: nothing
// recreates it, and effective values then fall back to Kopia's built-in
// defaults; use SetPolicy with DefaultGlobalPolicy to reset it instead.
func (c *Client) DeletePolicy(ctx context.Context, target SourceInfo) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/policy", target.query(), nil, nil)
}

// PolicyFilter narrows ListPolicies to targets whose fields equal the ones
// set. An empty field does not filter, so there is no way to ask for the
// global policy alone.
type PolicyFilter struct {
	Host     string
	UserName string
	Path     string
}

// PolicyEntry is one stored policy (internal/serverapi/serverapi.go:64-74).
// ID is the manifest id, which changes on every PUT, so it does not identify
// a policy for long.
type PolicyEntry struct {
	ID     string     `json:"id"`
	Target SourceInfo `json:"target"`
	Policy *Policy    `json:"policy"`
}

// ListPolicies returns every stored policy that matches filter, oldest change
// first. Two entries for one target are possible: concurrent writes to the
// same target can leave duplicate manifests behind (spec: policy.md §3.3).
func (c *Client) ListPolicies(ctx context.Context, filter PolicyFilter) ([]PolicyEntry, error) {
	var resp struct {
		Policies []PolicyEntry `json:"policies"`
	}
	query := SourceInfo(filter).query()
	if err := c.do(ctx, http.MethodGet, "/api/v1/policies", query, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Policies == nil {
		return []PolicyEntry{}, nil
	}
	return resp.Policies, nil
}

// PolicyDefinition says, for each field of an effective policy, which target's
// policy set it (snapshot/policy/*_policy.go, the *Definition structs). The
// zero SourceInfo stands for the global policy, a built-in default, or a field
// nothing sets, which cannot be told apart; 0.22.0 leaves such entries out,
// later releases send them, and both decode to the zero value. A few fields
// have no entry at all: the noParent flags and the folder actions, which are
// never inherited.
type PolicyDefinition struct {
	Retention struct {
		KeepLatest               SourceInfo `json:"keepLatest"`
		KeepHourly               SourceInfo `json:"keepHourly"`
		KeepDaily                SourceInfo `json:"keepDaily"`
		KeepWeekly               SourceInfo `json:"keepWeekly"`
		KeepMonthly              SourceInfo `json:"keepMonthly"`
		KeepAnnual               SourceInfo `json:"keepAnnual"`
		IgnoreIdenticalSnapshots SourceInfo `json:"ignoreIdenticalSnapshots"`
	} `json:"retention"`
	Files struct {
		IgnoreRules            SourceInfo `json:"ignore"`
		NoParentIgnoreRules    SourceInfo `json:"noParentIgnore"`
		DotIgnoreFiles         SourceInfo `json:"ignoreDotFiles"`
		NoParentDotIgnoreFiles SourceInfo `json:"noParentDotFiles"`
		IgnoreCacheDirectories SourceInfo `json:"ignoreCacheDirs"`
		MaxFileSize            SourceInfo `json:"maxFileSize"`
		OneFileSystem          SourceInfo `json:"oneFileSystem"`
	} `json:"files"`
	ErrorHandling struct {
		IgnoreFileErrors      SourceInfo `json:"ignoreFileErrors"`
		IgnoreDirectoryErrors SourceInfo `json:"ignoreDirectoryErrors"`
		IgnoreUnknownTypes    SourceInfo `json:"ignoreUnknownTypes"`
	} `json:"errorHandling"`
	Scheduling struct {
		IntervalSeconds SourceInfo `json:"intervalSeconds"`
		TimesOfDay      SourceInfo `json:"timeOfDay"`
		Cron            SourceInfo `json:"cron"`
		Manual          SourceInfo `json:"manual"`
		RunMissed       SourceInfo `json:"runMissed"`
	} `json:"scheduling"`
	Compression struct {
		CompressorName SourceInfo `json:"compressorName"`
		OnlyCompress   SourceInfo `json:"onlyCompress"`
		NeverCompress  SourceInfo `json:"neverCompress"`
		MinSize        SourceInfo `json:"minSize"`
		MaxSize        SourceInfo `json:"maxSize"`
	} `json:"compression"`
	MetadataCompression struct {
		CompressorName SourceInfo `json:"compressorName"`
	} `json:"metadataCompression"`
	Splitter struct {
		Algorithm SourceInfo `json:"algorithm"`
	} `json:"splitter"`
	Actions struct {
		BeforeSnapshotRoot SourceInfo `json:"beforeSnapshotRoot"`
		AfterSnapshotRoot  SourceInfo `json:"afterSnapshotRoot"`
	} `json:"actions"`
	OSSnapshots struct {
		VolumeShadowCopy struct {
			Enable SourceInfo `json:"enable"`
		} `json:"volumeShadowCopy"`
	} `json:"osSnapshots"`
	Logging struct {
		Directories struct {
			Snapshotted SourceInfo `json:"snapshotted"`
			Ignored     SourceInfo `json:"ignored"`
		} `json:"directories"`
		Entries struct {
			Snapshotted SourceInfo `json:"snapshotted"`
			Ignored     SourceInfo `json:"ignored"`
			CacheHit    SourceInfo `json:"cacheHit"`
			CacheMiss   SourceInfo `json:"cacheMiss"`
		} `json:"entries"`
	} `json:"logging"`
	Upload struct {
		MaxParallelSnapshots    SourceInfo `json:"maxParallelSnapshots"`
		MaxParallelFileReads    SourceInfo `json:"maxParallelFileReads"`
		ParallelUploadAboveSize SourceInfo `json:"parallelUploadAboveSize"`
	} `json:"upload"`
}

// ResolvedPolicy is the response of POST /api/v1/policy/resolve
// (internal/serverapi/serverapi.go:268-273).
type ResolvedPolicy struct {
	// Effective is the target's policy merged with everything it inherits,
	// down to Kopia's built-in defaults.
	Effective *Policy `json:"effective"`
	// Definition says where each effective value came from.
	Definition *PolicyDefinition `json:"definition"`
	// Defined is the policy stored on the target itself, or an empty one; it
	// never reflects the updates sent.
	Defined *Policy `json:"defined"`
	// UpcomingSnapshotTimes are the next scheduled snapshot times, in the
	// server's time zone, computed as if a snapshot had just finished.
	UpcomingSnapshotTimes []time.Time `json:"upcomingSnapshotTimes"`
	// SchedulingError is why Kopia would refuse the scheduling section, or ""
	// when it would accept it.
	SchedulingError string `json:"schedulingError,omitempty"`
}

// ResolvePolicy computes the effective policy of target. With updates nil,
// the stored policies are used; otherwise updates stands in for the target's
// own policy, and SchedulingError tells whether Kopia would accept its
// scheduling section — the only check a PUT makes that cannot be repeated
// without the server, because it parses cron expressions.
//
// numUpcomingSnapshotTimes asks for that many upcoming snapshot times; Kopia
// computes them in a loop without a limit, so callers should keep it small.
//
// With updates set, Effective and Definition are wrong for the global policy
// and for host targets: Kopia merges the stored policy of such a target in
// again after the updates (spec: policy.md §3.6). SchedulingError is right
// for every target.
func (c *Client) ResolvePolicy(ctx context.Context, target SourceInfo, updates *Policy, numUpcomingSnapshotTimes int) (*ResolvedPolicy, error) {
	if numUpcomingSnapshotTimes < 0 {
		numUpcomingSnapshotTimes = 0
	}
	// Kopia requires a body; "updates": null means "use the stored policy".
	body := struct {
		Updates                  *Policy `json:"updates"`
		NumUpcomingSnapshotTimes int     `json:"numUpcomingSnapshotTimes"`
	}{updates, numUpcomingSnapshotTimes}

	var resolved ResolvedPolicy
	if err := c.do(ctx, http.MethodPost, "/api/v1/policy/resolve", target.query(), body, &resolved); err != nil {
		return nil, err
	}
	if resolved.Effective == nil {
		resolved.Effective = &Policy{}
	}
	if resolved.Definition == nil {
		resolved.Definition = &PolicyDefinition{}
	}
	if resolved.Defined == nil {
		resolved.Defined = &Policy{}
	}
	return &resolved, nil
}

// DefaultGlobalPolicy returns the global policy `kopia repository create`
// stores (policy.DefaultPolicy, snapshot/policy/policy_tree.go:8-91): what
// GET /api/v1/policy reports for the global policy of a new repository. PUT
// it to reset the global policy; DELETE would remove it altogether.
func DefaultGlobalPolicy() *Policy {
	return &Policy{
		Retention: &RetentionPolicy{
			KeepLatest:               policyPointer[int64](10),
			KeepHourly:               policyPointer[int64](48),
			KeepDaily:                policyPointer[int64](7),
			KeepWeekly:               policyPointer[int64](4),
			KeepMonthly:              policyPointer[int64](24),
			KeepAnnual:               policyPointer[int64](3),
			IgnoreIdenticalSnapshots: policyPointer(false),
		},
		Files: &FilesPolicy{DotIgnoreFiles: []string{".kopiaignore"}},
		ErrorHandling: &ErrorHandlingPolicy{
			IgnoreFileErrors:      policyPointer(false),
			IgnoreDirectoryErrors: policyPointer(false),
			IgnoreUnknownTypes:    policyPointer(true),
		},
		Scheduling:          &SchedulingPolicy{RunMissed: policyPointer(true)},
		Compression:         &CompressionPolicy{CompressorName: "none"},
		MetadataCompression: &MetadataCompressionPolicy{CompressorName: "zstd-fastest"},
		OSSnapshots: &OSSnapshotPolicy{
			VolumeShadowCopy: &VolumeShadowCopyPolicy{Enable: policyPointer[int64](OSSnapshotNever)},
		},
		Logging: &LoggingPolicy{
			Directories: &DirLoggingPolicy{Snapshotted: policyPointer[int64](5), Ignored: policyPointer[int64](5)},
			Entries: &EntryLoggingPolicy{
				Snapshotted: policyPointer[int64](0),
				Ignored:     policyPointer[int64](5),
				CacheHit:    policyPointer[int64](0),
				CacheMiss:   policyPointer[int64](0),
			},
		},
		Upload: &UploadPolicy{
			MaxParallelSnapshots:    policyPointer[int64](1),
			ParallelUploadAboveSize: policyPointer[int64](2 << 30),
		},
	}
}

// policyPointer returns a pointer to a copy of v.
func policyPointer[T any](v T) *T { return &v }
