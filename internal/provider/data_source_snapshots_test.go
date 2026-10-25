package provider

import (
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// snapshotsDataSourceReleaseJSON is GET /api/v1/snapshots?...&all=1 from
// Kopia 0.23.1, indented as the releases write it (spec: repo.md §8.3): two
// complete snapshots of the same root, the oldest carrying the merged
// retention reasons and pins of both, and a newer checkpoint with failed
// entries.
const snapshotsDataSourceReleaseJSON = `{
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
      "id": "a1b2c3d4e5f60718293a4b5c6d7e8f90",
      "description": "before upgrade",
      "startTime": "2026-10-17T03:00:00.2Z",
      "endTime": "2026-10-17T03:00:04.9Z",
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
        "latest-1",
        "hourly-1",
        "daily-1",
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
      "description": "",
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

// snapshotsDataSourceSummary is the summary of the first two snapshots of
// snapshotsDataSourceReleaseJSON, as the data source reports it.
func snapshotsDataSourceSummary() map[string]any {
	return map[string]any{
		"size":               1048576,
		"files":              12,
		"symlinks":           0,
		"dirs":               3,
		"max_time":           "2026-10-15T22:10:00Z",
		"num_failed":         0,
		"num_ignored_errors": 0,
		"errors":             []any{},
	}
}

func TestSnapshotsDataSourceSchemaIsValid(t *testing.T) {
	requireValidDataSourceSchema(t, &snapshotsDataSource{})
}

// Kopia files every snapshot under its host and lists nothing without one,
// so host is required and may not be empty.
func TestSnapshotsDataSourceRequiresAHost(t *testing.T) {
	if !dataSourceSchema(t, &snapshotsDataSource{}).Attributes["host"].IsRequired() {
		t.Error("host is not required")
	}
	h := newProtocolHarness(t, newFakeKopia(), withDataSources(NewSnapshotsDataSource))
	for _, attribute := range []string{"host", "user_name", "path"} {
		config := map[string]any{"host": "host1"}
		config[attribute] = ""
		diags := h.validateDataSource(t, "kopia_snapshots", h.dataSourceValue(t, "kopia_snapshots", config))
		requireProtocolErrorContaining(t, diags, "length must be at least 1")
	}
}

func TestSnapshotsDataSourceReadsTheReleaseShape(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/snapshots", http.StatusOK, snapshotsDataSourceReleaseJSON)
	d := &snapshotsDataSource{client: fake.client(t)}

	config := map[string]any{"host": "host1", "user_name": "docker", "path": "/data/app", "all": true}
	resp := readDataSource(t, d, dataSourceValue(t, d, config))
	requireNoErrors(t, resp.Diagnostics)
	if got := fake.query(t, "GET /api/v1/snapshots").Encode(); got != "all=1&host=host1&path=%2Fdata%2Fapp&userName=docker" {
		t.Errorf("query = %s", got)
	}

	config["unfiltered_count"] = 3
	config["unique_count"] = 2
	config["snapshots"] = []any{
		map[string]any{
			"id":          "0d8b6f2e4c1a49e7b3f5a7c9d1e3f5a7",
			"description": "",
			"start_time":  "2026-10-16T03:00:00.123456789Z",
			"end_time":    "2026-10-16T03:00:05.5Z",
			"root_id":     "k9f1e2d3c4b5a69788766554433221100",
			"retention":   []any{"latest-1..2", "hourly-1..2", "daily-1..2", "weekly-1", "monthly-1", "annual-1"},
			"pins":        []any{"keep"},
			"summary":     snapshotsDataSourceSummary(),
		},
		map[string]any{
			"id":          "a1b2c3d4e5f60718293a4b5c6d7e8f90",
			"description": "before upgrade",
			"start_time":  "2026-10-17T03:00:00.2Z",
			"end_time":    "2026-10-17T03:00:04.9Z",
			"root_id":     "k9f1e2d3c4b5a69788766554433221100",
			"retention":   []any{"latest-1", "hourly-1", "daily-1", "weekly-1", "monthly-1", "annual-1"},
			"pins":        []any{"keep"},
			"summary":     snapshotsDataSourceSummary(),
		},
		map[string]any{
			"id":                "5f1c0e4b9a3d4e7f8a2b6c1d0e9f8a7b",
			"description":       "",
			"start_time":        "2026-10-18T03:00:00.481516234Z",
			"end_time":          "2026-10-18T03:00:07.12Z",
			"incomplete_reason": "checkpoint",
			"root_id":           "kfe0d1c2b3a4958677869504132231405",
			"retention":         []any{"incomplete"},
			"pins":              []any{},
			"summary": map[string]any{
				"size":               524288,
				"files":              6,
				"symlinks":           1,
				"dirs":               2,
				"max_time":           "2026-10-17T22:10:00Z",
				"incomplete_reason":  "checkpoint",
				"num_failed":         1,
				"num_ignored_errors": 2,
				"errors":             []any{map[string]any{"path": "secret/key", "error": "permission denied"}},
			},
		},
	}
	if want := dataSourceValue(t, d, config); !resp.State.Raw.Equal(want) {
		diffs, _ := resp.State.Raw.Diff(want)
		t.Errorf("state differs from the expected one: %v", diffs)
	}
}

// master writes compact JSON. A snapshot without a root entry has an empty
// rootID and a null summary.
func TestSnapshotsDataSourceReadsTheCompactShapeOfMaster(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/snapshots", http.StatusOK, `{"snapshots":[{"id":"0d8b6f2e4c1a49e7b3f5a7c9d1e3f5a7",`+
		`"description":"","startTime":"2026-10-16T03:00:00Z","endTime":"2026-10-16T03:00:00.000000001Z",`+
		`"incomplete":"canceled","summary":null,"rootID":"","retention":[],"pins":[]}],"unfilteredCount":1,"uniqueCount":1}`+"\n")
	d := &snapshotsDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{"host": "host1"}))
	requireNoErrors(t, resp.Diagnostics)
	state := resp.State.Raw
	if !isNullAt(t, state, "snapshots", 0, "summary") || !isNullAt(t, state, "snapshots", 0, "root_id") {
		t.Errorf("snapshot = %s, want no summary and no root id", valueAt(t, state, "snapshots", 0))
	}
	if got := stringAt(t, state, "snapshots", 0, "end_time"); got != "2026-10-16T03:00:00.000000001Z" {
		t.Errorf("end_time = %q, want every fractional digit kept", got)
	}
	if got := stringsAt(t, state, "snapshots", 0, "retention"); got == nil || len(got) != 0 {
		t.Errorf("retention = %#v, want an empty list", got)
	}
	if int64At(t, state, "unfiltered_count") != 1 || int64At(t, state, "unique_count") != 1 {
		t.Errorf("counts = %s, %s", valueAt(t, state, "unfiltered_count"), valueAt(t, state, "unique_count"))
	}
	if !isNullAt(t, state, "all") || !isNullAt(t, state, "user_name") {
		t.Errorf("state = %s, want the unset arguments left null", state)
	}
}

// Any non-empty all, "false" and "0" included, makes Kopia list every
// snapshot, so the parameter is sent as all=1 for true and left out
// otherwise.
func TestSnapshotsDataSourceSendsAllOnlyWhenTrue(t *testing.T) {
	for _, tc := range []struct {
		all   any
		query string
	}{
		{nil, "host=host1"},
		{false, "host=host1"},
		{true, "all=1&host=host1"},
	} {
		t.Run(fmt.Sprint(tc.all), func(t *testing.T) {
			fake := newFakeKopia()
			fake.reply("GET /api/v1/snapshots", http.StatusOK, `{"snapshots":[],"unfilteredCount":0,"uniqueCount":0}`+"\n")
			d := &snapshotsDataSource{client: fake.client(t)}

			resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{"host": "host1", "all": tc.all}))
			requireNoErrors(t, resp.Diagnostics)
			if got := fake.query(t, "GET /api/v1/snapshots").Encode(); got != tc.query {
				t.Errorf("query = %s, want %s", got, tc.query)
			}
			var listed []tftypes.Value
			if err := knownAt(t, resp.State.Raw, "snapshots").As(&listed); err != nil || len(listed) != 0 {
				t.Errorf("snapshots = %v (%v), want an empty list", listed, err)
			}
		})
	}
}

func TestSnapshotsDataSourceReportsErrors(t *testing.T) {
	for _, tc := range []struct {
		status        int
		code, message string
		want          string
	}{
		{http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected", "has no repository connected"},
		{http.StatusInternalServerError, kopiaclient.CodeInternal,
			"internal server error: unable to find snapshot manifests: index unavailable", "index unavailable"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fake := newFakeKopia()
			fake.replyError("GET /api/v1/snapshots", tc.status, tc.code, tc.message)
			d := &snapshotsDataSource{client: fake.client(t)}

			resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{"host": "host1"}))
			requireErrorContaining(t, resp.Diagnostics, "Unable to list Kopia snapshots")
			requireErrorContaining(t, resp.Diagnostics, tc.want)
		})
	}
}

// --- acceptance --------------------------------------------------------------

// TestAccSnapshotsDataSource lists the snapshots of the test server's own
// host, both deduplicated and in full. The test server takes no snapshots of
// its own, so only the shape of the answer is checked.
func TestAccSnapshotsDataSource(t *testing.T) {
	host := os.Getenv(envAccSourceHost)
	if host == "" {
		host = "kopia-acc"
	}

	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []acc.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
data "kopia_snapshots" "unique" {
  host = %[1]q
}

data "kopia_snapshots" "all" {
  host = %[1]q
  all  = true
}

data "kopia_snapshots" "none" {
  host = "tfacc-no-such-host"
}
`, host),
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttrSet("data.kopia_snapshots.unique", "snapshots.#"),
					acc.TestCheckResourceAttrSet("data.kopia_snapshots.unique", "unfiltered_count"),
					acc.TestCheckResourceAttrSet("data.kopia_snapshots.unique", "unique_count"),
					acc.TestCheckResourceAttrPair("data.kopia_snapshots.all", "unfiltered_count",
						"data.kopia_snapshots.all", "snapshots.#"),
					acc.TestCheckResourceAttr("data.kopia_snapshots.none", "snapshots.#", "0"),
					acc.TestCheckResourceAttr("data.kopia_snapshots.none", "unfiltered_count", "0"),
				),
			},
		},
	})
}
