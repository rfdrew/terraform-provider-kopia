package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// effectivePolicyTestResolveJSON is POST /api/v1/policy/resolve for
// docker@host1 on Kopia 0.22.3 and later (spec: policy.md §3.6): the stored
// policy sets keepDaily and a cron entry, the global policy is the default
// one, and every definition key is present, the zero target included.
const effectivePolicyTestResolveJSON = `{
  "effective": {"retention":{"keepLatest":10,"keepHourly":48,"keepDaily":14,"keepWeekly":4,"keepMonthly":24,"keepAnnual":3,"ignoreIdenticalSnapshots":false},"files":{"ignoreDotFiles":[".kopiaignore"]},"errorHandling":{"ignoreFileErrors":false,"ignoreDirectoryErrors":false,"ignoreUnknownTypes":true},"scheduling":{"cron":["0 3 * * *"],"runMissed":true},"compression":{"compressorName":"none"},"metadataCompression":{"compressorName":"zstd-fastest"},"splitter":{},"actions":{},"osSnapshots":{"volumeShadowCopy":{"enable":0}},"logging":{"directories":{"snapshotted":5,"ignored":5},"entries":{"snapshotted":0,"ignored":5,"cacheHit":0,"cacheMiss":0}},"upload":{"maxParallelSnapshots":1,"parallelUploadAboveSize":2147483648}},
  "definition": {"retention":{"keepLatest":{"host":"","userName":"","path":""},"keepHourly":{"host":"","userName":"","path":""},"keepDaily":{"host":"host1","userName":"docker","path":""},"keepWeekly":{"host":"","userName":"","path":""},"keepMonthly":{"host":"","userName":"","path":""},"keepAnnual":{"host":"","userName":"","path":""},"ignoreIdenticalSnapshots":{"host":"","userName":"","path":""}},"files":{"ignore":{"host":"","userName":"","path":""},"noParentIgnore":{"host":"","userName":"","path":""},"ignoreDotFiles":{"host":"","userName":"","path":""},"noParentDotFiles":{"host":"","userName":"","path":""},"ignoreCacheDirs":{"host":"","userName":"","path":""},"maxFileSize":{"host":"","userName":"","path":""},"oneFileSystem":{"host":"","userName":"","path":""}},"errorHandling":{"ignoreFileErrors":{"host":"","userName":"","path":""},"ignoreDirectoryErrors":{"host":"","userName":"","path":""},"ignoreUnknownTypes":{"host":"","userName":"","path":""}},"scheduling":{"intervalSeconds":{"host":"","userName":"","path":""},"timeOfDay":{"host":"","userName":"","path":""},"cron":{"host":"host1","userName":"docker","path":""},"manual":{"host":"","userName":"","path":""},"runMissed":{"host":"","userName":"","path":""}},"compression":{"compressorName":{"host":"","userName":"","path":""},"onlyCompress":{"host":"","userName":"","path":""},"neverCompress":{"host":"","userName":"","path":""},"minSize":{"host":"","userName":"","path":""},"maxSize":{"host":"","userName":"","path":""}},"metadataCompression":{"compressorName":{"host":"","userName":"","path":""}},"splitter":{"algorithm":{"host":"","userName":"","path":""}},"actions":{"beforeSnapshotRoot":{"host":"","userName":"","path":""},"afterSnapshotRoot":{"host":"","userName":"","path":""}},"osSnapshots":{"volumeShadowCopy":{"enable":{"host":"","userName":"","path":""}}},"logging":{"directories":{"snapshotted":{"host":"","userName":"","path":""},"ignored":{"host":"","userName":"","path":""}},"entries":{"snapshotted":{"host":"","userName":"","path":""},"ignored":{"host":"","userName":"","path":""},"cacheHit":{"host":"","userName":"","path":""},"cacheMiss":{"host":"","userName":"","path":""}}},"upload":{"maxParallelSnapshots":{"host":"","userName":"","path":""},"maxParallelFileReads":{"host":"","userName":"","path":""},"parallelUploadAboveSize":{"host":"","userName":"","path":""}}},
  "defined": {"retention":{"keepDaily":14},"files":{},"errorHandling":{},"scheduling":{"cron":["0 3 * * *"]},"compression":{},"metadataCompression":{},"splitter":{},"actions":{},"osSnapshots":{"volumeShadowCopy":{}},"logging":{"directories":{},"entries":{}},"upload":{}},
  "upcomingSnapshotTimes": ["2026-10-19T03:00:00Z", "2026-10-20T03:00:00Z", "2026-10-21T03:00:00+03:00"]
}
`

// effectivePolicyTestResolveJSON0220 is the same answer from Kopia 0.22.0:
// no empty sections, and definition entries only where the source is not the
// zero target, plus the slice entries of every section present.
const effectivePolicyTestResolveJSON0220 = `{
  "effective": {"retention":{"keepLatest":10,"keepHourly":48,"keepDaily":14,"keepWeekly":4,"keepMonthly":24,"keepAnnual":3,"ignoreIdenticalSnapshots":false},"files":{"ignoreDotFiles":[".kopiaignore"]},"errorHandling":{"ignoreFileErrors":false,"ignoreDirectoryErrors":false,"ignoreUnknownTypes":true},"scheduling":{"cron":["0 3 * * *"],"runMissed":true},"compression":{"compressorName":"none"},"metadataCompression":{"compressorName":"zstd-fastest"},"osSnapshots":{"volumeShadowCopy":{"enable":0}},"logging":{"directories":{"snapshotted":5,"ignored":5},"entries":{"snapshotted":0,"ignored":5,"cacheHit":0,"cacheMiss":0}},"upload":{"maxParallelSnapshots":1,"parallelUploadAboveSize":2147483648}},
  "definition": {"retention":{"keepDaily":{"host":"host1","userName":"docker","path":""}},"scheduling":{"timeOfDay":{"host":"","userName":"","path":""},"cron":{"host":"host1","userName":"docker","path":""}}},
  "defined": {"retention":{"keepDaily":14},"scheduling":{"cron":["0 3 * * *"]}},
  "upcomingSnapshotTimes": ["2026-10-19T03:00:00Z", "2026-10-20T03:00:00Z", "2026-10-21T03:00:00+03:00"]
}
`

// effectivePolicyTestDefinition returns the definition map of a state as
// sorted "key=target" lines.
func effectivePolicyTestDefinition(t *testing.T, state tftypes.Value) []string {
	t.Helper()
	var entries map[string]tftypes.Value
	if err := knownAt(t, state, "definition").As(&entries); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, key := range harnessSortedKeys(entries) {
		var source string
		if err := entries[key].As(&source); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, key+"="+source)
	}
	return lines
}

func TestEffectivePolicyDataSource(t *testing.T) {
	var states []tftypes.Value
	for _, body := range []string{effectivePolicyTestResolveJSON, effectivePolicyTestResolveJSON0220} {
		fake := newFakeKopia()
		fake.reply("POST /api/v1/policy/resolve", http.StatusOK, body)
		h := policyTestHarness(t, fake)
		state := h.readDataSource(t, "kopia_effective_policy", h.dataSourceValue(t, "kopia_effective_policy", map[string]any{
			"target": "docker@host1", "num_upcoming_snapshot_times": 3,
		}))
		call := fake.lastCall(t, "POST /api/v1/policy/resolve")
		// Without updates Kopia resolves the stored policies, which is right
		// for every target.
		if got := string(call.body); got != `{"updates":null,"numUpcomingSnapshotTimes":3}` {
			t.Errorf("resolve body = %s", got)
		}
		if got := call.query.Encode(); got != "host=host1&userName=docker" {
			t.Errorf("resolve query = %q", got)
		}
		states = append(states, state)
	}
	// Both releases' answers read the same.
	policyTestRequireEqual(t, "0.22.0 answer", states[1], "0.22.3 answer", states[0])
	state := states[0]

	if got := int64At(t, state, "effective", "retention", "keep_daily"); got != 14 {
		t.Errorf("retention.keep_daily = %d", got)
	}
	if got := stringAt(t, state, "effective", "compression", "compressor_name"); got != "none" {
		t.Errorf("compression.compressor_name = %q", got)
	}
	if got := stringsAt(t, state, "effective", "files", "ignore_dot_files"); strings.Join(got, ",") != ".kopiaignore" {
		t.Errorf("files.ignore_dot_files = %v", got)
	}
	if got := stringAt(t, state, "effective", "os_snapshots", "volume_shadow_copy"); got != "never" {
		t.Errorf("os_snapshots.volume_shadow_copy = %q", got)
	}
	if !boolAt(t, state, "effective", "scheduling", "run_missed") || boolAt(t, state, "effective", "retention", "ignore_identical_snapshots") {
		t.Errorf("scheduling = %s", valueAt(t, state, "effective", "scheduling"))
	}
	if got := int64At(t, state, "effective", "upload", "parallel_upload_above_size"); got != 2147483648 {
		t.Errorf("upload.parallel_upload_above_size = %d", got)
	}
	for _, name := range []string{"splitter", "actions", "no_parent"} {
		if !isNullAt(t, state, "effective", name) {
			t.Errorf("effective.%s is not null although nothing sets it", name)
		}
	}
	if !isNullAt(t, state, "effective", "files", "ignore") || !isNullAt(t, state, "effective", "scheduling", "manual") {
		t.Error("effective values nothing sets are not null")
	}

	definition := strings.Join(effectivePolicyTestDefinition(t, state), "\n")
	for _, line := range []string{
		"retention.keep_daily=docker@host1",
		"retention.keep_latest=(global)",
		"scheduling.cron=docker@host1",
		"scheduling.run_missed=(global)",
		"logging.entries.cache_hit=(global)",
		"os_snapshots.volume_shadow_copy=(global)",
		"upload.max_parallel_snapshots=(global)",
	} {
		if !strings.Contains(definition, line+"\n") && !strings.HasSuffix(definition, line) {
			t.Errorf("definition lacks %s:\n%s", line, definition)
		}
	}
	for _, key := range []string{"files.ignore=", "scheduling.time_of_day=", "scheduling.manual=", "splitter.algorithm=", "upload.max_parallel_file_reads="} {
		if strings.Contains(definition, key) {
			t.Errorf("definition names a source for %s, which nothing sets:\n%s", strings.TrimSuffix(key, "="), definition)
		}
	}

	if got := stringsAt(t, state, "upcoming_snapshot_times"); strings.Join(got, ",") != "2026-10-19T03:00:00Z,2026-10-20T03:00:00Z,2026-10-21T03:00:00+03:00" {
		t.Errorf("upcoming_snapshot_times = %v", got)
	}
	if !isNullAt(t, state, "scheduling_error") {
		t.Errorf("scheduling_error = %s", valueAt(t, state, "scheduling_error"))
	}
}

// The folder actions are never inherited, so their source is the target
// itself; the scheduling error is reported as Kopia words it.
func TestEffectivePolicyDataSourceFolderActionsAndSchedulingError(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("POST /api/v1/policy/resolve", http.StatusOK, `{
  "effective": {"actions": {"beforeFolder": {"path": "/bin/true"}, "beforeSnapshotRoot": {"script": "true", "mode": "async"}}, "scheduling": {"manual": true}},
  "definition": {"actions": {"beforeSnapshotRoot": {"host": "host1", "userName": "", "path": ""}}, "scheduling": {"manual": {"host": "host1", "userName": "docker", "path": ""}}},
  "defined": {},
  "upcomingSnapshotTimes": [],
  "schedulingError": "invalid cron expression \"x\""
}
`)
	h := policyTestHarness(t, fake)
	state := h.readDataSource(t, "kopia_effective_policy", h.dataSourceValue(t, "kopia_effective_policy", map[string]any{
		"target": "docker@host1:/data",
	}))
	if got := string(fake.lastCall(t, "POST /api/v1/policy/resolve").body); got != `{"updates":null,"numUpcomingSnapshotTimes":0}` {
		t.Errorf("resolve body = %s", got)
	}
	want := []string{
		"actions.before_folder=docker@host1:/data",
		"actions.before_snapshot_root=@host1",
		"scheduling.manual=docker@host1",
	}
	if got := effectivePolicyTestDefinition(t, state); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("definition = %v, want %v", got, want)
	}
	if got := stringAt(t, state, "effective", "actions", "before_folder", "command"); got != "/bin/true" {
		t.Errorf("actions.before_folder.command = %q", got)
	}
	if got := stringAt(t, state, "scheduling_error"); got != `invalid cron expression "x"` {
		t.Errorf("scheduling_error = %q", got)
	}
	if got := stringsAt(t, state, "upcoming_snapshot_times"); len(got) != 0 {
		t.Errorf("upcoming_snapshot_times = %v, want an empty list", got)
	}
}

func TestEffectivePolicyDataSourceValidation(t *testing.T) {
	fake := newFakeKopia()
	h := policyTestHarness(t, fake)
	for _, tc := range []struct {
		attrs  map[string]any
		expect string
	}{
		{map[string]any{"target": "docker@host1:/data/"}, "Invalid policy target"},
		{map[string]any{"target": "docker@host1", "num_upcoming_snapshot_times": 101}, "between 0 and 100"},
		{map[string]any{"target": "docker@host1", "num_upcoming_snapshot_times": -1}, "between 0 and 100"},
	} {
		diags := h.validateDataSource(t, "kopia_effective_policy", h.dataSourceValue(t, "kopia_effective_policy", tc.attrs))
		requireProtocolErrorContaining(t, diags, tc.expect)
	}

	fake.replyError("POST /api/v1/policy/resolve", http.StatusInternalServerError, kopiaclient.CodeInternal, "internal server error: unable to find policies")
	resp := h.readDataSourceResponse(t, "kopia_effective_policy", h.dataSourceValue(t, "kopia_effective_policy", map[string]any{"target": "(global)"}))
	requireProtocolErrorContaining(t, resp.Diagnostics, "Unable to resolve Kopia policy")
	if got := fake.query(t, "POST /api/v1/policy/resolve"); len(got) != 0 {
		t.Errorf("query = %v, want none for the global policy", got)
	}
}

// --- acceptance -----------------------------------------------------------------

func TestAccEffectivePolicyDataSource(t *testing.T) {
	user := "tfacc@" + testAccPolicyHost + "-effective"
	path := user + ":/data"
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: acc.ComposeAggregateTestCheckFunc(
			testAccCheckPolicyStored(user, ""),
			testAccCheckPolicyStored(path, ""),
		),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
resource "kopia_policy" "user" {
  target = %[1]q

  scheduling = {
    cron = ["0 3 * * *"]
  }
}

resource "kopia_policy" "path" {
  target = %[2]q

  retention = {
    keep_daily = 21
  }
}

data "kopia_effective_policy" "path" {
  target                      = kopia_policy.path.target
  num_upcoming_snapshot_times = 2

  depends_on = [kopia_policy.user]
}
`, user, path),
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("data.kopia_effective_policy.path", "effective.retention.keep_daily", "21"),
					acc.TestCheckResourceAttr("data.kopia_effective_policy.path", "definition.retention.keep_daily", path),
					acc.TestCheckResourceAttr("data.kopia_effective_policy.path", "effective.scheduling.cron.0", "0 3 * * *"),
					acc.TestCheckResourceAttr("data.kopia_effective_policy.path", "definition.scheduling.cron", user),
					// The built-in defaults fill in what no policy sets.
					acc.TestCheckResourceAttrSet("data.kopia_effective_policy.path", "effective.retention.keep_latest"),
					acc.TestCheckResourceAttr("data.kopia_effective_policy.path", "upcoming_snapshot_times.#", "2"),
					acc.TestMatchResourceAttr("data.kopia_effective_policy.path", "upcoming_snapshot_times.0", regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T03:00:00`)),
					acc.TestCheckNoResourceAttr("data.kopia_effective_policy.path", "scheduling_error"),
				),
			},
		},
	})
}
