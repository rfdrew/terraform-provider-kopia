package kopiaclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// SourcesFilter narrows ListSources. Kopia matches each non-empty field
// exactly and case-sensitively, the path as a plain string; an empty field
// matches everything (internal/server/api_snapshots.go:235-249).
type SourcesFilter struct {
	Host     string
	UserName string
	Path     string
}

// SourcesResponse mirrors serverapi.SourcesResponse (serverapi.go:42-51), the
// body of GET /api/v1/sources.
type SourcesResponse struct {
	// LocalUsername and LocalHost are the server's connection identity, the
	// same as Username and Hostname of RepoStatus. A source is local, and
	// snapshotted by this server, when its host is LocalHost.
	LocalUsername string `json:"localUsername"`
	LocalHost     string `json:"localHost"`
	// MultiUser is true for a repository connected directly to its storage
	// and false for one reached through another Kopia server.
	MultiUser bool `json:"multiUser"`
	// Sources is never null, and sorted by the source's string form.
	Sources []SourceStatus `json:"sources"`
}

// SourceStatus mirrors serverapi.SourceStatus (serverapi.go:53-62): the
// state of the server's source manager for one source. The list holds every
// source with a snapshot in the repository, from any host, plus every policy
// target with a user, a host and a path (spec: repo.md §7.2).
type SourceStatus struct {
	Source SourceInfo `json:"source"`
	// Status is one of UNKNOWN, INITIALIZING, IDLE, PENDING, UPLOADING,
	// PAUSED, REMOTE, FAILED or STOPPED (internal/server/source_manager.go).
	Status string `json:"status"`
	// Schedule is the effective scheduling policy as of the manager's last
	// successful refresh; {} when it never had one.
	Schedule SourceSchedule `json:"schedule"`
	// LastSnapshot is the newest snapshot, complete or not, as of the last
	// refresh; nil when the source has none.
	LastSnapshot *SnapshotManifest `json:"lastSnapshot,omitempty"`
	// NextSnapshotTime is in the server's local time zone, and may lie in the
	// past for an overdue source. It is nil for a manual schedule, a paused
	// source, and while a requested snapshot has not finished.
	NextSnapshotTime *time.Time `json:"nextSnapshotTime,omitempty"`
	// Upload and CurrentTask are set only while the status is UPLOADING.
	Upload      *UploadCounters `json:"upload,omitempty"`
	CurrentTask string          `json:"currentTask,omitempty"`
}

// SourceSchedule mirrors policy.SchedulingPolicy
// (snapshot/policy/scheduling_policy.go:65-72), every field of which Kopia
// leaves out when it is zero.
type SourceSchedule struct {
	IntervalSeconds   int64             `json:"intervalSeconds,omitempty"`
	TimeOfDay         []SourceTimeOfDay `json:"timeOfDay,omitempty"`
	NoParentTimeOfDay bool              `json:"noParentTimeOfDay,omitempty"`
	Manual            bool              `json:"manual,omitempty"`
	Cron              []string          `json:"cron,omitempty"`
	// RunMissed is an OptionalBool: nil when no policy sets it.
	RunMissed *bool `json:"runMissed,omitempty"`
}

// SourceTimeOfDay mirrors policy.TimeOfDay, whose minute is keyed "min".
type SourceTimeOfDay struct {
	Hour   int `json:"hour"`
	Minute int `json:"min"`
}

// SnapshotManifest mirrors the parts of snapshot.Manifest
// (snapshot/manifest.go:18-40) that GET /api/v1/sources reports as a source's
// last snapshot. It is the stored manifest, a different shape from the
// Snapshot entries of ListSnapshots.
type SnapshotManifest struct {
	// ID is 32 lower-case hex digits, and changes whenever the snapshot is
	// edited.
	ID          string     `json:"id"`
	Source      SourceInfo `json:"source"`
	Description string     `json:"description"`
	// StartTime and EndTime are written in UTC with nanoseconds.
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
	// Stats is always sent, except by Kopia 0.22.0 when every counter is 0;
	// it then decodes as zeros all the same.
	Stats SnapshotStats `json:"stats"`
	// IncompleteReason is "checkpoint", "canceled" or "limit reached" for a
	// snapshot that did not complete, and empty otherwise.
	IncompleteReason string             `json:"incomplete,omitempty"`
	RootEntry        *SnapshotRootEntry `json:"rootEntry"`
	Tags             map[string]string  `json:"tags,omitempty"`
	Pins             []string           `json:"pins,omitempty"`
}

// SnapshotStats mirrors snapshot.Stats (snapshot/stats.go:10-37): what the
// upload that made a snapshot saw. Sizes are in bytes.
type SnapshotStats struct {
	TotalSize         int64 `json:"totalSize"`
	ExcludedTotalSize int64 `json:"excludedTotalSize"`
	FileCount         int64 `json:"fileCount"`
	CachedFiles       int64 `json:"cachedFiles"`
	NonCachedFiles    int64 `json:"nonCachedFiles"`
	DirCount          int64 `json:"dirCount"`
	ExcludedFileCount int64 `json:"excludedFileCount"`
	ExcludedDirCount  int64 `json:"excludedDirCount"`
	IgnoredErrorCount int64 `json:"ignoredErrorCount"`
	ErrorCount        int64 `json:"errorCount"`
}

// SnapshotRootEntry mirrors snapshot.DirEntry (snapshot/manifest.go:121-131)
// for the root of a snapshot.
type SnapshotRootEntry struct {
	Name string `json:"name,omitempty"`
	// Type is "d" for a directory, "f" for a file and "s" for a symbolic
	// link.
	Type string `json:"type,omitempty"`
	// Mode is the permissions as an octal string such as "0755".
	Mode    string    `json:"mode,omitempty"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mtime,omitempty"`
	UID     uint32    `json:"uid,omitempty"`
	GID     uint32    `json:"gid,omitempty"`
	// ObjectID identifies the root's contents; it is what ListSnapshots
	// reports as rootID.
	ObjectID string            `json:"obj"`
	Summary  *DirectorySummary `json:"summ,omitempty"`
}

// DirectorySummary mirrors fs.DirectorySummary (fs/entry.go:176-196): totals
// over a snapshot's root directory.
type DirectorySummary struct {
	Size     int64 `json:"size"`
	Files    int64 `json:"files"`
	Symlinks int64 `json:"symlinks"`
	Dirs     int64 `json:"dirs"`
	// MaxTime is the newest modification time in the tree; the Unix epoch
	// when there is none.
	MaxTime          time.Time `json:"maxTime"`
	IncompleteReason string    `json:"incomplete,omitempty"`
	NumFailed        int64     `json:"numFailed"`
	NumIgnoredErrors int64     `json:"numIgnoredErrors,omitempty"`
	// Errors holds the first ten entries that failed.
	Errors []DirectorySummaryError `json:"errors,omitempty"`
}

// DirectorySummaryError mirrors fs.EntryWithError.
type DirectorySummaryError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// UploadCounters mirrors upload.Counters (snapshot/upload/
// upload_progress.go:168-201) for a running upload. Kopia never fills in
// uploadedBytes, excludedFiles and excludedDirs there (upload_progress.go:
// 292-309), so they are always 0 and left out here.
type UploadCounters struct {
	CachedBytes    int64  `json:"cachedBytes"`
	HashedBytes    int64  `json:"hashedBytes"`
	EstimatedBytes int64  `json:"estimatedBytes"`
	CachedFiles    int64  `json:"cachedFiles"`
	HashedFiles    int64  `json:"hashedFiles"`
	EstimatedFiles int64  `json:"estimatedFiles"`
	Errors         int64  `json:"errors"`
	IgnoredErrors  int64  `json:"ignoredErrors"`
	Directory      string `json:"directory"`
	LastErrorPath  string `json:"lastErrorPath"`
	LastError      string `json:"lastError"`
}

// ListSources reads GET /api/v1/sources, narrowed by filter. It needs a
// repository connected.
func (c *Client) ListSources(ctx context.Context, filter SourcesFilter) (*SourcesResponse, error) {
	// An empty parameter matches everything, as an absent one does, so only
	// the filters that are set are sent. The user filter is "userName": Kopia
	// ignores "username", and would then return every user's sources.
	query := url.Values{}
	if filter.Host != "" {
		query.Set("host", filter.Host)
	}
	if filter.UserName != "" {
		query.Set("userName", filter.UserName)
	}
	if filter.Path != "" {
		query.Set("path", filter.Path)
	}

	var resp SourcesResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/sources", query, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SnapshotsResponse mirrors serverapi.SnapshotsResponse
// (serverapi.go:180-185), the body of GET /api/v1/snapshots.
type SnapshotsResponse struct {
	// Snapshots is never null, and sorted by start time, oldest first.
	Snapshots []Snapshot `json:"snapshots"`
	// UnfilteredCount is the number of snapshots found, and UniqueCount the
	// number of distinct root object ids among them.
	UnfilteredCount int64 `json:"unfilteredCount"`
	UniqueCount     int64 `json:"uniqueCount"`
}

// Snapshot mirrors serverapi.Snapshot (serverapi.go:167-178).
type Snapshot struct {
	// ID changes whenever the snapshot is edited; StartTime identifies a
	// snapshot across edits.
	ID          string `json:"id"`
	Description string `json:"description"`
	// StartTime and EndTime are written in UTC with nanoseconds.
	StartTime        time.Time `json:"startTime"`
	EndTime          time.Time `json:"endTime"`
	IncompleteReason string    `json:"incomplete,omitempty"`
	// Summary is null for a snapshot without a root entry or without a
	// directory summary.
	Summary *DirectorySummary `json:"summary"`
	// RootID identifies the snapshot's contents: two snapshots of
	// unchanged data share it.
	RootID string `json:"rootID"`
	// Retention holds the reasons the effective retention policy of the
	// queried source keeps the snapshot, such as "latest-1" or "daily-2..3",
	// computed live. Pins are the snapshot's pins. Neither is ever null.
	Retention []string `json:"retention"`
	Pins      []string `json:"pins"`
}

// errSnapshotsHost is returned by ListSnapshots for an empty host.
var errSnapshotsHost = errors.New("listing snapshots needs a host: kopia matches snapshots by host and returns none without one")

// ListSnapshots reads GET /api/v1/snapshots for the snapshots of host,
// narrowed to userName and path when they are not empty. It needs a
// repository connected.
//
// Kopia deduplicates the list by root object id unless all is set: each
// group of snapshots with identical contents is reported once, as its oldest
// member, carrying the merged retention reasons and pins of the group. With
// all, every snapshot is listed, but the oldest of each group still carries
// those merged values, because the server computes the deduplicated count on
// the same objects (spec: repo.md §8.3). Retention reasons only make sense
// for one source, so pass a user and a path when they matter.
func (c *Client) ListSnapshots(ctx context.Context, host, userName, path string, all bool) (*SnapshotsResponse, error) {
	// Every snapshot manifest is labelled with its host, and Kopia always
	// filters on that label, so without a host the answer is an empty list
	// rather than every snapshot (snapshot/manager.go:64-79).
	if host == "" {
		return nil, errSnapshotsHost
	}
	query := url.Values{"host": {host}}
	if userName != "" {
		query.Set("userName", userName)
	}
	if path != "" {
		query.Set("path", path)
	}
	// Any non-empty value, "false" and "0" included, means all; the only way
	// to ask for the deduplicated list is to leave the parameter out.
	if all {
		query.Set("all", "1")
	}

	var resp SnapshotsResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/snapshots", query, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
