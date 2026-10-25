package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// Unit tests of kopia_policy. Most run through the protocol harness, against
// the in-memory policy store of policy_fake_test.go, because what matters
// most here is what Terraform itself checks between the calls: that the
// applied state matches the plan, and that planning the same configuration
// again changes nothing. policyTestApply checks both on every apply.

const policyTestType = "kopia_policy"

// policyTestHarness serves kopia_policy and the two policy data sources.
func policyTestHarness(t *testing.T, fake *fakeKopia) *protocolHarness {
	t.Helper()
	return newProtocolHarness(t, fake,
		withResources(NewPolicyResource),
		withDataSources(NewPoliciesDataSource, NewEffectivePolicyDataSource))
}

// policyTestConfig builds a kopia_policy configuration of the served schema.
func policyTestConfig(t *testing.T, h *protocolHarness, attrs map[string]any) tftypes.Value {
	t.Helper()
	return h.resourceValue(t, policyTestType, attrs)
}

// policyTestApply validates, plans and applies config, failing the test on
// any error, and then checks what Terraform core checks on top of that. The
// applied state must equal the plan wherever the plan is known, or Terraform
// fails with "Provider produced inconsistent result after apply"; and as the
// plan of this resource is always fully known, that means equal throughout.
// Planning the same configuration again must then change nothing, or every
// later plan would show a difference.
func policyTestApply(t *testing.T, h *protocolHarness, prior *resourceInstance, config tftypes.Value) *resourceInstance {
	t.Helper()
	requireNoProtocolErrors(t, h.validateResource(t, policyTestType, config))
	planned := h.plan(t, policyTestType, prior, config)
	requireNoProtocolErrors(t, planned.Diagnostics)
	plannedState := h.decodeResource(t, policyTestType, planned.PlannedState)
	if !plannedState.IsFullyKnown() {
		t.Fatalf("the plan holds unknown values: %s", plannedState)
	}

	applied := h.apply(t, policyTestType, prior, config, planned)
	requireNoProtocolErrors(t, applied.Diagnostics)
	inst := &resourceInstance{
		typeName: policyTestType,
		state:    h.decodeResource(t, policyTestType, applied.NewState),
		identity: applied.NewIdentity,
		private:  applied.Private,
	}
	policyTestRequireEqual(t, "applied state", inst.state, "plan", plannedState)

	replanned := h.plan(t, policyTestType, inst, config)
	requireNoProtocolErrors(t, replanned.Diagnostics)
	policyTestRequireEqual(t, "plan after apply", h.decodeResource(t, policyTestType, replanned.PlannedState), "state", inst.state)
	return inst
}

// policyTestRequireEqual fails the test, listing the differences, unless got
// equals want.
func policyTestRequireEqual(t *testing.T, gotName string, got tftypes.Value, wantName string, want tftypes.Value) {
	t.Helper()
	if got.Equal(want) {
		return
	}
	diffs, err := got.Diff(want)
	if err != nil {
		t.Fatalf("%s differs from the %s: %s\nwant %s", gotName, wantName, got, want)
	}
	var lines []string
	for _, d := range diffs {
		lines = append(lines, fmt.Sprintf("%s: %s, %s %s", d.Path, d.Value1, wantName, d.Value2))
	}
	t.Fatalf("%s differs from the %s:\n%s", gotName, wantName, strings.Join(lines, "\n"))
}

// policyTestTimes returns the times of day of a state as sorted "hh:mm".
func policyTestTimes(t *testing.T, state tftypes.Value) []string {
	t.Helper()
	var elements []tftypes.Value
	if err := knownAt(t, state, "scheduling", "time_of_day").As(&elements); err != nil {
		t.Fatal(err)
	}
	var times []string
	for _, e := range elements {
		times = append(times, fmt.Sprintf("%02d:%02d", int64At(t, e, "hour"), int64At(t, e, "minute")))
	}
	sort.Strings(times)
	return times
}

// policyTestPutBody returns the body of the most recent PUT.
func policyTestPutBody(t *testing.T, fake *fakeKopia) string {
	t.Helper()
	return string(fake.lastCall(t, "PUT /api/v1/policy").body)
}

// policyTestFullConfig sets every attribute of kopia_policy, with explicit
// false and 0 values on Kopia's pointer fields, and no plain false, which an
// import could not tell from null.
func policyTestFullConfig(target string) map[string]any {
	return map[string]any{
		"target": target,
		"retention": map[string]any{
			"keep_latest": 10, "keep_hourly": 0, "keep_daily": 14, "keep_weekly": 4, "keep_monthly": 24,
			"keep_annual": 3, "ignore_identical_snapshots": false,
		},
		"files": map[string]any{
			"ignore":           []string{"*.tmp", "/cache"},
			"no_parent_ignore": true, "ignore_dot_files": []string{".kopiaignore", ".gitignore"},
			"no_parent_dot_files": true, "ignore_cache_dirs": true, "max_file_size": -1, "one_file_system": false,
		},
		"error_handling": map[string]any{
			"ignore_file_errors": true, "ignore_directory_errors": false, "ignore_unknown_types": true,
		},
		"scheduling": map[string]any{
			"interval_seconds": 3600,
			"time_of_day": []any{
				map[string]any{"hour": 15, "minute": 0},
				map[string]any{"hour": 3, "minute": 15},
			},
			"no_parent_time_of_day": true,
			"cron":                  []string{"0 3 * * * # nightly"},
			"run_missed":            false,
		},
		"compression": map[string]any{
			"compressor_name": "zstd", "only_compress": []string{".txt"}, "no_parent_only_compress": true,
			"never_compress": []string{".jpg", ".gz"}, "no_parent_never_compress": true, "min_size": 1024, "max_size": -1,
		},
		"metadata_compression": map[string]any{"compressor_name": "zstd-fastest"},
		"splitter":             map[string]any{"algorithm": "DYNAMIC-4M-BUZHASH"},
		"actions": map[string]any{
			"before_folder": map[string]any{"command": "/bin/true"},
			"before_snapshot_root": map[string]any{
				"script": "#!/bin/sh\npg_dump app > /backup/app.sql", "timeout_seconds": 300, "mode": "essential",
			},
			"after_snapshot_root": map[string]any{"command": "/usr/local/bin/notify", "args": []string{"done"}, "mode": "async"},
		},
		"os_snapshots": map[string]any{"volume_shadow_copy": "when-available"},
		"logging": map[string]any{
			"directories": map[string]any{"snapshotted": 5, "ignored": 0},
			"entries":     map[string]any{"cache_hit": 0, "cache_miss": 10},
		},
		"upload": map[string]any{
			"max_parallel_snapshots": 2, "max_parallel_file_reads": 8, "parallel_upload_above_size": -1,
		},
		"no_parent": true,
	}
}

// policyTestFullBody is what policyTestFullConfig sends: Kopia's field names
// and order, the minute as "min", the times of day sorted, the extensions of a
// set sorted, and every plain field that holds nothing left out.
const policyTestFullBody = `{"retention":{"keepLatest":10,"keepHourly":0,"keepDaily":14,"keepWeekly":4,"keepMonthly":24,"keepAnnual":3,"ignoreIdenticalSnapshots":false},` +
	`"files":{"ignore":["*.tmp","/cache"],"noParentIgnore":true,"ignoreDotFiles":[".kopiaignore",".gitignore"],"noParentDotFiles":true,"ignoreCacheDirs":true,"maxFileSize":-1,"oneFileSystem":false},` +
	`"errorHandling":{"ignoreFileErrors":true,"ignoreDirectoryErrors":false,"ignoreUnknownTypes":true},` +
	`"scheduling":{"intervalSeconds":3600,"timeOfDay":[{"hour":3,"min":15},{"hour":15,"min":0}],"noParentTimeOfDay":true,"cron":["0 3 * * * # nightly"],"runMissed":false},` +
	`"compression":{"compressorName":"zstd","onlyCompress":[".txt"],"noParentOnlyCompress":true,"neverCompress":[".gz",".jpg"],"noParentNeverCompress":true,"minSize":1024,"maxSize":-1},` +
	`"metadataCompression":{"compressorName":"zstd-fastest"},"splitter":{"algorithm":"DYNAMIC-4M-BUZHASH"},` +
	`"actions":{"beforeFolder":{"path":"/bin/true"},"beforeSnapshotRoot":{"script":"#!/bin/sh\npg_dump app \u003e /backup/app.sql","timeout":300,"mode":"essential"},"afterSnapshotRoot":{"path":"/usr/local/bin/notify","args":["done"],"mode":"async"}},` +
	`"osSnapshots":{"volumeShadowCopy":{"enable":2}},` +
	`"logging":{"directories":{"snapshotted":5,"ignored":0},"entries":{"cacheHit":0,"cacheMiss":10}},` +
	`"upload":{"maxParallelSnapshots":2,"maxParallelFileReads":8,"parallelUploadAboveSize":-1},"noParent":true}`

func TestPolicySchemasAreValid(t *testing.T) {
	requireValidResourceSchema(t, NewPolicyResource())
	requireValidDataSourceSchema(t, NewPoliciesDataSource())
	requireValidDataSourceSchema(t, NewEffectivePolicyDataSource())
}

// Every attribute is documented: the descriptions are the registry pages.
func TestPolicySchemasDescribeEveryAttribute(t *testing.T) {
	var walkResource func(prefix string, attrs map[string]schema.Attribute)
	walkResource = func(prefix string, attrs map[string]schema.Attribute) {
		for name, a := range attrs {
			if a.GetMarkdownDescription() == "" {
				t.Errorf("kopia_policy attribute %s%s has no description", prefix, name)
			}
			switch nested := a.(type) {
			case schema.SingleNestedAttribute:
				walkResource(prefix+name+".", nested.Attributes)
			case schema.SetNestedAttribute:
				walkResource(prefix+name+".", nested.NestedObject.Attributes)
			}
		}
	}
	s := resourceSchema(t, NewPolicyResource())
	if s.MarkdownDescription == "" {
		t.Error("kopia_policy has no description")
	}
	walkResource("", s.Attributes)

	var walkDataSource func(typeName, prefix string, attrs map[string]datasourceschema.Attribute)
	walkDataSource = func(typeName, prefix string, attrs map[string]datasourceschema.Attribute) {
		for name, a := range attrs {
			if a.GetMarkdownDescription() == "" {
				t.Errorf("%s attribute %s%s has no description", typeName, prefix, name)
			}
			switch nested := a.(type) {
			case datasourceschema.SingleNestedAttribute:
				walkDataSource(typeName, prefix+name+".", nested.Attributes)
			case datasourceschema.SetNestedAttribute:
				walkDataSource(typeName, prefix+name+".", nested.NestedObject.Attributes)
			case datasourceschema.ListNestedAttribute:
				walkDataSource(typeName, prefix+name+".", nested.NestedObject.Attributes)
			}
		}
	}
	for typeName, d := range map[string]datasource.DataSource{
		"kopia_policies":         NewPoliciesDataSource(),
		"kopia_effective_policy": NewEffectivePolicyDataSource(),
	} {
		s := dataSourceSchema(t, d)
		if s.MarkdownDescription == "" {
			t.Errorf("%s has no description", typeName)
		}
		walkDataSource(typeName, "", s.Attributes)
	}
}

// The policy's sections are optional, never computed: kopia_policy owns the
// whole policy, and a section left out is inherited, not adopted.
func TestPolicySectionsAreOptionalOnly(t *testing.T) {
	s := resourceSchema(t, NewPolicyResource())
	for name, a := range s.Attributes {
		if name == "id" || name == "target" {
			continue
		}
		if !a.IsOptional() || a.IsComputed() || a.IsRequired() {
			t.Errorf("%s: optional %v, computed %v, required %v; want optional only", name, a.IsOptional(), a.IsComputed(), a.IsRequired())
		}
	}
}

// A policy with every attribute set goes through create, refresh, import,
// update and destroy, sending exactly Kopia's JSON and reading back exactly
// the configuration.
func TestPolicyResourceLifecycle(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)

	inst := policyTestApply(t, h, nil, policyTestConfig(t, h, policyTestFullConfig("docker@host1")))
	if got := policyTestPutBody(t, fake); got != policyTestFullBody {
		t.Errorf("PUT body =\n%s\nwant\n%s", got, policyTestFullBody)
	}
	if got := fake.query(t, "PUT /api/v1/policy").Encode(); got != "host=host1&userName=docker" {
		t.Errorf("PUT query = %q", got)
	}
	// The existence check comes first, then the write and the read-back.
	want := []string{"GET /api/v1/policy", "PUT /api/v1/policy", "GET /api/v1/policy"}
	if got := fake.apiRoutes(); len(got) < 3 || strings.Join(got[len(got)-3:], ",") != strings.Join(want, ",") {
		t.Errorf("API calls = %v, want them to end with %v", got, want)
	}

	if got := stringAt(t, inst.state, "id"); got != "docker@host1" {
		t.Errorf("id = %q", got)
	}
	if got := stringAt(t, h.identityValue(t, policyTestType, inst.identity), "target"); got != "docker@host1" {
		t.Errorf("identity target = %q", got)
	}
	if got := int64At(t, inst.state, "retention", "keep_hourly"); got != 0 {
		t.Errorf("retention.keep_hourly = %d, want the explicit 0", got)
	}
	if boolAt(t, inst.state, "files", "one_file_system") {
		t.Error("files.one_file_system is not the explicit false")
	}
	if got := policyTestTimes(t, inst.state); strings.Join(got, ",") != "03:15,15:00" {
		t.Errorf("scheduling.time_of_day = %v", got)
	}
	if got := stringAt(t, inst.state, "os_snapshots", "volume_shadow_copy"); got != "when-available" {
		t.Errorf("os_snapshots.volume_shadow_copy = %q", got)
	}
	if !isNullAt(t, inst.state, "actions", "after_folder") || !isNullAt(t, inst.state, "logging", "entries", "ignored") {
		t.Error("attributes that are not configured are not null")
	}

	// A refresh finds nothing to change.
	refreshed := h.refresh(t, inst)
	policyTestRequireEqual(t, "refreshed state", refreshed.state, "state after apply", inst.state)

	// Importing the same target reads it back identically: the configuration
	// holds nothing an import cannot recover.
	imported := h.importByID(t, policyTestType, "docker@host1")
	policyTestRequireEqual(t, "imported state", imported.state, "state after apply", inst.state)

	// Shrinking the configuration replaces the whole policy, and whatever is
	// left out is inherited again: it is not sent, and reads back as null.
	inst = policyTestApply(t, h, inst, policyTestConfig(t, h, map[string]any{
		"target":     "docker@host1",
		"retention":  map[string]any{"keep_daily": 30},
		"scheduling": map[string]any{"manual": true},
	}))
	if got := policyTestPutBody(t, fake); got != `{"retention":{"keepDaily":30},"scheduling":{"manual":true}}` {
		t.Errorf("PUT body after update = %s", got)
	}
	for _, name := range []string{"files", "compression", "actions", "upload", "no_parent"} {
		if !isNullAt(t, inst.state, name) {
			t.Errorf("%s is not null after it was removed from the configuration", name)
		}
	}
	if !isNullAt(t, inst.state, "retention", "keep_latest") {
		t.Error("retention.keep_latest is not null after it was removed from the configuration")
	}

	h.destroy(t, inst)
	if got := fake.query(t, "DELETE /api/v1/policy").Encode(); got != "host=host1&userName=docker" {
		t.Errorf("DELETE query = %q", got)
	}
	if store.has(t, "docker@host1") {
		t.Error("destroy left the policy in place")
	}
}

// The values Kopia cannot store — an empty section, a plain false — are
// accepted, sent as nothing at all, and still read back as configured, so
// that neither the apply nor the next plan sees a difference.
func TestPolicyResourceKeepsWhatKopiaCannotStore(t *testing.T) {
	fake := newFakeKopia()
	newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)

	config := policyTestConfig(t, h, map[string]any{
		"target":         "docker@host1",
		"retention":      map[string]any{},
		"error_handling": map[string]any{},
		"files":          map[string]any{"no_parent_ignore": false, "no_parent_dot_files": false},
		"scheduling":     map[string]any{"manual": false, "no_parent_time_of_day": false},
		"compression":    map[string]any{"no_parent_only_compress": false, "no_parent_never_compress": false},
		"actions":        map[string]any{},
		"os_snapshots":   map[string]any{},
		"logging":        map[string]any{"directories": map[string]any{}},
		"upload":         map[string]any{},
		"no_parent":      false,
	})
	inst := policyTestApply(t, h, nil, config)
	if got := policyTestPutBody(t, fake); got != "{}" {
		t.Fatalf("PUT body = %s, want {}: Kopia stores none of these values", got)
	}
	if isNullAt(t, inst.state, "retention") || isNullAt(t, inst.state, "logging", "directories") {
		t.Error("an empty section did not read back as an object")
	}
	if !isNullAt(t, inst.state, "logging", "entries") || !isNullAt(t, inst.state, "splitter") {
		t.Error("a section that is not configured did not read back as null")
	}
	if boolAt(t, inst.state, "scheduling", "manual") || boolAt(t, inst.state, "no_parent") {
		t.Error("a plain false did not read back as false")
	}

	// A refresh keeps them too.
	refreshed := h.refresh(t, inst)
	policyTestRequireEqual(t, "refreshed state", refreshed.state, "state after apply", inst.state)
}

// Kopia 0.22.0 leaves empty sections out of a policy, 0.22.3 and later send
// them as {}; the same stored policy imports identically from both.
func TestPolicyResourceReadsBothReleaseShapes(t *testing.T) {
	const body = `{"retention":{"keepLatest":10,"keepDaily":14},"scheduling":{"cron":["0 3 * * * # nightly"],"runMissed":true},"compression":{"compressorName":"zstd","neverCompress":[".gz",".jpg",".zip"]},"upload":{"maxParallelSnapshots":2}}`
	var states []tftypes.Value
	for _, compact := range []bool{false, true} {
		fake := newFakeKopia()
		store := newPolicyTestStore(fake)
		store.compact = compact
		store.putRaw(t, "docker@host1", body)
		h := policyTestHarness(t, fake)
		states = append(states, h.importByID(t, policyTestType, "docker@host1").state)
	}
	policyTestRequireEqual(t, "0.22.0 shape", states[1], "0.22.3 shape", states[0])
	for _, name := range []string{"files", "error_handling", "splitter", "actions", "os_snapshots", "logging", "metadata_compression"} {
		if !isNullAt(t, states[0], name) {
			t.Errorf("%s: an empty section did not import as null", name)
		}
	}
	if got := stringsAt(t, states[0], "compression", "never_compress"); strings.Join(got, ",") != ".gz,.jpg,.zip" {
		t.Errorf("compression.never_compress = %v", got)
	}
}

// Read decides between null and an object, and between null and false, from
// the prior state: what Kopia reports cannot say.
func TestPolicyResourceReadPreservesTheShapeOfThePriorState(t *testing.T) {
	r := &policyResource{}
	prior := resourceValue(t, r, map[string]any{
		"id": "docker@host1", "target": "docker@host1",
		"retention":  map[string]any{"keep_latest": 5},
		"scheduling": map[string]any{"manual": false, "run_missed": true},
		"logging":    map[string]any{"entries": map[string]any{}},
		"no_parent":  false,
	})

	fake := newFakeKopia()
	// Another writer replaced the policy: retention is gone, run_missed is
	// unset, and scheduling and logging hold nothing.
	fake.reply("GET /api/v1/policy", http.StatusOK, `{"retention":{},"files":{},"errorHandling":{},"scheduling":{},"compression":{},"metadataCompression":{},"splitter":{},"actions":{},"osSnapshots":{"volumeShadowCopy":{}},"logging":{"directories":{},"entries":{}},"upload":{}}`)
	r.client = fake.client(t)
	resp := readResource(t, r, prior)
	requireNoErrors(t, resp.Diagnostics)
	state := resp.State.Raw

	// The configured retention stays an object, now of nulls, so the next
	// plan shows keep_latest coming back rather than the whole section.
	if isNullAt(t, state, "retention") || !isNullAt(t, state, "retention", "keep_latest") {
		t.Errorf("retention = %s, want an object with keep_latest null", valueAt(t, state, "retention"))
	}
	if boolAt(t, state, "scheduling", "manual") || !isNullAt(t, state, "scheduling", "run_missed") {
		t.Errorf("scheduling = %s, want manual false and run_missed null", valueAt(t, state, "scheduling"))
	}
	if isNullAt(t, state, "logging", "entries") || !isNullAt(t, state, "logging", "directories") {
		t.Errorf("logging = %s, want entries {} and directories null", valueAt(t, state, "logging"))
	}
	if boolAt(t, state, "no_parent") {
		t.Error("no_parent is not the prior false")
	}
	for _, name := range []string{"files", "error_handling", "compression", "splitter", "actions", "os_snapshots", "upload"} {
		if !isNullAt(t, state, name) {
			t.Errorf("%s is not null: neither Kopia nor the prior state has it", name)
		}
	}

	// What Kopia does report wins over the prior state.
	fake.reply("GET /api/v1/policy", http.StatusOK, `{"scheduling":{"manual":true},"noParent":true}`)
	resp = readResource(t, r, prior)
	requireNoErrors(t, resp.Diagnostics)
	if !boolAt(t, resp.State.Raw, "scheduling", "manual") || !boolAt(t, resp.State.Raw, "no_parent") {
		t.Errorf("state = %s, want manual and no_parent true", resp.State.Raw)
	}
	// Kopia 0.22.0 leaves the empty sections out altogether, which reads the
	// same as the empty sections of 0.22.3.
	if isNullAt(t, resp.State.Raw, "logging", "entries") || !isNullAt(t, resp.State.Raw, "files") {
		t.Errorf("state = %s, want logging.entries {} and files null", resp.State.Raw)
	}
}

// Create does not take over a policy that already exists: it may belong to
// the Kopia UI or kopia-docker-watcher, and PUT would replace it unseen.
func TestPolicyResourceCreateRefusesAnExistingPolicy(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	store.putRaw(t, "docker@host1:/data/app", `{"scheduling":{"cron":["0 4 * * *"]}}`)
	h := policyTestHarness(t, fake)

	config := policyTestConfig(t, h, map[string]any{
		"target":     "docker@host1:/data/app",
		"scheduling": map[string]any{"interval_seconds": 3600},
	})
	planned := h.plan(t, policyTestType, nil, config)
	requireNoProtocolErrors(t, planned.Diagnostics)
	applied := h.apply(t, policyTestType, nil, config, planned)
	requireProtocolErrorContaining(t, applied.Diagnostics, "Kopia policy already exists")
	requireProtocolErrorContaining(t, applied.Diagnostics, `"docker@host1:/data/app"`)
	if fake.called("PUT /api/v1/policy") {
		t.Error("the existing policy was overwritten")
	}
	if got := store.stored(t, "docker@host1:/data/app"); got != `{"scheduling":{"cron":["0 4 * * *"]}}` {
		t.Errorf("stored policy = %s", got)
	}
}

// A failed existence check stops the create too: the policy may well exist.
func TestPolicyResourceCreateStopsWhenTheCheckFails(t *testing.T) {
	r := &policyResource{}
	fake := newFakeKopia()
	fake.reply("GET /api/v1/policy", http.StatusOK, "null\n")
	r.client = fake.client(t)

	config := resourceValue(t, r, map[string]any{"target": "@host1"})
	plan := resourceValue(t, r, map[string]any{"id": "@host1", "target": "@host1"})
	resp := createResource(t, r, config, plan)
	requireErrorContaining(t, resp.Diagnostics, "Unable to create Kopia policy")
	requireErrorContaining(t, resp.Diagnostics, "Kopia answered with null")
	if fake.called("PUT /api/v1/policy") {
		t.Error("the policy was written although the existence check failed")
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %s, want none", resp.State.Raw)
	}
}

// (global) always exists, so it is adopted rather than refused.
func TestPolicyResourceCreateAdoptsTheGlobalPolicy(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	store.putRaw(t, "(global)", policyTestDefaultGlobalJSON)
	h := policyTestHarness(t, fake)

	inst := policyTestApply(t, h, nil, policyTestConfig(t, h, map[string]any{
		"target":      "(global)",
		"retention":   map[string]any{"keep_daily": 14},
		"compression": map[string]any{"compressor_name": "zstd"},
	}))
	if got := policyTestPutBody(t, fake); got != `{"retention":{"keepDaily":14},"compression":{"compressorName":"zstd"}}` {
		t.Errorf("PUT body = %s", got)
	}
	if got := fake.query(t, "PUT /api/v1/policy"); len(got) != 0 {
		t.Errorf("PUT query = %v, want none for the global policy", got)
	}
	if got := stringAt(t, inst.state, "id"); got != "(global)" {
		t.Errorf("id = %q", got)
	}
	// The resource owns the whole global policy: Kopia's stored defaults are
	// replaced, and the built-in defaults apply in their place.
	if !isNullAt(t, inst.state, "retention", "keep_latest") || !isNullAt(t, inst.state, "upload") {
		t.Errorf("state = %s, want only what was configured", inst.state)
	}
}

// Destroying (global) resets it to the policy a new repository has, because
// DELETE would remove it for good; every other target is deleted.
func TestPolicyResourceDelete(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)

	global := policyTestApply(t, h, nil, policyTestConfig(t, h, map[string]any{
		"target": "(global)", "scheduling": map[string]any{"cron": []string{"0 2 * * *"}},
	}))
	h.destroy(t, global)
	if fake.called("DELETE /api/v1/policy") {
		t.Error("destroying (global) deleted it")
	}
	if got := policyTestPutBody(t, fake); got != policyTestDefaultGlobalJSON {
		t.Errorf("PUT body of the reset =\n%s\nwant\n%s", got, policyTestDefaultGlobalJSON)
	}
	if !store.has(t, "(global)") {
		t.Error("the global policy is gone")
	}

	host := policyTestApply(t, h, nil, policyTestConfig(t, h, map[string]any{
		"target": "@host1", "upload": map[string]any{"max_parallel_snapshots": 2},
	}))
	h.destroy(t, host)
	if got := fake.query(t, "DELETE /api/v1/policy").Encode(); got != "host=host1" {
		t.Errorf("DELETE query = %q", got)
	}
	if store.has(t, "@host1") {
		t.Error("the host policy is still there")
	}
}

// A failed reset of (global) is an error, so the resource stays in state.
func TestPolicyResourceDeleteReportsAFailedReset(t *testing.T) {
	r := &policyResource{}
	fake := newFakeKopia()
	fake.replyError("PUT /api/v1/policy", http.StatusInternalServerError, kopiaclient.CodeInternal,
		"internal server error: error flushing writer: storage is read-only")
	r.client = fake.client(t)

	resp := deleteResource(t, r, resourceValue(t, r, map[string]any{"id": "(global)", "target": "(global)"}))
	requireErrorContaining(t, resp.Diagnostics, "Unable to reset the Kopia global policy")
	requireErrorContaining(t, resp.Diagnostics, "storage is read-only")
}

// A policy deleted outside Terraform leaves state on the next refresh, and
// the identity is still reported then.
func TestPolicyResourceReadRemovesAMissingPolicy(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)
	inst := policyTestApply(t, h, nil, policyTestConfig(t, h, map[string]any{"target": "docker@host1"}))

	store.remove(t, "docker@host1")
	// State written before Terraform 1.12 has no identity.
	inst.identity = nil
	resp := h.read(t, inst)
	requireNoProtocolErrors(t, resp.Diagnostics)
	if got := h.decodeResource(t, policyTestType, resp.NewState); !got.IsNull() {
		t.Fatalf("state = %s, want the resource removed", got)
	}
	if got := stringAt(t, h.identityValue(t, policyTestType, resp.NewIdentity), "target"); got != "docker@host1" {
		t.Errorf("identity target = %q", got)
	}
}

// 200 null says loading the policy failed, not that it is gone: Read keeps
// the resource and reports an error.
func TestPolicyResourceReadOfNullIsAnError(t *testing.T) {
	r := &policyResource{}
	fake := newFakeKopia()
	fake.reply("GET /api/v1/policy", http.StatusOK, "null\n")
	r.client = fake.client(t)

	prior := resourceValue(t, r, map[string]any{"id": "docker@host1", "target": "docker@host1"})
	resp := readResource(t, r, prior)
	requireErrorContaining(t, resp.Diagnostics, "Unable to read Kopia policy")
	requireErrorContaining(t, resp.Diagnostics, "usually transient")
	if !resp.State.Raw.Equal(prior) {
		t.Errorf("state = %s, want it unchanged", resp.State.Raw)
	}
}

// Kopia reports a policy it refuses as 500 INTERNAL; the error is pinned to
// the attribute concerned.
func TestPolicyResourceReportsKopiasRefusal(t *testing.T) {
	tests := []struct {
		message string
		path    string
	}{
		{`internal server error: unable to set policy: failed to validate policy: invalid scheduling policy: invalid cron expression "x"`, "scheduling"},
		{"internal server error: unable to set policy: failed to validate policy: invalid upload policy: max parallel snapshots cannot be specified for paths, only global, username@hostname or @hostname", "upload.max_parallel_snapshots"},
		{"internal server error: unable to set policy: invalid policy path: path cannot end with a slash or a backslash", "target"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			r := &policyResource{}
			fake := newFakeKopia()
			fake.replyError("GET /api/v1/policy", http.StatusBadRequest, kopiaclient.CodeNotFound, "policy not found")
			fake.replyError("PUT /api/v1/policy", http.StatusInternalServerError, kopiaclient.CodeInternal, tc.message)
			r.client = fake.client(t)

			config := resourceValue(t, r, map[string]any{"target": "docker@host1"})
			plan := resourceValue(t, r, map[string]any{"id": "docker@host1", "target": "docker@host1"})
			resp := createResource(t, r, config, plan)
			requireAttributeError(t, resp.Diagnostics, tc.path)
			requireErrorContaining(t, resp.Diagnostics, "Kopia did not store the policy of docker@host1")
			if !resp.State.Raw.IsNull() {
				t.Errorf("state = %s, want none", resp.State.Raw)
			}
		})
	}
}

// When reading back after a write fails, the write has still happened: state
// is written from the plan, with a warning, rather than leaving the policy
// out of state.
func TestPolicyResourceWriteWithAFailedReadBack(t *testing.T) {
	r := &policyResource{}
	fake := newFakeKopia()
	fake.reply("PUT /api/v1/policy", http.StatusOK, "{}\n")
	// The existence check finds nothing; the read-back then fails.
	fake.handle("GET /api/v1/policy", func() http.HandlerFunc {
		calls := 0
		return func(w http.ResponseWriter, _ *http.Request) {
			calls++
			if calls == 1 {
				writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeNotFound, "policy not found"))
				return
			}
			writeFakeResponse(w, http.StatusOK, "null\n")
		}
	}())
	r.client = fake.client(t)

	config := resourceValue(t, r, map[string]any{"target": "docker@host1", "retention": map[string]any{"keep_daily": 7}})
	plan := resourceValue(t, r, map[string]any{"id": "docker@host1", "target": "docker@host1", "retention": map[string]any{"keep_daily": 7}})
	resp := createResource(t, r, config, plan)
	requireNoErrors(t, resp.Diagnostics)
	requireWarningContaining(t, resp.Diagnostics, "Unable to read Kopia policy back")
	if !resp.State.Raw.Equal(plan) {
		t.Errorf("state = %s, want the plan %s", resp.State.Raw, plan)
	}
	if got := identityAttr(t, resp.Identity, "target"); got != "docker@host1" {
		t.Errorf("identity target = %q", got)
	}
}

// Update writes the whole policy even when it was deleted since the last
// refresh: PUT creates it again, which is what the plan asks for.
func TestPolicyResourceUpdateRewritesTheWholePolicy(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)
	inst := policyTestApply(t, h, nil, policyTestConfig(t, h, map[string]any{
		"target": "docker@host1", "retention": map[string]any{"keep_daily": 7},
	}))

	store.remove(t, "docker@host1")
	puts := fake.count("PUT /api/v1/policy")
	inst = policyTestApply(t, h, inst, policyTestConfig(t, h, map[string]any{
		"target": "docker@host1", "retention": map[string]any{"keep_daily": 8},
	}))
	if fake.count("PUT /api/v1/policy") != puts+1 {
		t.Error("the update did not write the policy")
	}
	if got := store.stored(t, "docker@host1"); got != `{"retention":{"keepDaily":8}}` {
		t.Errorf("stored policy = %s", got)
	}
	if got := int64At(t, inst.state, "retention", "keep_daily"); got != 8 {
		t.Errorf("retention.keep_daily = %d", got)
	}
}

// Changing the target replaces the policy, and the plan knows the new id.
func TestPolicyResourceTargetChangeReplaces(t *testing.T) {
	fake := newFakeKopia()
	newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)
	inst := policyTestApply(t, h, nil, policyTestConfig(t, h, map[string]any{"target": "docker@host1"}))

	planned := h.plan(t, policyTestType, inst, policyTestConfig(t, h, map[string]any{"target": "docker@host2"}))
	requireNoProtocolErrors(t, planned.Diagnostics)
	if len(planned.RequiresReplace) != 1 || planned.RequiresReplace[0].String() != tftypes.NewAttributePath().WithAttributeName("target").String() {
		t.Errorf("RequiresReplace = %v, want target", planned.RequiresReplace)
	}
	if got := stringAt(t, h.decodeResource(t, policyTestType, planned.PlannedState), "id"); got != "docker@host2" {
		t.Errorf("planned id = %q", got)
	}
}

// A path target with a Windows path reaches Kopia exactly as written.
func TestPolicyResourceWindowsPathTarget(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)
	const target = `Administrator@WIN1:C:\Data`

	inst := policyTestApply(t, h, nil, policyTestConfig(t, h, map[string]any{
		"target": target, "os_snapshots": map[string]any{"volume_shadow_copy": "always"},
	}))
	if got := fake.query(t, "PUT /api/v1/policy").Encode(); got != "host=WIN1&path=C%3A%5CData&userName=Administrator" {
		t.Errorf("PUT query = %q", got)
	}
	if got := store.stored(t, target); got != `{"osSnapshots":{"volumeShadowCopy":{"enable":1}}}` {
		t.Errorf("stored policy = %s", got)
	}
	if got := stringAt(t, inst.state, "id"); got != target {
		t.Errorf("id = %q", got)
	}
}

// --- validation ---------------------------------------------------------------

func TestPolicyResourceValidation(t *testing.T) {
	h := policyTestHarness(t, newFakeKopia())
	invalid := []struct {
		name   string
		attrs  map[string]any
		expect string
	}{
		{"target with a trailing slash", map[string]any{"target": "docker@host1:/data/"}, "Invalid policy target"},
		{"target without a user", map[string]any{"target": "@host1:/data"}, "needs a user name"},
		{"relative target path", map[string]any{"target": "docker@host1:data"}, "must be absolute"},
		{"bare host", map[string]any{"target": "host1"}, "Invalid policy target"},
		{"manual with cron", map[string]any{"target": "docker@host1", "scheduling": map[string]any{
			"manual": true, "cron": []string{"0 3 * * *"},
		}}, "cannot be set together with manual = true"},
		{"manual with run_missed false", map[string]any{"target": "docker@host1", "scheduling": map[string]any{
			"manual": true, "run_missed": false,
		}}, "even run_missed = false"},
		{"manual with times of day", map[string]any{"target": "docker@host1", "scheduling": map[string]any{
			"manual": true, "time_of_day": []any{map[string]any{"hour": 3, "minute": 0}},
		}}, "cannot be set together with manual = true"},
		{"manual with no_parent_time_of_day", map[string]any{"target": "docker@host1", "scheduling": map[string]any{
			"manual": true, "no_parent_time_of_day": true,
		}}, "cannot be set together with manual = true"},
		{"max_parallel_snapshots on a path", map[string]any{"target": "docker@host1:/data", "upload": map[string]any{
			"max_parallel_snapshots": 2,
		}}, "Setting not allowed on a path policy"},
		{"action without command or script", map[string]any{"target": "docker@host1", "actions": map[string]any{
			"before_snapshot_root": map[string]any{"mode": "essential"},
		}}, "No attribute specified"},
		{"action with command and script", map[string]any{"target": "docker@host1", "actions": map[string]any{
			"after_snapshot_root": map[string]any{"command": "/bin/true", "script": "true"},
		}}, "2 attributes specified"},
		{"args without command", map[string]any{"target": "docker@host1", "actions": map[string]any{
			"after_folder": map[string]any{"script": "true", "args": []string{"x"}},
		}}, "must be specified when"},
		{"unknown action mode", map[string]any{"target": "docker@host1", "actions": map[string]any{
			"after_folder": map[string]any{"script": "true", "mode": "fatal"},
		}}, "value must be one of"},
		{"zero timeout", map[string]any{"target": "docker@host1", "actions": map[string]any{
			"after_folder": map[string]any{"script": "true", "timeout_seconds": 0},
		}}, "must be between 1"},
		{"oversized script", map[string]any{"target": "docker@host1", "actions": map[string]any{
			"after_folder": map[string]any{"script": strings.Repeat("x", 32001)},
		}}, "string length must be between 1 and 32000"},
		{"zero interval", map[string]any{"target": "docker@host1", "scheduling": map[string]any{"interval_seconds": 0}}, "must be at least 1"},
		{"negative interval", map[string]any{"target": "docker@host1", "scheduling": map[string]any{"interval_seconds": -60}}, "must be at least 1"},
		{"empty cron list", map[string]any{"target": "docker@host1", "scheduling": map[string]any{"cron": []string{}}}, "at least 1"},
		{"empty cron entry", map[string]any{"target": "docker@host1", "scheduling": map[string]any{"cron": []string{""}}}, "at least 1"},
		{"hour 24", map[string]any{"target": "docker@host1", "scheduling": map[string]any{
			"time_of_day": []any{map[string]any{"hour": 24, "minute": 0}},
		}}, "between 0 and 23"},
		{"minute 60", map[string]any{"target": "docker@host1", "scheduling": map[string]any{
			"time_of_day": []any{map[string]any{"hour": 3, "minute": 60}},
		}}, "between 0 and 59"},
		{"empty time_of_day", map[string]any{"target": "docker@host1", "scheduling": map[string]any{"time_of_day": []any{}}}, "at least 1"},
		{"negative retention", map[string]any{"target": "docker@host1", "retention": map[string]any{"keep_latest": -1}}, "between 0 and"},
		{"zero max_file_size", map[string]any{"target": "docker@host1", "files": map[string]any{"max_file_size": 0}}, "or -1 for no limit"},
		{"max_file_size -2", map[string]any{"target": "docker@host1", "files": map[string]any{"max_file_size": -2}}, "or -1 for no limit"},
		{"zero min_size", map[string]any{"target": "docker@host1", "compression": map[string]any{"min_size": 0}}, "or -1 for no limit"},
		{"zero parallel_upload_above_size", map[string]any{"target": "docker@host1", "upload": map[string]any{"parallel_upload_above_size": 0}}, "must not be 0"},
		{"zero max_parallel_snapshots", map[string]any{"target": "docker@host1", "upload": map[string]any{"max_parallel_snapshots": 0}}, "must be between 1"},
		{"extension without a dot", map[string]any{"target": "docker@host1", "compression": map[string]any{"never_compress": []string{"gz"}}}, "leading dot"},
		{"empty compressor name", map[string]any{"target": "docker@host1", "compression": map[string]any{"compressor_name": ""}}, "at least 1"},
		{"empty splitter", map[string]any{"target": "docker@host1", "splitter": map[string]any{"algorithm": ""}}, "at least 1"},
		{"unknown shadow copy mode", map[string]any{"target": "docker@host1", "os_snapshots": map[string]any{"volume_shadow_copy": "sometimes"}}, "value must be one of"},
		{"log level 11", map[string]any{"target": "docker@host1", "logging": map[string]any{"entries": map[string]any{"ignored": 11}}}, "between 0 and 10"},
		{"empty ignore list", map[string]any{"target": "docker@host1", "files": map[string]any{"ignore": []string{}}}, "at least 1"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			diags := h.validateResource(t, policyTestType, policyTestConfig(t, h, tc.attrs))
			requireProtocolErrorContaining(t, diags, tc.expect)
		})
	}

	valid := []struct {
		name  string
		attrs map[string]any
	}{
		{"manual with no_parent_time_of_day false", map[string]any{"target": "docker@host1", "scheduling": map[string]any{
			"manual": true, "no_parent_time_of_day": false,
		}}},
		{"manual with an unknown cron", map[string]any{"target": "docker@host1", "scheduling": map[string]any{
			"manual": true, "cron": tfUnknown,
		}}},
		{"max_parallel_snapshots on user@host", map[string]any{"target": "docker@host1", "upload": map[string]any{"max_parallel_snapshots": 4}}},
		{"max_parallel_snapshots on (global)", map[string]any{"target": "(global)", "upload": map[string]any{"max_parallel_snapshots": 4}}},
		{"max_parallel_snapshots with an unknown target", map[string]any{"target": tfUnknown, "upload": map[string]any{"max_parallel_snapshots": 4}}},
		{"no limits", map[string]any{"target": "docker@host1", "files": map[string]any{"max_file_size": -1},
			"compression": map[string]any{"min_size": -1, "max_size": -1}, "upload": map[string]any{"parallel_upload_above_size": -1}}},
		{"command with args", map[string]any{"target": "docker@host1", "actions": map[string]any{
			"before_folder": map[string]any{"command": "/bin/echo", "args": []string{"hi"}},
		}}},
		{"Windows path", map[string]any{"target": `Administrator@WIN1:\\srv\share\dir`}},
		{"root path", map[string]any{"target": "docker@host1:/"}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			requireNoProtocolErrors(t, h.validateResource(t, policyTestType, policyTestConfig(t, h, tc.attrs)))
		})
	}
}

// The warnings about values Kopia reads differently from how they look.
func TestPolicyResourceValidationWarnings(t *testing.T) {
	h := policyTestHarness(t, newFakeKopia())
	warned := []struct {
		name   string
		attrs  map[string]any
		expect string
	}{
		{"n-th weekday", map[string]any{"cron": []string{"0 0 * * 5#3"}}, "Cron entry truncated at #"},
		{"comment only", map[string]any{"cron": []string{"# disabled"}}, "Cron entry without a schedule"},
	}
	for _, tc := range warned {
		t.Run(tc.name, func(t *testing.T) {
			diags := h.validateResource(t, policyTestType, policyTestConfig(t, h, map[string]any{
				"target": "docker@host1", "scheduling": tc.attrs,
			}))
			requireNoProtocolErrors(t, diags)
			requireProtocolWarningContaining(t, diags, tc.expect)
		})
	}
	for rule, expect := range map[string]string{`cache\`: "ends in a backslash", "[abc": "leaves a [ unclosed"} {
		diags := h.validateResource(t, policyTestType, policyTestConfig(t, h, map[string]any{
			"target": "docker@host1", "files": map[string]any{"ignore": []string{rule}},
		}))
		requireNoProtocolErrors(t, diags)
		requireProtocolWarningContaining(t, diags, expect)
	}

	// A comment after a space, and rules that are fine, pass silently.
	diags := h.validateResource(t, policyTestType, policyTestConfig(t, h, map[string]any{
		"target":     "docker@host1",
		"scheduling": map[string]any{"cron": []string{"0 3 * * * # nightly", "30 4 * * 1-5"}},
		"files":      map[string]any{"ignore": []string{"*.tmp", `\[literal`, "[abc]", `escaped\\`}},
	}))
	requireNoProtocolErrors(t, diags)
	for _, d := range diags {
		t.Errorf("unexpected %s: %s: %s", d.Severity, d.Summary, d.Detail)
	}
}

// --- plan-time checks with Kopia ------------------------------------------------

// Kopia parses cron entries; the provider asks it at plan time, with the
// planned policy as the updates of a resolve.
func TestPolicyResourcePlanChecksSchedulingWithKopia(t *testing.T) {
	fake := newFakeKopia()
	newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)

	planned := h.plan(t, policyTestType, nil, policyTestConfig(t, h, map[string]any{
		"target":     "docker@host1",
		"retention":  map[string]any{"keep_daily": 7},
		"scheduling": map[string]any{"cron": []string{"0 3 * * *", "invalid entry"}},
	}))
	requireProtocolErrorContaining(t, planned.Diagnostics, "Invalid scheduling")
	requireProtocolErrorContaining(t, planned.Diagnostics, `invalid cron expression "invalid entry"`)
	call := fake.lastCall(t, "POST /api/v1/policy/resolve")
	if got := string(call.body); got != `{"updates":{"retention":{"keepDaily":7},"scheduling":{"cron":["0 3 * * *","invalid entry"]}},"numUpcomingSnapshotTimes":0}` {
		t.Errorf("resolve body = %s", got)
	}
	if got := call.query.Encode(); got != "host=host1&userName=docker" {
		t.Errorf("resolve query = %q", got)
	}
	if fake.called("PUT /api/v1/policy") {
		t.Error("planning wrote a policy")
	}

	// A valid section passes; one without scheduling needs no request.
	planned = h.plan(t, policyTestType, nil, policyTestConfig(t, h, map[string]any{
		"target": "docker@host1", "scheduling": map[string]any{"cron": []string{"0 3 * * *"}},
	}))
	requireNoProtocolErrors(t, planned.Diagnostics)
	resolves := fake.count("POST /api/v1/policy/resolve")
	planned = h.plan(t, policyTestType, nil, policyTestConfig(t, h, map[string]any{
		"target": "docker@host1", "retention": map[string]any{"keep_daily": 7},
	}))
	requireNoProtocolErrors(t, planned.Diagnostics)
	if fake.count("POST /api/v1/policy/resolve") != resolves {
		t.Error("a policy without scheduling was resolved")
	}
}

// A failed resolve does not block the plan: Kopia checks again on PUT.
func TestPolicyResourcePlanWarnsWhenKopiaCannotCheck(t *testing.T) {
	fake := newFakeKopia()
	newPolicyTestStore(fake)
	fake.replyError("POST /api/v1/policy/resolve", http.StatusInternalServerError, kopiaclient.CodeInternal, "internal server error: unable to find policies")
	fake.replyError("GET /api/v1/repo/algorithms", http.StatusInternalServerError, kopiaclient.CodeInternal, "boom")
	h := policyTestHarness(t, fake)

	planned := h.plan(t, policyTestType, nil, policyTestConfig(t, h, map[string]any{
		"target":      "docker@host1",
		"scheduling":  map[string]any{"cron": []string{"0 3 * * *"}},
		"compression": map[string]any{"compressor_name": "zstd"},
	}))
	requireNoProtocolErrors(t, planned.Diagnostics)
	requireProtocolWarningContaining(t, planned.Diagnostics, "Unable to check the scheduling with Kopia")
	requireProtocolWarningContaining(t, planned.Diagnostics, "Unable to check algorithm names")
}

func TestPolicyResourcePlanChecksAlgorithmNames(t *testing.T) {
	tests := []struct {
		name    string
		version string
		attrs   map[string]any
		path    string
		error   string
		warning string
	}{
		{name: "unknown compressor", attrs: map[string]any{"compression": map[string]any{"compressor_name": "zstd-ultra"}},
			path: "compression.compressor_name", error: "Unknown compressor"},
		{name: "unknown metadata compressor", attrs: map[string]any{"metadata_compression": map[string]any{"compressor_name": "brotli"}},
			path: "metadata_compression.compressor_name", error: "Unknown compressor"},
		{name: "unknown splitter", attrs: map[string]any{"splitter": map[string]any{"algorithm": "DYNAMIC-3M-BUZHASH"}},
			path: "splitter.algorithm", error: "Unknown splitter"},
		{name: "lz4 on 0.23", attrs: map[string]any{"compression": map[string]any{"compressor_name": "lz4"}},
			path: "compression.compressor_name", error: "Compressor no longer supported"},
		{name: "lz4 metadata on 0.23", attrs: map[string]any{"metadata_compression": map[string]any{"compressor_name": "lz4"}},
			path: "metadata_compression.compressor_name", error: "Compressor no longer supported"},
		{name: "lz4 on 0.22", version: "0.22.3", attrs: map[string]any{"compression": map[string]any{"compressor_name": "lz4"}},
			path: "compression.compressor_name", warning: "lists the compressor \"lz4\" as deprecated"},
		{name: "lz4 on an unknown version", version: "-", attrs: map[string]any{"compression": map[string]any{"compressor_name": "lz4"}},
			path: "compression.compressor_name", warning: "cannot tell whether it is affected"},
		{name: "deprecated compressor", attrs: map[string]any{"compression": map[string]any{"compressor_name": "zstd-best-compression"}},
			path: "compression.compressor_name", warning: "Compressor deprecated"},
		{name: "none", attrs: map[string]any{"compression": map[string]any{"compressor_name": "none"}, "metadata_compression": map[string]any{"compressor_name": "none"}}},
		{name: "known names", attrs: map[string]any{"compression": map[string]any{"compressor_name": "s2-default"}, "splitter": map[string]any{"algorithm": "FIXED-1M"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			switch tc.version {
			case "":
			case "-":
				fake.title = "KopiaUI"
			default:
				fake.title = "KopiaUI v" + tc.version
			}
			newPolicyTestStore(fake)
			h := policyTestHarness(t, fake)

			attrs := map[string]any{"target": "docker@host1"}
			for k, v := range tc.attrs {
				attrs[k] = v
			}
			planned := h.plan(t, policyTestType, nil, policyTestConfig(t, h, attrs))
			switch {
			case tc.error != "":
				requireProtocolErrorContaining(t, planned.Diagnostics, tc.error)
				policyTestRequireProtocolPath(t, planned.Diagnostics, tc.path)
			case tc.warning != "":
				requireNoProtocolErrors(t, planned.Diagnostics)
				requireProtocolWarningContaining(t, planned.Diagnostics, tc.warning)
				policyTestRequireProtocolPath(t, planned.Diagnostics, tc.path)
			default:
				requireNoProtocolErrors(t, planned.Diagnostics)
				for _, d := range planned.Diagnostics {
					t.Errorf("unexpected %s: %s: %s", d.Severity, d.Summary, d.Detail)
				}
			}
		})
	}
}

// policyTestRequireProtocolPath fails the test unless a diagnostic is
// attached to the attribute at dotted.
func policyTestRequireProtocolPath(t *testing.T, diags []*tfprotov6.Diagnostic, dotted string) {
	t.Helper()
	want := tftypes.NewAttributePath()
	for _, name := range strings.Split(dotted, ".") {
		want = want.WithAttributeName(name)
	}
	for _, d := range diags {
		if d.Attribute != nil && d.Attribute.Equal(want) {
			return
		}
	}
	t.Fatalf("no diagnostic on %s", dotted)
}

// Nothing is checked with Kopia while a value is unknown, and a plan that
// changes nothing makes no requests: refreshes stay cheap, and a policy Kopia
// accepted once does not fail later plans.
func TestPolicyResourcePlanSkipsTheChecksItCannotOrNeedNotMake(t *testing.T) {
	fake := newFakeKopia()
	newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)

	planned := h.plan(t, policyTestType, nil, policyTestConfig(t, h, map[string]any{
		"target":      "docker@host1",
		"scheduling":  map[string]any{"cron": tfUnknown},
		"compression": map[string]any{"compressor_name": "zstd-ultra"},
	}))
	requireNoProtocolErrors(t, planned.Diagnostics)
	if fake.called("POST /api/v1/policy/resolve") || fake.called("GET /api/v1/repo/algorithms") {
		t.Errorf("a plan with unknown values was checked with Kopia: %v", fake.apiRoutes())
	}
	if got := stringAt(t, h.decodeResource(t, policyTestType, planned.PlannedState), "id"); got != "docker@host1" {
		t.Errorf("planned id = %q, want the target even while other values are unknown", got)
	}

	config := policyTestConfig(t, h, map[string]any{
		"target":      "docker@host1",
		"scheduling":  map[string]any{"cron": []string{"0 3 * * *"}},
		"compression": map[string]any{"compressor_name": "zstd"},
	})
	inst := policyTestApply(t, h, nil, config)
	before := len(fake.apiRoutes())
	replanned := h.plan(t, policyTestType, inst, config)
	requireNoProtocolErrors(t, replanned.Diagnostics)
	if got := fake.apiRoutes()[before:]; len(got) != 0 {
		t.Errorf("an unchanged plan made requests: %v", got)
	}

	// Planning a destroy checks nothing either.
	destroyed := h.plan(t, policyTestType, inst, tftypes.NewValue(h.resourceType(t, policyTestType), nil))
	requireNoProtocolErrors(t, destroyed.Diagnostics)
	if got := fake.apiRoutes()[before:]; len(got) != 0 {
		t.Errorf("planning a destroy made requests: %v", got)
	}
}

// A policy applied with lz4 before the server moved to 0.23 is not an error
// on every later plan, which would block unrelated changes, only a warning.
func TestPolicyResourcePlanWarnsAboutAnAppliedLZ4(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	store.putRaw(t, "docker@host1", `{"compression":{"compressorName":"lz4"}}`)
	h := policyTestHarness(t, fake)

	inst := h.importByID(t, policyTestType, "docker@host1")
	config := policyTestConfig(t, h, map[string]any{
		"target": "docker@host1", "compression": map[string]any{"compressor_name": "lz4"},
	})
	planned := h.plan(t, policyTestType, inst, config)
	requireNoProtocolErrors(t, planned.Diagnostics)
	requireProtocolWarningContaining(t, planned.Diagnostics, "Compressor no longer supported")
}

// --- identity and import --------------------------------------------------------

func TestPolicyResourceImport(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	store.putRaw(t, "docker@host1", `{"scheduling":{"manual":true},"noParent":true}`)
	h := policyTestHarness(t, fake)

	byID := h.importByID(t, policyTestType, "docker@host1")
	if got := stringAt(t, byID.state, "target"); got != "docker@host1" {
		t.Errorf("target = %q", got)
	}
	if got := stringAt(t, byID.state, "id"); got != "docker@host1" {
		t.Errorf("id = %q", got)
	}
	if !boolAt(t, byID.state, "scheduling", "manual") || !boolAt(t, byID.state, "no_parent") {
		t.Errorf("state = %s", byID.state)
	}
	if got := stringAt(t, h.identityValue(t, policyTestType, byID.identity), "target"); got != "docker@host1" {
		t.Errorf("identity target = %q", got)
	}

	byIdentity := h.importByIdentity(t, policyTestType, map[string]any{"target": "docker@host1"})
	policyTestRequireEqual(t, "import by identity", byIdentity.state, "import by id", byID.state)

	// The identity stays the same through a refresh.
	refreshed := h.refresh(t, byIdentity)
	if got := stringAt(t, h.identityValue(t, policyTestType, refreshed.identity), "target"); got != "docker@host1" {
		t.Errorf("identity after refresh = %q", got)
	}

	// The imported policy plans cleanly against a configuration that matches.
	planned := h.plan(t, policyTestType, byID, policyTestConfig(t, h, map[string]any{
		"target": "docker@host1", "scheduling": map[string]any{"manual": true}, "no_parent": true,
	}))
	requireNoProtocolErrors(t, planned.Diagnostics)
	policyTestRequireEqual(t, "plan after import", h.decodeResource(t, policyTestType, planned.PlannedState), "imported state", byID.state)
}

func TestPolicyResourceImportRefusesAnInvalidTarget(t *testing.T) {
	fake := newFakeKopia()
	newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)

	for _, id := range []string{"docker@host1:/data/", "host1", " (global)"} {
		resp := h.importResource(t, policyTestType, id, nil)
		requireProtocolErrorContaining(t, resp.Diagnostics, "Unexpected import identifier")
	}
	resp := h.importResource(t, policyTestType, "", h.identityData(t, policyTestType, map[string]any{"target": "docker@"}))
	requireProtocolErrorContaining(t, resp.Diagnostics, "Unexpected import identifier")
	resp = h.importResource(t, policyTestType, "", h.identityData(t, policyTestType, map[string]any{"target": ""}))
	requireProtocolErrorContaining(t, resp.Diagnostics, "Missing import identity")
	if got := fake.apiRoutes(); len(got) != 0 {
		t.Errorf("a refused import made requests: %v", got)
	}
}

// Importing a target without a policy reads nothing back, which Terraform
// reports as a missing remote object.
func TestPolicyResourceImportOfAMissingPolicy(t *testing.T) {
	fake := newFakeKopia()
	newPolicyTestStore(fake)
	h := policyTestHarness(t, fake)

	imported := h.importResource(t, policyTestType, "docker@host1", nil)
	requireNoProtocolErrors(t, imported.Diagnostics)
	inst := &resourceInstance{
		typeName: policyTestType,
		state:    h.decodeResource(t, policyTestType, imported.ImportedResources[0].State),
		identity: imported.ImportedResources[0].Identity,
	}
	read := h.read(t, inst)
	requireNoProtocolErrors(t, read.Diagnostics)
	if got := h.decodeResource(t, policyTestType, read.NewState); !got.IsNull() {
		t.Errorf("state = %s, want none", got)
	}
}

// --- acceptance -----------------------------------------------------------------

// testAccCheckPolicyStored compares the policy Kopia stores for target with
// want, as compact JSON in the provider's normalised form; "" means none.
func testAccCheckPolicyStored(target, want string) acc.TestCheckFunc {
	return func(*terraform.State) error {
		c, err := testAccClient()
		if err != nil {
			return err
		}
		si, err := kopiaclient.ParseTarget(target)
		if err != nil {
			return err
		}
		p, err := c.GetPolicy(context.Background(), si)
		switch {
		case kopiaclient.IsNotFound(err) && want == "":
			return nil
		case err != nil:
			return fmt.Errorf("reading the policy of %s: %w", target, err)
		case want == "":
			return fmt.Errorf("%s still has a policy", target)
		}
		got, err := p.MarshalJSON()
		if err != nil {
			return err
		}
		if string(got) != want {
			return fmt.Errorf("%s stores %s, want %s", target, got, want)
		}
		return nil
	}
}

// testAccPolicyHost is a host no snapshot source of the acceptance server
// has, so the policies the tests write affect nothing else.
const testAccPolicyHost = "tfacc-policy"

// The global policy exists on every repository: kopia_policy adopts it,
// changes it, imports it and, when destroyed, resets it to Kopia's defaults.
func TestAccPolicyResource_global(t *testing.T) {
	defaults, _ := kopiaclient.DefaultGlobalPolicy().MarshalJSON()
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPolicyStored("(global)", string(defaults)),
		Steps: []acc.TestStep{
			{
				// Adopting the global policy replaces it as a whole.
				Config: providerConfig + `
resource "kopia_policy" "global" {
  target = "(global)"

  retention = {
    keep_latest  = 10
    keep_daily   = 14
    keep_weekly  = 8
    keep_monthly = 12
  }

  compression = {
    compressor_name = "zstd"
  }

  files = {
    ignore_cache_dirs = true
    one_file_system   = true
  }
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("kopia_policy.global", "id", "(global)"),
					acc.TestCheckResourceAttr("kopia_policy.global", "retention.keep_daily", "14"),
					acc.TestCheckNoResourceAttr("kopia_policy.global", "retention.keep_hourly"),
					acc.TestCheckResourceAttr("kopia_policy.global", "compression.compressor_name", "zstd"),
					testAccCheckPolicyStored("(global)", `{"retention":{"keepLatest":10,"keepDaily":14,"keepWeekly":8,"keepMonthly":12},"files":{"ignoreCacheDirs":true,"oneFileSystem":true},"compression":{"compressorName":"zstd"}}`),
				),
			},
			{
				// Import must round-trip without producing a diff.
				ResourceName:      "kopia_policy.global",
				ImportState:       true,
				ImportStateId:     "(global)",
				ImportStateVerify: true,
			},
			{
				// An update changes values and drops what is left out.
				Config: providerConfig + `
resource "kopia_policy" "global" {
  target = "(global)"

  retention = {
    keep_latest = 5
  }

  metadata_compression = {
    compressor_name = "zstd-fastest"
  }
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("kopia_policy.global", "retention.keep_latest", "5"),
					acc.TestCheckNoResourceAttr("kopia_policy.global", "compression.compressor_name"),
					testAccCheckPolicyStored("(global)", `{"retention":{"keepLatest":5},"metadataCompression":{"compressorName":"zstd-fastest"}}`),
				),
			},
		},
	})
}

// A user@host policy with every section, through create, import, an update
// that keeps what Kopia cannot store, and destroy.
func TestAccPolicyResource_everySection(t *testing.T) {
	const address = "kopia_policy.user"
	target := "tfacc@" + testAccPolicyHost
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPolicyStored(target, ""),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
resource "kopia_policy" "user" {
  target = %q

  retention = {
    keep_latest                = 10
    keep_hourly                = 0
    keep_daily                 = 14
    keep_weekly                = 4
    keep_monthly               = 24
    keep_annual                = 3
    ignore_identical_snapshots = false
  }

  files = {
    ignore              = ["*.tmp", "/cache"]
    no_parent_ignore    = true
    ignore_dot_files    = [".kopiaignore", ".gitignore"]
    no_parent_dot_files = true
    ignore_cache_dirs   = true
    max_file_size       = -1
    one_file_system     = false
  }

  error_handling = {
    ignore_file_errors      = true
    ignore_directory_errors = false
    ignore_unknown_types    = true
  }

  scheduling = {
    interval_seconds      = 3600
    time_of_day           = [{ hour = 3, minute = 15 }, { hour = 15, minute = 0 }]
    no_parent_time_of_day = true
    cron                  = ["0 3 * * * # nightly"]
    run_missed            = false
  }

  compression = {
    compressor_name          = "zstd"
    only_compress            = [".txt"]
    no_parent_only_compress  = true
    never_compress           = [".gz", ".jpg"]
    no_parent_never_compress = true
    min_size                 = 1024
    max_size                 = -1
  }

  metadata_compression = {
    compressor_name = "zstd-fastest"
  }

  splitter = {
    algorithm = "DYNAMIC-4M-BUZHASH"
  }

  actions = {
    before_folder = {
      command = "/bin/true"
    }
    before_snapshot_root = {
      script          = "#!/bin/sh\necho before"
      timeout_seconds = 300
      mode            = "essential"
    }
    after_snapshot_root = {
      command = "/bin/echo"
      args    = ["done"]
      mode    = "async"
    }
  }

  os_snapshots = {
    volume_shadow_copy = "when-available"
  }

  logging = {
    directories = {
      snapshotted = 5
      ignored     = 0
    }
    entries = {
      cache_hit  = 0
      cache_miss = 10
    }
  }

  upload = {
    max_parallel_snapshots     = 2
    max_parallel_file_reads    = 8
    parallel_upload_above_size = -1
  }

  no_parent = true
}
`, target),
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "id", target),
					acc.TestCheckResourceAttr(address, "retention.keep_hourly", "0"),
					acc.TestCheckResourceAttr(address, "files.one_file_system", "false"),
					acc.TestCheckResourceAttr(address, "scheduling.time_of_day.#", "2"),
					acc.TestCheckTypeSetElemNestedAttrs(address, "scheduling.time_of_day.*", map[string]string{"hour": "3", "minute": "15"}),
					acc.TestCheckResourceAttr(address, "os_snapshots.volume_shadow_copy", "when-available"),
					acc.TestCheckResourceAttr(address, "actions.after_snapshot_root.args.0", "done"),
					acc.TestCheckNoResourceAttr(address, "actions.after_folder.command"),
					acc.TestCheckResourceAttr(address, "no_parent", "true"),
					testAccCheckPolicyStored(target, policyTestFullBodyForAcc),
				),
			},
			{
				// Everything set above can be imported: no plain false, no
				// empty section.
				ResourceName:      address,
				ImportState:       true,
				ImportStateId:     target,
				ImportStateVerify: true,
			},
			{
				// Values Kopia cannot store are sent as nothing and still read
				// back as configured, so the plan after the apply is empty.
				Config: providerConfig + fmt.Sprintf(`
resource "kopia_policy" "user" {
  target = %q

  retention = {}

  scheduling = {
    manual                = false
    no_parent_time_of_day = false
  }

  no_parent = false
}
`, target),
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "scheduling.manual", "false"),
					acc.TestCheckResourceAttr(address, "no_parent", "false"),
					testAccCheckPolicyStored(target, "{}"),
				),
			},
		},
	})
}

// policyTestFullBodyForAcc is what TestAccPolicyResource_everySection stores,
// in the client's normalised encoding.
const policyTestFullBodyForAcc = `{"retention":{"keepLatest":10,"keepHourly":0,"keepDaily":14,"keepWeekly":4,"keepMonthly":24,"keepAnnual":3,"ignoreIdenticalSnapshots":false},` +
	`"files":{"ignore":["*.tmp","/cache"],"noParentIgnore":true,"ignoreDotFiles":[".kopiaignore",".gitignore"],"noParentDotFiles":true,"ignoreCacheDirs":true,"maxFileSize":-1,"oneFileSystem":false},` +
	`"errorHandling":{"ignoreFileErrors":true,"ignoreDirectoryErrors":false,"ignoreUnknownTypes":true},` +
	`"scheduling":{"intervalSeconds":3600,"timeOfDay":[{"hour":3,"min":15},{"hour":15,"min":0}],"noParentTimeOfDay":true,"cron":["0 3 * * * # nightly"],"runMissed":false},` +
	`"compression":{"compressorName":"zstd","onlyCompress":[".txt"],"noParentOnlyCompress":true,"neverCompress":[".gz",".jpg"],"noParentNeverCompress":true,"minSize":1024,"maxSize":-1},` +
	`"metadataCompression":{"compressorName":"zstd-fastest"},"splitter":{"algorithm":"DYNAMIC-4M-BUZHASH"},` +
	`"actions":{"beforeFolder":{"path":"/bin/true"},"beforeSnapshotRoot":{"script":"#!/bin/sh\necho before","timeout":300,"mode":"essential"},"afterSnapshotRoot":{"path":"/bin/echo","args":["done"],"mode":"async"}},` +
	`"osSnapshots":{"volumeShadowCopy":{"enable":2}},` +
	`"logging":{"directories":{"snapshotted":5,"ignored":0},"entries":{"cacheHit":0,"cacheMiss":10}},` +
	`"upload":{"maxParallelSnapshots":2,"maxParallelFileReads":8,"parallelUploadAboveSize":-1},"noParent":true}`

// A path policy of the kind kopia-docker-watcher writes, and a manual one.
func TestAccPolicyResource_pathAndManual(t *testing.T) {
	path := "tfacc@" + testAccPolicyHost + ":/data/app"
	user := "tfacc-paused@" + testAccPolicyHost
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: acc.ComposeAggregateTestCheckFunc(
			testAccCheckPolicyStored(path, ""),
			testAccCheckPolicyStored(user, ""),
		),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
resource "kopia_policy" "path" {
  target = %q

  scheduling = {
    cron = ["30 2 * * *"]
  }

  files = {
    ignore = ["node_modules/"]
  }
}

resource "kopia_policy" "paused" {
  target = %q

  scheduling = {
    manual = true
  }
}
`, path, user),
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("kopia_policy.path", "id", path),
					acc.TestCheckResourceAttr("kopia_policy.paused", "scheduling.manual", "true"),
					testAccCheckPolicyStored(path, `{"files":{"ignore":["node_modules/"]},"scheduling":{"cron":["30 2 * * *"]}}`),
					testAccCheckPolicyStored(user, `{"scheduling":{"manual":true}}`),
				),
			},
			{
				ResourceName:      "kopia_policy.path",
				ImportState:       true,
				ImportStateId:     path,
				ImportStateVerify: true,
			},
		},
	})
}

// Kopia refuses what it cannot parse; the provider asks it at plan time, so
// the plan fails before anything is written.
func TestAccPolicyResource_invalidCron(t *testing.T) {
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []acc.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
resource "kopia_policy" "bad" {
  target = "tfacc-cron@%s"

  scheduling = {
    cron = ["not a cron expression"]
  }
}
`, testAccPolicyHost),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`invalid cron expression`),
			},
		},
	})
}

// A policy that already exists, written here as another tool would, is not
// taken over by a create.
func TestAccPolicyResource_existingPolicy(t *testing.T) {
	target := kopiaclient.SourceInfo{Host: testAccPolicyHost + "-taken"}
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []acc.TestStep{
			{
				PreConfig: func() {
					c, err := testAccClient()
					if err != nil {
						t.Fatal(err)
					}
					keep := int64(3)
					if err := c.SetPolicy(context.Background(), target, &kopiaclient.Policy{
						Retention: &kopiaclient.RetentionPolicy{KeepLatest: &keep},
					}); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = c.DeletePolicy(context.Background(), target) })
				},
				Config: providerConfig + fmt.Sprintf(`
resource "kopia_policy" "taken" {
  target = %q
}
`, target.String()),
				ExpectError: regexp.MustCompile(`Kopia policy already exists`),
			},
			{
				// The policy is untouched.
				Config: providerConfig,
				Check:  testAccCheckPolicyStored(target.String(), `{"retention":{"keepLatest":3}}`),
			},
		},
	})
}

// Import blocks that name the policy by its identity, from Terraform 1.12.
func TestAccPolicyResource_importByIdentity(t *testing.T) {
	const address = "kopia_policy.identity"
	target := "@" + testAccPolicyHost
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			// Resource identity, and import blocks that use one, arrived in
			// Terraform 1.12.
			tfversion.SkipBelow(tfversion.Version1_12_0),
		},
		CheckDestroy: testAccCheckPolicyStored(target, ""),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
resource "kopia_policy" "identity" {
  target = %q

  upload = {
    max_parallel_file_reads = 4
  }
}
`, target),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectIdentityValue(address, tfjsonpath.New("target"), knownvalue.StringExact(target)),
					statecheck.ExpectIdentityValueMatchesState(address, tfjsonpath.New("target")),
				},
			},
			{
				ResourceName:    address,
				ImportState:     true,
				ImportStateKind: acc.ImportBlockWithResourceIdentity,
			},
			{
				// The import must plan no change, which the test framework
				// checks for import blocks; the id is the target.
				ResourceName:    address,
				ImportState:     true,
				ImportStateKind: acc.ImportBlockWithID,
				ImportStateId:   target,
			},
		},
	})
}
