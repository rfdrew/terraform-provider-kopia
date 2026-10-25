package provider

import (
	"net/http"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// sourcesDataSourceReleaseJSON is GET /api/v1/sources from Kopia 0.23.1,
// indented as the releases write it: a remote source with a last snapshot
// and a next snapshot time in a server running two hours ahead of UTC, and a
// local source that is uploading.
const sourcesDataSourceReleaseJSON = `{
  "localUsername": "root",
  "localHost": "kopia-acc",
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
        "noParentTimeOfDay": true,
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
        "description": "nightly",
        "startTime": "2026-10-18T03:00:00.481516234Z",
        "endTime": "2026-10-18T03:00:07.12Z",
        "stats": {
          "totalSize": 1048576,
          "excludedTotalSize": 4096,
          "fileCount": 12,
          "cachedFiles": 10,
          "nonCachedFiles": 2,
          "dirCount": 3,
          "excludedFileCount": 1,
          "excludedDirCount": 1,
          "ignoredErrorCount": 2,
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
        "tags": {
          "type": "snapshot"
        },
        "pins": [
          "keep"
        ]
      },
      "nextSnapshotTime": "2026-10-19T05:30:00.25+02:00"
    },
    {
      "source": {
        "host": "kopia-acc",
        "userName": "root",
        "path": "/srv"
      },
      "status": "UPLOADING",
      "schedule": {
        "intervalSeconds": 3600,
        "runMissed": false
      },
      "upload": {
        "cachedBytes": 1024,
        "hashedBytes": 52428800,
        "uploadedBytes": 0,
        "estimatedBytes": 104857600,
        "cachedFiles": 1,
        "hashedFiles": 5,
        "excludedFiles": 0,
        "excludedDirs": 0,
        "errors": 1,
        "ignoredErrors": 2,
        "estimatedFiles": 10,
        "directory": "sub",
        "lastErrorPath": "sub/locked",
        "lastError": "permission denied"
      },
      "currentTask": "1a"
    }
  ]
}
`

func TestSourcesDataSourceSchemaIsValid(t *testing.T) {
	requireValidDataSourceSchema(t, &sourcesDataSource{})
}

func TestSourcesDataSourceReadsTheReleaseShape(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/sources", http.StatusOK, sourcesDataSourceReleaseJSON)
	d := &sourcesDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	if got := fake.query(t, "GET /api/v1/sources"); len(got) != 0 {
		t.Errorf("query = %v, want no filter", got)
	}

	want := dataSourceValue(t, d, map[string]any{
		"local_host":     "kopia-acc",
		"local_username": "root",
		"multi_user":     true,
		"sources": []any{
			map[string]any{
				"target":    "docker@host1:/data/app",
				"host":      "host1",
				"user_name": "docker",
				"path":      "/data/app",
				"status":    "REMOTE",
				"schedule": map[string]any{
					"time_of_day":           []any{map[string]any{"hour": 3, "minute": 30}},
					"no_parent_time_of_day": true,
					"manual":                false,
					"cron":                  []any{"0 3 * * *"},
					"run_missed":            true,
				},
				// Kopia writes it in the server's time zone; the provider
				// reports every time in UTC.
				"next_snapshot_time": "2026-10-19T03:30:00.25Z",
				"last_snapshot": map[string]any{
					"id":          "5f1c0e4b9a3d4e7f8a2b6c1d0e9f8a7b",
					"description": "nightly",
					"start_time":  "2026-10-18T03:00:00.481516234Z",
					"end_time":    "2026-10-18T03:00:07.12Z",
					"root_id":     "k9f1e2d3c4b5a69788766554433221100",
					"pins":        []any{"keep"},
					"stats": map[string]any{
						"total_size":          1048576,
						"excluded_total_size": 4096,
						"file_count":          12,
						"cached_files":        10,
						"non_cached_files":    2,
						"dir_count":           3,
						"excluded_file_count": 1,
						"excluded_dir_count":  1,
						"ignored_error_count": 2,
						"error_count":         1,
					},
				},
			},
			map[string]any{
				"target":    "root@kopia-acc:/srv",
				"host":      "kopia-acc",
				"user_name": "root",
				"path":      "/srv",
				"status":    "UPLOADING",
				"schedule": map[string]any{
					"interval_seconds":      3600,
					"time_of_day":           []any{},
					"no_parent_time_of_day": false,
					"manual":                false,
					"cron":                  []any{},
					"run_missed":            false,
				},
				"upload": map[string]any{
					"cached_bytes":    1024,
					"hashed_bytes":    52428800,
					"estimated_bytes": 104857600,
					"cached_files":    1,
					"hashed_files":    5,
					"estimated_files": 10,
					"errors":          1,
					"ignored_errors":  2,
					"directory":       "sub",
					"last_error_path": "sub/locked",
					"last_error":      "permission denied",
				},
				"current_task": "1a",
			},
		},
	})
	if !resp.State.Raw.Equal(want) {
		diffs, _ := resp.State.Raw.Diff(want)
		t.Errorf("state differs from the expected one: %v", diffs)
	}
}

// master writes compact JSON, and Kopia 0.22.0 leaves out a last snapshot's
// stats when every counter is 0. A source whose first refresh failed has an
// empty schedule, and a snapshot without a root entry has no root id.
func TestSourcesDataSourceReadsOtherShapes(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/sources", http.StatusOK, `{"localUsername":"backup","localHost":"nas","multiUser":false,"sources":[`+
		`{"source":{"host":"nas","userName":"backup","path":"/empty"},"status":"FAILED","schedule":{},`+
		`"lastSnapshot":{"id":"0d8b6f2e4c1a49e7b3f5a7c9d1e3f5a7","source":{"host":"nas","userName":"backup","path":"/empty"},`+
		`"description":"","startTime":"2026-10-16T03:00:00Z","endTime":"2026-10-16T03:00:01Z","incomplete":"canceled",`+
		`"rootEntry":null}}]}`+"\n")
	d := &sourcesDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	state := resp.State.Raw
	if boolAt(t, state, "multi_user") || stringAt(t, state, "local_host") != "nas" {
		t.Errorf("state = %s", state)
	}
	if got := stringAt(t, state, "sources", 0, "status"); got != "FAILED" {
		t.Errorf("status = %q", got)
	}
	for _, attribute := range []string{"interval_seconds", "run_missed"} {
		if !isNullAt(t, state, "sources", 0, "schedule", attribute) {
			t.Errorf("schedule.%s = %s, want null", attribute, valueAt(t, state, "sources", 0, "schedule", attribute))
		}
	}
	if got := stringsAt(t, state, "sources", 0, "schedule", "cron"); len(got) != 0 {
		t.Errorf("schedule.cron = %v, want empty", got)
	}
	last := []any{"sources", 0, "last_snapshot"}
	if got := stringAt(t, state, append(last, "incomplete_reason")...); got != "canceled" {
		t.Errorf("incomplete_reason = %q", got)
	}
	if !isNullAt(t, state, append(last, "root_id")...) {
		t.Errorf("root_id = %s, want null without a root entry", valueAt(t, state, append(last, "root_id")...))
	}
	if got := int64At(t, state, append(last, "stats", "total_size")...); got != 0 {
		t.Errorf("stats.total_size = %d, want 0", got)
	}
	if got := stringsAt(t, state, append(last, "pins")...); len(got) != 0 {
		t.Errorf("pins = %v, want empty", got)
	}
	for _, attribute := range []string{"next_snapshot_time", "upload", "current_task"} {
		if !isNullAt(t, state, "sources", 0, attribute) {
			t.Errorf("%s = %s, want null", attribute, valueAt(t, state, "sources", 0, attribute))
		}
	}
}

// The filters are sent only when set, the user as userName: Kopia ignores
// username, and would return every user's sources. The configuration's
// filters are kept in state as they were given.
func TestSourcesDataSourceSendsTheFilters(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/sources", http.StatusOK, `{"localUsername":"root","localHost":"kopia-acc","multiUser":true,"sources":[]}`)
	d := &sourcesDataSource{client: fake.client(t)}

	config := dataSourceValue(t, d, map[string]any{"host": "host1", "user_name": "docker", "path": "/data/app"})
	resp := readDataSource(t, d, config)
	requireNoErrors(t, resp.Diagnostics)
	if got := fake.query(t, "GET /api/v1/sources").Encode(); got != "host=host1&path=%2Fdata%2Fapp&userName=docker" {
		t.Errorf("query = %s", got)
	}
	if stringAt(t, resp.State.Raw, "host") != "host1" || stringAt(t, resp.State.Raw, "user_name") != "docker" ||
		stringAt(t, resp.State.Raw, "path") != "/data/app" {
		t.Errorf("filters in state = %s", resp.State.Raw)
	}
	var listed []tftypes.Value
	if err := knownAt(t, resp.State.Raw, "sources").As(&listed); err != nil || len(listed) != 0 {
		t.Errorf("sources = %v (%v), want an empty list", listed, err)
	}

	resp = readDataSource(t, d, dataSourceValue(t, d, map[string]any{"user_name": "docker"}))
	requireNoErrors(t, resp.Diagnostics)
	if got := fake.query(t, "GET /api/v1/sources").Encode(); got != "userName=docker" {
		t.Errorf("query = %s, want the user alone", got)
	}
}

// An empty filter would match everything, which leaving it out already says.
func TestSourcesDataSourceRejectsEmptyFilters(t *testing.T) {
	h := newProtocolHarness(t, newFakeKopia(), withDataSources(NewSourcesDataSource))
	for _, attribute := range []string{"host", "user_name", "path"} {
		diags := h.validateDataSource(t, "kopia_sources", h.dataSourceValue(t, "kopia_sources", map[string]any{attribute: ""}))
		requireProtocolErrorContaining(t, diags, "length must be at least 1")
	}
}

func TestSourcesDataSourceWithoutARepository(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/sources", http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected")
	d := &sourcesDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireErrorContaining(t, resp.Diagnostics, "Unable to list Kopia sources")
	requireErrorContaining(t, resp.Diagnostics, "has no repository connected")
}

// --- acceptance --------------------------------------------------------------

// TestAccSourcesDataSource lists the test server's sources. A fresh server
// has none, and other tests may add some through policies, so only the
// server's own identity is pinned down.
func TestAccSourcesDataSource(t *testing.T) {
	checks := []acc.TestCheckFunc{
		acc.TestCheckResourceAttr("data.kopia_sources.all", "multi_user", "true"),
		acc.TestCheckResourceAttrSet("data.kopia_sources.all", "local_host"),
		acc.TestCheckResourceAttrSet("data.kopia_sources.all", "local_username"),
		acc.TestCheckResourceAttrSet("data.kopia_sources.all", "sources.#"),
		// No source has this host, so the filter leaves none.
		acc.TestCheckResourceAttr("data.kopia_sources.none", "sources.#", "0"),
	}
	if host := os.Getenv(envAccSourceHost); host != "" {
		checks = append(checks, acc.TestCheckResourceAttr("data.kopia_sources.all", "local_host", host))
	}
	if user := os.Getenv(envAccSourceUser); user != "" {
		checks = append(checks, acc.TestCheckResourceAttr("data.kopia_sources.all", "local_username", user))
	}

	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
data "kopia_sources" "all" {}

data "kopia_sources" "none" {
  host = "tfacc-no-such-host"
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(checks...),
			},
		},
	})
}
