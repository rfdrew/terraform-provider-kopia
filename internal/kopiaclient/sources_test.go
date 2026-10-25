package kopiaclient

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// sourcesReleaseJSON is GET /api/v1/sources from Kopia 0.23.1, in the
// release's indented formatting: a remote source with a last snapshot and a
// next snapshot time in another time zone, and a local one that is uploading.
const sourcesReleaseJSON = `{
  "localUsername": "root",
  "localHost": "kopia",
  "multiUser": true,
  "sources": [
    {
      "source": {
        "host": "host1",
        "userName": "docker",
        "path": "/data/app"
      },
      "status": "REMOTE",
      "schedule": {
        "timeOfDay": [
          {
            "hour": 3,
            "min": 30
          }
        ],
        "cron": [
          "0 3 * * *"
        ],
        "runMissed": true
      },
      "lastSnapshot": {
        "id": "5f1c0e4b9a3d4e7f8a2b6c1d0e9f8a7b",
        "source": {
          "host": "host1",
          "userName": "docker",
          "path": "/data/app"
        },
        "description": "",
        "startTime": "2026-10-18T03:00:00.481516234Z",
        "endTime": "2026-10-18T03:00:07.12Z",
        "stats": {
          "totalSize": 1048576,
          "excludedTotalSize": 0,
          "fileCount": 12,
          "cachedFiles": 10,
          "nonCachedFiles": 2,
          "dirCount": 3,
          "excludedFileCount": 0,
          "excludedDirCount": 0,
          "ignoredErrorCount": 0,
          "errorCount": 1
        },
        "rootEntry": {
          "name": "app",
          "type": "d",
          "mode": "0755",
          "mtime": "2026-10-17T22:10:00Z",
          "obj": "k9f1e2d3c4b5a69788766554433221100",
          "summ": {
            "size": 1048576,
            "files": 12,
            "symlinks": 0,
            "dirs": 3,
            "maxTime": "2026-10-17T22:10:00Z",
            "numFailed": 0
          }
        },
        "pins": [
          "keep"
        ]
      },
      "nextSnapshotTime": "2026-10-19T05:30:00+02:00"
    },
    {
      "source": {
        "host": "kopia",
        "userName": "root",
        "path": "/srv"
      },
      "status": "UPLOADING",
      "schedule": {
        "intervalSeconds": 3600,
        "runMissed": false
      },
      "upload": {
        "cachedBytes": 0,
        "hashedBytes": 52428800,
        "uploadedBytes": 0,
        "estimatedBytes": 104857600,
        "cachedFiles": 0,
        "hashedFiles": 5,
        "excludedFiles": 0,
        "excludedDirs": 0,
        "errors": 0,
        "ignoredErrors": 0,
        "estimatedFiles": 10,
        "directory": "sub",
        "lastErrorPath": "",
        "lastError": ""
      },
      "currentTask": "1a"
    }
  ]
}
`

func TestListSourcesDecodesTheReleaseShape(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/sources", http.StatusOK, sourcesReleaseJSON)

	resp, err := fake.client(t).ListSources(context.Background(), SourcesFilter{})
	if err != nil {
		t.Fatalf("ListSources() error = %v", err)
	}
	if got := fake.query(t, "GET /api/v1/sources"); len(got) != 0 {
		t.Errorf("query = %v, want none without a filter", got)
	}
	if resp.LocalUsername != "root" || resp.LocalHost != "kopia" || !resp.MultiUser || len(resp.Sources) != 2 {
		t.Fatalf("ListSources() = %+v", *resp)
	}

	remote := resp.Sources[0]
	if remote.Source != (SourceInfo{Host: "host1", UserName: "docker", Path: "/data/app"}) || remote.Status != "REMOTE" {
		t.Errorf("first source = %+v", remote)
	}
	if s := remote.Schedule; len(s.TimeOfDay) != 1 || s.TimeOfDay[0] != (SourceTimeOfDay{Hour: 3, Minute: 30}) ||
		len(s.Cron) != 1 || s.RunMissed == nil || !*s.RunMissed || s.IntervalSeconds != 0 {
		t.Errorf("schedule = %+v", s)
	}
	want := time.Date(2026, 10, 19, 3, 30, 0, 0, time.UTC)
	if remote.NextSnapshotTime == nil || !remote.NextSnapshotTime.Equal(want) {
		t.Errorf("next snapshot time = %v, want %v", remote.NextSnapshotTime, want)
	}

	last := remote.LastSnapshot
	if last == nil {
		t.Fatal("no last snapshot")
	}
	if last.ID != "5f1c0e4b9a3d4e7f8a2b6c1d0e9f8a7b" || last.StartTime.Nanosecond() != 481516234 ||
		!last.EndTime.Equal(time.Date(2026, 10, 18, 3, 0, 7, 120000000, time.UTC)) {
		t.Errorf("last snapshot = %+v", *last)
	}
	if last.Stats.TotalSize != 1048576 || last.Stats.FileCount != 12 || last.Stats.ErrorCount != 1 {
		t.Errorf("stats = %+v", last.Stats)
	}
	if last.RootEntry == nil || last.RootEntry.ObjectID != "k9f1e2d3c4b5a69788766554433221100" || last.RootEntry.Mode != "0755" ||
		last.RootEntry.Summary == nil || last.RootEntry.Summary.Files != 12 {
		t.Errorf("root entry = %+v", last.RootEntry)
	}
	if len(last.Pins) != 1 || last.Pins[0] != "keep" {
		t.Errorf("pins = %v", last.Pins)
	}

	uploading := resp.Sources[1]
	if uploading.Upload == nil || uploading.Upload.HashedBytes != 52428800 || uploading.Upload.EstimatedFiles != 10 ||
		uploading.Upload.Directory != "sub" || uploading.CurrentTask != "1a" {
		t.Errorf("uploading source = %+v, upload %+v", uploading, uploading.Upload)
	}
	if uploading.LastSnapshot != nil || uploading.NextSnapshotTime != nil {
		t.Errorf("uploading source has a last snapshot or a next time: %+v", uploading)
	}
	if r := uploading.Schedule.RunMissed; r == nil || *r {
		t.Errorf("runMissed = %v, want an explicit false", r)
	}
}

// Kopia 0.22.0 leaves out a last snapshot's stats when every counter is 0, and
// master writes compact JSON; both decode.
func TestListSourcesDecodesOtherShapes(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/sources", http.StatusOK, `{"localUsername":"root","localHost":"kopia","multiUser":false,"sources":[`+
		`{"source":{"host":"kopia","userName":"root","path":"/empty"},"status":"IDLE","schedule":{},`+
		`"lastSnapshot":{"id":"0d8b6f2e4c1a49e7b3f5a7c9d1e3f5a7","source":{"host":"kopia","userName":"root","path":"/empty"},`+
		`"description":"","startTime":"2026-10-16T03:00:00Z","endTime":"2026-10-16T03:00:01Z","incomplete":"canceled","rootEntry":null}}]}`+"\n")

	resp, err := fake.client(t).ListSources(context.Background(), SourcesFilter{})
	if err != nil {
		t.Fatalf("ListSources() error = %v", err)
	}
	if resp.MultiUser || len(resp.Sources) != 1 {
		t.Fatalf("ListSources() = %+v", *resp)
	}
	source := resp.Sources[0]
	if source.Schedule.RunMissed != nil || source.Schedule.TimeOfDay != nil {
		t.Errorf("schedule = %+v, want empty", source.Schedule)
	}
	if source.LastSnapshot == nil || source.LastSnapshot.Stats != (SnapshotStats{}) ||
		source.LastSnapshot.RootEntry != nil || source.LastSnapshot.IncompleteReason != "canceled" {
		t.Errorf("last snapshot = %+v", source.LastSnapshot)
	}
}

// The user filter is userName in camelCase: Kopia ignores username, and would
// return every user's sources.
func TestListSourcesSendsOnlyTheFiltersThatAreSet(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/sources", http.StatusOK, `{"localUsername":"root","localHost":"kopia","multiUser":true,"sources":[]}`)
	c := fake.client(t)

	if _, err := c.ListSources(context.Background(), SourcesFilter{Host: "host1", UserName: "docker", Path: "/data/app"}); err != nil {
		t.Fatalf("ListSources() error = %v", err)
	}
	if got := fake.query(t, "GET /api/v1/sources").Encode(); got != "host=host1&path=%2Fdata%2Fapp&userName=docker" {
		t.Errorf("query = %s", got)
	}

	if _, err := c.ListSources(context.Background(), SourcesFilter{UserName: "docker"}); err != nil {
		t.Fatalf("ListSources() error = %v", err)
	}
	if got := fake.query(t, "GET /api/v1/sources").Encode(); got != "userName=docker" {
		t.Errorf("query = %s", got)
	}
}

func TestListSourcesNeedsARepository(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/sources", http.StatusBadRequest, CodeNotConnected, "not connected")

	if _, err := fake.client(t).ListSources(context.Background(), SourcesFilter{}); !IsNotConnected(err) {
		t.Fatalf("ListSources() error = %v, want NOT_CONNECTED", err)
	}
}

// snapshotsReleaseJSON is GET /api/v1/snapshots?...&all=1 from Kopia 0.23.1
// (spec: repo.md §8.3): two complete snapshots of the same root, the oldest
// carrying the merged reasons and pins of both, and a newer checkpoint.
const snapshotsReleaseJSON = `{
  "snapshots": [
    {
      "id": "0d8b6f2e4c1a49e7b3f5a7c9d1e3f5a7",
      "description": "",
      "startTime": "2026-10-16T03:00:00.123456789Z",
      "endTime": "2026-10-16T03:00:05.5Z",
      "summary": {
        "size": 1048576,
        "files": 12,
        "symlinks": 0,
        "dirs": 3,
        "maxTime": "2026-10-15T22:10:00Z",
        "numFailed": 0
      },
      "rootID": "k9f1e2d3c4b5a69788766554433221100",
      "retention": [
        "latest-1..2",
        "hourly-1..2",
        "daily-1..2",
        "weekly-1",
        "monthly-1",
        "annual-1"
      ],
      "pins": [
        "keep"
      ]
    },
    {
      "id": "5f1c0e4b9a3d4e7f8a2b6c1d0e9f8a7b",
      "description": "before upgrade",
      "startTime": "2026-10-18T03:00:00.481516234Z",
      "endTime": "2026-10-18T03:00:07.12Z",
      "incomplete": "checkpoint",
      "summary": {
        "size": 524288,
        "files": 6,
        "symlinks": 1,
        "dirs": 2,
        "maxTime": "2026-10-17T22:10:00Z",
        "incomplete": "checkpoint",
        "numFailed": 1,
        "numIgnoredErrors": 2,
        "errors": [
          {
            "path": "secret/key",
            "error": "permission denied"
          }
        ]
      },
      "rootID": "kfe0d1c2b3a4958677869504132231405",
      "retention": [
        "incomplete"
      ],
      "pins": []
    }
  ],
  "unfilteredCount": 3,
  "uniqueCount": 2
}
`

func TestListSnapshotsDecodesTheReleaseShape(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/snapshots", http.StatusOK, snapshotsReleaseJSON)

	resp, err := fake.client(t).ListSnapshots(context.Background(), "host1", "docker", "/data/app", true)
	if err != nil {
		t.Fatalf("ListSnapshots() error = %v", err)
	}
	if got := fake.query(t, "GET /api/v1/snapshots").Encode(); got != "all=1&host=host1&path=%2Fdata%2Fapp&userName=docker" {
		t.Errorf("query = %s", got)
	}
	if resp.UnfilteredCount != 3 || resp.UniqueCount != 2 || len(resp.Snapshots) != 2 {
		t.Fatalf("ListSnapshots() = %+v", *resp)
	}

	first := resp.Snapshots[0]
	if first.ID != "0d8b6f2e4c1a49e7b3f5a7c9d1e3f5a7" || first.RootID != "k9f1e2d3c4b5a69788766554433221100" ||
		len(first.Retention) != 6 || first.Retention[0] != "latest-1..2" || len(first.Pins) != 1 ||
		first.StartTime.Nanosecond() != 123456789 || first.Summary == nil || first.Summary.Size != 1048576 {
		t.Errorf("first snapshot = %+v", first)
	}

	checkpoint := resp.Snapshots[1]
	if checkpoint.IncompleteReason != "checkpoint" || checkpoint.Description != "before upgrade" || checkpoint.Pins == nil ||
		len(checkpoint.Pins) != 0 {
		t.Errorf("checkpoint = %+v", checkpoint)
	}
	summary := checkpoint.Summary
	if summary == nil || summary.Symlinks != 1 || summary.NumFailed != 1 || summary.NumIgnoredErrors != 2 ||
		summary.IncompleteReason != "checkpoint" || len(summary.Errors) != 1 ||
		summary.Errors[0] != (DirectorySummaryError{Path: "secret/key", Error: "permission denied"}) {
		t.Errorf("checkpoint summary = %+v", summary)
	}
}

// Any non-empty all, "false" and "0" included, makes Kopia list every
// snapshot, so the parameter is only ever sent as all=1, and only for true.
func TestListSnapshotsSendsAllOnlyWhenAsked(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/snapshots", http.StatusOK, `{"snapshots":[],"unfilteredCount":0,"uniqueCount":0}`+"\n")

	resp, err := fake.client(t).ListSnapshots(context.Background(), "host1", "", "", false)
	if err != nil {
		t.Fatalf("ListSnapshots() error = %v", err)
	}
	if got := fake.query(t, "GET /api/v1/snapshots").Encode(); got != "host=host1" {
		t.Errorf("query = %s, want only the host", got)
	}
	if resp.Snapshots == nil || len(resp.Snapshots) != 0 {
		t.Errorf("snapshots = %#v, want an empty list", resp.Snapshots)
	}
}

// Without a host Kopia answers an empty list for any repository, so the call
// is refused before it is sent.
func TestListSnapshotsRequiresAHost(t *testing.T) {
	fake := newFakeKopia()
	_, err := fake.client(t).ListSnapshots(context.Background(), "", "docker", "/data", false)
	if !errors.Is(err, errSnapshotsHost) {
		t.Fatalf("ListSnapshots() error = %v, want the missing-host error", err)
	}
	if len(fake.routes()) != 0 {
		t.Errorf("calls = %v, want none", fake.routes())
	}
}
