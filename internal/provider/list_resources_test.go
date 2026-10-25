package provider

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/querycheck"
	"github.com/hashicorp/terraform-plugin-testing/querycheck/queryfilter"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// These tests run the list resources through the protocol server, as
// `terraform query` does: list, with or without include_resource, and with a
// limit. A listed resource must be exactly what importing the object and
// refreshing it records, so that configuration generated from the results
// plans no change; several tests compare the two directly.

// listedIdentities returns the identity attribute of every result, in order.
func listedIdentities(t *testing.T, h *protocolHarness, typeName, attribute string, results []tfprotov6.ListResourceResult) []string {
	t.Helper()
	var ids []string
	for _, r := range results {
		ids = append(ids, stringAt(t, h.identityValue(t, typeName, r.Identity), attribute))
	}
	return ids
}

// The list resources are those of the resources with an identity, under the
// same type names; kopia_policy takes the filters of GET /api/v1/policies,
// kopia_notification_profile nothing.
func TestListResourceSchemas(t *testing.T) {
	h := newProtocolHarness(t, newFakeKopia())
	want := map[string][]string{
		"kopia_policy":               {"host", "path", "user_name"},
		"kopia_notification_profile": nil,
	}
	if got := harnessSortedKeys(h.schemas.ListResourceSchemas); !reflect.DeepEqual(got, harnessSortedKeys(want)) {
		t.Fatalf("list resources = %v, want %v", got, harnessSortedKeys(want))
	}
	for typeName, wantAttrs := range want {
		var attrs []string
		for _, a := range h.listResourceSchema(t, typeName).Block.Attributes {
			if !a.Optional || a.Required || a.Computed {
				t.Errorf("%s: argument %s is not merely optional", typeName, a.Name)
			}
			attrs = append(attrs, a.Name)
		}
		sort.Strings(attrs)
		if !reflect.DeepEqual(attrs, wantAttrs) {
			t.Errorf("%s arguments = %v, want %v", typeName, attrs, wantAttrs)
		}
		h.identityType(t, typeName) // fails the test when the resource has no identity
	}
}

// policyListListingJSON is GET /api/v1/policies holding two manifests for
// docker@host1 — the later one is the newer — and four targets the strict
// parser refuses, which only other clients write. The last of them, user
// docker on host "host1:/data", writes as docker@host1:/data, which the
// parser reads as another target, a path of host1: importing it would
// address a policy other than the one listed.
const policyListListingJSON = `{
  "policies": [
    {"id": "a1", "target": {"host": "host1", "userName": "docker", "path": ""}, "policy": {"retention": {"keepDaily": 7}}},
    {"id": "a2", "target": {"host": "", "userName": "", "path": ""}, "policy": {"retention": {"keepLatest": 10}}},
    {"id": "a3", "target": {"host": "host1", "userName": "", "path": ""}, "policy": {}},
    {"id": "a4", "target": {"host": "", "userName": "docker", "path": ""}, "policy": {}},
    {"id": "a5", "target": {"host": "host1", "userName": "", "path": "/data"}, "policy": {}},
    {"id": "a6", "target": {"host": "host1", "userName": "docker", "path": "/data/../etc"}, "policy": {}},
    {"id": "a7", "target": {"host": "host1", "userName": "docker", "path": ""}, "policy": {"retention": {"keepDaily": 14}}},
    {"id": "a8", "target": {"host": "host1:/data", "userName": "docker", "path": ""}, "policy": {}}
  ]
}
`

// Each target is listed once, with its newest policy, sorted by target and
// named after it. The targets kopia_policy refuses are left out, and a warning
// that names them travels with the first result.
func TestPolicyListReportsEachTargetOnce(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/policies", http.StatusOK, policyListListingJSON)
	h := newProtocolHarness(t, fake)

	results := h.list(t, policyTestType, h.listValue(t, policyTestType, nil), true, 0)
	requireNoProtocolErrors(t, listDiagnostics(results))
	want := []string{"(global)", "@host1", "docker@host1"}
	if got := listedIdentities(t, h, policyTestType, "target", results); !reflect.DeepEqual(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for i, r := range results {
		if r.DisplayName != want[i] {
			t.Errorf("result %d display name = %q, want %q", i, r.DisplayName, want[i])
		}
	}

	for _, refused := range []string{`"docker@"`, `"@host1:/data"`, `"docker@host1:/data/../etc"`, `"docker@host1:/data"`} {
		requireProtocolWarningContaining(t, results[0].Diagnostics, refused)
	}
	if len(results[1].Diagnostics)+len(results[2].Diagnostics) != 0 {
		t.Errorf("the warning was repeated: %v, %v", results[1].Diagnostics, results[2].Diagnostics)
	}

	typ := h.resourceType(t, policyTestType)
	if got := int64At(t, decodeDynamicValue(t, results[2].Resource, typ), "retention", "keep_daily"); got != 14 {
		t.Errorf("docker@host1 keep_daily = %d, want 14 from the newer manifest", got)
	}
	if got := stringAt(t, decodeDynamicValue(t, results[0].Resource, typ), "id"); got != "(global)" {
		t.Errorf("(global) id = %q", got)
	}
	if got := fake.apiRoutes(); !reflect.DeepEqual(got, []string{"GET /api/v1/policies"}) {
		t.Errorf("calls = %v, want only the listing: it holds every policy", got)
	}
}

// With include_resource, every listed policy is what importing its target
// and refreshing it records, whichever release's shape Kopia answers in, so
// generated configuration plans no change.
func TestPolicyListIncludesTheResourceAsAnImportRecordsIt(t *testing.T) {
	for _, compact := range []bool{false, true} {
		fake := newFakeKopia()
		store := newPolicyTestStore(fake)
		store.compact = compact
		store.putRaw(t, "(global)", policyTestDefaultGlobalJSON)
		store.putRaw(t, "docker@host1", policyTestFullBody)
		store.putRaw(t, "docker@host1:/data/app", `{"scheduling":{"manual":true},"files":{}}`)
		store.putRaw(t, "@host1", `{}`)
		h := newProtocolHarness(t, fake)

		results := h.list(t, policyTestType, h.listValue(t, policyTestType, nil), true, 0)
		requireNoProtocolErrors(t, listDiagnostics(results))
		if len(results) != 4 {
			t.Fatalf("compact %v: listed %d policies, want 4", compact, len(results))
		}
		for _, r := range results {
			target := stringAt(t, h.identityValue(t, policyTestType, r.Identity), "target")
			listed := h.decodeResource(t, policyTestType, r.Resource)
			imported := h.importByID(t, policyTestType, target)
			if !listed.Equal(imported.state) {
				diffs, _ := listed.Diff(imported.state)
				t.Errorf("compact %v: %s listed differs from the import: %v", compact, target, diffs)
			}
		}
	}
}

// The filters reach Kopia as host, userName and path: Kopia ignores a
// lowercase username, which would list every user's policies.
func TestPolicyListSendsTheFilters(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	store.putRaw(t, "docker@host1", `{}`)
	store.putRaw(t, "docker@host1:/data/app", `{}`)
	store.putRaw(t, "root@host1", `{}`)
	store.putRaw(t, "docker@host2", `{}`)
	h := newProtocolHarness(t, fake)

	results := h.list(t, policyTestType, h.listValue(t, policyTestType, map[string]any{"host": "host1", "user_name": "docker"}), false, 0)
	requireNoProtocolErrors(t, listDiagnostics(results))
	if got := fake.query(t, "GET /api/v1/policies").Encode(); got != "host=host1&userName=docker" {
		t.Errorf("query = %s", got)
	}
	want := []string{"docker@host1", "docker@host1:/data/app"}
	if got := listedIdentities(t, h, policyTestType, "target", results); !reflect.DeepEqual(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for _, r := range results {
		if r.Resource != nil {
			t.Errorf("%s carries a resource although include_resource was not set", r.DisplayName)
		}
	}

	results = h.list(t, policyTestType, h.listValue(t, policyTestType, map[string]any{"path": "/data/app"}), false, 0)
	if got := listedIdentities(t, h, policyTestType, "target", results); !reflect.DeepEqual(got, []string{"docker@host1:/data/app"}) {
		t.Errorf("listed %v for the path filter", got)
	}
}

// An empty filter would mean the same as none, so it is refused.
func TestPolicyListRefusesEmptyFilters(t *testing.T) {
	h := newProtocolHarness(t, newFakeKopia())
	for _, name := range []string{"host", "user_name", "path"} {
		diags := h.validateList(t, policyTestType, h.listValue(t, policyTestType, map[string]any{name: ""}))
		requireProtocolErrorContaining(t, diags, "length must be at least 1")
	}
	requireNoProtocolErrors(t, h.validateList(t, policyTestType, h.listValue(t, policyTestType, map[string]any{"host": "host1"})))
}

// A list block without a config block reaches the provider as a null
// configuration rather than an empty one: Terraform nests the arguments in an
// optional config block and sends that block's value. It sets no filter, so
// every object is listed.
func TestListResourcesWithoutAConfigBlock(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	store.putRaw(t, "@host1", `{}`)
	store.putRaw(t, "docker@host1", `{"retention":{"keepDaily":7}}`)
	notificationListStore(t, fake)
	h := newProtocolHarness(t, fake)

	tests := map[string]struct {
		attribute string
		want      []string
	}{
		policyTestType:       {"target", []string{"@host1", "docker@host1"}},
		notificationTestType: {"name", []string{"mail", "ops", "phone"}},
	}
	for typeName, tc := range tests {
		config := tftypes.NewValue(h.listResourceSchema(t, typeName).ValueType(), nil)
		requireNoProtocolErrors(t, h.validateList(t, typeName, config))
		results := h.list(t, typeName, config, true, 0)
		requireNoProtocolErrors(t, listDiagnostics(results))
		if got := listedIdentities(t, h, typeName, tc.attribute, results); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s listed %v without a config block, want %v", typeName, got, tc.want)
		}
	}
	if got := fake.query(t, "GET /api/v1/policies").Encode(); got != "" {
		t.Errorf("policies query = %s, want no filter", got)
	}
}

func TestPolicyListStopsAtTheLimit(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	for _, target := range []string{"@host1", "@host2", "@host3"} {
		store.putRaw(t, target, `{}`)
	}
	h := newProtocolHarness(t, fake)

	results := h.list(t, policyTestType, h.listValue(t, policyTestType, nil), false, 2)
	if got := listedIdentities(t, h, policyTestType, "target", results); !reflect.DeepEqual(got, []string{"@host1", "@host2"}) {
		t.Fatalf("listed %v with a limit of 2", got)
	}
}

// With nothing but refused targets, the warning comes in a result of its own.
func TestPolicyListWithOnlyRefusedTargets(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/policies", http.StatusOK,
		`{"policies":[{"id":"a1","target":{"host":"","userName":"docker","path":""},"policy":{}}]}`)
	h := newProtocolHarness(t, fake)

	results := h.list(t, policyTestType, h.listValue(t, policyTestType, nil), true, 0)
	if len(results) != 1 || results[0].Identity != nil || results[0].Resource != nil {
		t.Fatalf("results = %+v, want one carrying only the warning", results)
	}
	requireNoProtocolErrors(t, results[0].Diagnostics)
	requireProtocolWarningContaining(t, results[0].Diagnostics, `"docker@"`)
}

func TestPolicyListReportsAFailedListing(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/policies", http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected")
	h := newProtocolHarness(t, fake)

	results := h.list(t, policyTestType, h.listValue(t, policyTestType, nil), false, 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want one carrying the error", len(results))
	}
	requireProtocolErrorContaining(t, results[0].Diagnostics, "Unable to list Kopia policies")
	requireProtocolErrorContaining(t, results[0].Diagnostics, "no repository connected")
}

// notificationListStore stores a profile of each method, with secrets, and
// two profiles kopia_notification_profile cannot manage.
func notificationListStore(t *testing.T, fake *fakeKopia) *notificationTestStore {
	t.Helper()
	store := newNotificationTestStore(fake, "mail", "ops", "phone", "test")
	store.put(t, "phone", `{"profile":"phone","method":{"type":"pushover","config":{"appToken":"`+notificationTestAppToken+
		`","userKey":"`+notificationTestUserKey+`"}},"minSeverity":10}`)
	store.put(t, "ops", `{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"`+notificationTestHookURL+
		`","headers":"Authorization: `+notificationTestBearer+`","method":"PUT"}},"minSeverity":0}`)
	store.put(t, "mail", `{"profile":"mail","method":{"type":"email","config":{"smtpServer":"smtp.example.com",`+
		`"smtpUsername":"kopia","smtpPassword":"`+notificationTestPassword+`","from":"kopia@example.com",`+
		`"to":"ops@example.com"}},"minSeverity":20}`)
	store.put(t, "test", `{"profile":"test","method":{"type":"testsender","config":null},"minSeverity":0}`)
	store.put(t, "a/b", `{"profile":"a/b","method":{"type":"webhook","config":{"endpoint":"https://example.com/"}},"minSeverity":0}`)
	return store
}

// Listing names the email, Pushover and webhook profiles, sorted, without
// reading any of them, and warns about the ones it leaves out.
func TestNotificationProfileListReportsTheProfilesItCanManage(t *testing.T) {
	fake := newFakeKopia()
	notificationListStore(t, fake)
	h := newProtocolHarness(t, fake)

	results := h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), false, 0)
	requireNoProtocolErrors(t, listDiagnostics(results))
	want := []string{"mail", "ops", "phone"}
	if got := listedIdentities(t, h, notificationTestType, "name", results); !reflect.DeepEqual(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for i, r := range results {
		if r.DisplayName != want[i] || r.Resource != nil {
			t.Errorf("result %d = %q with resource %v", i, r.DisplayName, r.Resource)
		}
	}
	requireProtocolWarningContaining(t, results[0].Diagnostics, `"test" (method "testsender")`)
	requireProtocolWarningContaining(t, results[0].Diagnostics, `"a/b" (a name Kopia's API cannot address)`)
	if got := fake.apiRoutes(); !reflect.DeepEqual(got, []string{"GET /api/v1/notificationProfiles"}) {
		t.Errorf("calls = %v, want only the listing", got)
	}
}

// With include_resource every profile is read as an import reads it,
// secrets included, so generated configuration plans no change.
func TestNotificationProfileListIncludesTheResourceAsAnImportRecordsIt(t *testing.T) {
	fake := newFakeKopia()
	notificationListStore(t, fake)
	h := newProtocolHarness(t, fake)

	results := h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), true, 0)
	requireNoProtocolErrors(t, listDiagnostics(results))
	if len(results) != 3 {
		t.Fatalf("listed %d profiles, want 3", len(results))
	}
	for _, r := range results {
		name := stringAt(t, h.identityValue(t, notificationTestType, r.Identity), "name")
		listed := h.decodeResource(t, notificationTestType, r.Resource)
		imported := h.importByID(t, notificationTestType, name)
		if !listed.Equal(imported.state) {
			diffs, _ := listed.Diff(imported.state)
			t.Errorf("%s listed differs from the import: %v", name, diffs)
		}
	}
	ops := h.decodeResource(t, notificationTestType, results[1].Resource)
	if got := stringAt(t, ops, "webhook", "endpoint"); got != notificationTestHookURL {
		t.Errorf("webhook.endpoint = %q, want the stored URL", got)
	}
	if got := stringAt(t, ops, "webhook", "headers", "Authorization"); got != notificationTestBearer {
		t.Errorf("webhook.headers.Authorization = %q", got)
	}
	if boolAt(t, ops, "send_test_on_apply") {
		t.Error("send_test_on_apply = true, want the import's false")
	}
	requireNoNotificationSecrets(t, listDiagnostics(results))
}

// A profile deleted since it was listed is left out silently, one stored in a
// form the provider cannot read with a warning, and the limit counts only the
// profiles reported.
func TestNotificationProfileListLeavesOutWhatItCannotRead(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/notificationProfiles", http.StatusOK, `[
  {"profile": "gone", "method": {"type": "webhook", "config": {}}, "minSeverity": 0},
  {"profile": "odd", "method": {"type": "webhook", "config": {}}, "minSeverity": 0},
  {"profile": "ops", "method": {"type": "webhook", "config": {}}, "minSeverity": 0},
  {"profile": "zz", "method": {"type": "webhook", "config": {}}, "minSeverity": 0}
]
`)
	fake.replyError("GET /api/v1/notificationProfiles/gone", http.StatusInternalServerError, kopiaclient.CodeInternal,
		"internal server error: profile not found")
	fake.reply("GET /api/v1/notificationProfiles/odd", http.StatusOK,
		`{"profile":"odd","method":{"type":"webhook","config":{"endpoint":42}},"minSeverity":0}`)
	fake.reply("GET /api/v1/notificationProfiles/ops", http.StatusOK,
		`{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"https://example.com/hook"}},"minSeverity":0}`)
	h := newProtocolHarness(t, fake)

	results := h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), true, 1)
	requireNoProtocolErrors(t, listDiagnostics(results))
	if got := listedIdentities(t, h, notificationTestType, "name", results); !reflect.DeepEqual(got, []string{"ops"}) {
		t.Fatalf("listed %v, want only ops", got)
	}
	requireProtocolWarningContaining(t, results[0].Diagnostics, `notification profile "odd" in a form`)
	if fake.called("GET /api/v1/notificationProfiles/zz") {
		t.Error("read a profile beyond the limit")
	}

	// Without include_resource no profile is read, so nothing shows that
	// "odd" cannot be imported: it is listed like the others, as the list
	// resource's description says.
	results = h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), false, 0)
	if diags := listDiagnostics(results); len(diags) != 0 {
		t.Errorf("diagnostics = %v, want none from a listing that reads no profile", diags)
	}
	want := []string{"gone", "odd", "ops", "zz"}
	if got := listedIdentities(t, h, notificationTestType, "name", results); !reflect.DeepEqual(got, want) {
		t.Fatalf("listed %v without include_resource, want %v", got, want)
	}
}

// A profile stored in a form Kopia cannot use is still listed in full, with
// the warning a refresh after importing it reports.
func TestNotificationProfileListWarnsAboutAProfileKopiaCannotUse(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "mail")
	store.put(t, "mail", `{"profile":"mail","method":{"type":"email","config":{"smtpServer":"smtp.example.com",`+
		`"smtpPort":"2525","smtpPassword":"`+notificationTestPassword+`","from":"kopia@example.com","to":"ops@example.com"}},"minSeverity":0}`)
	h := newProtocolHarness(t, fake)

	results := h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), true, 0)
	requireNoProtocolErrors(t, listDiagnostics(results))
	if got := listedIdentities(t, h, notificationTestType, "name", results); !reflect.DeepEqual(got, []string{"mail"}) {
		t.Fatalf("listed %v, want mail", got)
	}
	requireProtocolWarningContaining(t, results[0].Diagnostics, "Kopia cannot use notification profile as stored")
	requireProtocolWarningContaining(t, results[0].Diagnostics, `Notification profile "mail"`)
	requireNoNotificationSecrets(t, results[0].Diagnostics)
	if results[0].Resource == nil {
		t.Error("the profile was listed without its resource")
	}
}

func TestNotificationProfileListReportsAFailedRead(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/notificationProfiles", http.StatusOK,
		`[{"profile": "ops", "method": {"type": "webhook", "config": {}}, "minSeverity": 0}]`)
	fake.replyError("GET /api/v1/notificationProfiles/ops", http.StatusInternalServerError, kopiaclient.CodeInternal,
		"internal server error: unable to get notification profile: storage is offline")
	h := newProtocolHarness(t, fake)

	results := h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), true, 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want one carrying the error", len(results))
	}
	requireProtocolErrorContaining(t, results[0].Diagnostics, "storage is offline")
}

func TestNotificationProfileListWithoutProfiles(t *testing.T) {
	fake := newFakeKopia()
	newNotificationTestStore(fake)
	h := newProtocolHarness(t, fake)

	if results := h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), true, 0); len(results) != 0 {
		t.Fatalf("results = %+v, want none", results)
	}
}

// Kopia lists a name twice should two clients have saved it at once; it is
// listed once. A profile without a method is left out with a warning.
func TestNotificationProfileListReportsANameOnce(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/notificationProfiles", http.StatusOK, `[
  {"profile": "ops", "method": {"type": "webhook", "config": {}}, "minSeverity": 0},
  {"profile": "bare", "method": null, "minSeverity": 0},
  {"profile": "ops", "method": {"type": "pushover", "config": {}}, "minSeverity": 10}
]
`)
	h := newProtocolHarness(t, fake)

	results := h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), false, 0)
	if got := listedIdentities(t, h, notificationTestType, "name", results); !reflect.DeepEqual(got, []string{"ops"}) {
		t.Fatalf("listed %v, want ops once", got)
	}
	requireProtocolWarningContaining(t, results[0].Diagnostics, `"bare" (no method)`)
}

func TestNotificationProfileListReportsAFailedListing(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/notificationProfiles", http.StatusInternalServerError, kopiaclient.CodeInternal,
		"internal server error: unable to list notification profiles")
	h := newProtocolHarness(t, fake)

	results := h.list(t, notificationTestType, h.listValue(t, notificationTestType, nil), false, 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want one carrying the error", len(results))
	}
	requireProtocolErrorContaining(t, results[0].Diagnostics, "Unable to list Kopia notification profiles")
}

// A list run before the provider was configured reports it instead of
// failing on a nil client; Terraform never does that.
func TestListResourcesWithoutAConfiguredProvider(t *testing.T) {
	for _, l := range []list.ListResource{NewPolicyListResource(), NewNotificationProfileListResource()} {
		var stream list.ListResultsStream
		l.List(context.Background(), list.ListRequest{}, &stream)
		var diags diag.Diagnostics
		for result := range stream.Results {
			diags.Append(result.Diagnostics...)
		}
		requireErrorContaining(t, diags, "Unconfigured Kopia provider")
	}
}

// --- acceptance ---------------------------------------------------------------

// testAccListHost is a host only TestAccListResources writes policies for,
// so that the filtered list holds exactly those.
const testAccListHost = "tfacc-list"

func TestAccListResources(t *testing.T) {
	pathTarget := "tfacc@" + testAccListHost + ":/data/app"
	hostTarget := "@" + testAccListHost
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			// List resources and `terraform query` arrived in Terraform 1.14.
			tfversion.SkipBelow(tfversion.Version1_14_0),
		},
		CheckDestroy: acc.ComposeAggregateTestCheckFunc(
			testAccCheckPolicyStored(pathTarget, ""),
			testAccCheckPolicyStored(hostTarget, ""),
			testAccCheckNotificationProfilesDestroyed(t, "tfacc-listed"),
		),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
resource "kopia_policy" "path" {
  target = "` + pathTarget + `"

  retention = {
    keep_daily = 9
  }
}

resource "kopia_policy" "host" {
  target = "` + hostTarget + `"
}

resource "kopia_notification_profile" "listed" {
  name         = "tfacc-listed"
  min_severity = "warning"

  webhook = {
    endpoint = "https://ntfy.example.invalid/tfacc-listed"
  }
}
`,
			},
			{
				// The query file is read together with the configuration above,
				// which already configures the provider.
				Query: true,
				Config: `
list "kopia_policy" "test_host" {
  provider         = kopia
  include_resource = true

  config {
    host = "` + testAccListHost + `"
  }
}

list "kopia_policy" "all" {
  provider = kopia
}

list "kopia_notification_profile" "all" {
  provider         = kopia
  include_resource = true
}
`,
				QueryResultChecks: []querycheck.QueryResultCheck{
					// The host filter leaves exactly the two policies of this test.
					querycheck.ExpectLength("kopia_policy.test_host", 2),
					querycheck.ExpectIdentity("kopia_policy.test_host", map[string]knownvalue.Check{
						"target": knownvalue.StringExact(hostTarget),
					}),
					querycheck.ExpectResourceKnownValues(
						"kopia_policy.test_host",
						queryfilter.ByDisplayName(knownvalue.StringExact(pathTarget)),
						[]querycheck.KnownValueCheck{
							{Path: tfjsonpath.New("id"), KnownValue: knownvalue.StringExact(pathTarget)},
							{Path: tfjsonpath.New("retention").AtMapKey("keep_daily"), KnownValue: knownvalue.Int64Exact(9)},
							{Path: tfjsonpath.New("retention").AtMapKey("keep_latest"), KnownValue: knownvalue.Null()},
							{Path: tfjsonpath.New("scheduling"), KnownValue: knownvalue.Null()},
						},
					),
					// Every repository has a global policy.
					querycheck.ExpectIdentity("kopia_policy.all", map[string]knownvalue.Check{
						"target": knownvalue.StringExact(kopiaclient.GlobalTarget),
					}),
					querycheck.ExpectIdentity("kopia_notification_profile.all", map[string]knownvalue.Check{
						"name": knownvalue.StringExact("tfacc-listed"),
					}),
					querycheck.ExpectResourceKnownValues(
						"kopia_notification_profile.all",
						queryfilter.ByDisplayName(knownvalue.StringExact("tfacc-listed")),
						[]querycheck.KnownValueCheck{
							// The webhook's endpoint and headers are sensitive, so the
							// checks stay outside it.
							{Path: tfjsonpath.New("min_severity"), KnownValue: knownvalue.StringExact("warning")},
							{Path: tfjsonpath.New("email"), KnownValue: knownvalue.Null()},
							{Path: tfjsonpath.New("pushover"), KnownValue: knownvalue.Null()},
							{Path: tfjsonpath.New("send_test_on_apply"), KnownValue: knownvalue.Bool(false)},
						},
					),
				},
			},
		},
	})
}
