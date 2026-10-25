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

// policiesTestEntries returns the policies of a kopia_policies state as
// "target|host|user_name|path|policy_json", with "-" for null.
func policiesTestEntries(t *testing.T, state tftypes.Value) []string {
	t.Helper()
	var elements []tftypes.Value
	if err := knownAt(t, state, "policies").As(&elements); err != nil {
		t.Fatal(err)
	}
	orNull := func(e tftypes.Value, name string) string {
		if isNullAt(t, e, name) {
			return "-"
		}
		return stringAt(t, e, name)
	}
	var entries []string
	for _, e := range elements {
		entries = append(entries, strings.Join([]string{
			stringAt(t, e, "target"), orNull(e, "host"), orNull(e, "user_name"), orNull(e, "path"), stringAt(t, e, "policy_json"),
		}, "|"))
	}
	return entries
}

// policiesTestSeed stores policies for every kind of target, including the
// "user@" one ParseTarget refuses but Kopia can hold, in an order other than
// that of their targets.
func policiesTestSeed(t *testing.T, store *policyTestStore) {
	t.Helper()
	store.putRaw(t, "docker@host1:/data/app", `{"scheduling":{"cron":["0 3 * * *"]},"files":{"ignore":["*.tmp"]}}`)
	store.putRaw(t, "(global)", policyTestDefaultGlobalJSON)
	store.putRaw(t, "docker@", `{"retention":{"keepDaily":1}}`)
	store.putRaw(t, "docker@host1", `{"scheduling":{"manual":true}}`)
	store.putRaw(t, "@host1", `{"upload":{"maxParallelSnapshots":2}}`)
}

// Every stored policy is listed, sorted by target rather than in Kopia's
// order of change, with its policy as compact JSON that does not depend on
// the release.
func TestPoliciesDataSourceListsEveryPolicy(t *testing.T) {
	want := []string{
		"(global)|-|-|-|" + policyTestDefaultGlobalJSON,
		`@host1|host1|-|-|{"upload":{"maxParallelSnapshots":2}}`,
		`docker@|-|docker|-|{"retention":{"keepDaily":1}}`,
		`docker@host1|host1|docker|-|{"scheduling":{"manual":true}}`,
		`docker@host1:/data/app|host1|docker|/data/app|{"files":{"ignore":["*.tmp"]},"scheduling":{"cron":["0 3 * * *"]}}`,
	}
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%v", compact), func(t *testing.T) {
			fake := newFakeKopia()
			store := newPolicyTestStore(fake)
			store.compact = compact
			policiesTestSeed(t, store)
			h := policyTestHarness(t, fake)

			state := h.readDataSource(t, "kopia_policies", h.dataSourceValue(t, "kopia_policies", map[string]any{}))
			if got := policiesTestEntries(t, state); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("policies =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if got := fake.query(t, "GET /api/v1/policies"); len(got) != 0 {
				t.Errorf("query = %v, want no filter", got)
			}
			if id := stringAt(t, state, "policies", 0, "manifest_id"); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
				t.Errorf("manifest_id = %q", id)
			}
		})
	}
}

// The filters are sent as Kopia's query parameters, the user as camelCase
// userName, which Kopia matches exactly.
func TestPoliciesDataSourceFilters(t *testing.T) {
	fake := newFakeKopia()
	store := newPolicyTestStore(fake)
	policiesTestSeed(t, store)
	h := policyTestHarness(t, fake)

	state := h.readDataSource(t, "kopia_policies", h.dataSourceValue(t, "kopia_policies", map[string]any{
		"host": "host1", "user_name": "docker",
	}))
	if got := fake.query(t, "GET /api/v1/policies").Encode(); got != "host=host1&userName=docker" {
		t.Errorf("query = %q", got)
	}
	var targets []string
	for _, e := range policiesTestEntries(t, state) {
		target, _, _ := strings.Cut(e, "|")
		targets = append(targets, target)
	}
	if strings.Join(targets, ",") != "docker@host1,docker@host1:/data/app" {
		t.Errorf("targets = %v", targets)
	}

	state = h.readDataSource(t, "kopia_policies", h.dataSourceValue(t, "kopia_policies", map[string]any{"path": "/nowhere"}))
	if got := fake.query(t, "GET /api/v1/policies").Encode(); got != "path=%2Fnowhere" {
		t.Errorf("query = %q", got)
	}
	if got := policiesTestEntries(t, state); len(got) != 0 {
		t.Errorf("policies = %v, want none", got)
	}

	// An empty filter would match everything, which leaving it out says.
	diags := h.validateDataSource(t, "kopia_policies", h.dataSourceValue(t, "kopia_policies", map[string]any{"user_name": ""}))
	requireProtocolErrorContaining(t, diags, "at least 1")
}

// policy_json also holds the fields kopia_policy has no attribute for, as
// Kopia sent them, empty ones included, after the ones it has; the fields it
// has attributes for read the same whichever release answered.
func TestPoliciesDataSourceShowsWhatTheProviderDoesNotModel(t *testing.T) {
	const want = `docker@host1|host1|docker|-|{"retention":{"keepDaily":7,"keepQuarterly":4},"logging":{"entries":{"newLevel":1}},` +
		`"encryption":{"keys":[{"id":"a"}],"mode":"strict"},"newFlag":true,"newSection":{}}`
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%v", compact), func(t *testing.T) {
			fake := newFakeKopia()
			store := newPolicyTestStore(fake)
			store.compact = compact
			store.putRaw(t, "docker@host1", `{"newFlag":true,"logging":{"entries":{"newLevel":1}},"retention":{"keepQuarterly":4,"keepDaily":7},`+
				`"encryption":{"mode":"strict","keys":[{"id":"a"}]},"newSection":{}}`)
			h := policyTestHarness(t, fake)

			state := h.readDataSource(t, "kopia_policies", h.dataSourceValue(t, "kopia_policies", map[string]any{}))
			if got := policiesTestEntries(t, state); strings.Join(got, "\n") != want {
				t.Errorf("policies =\n%s\nwant\n%s", strings.Join(got, "\n"), want)
			}
		})
	}
}

func TestPoliciesDataSourceReportsKopiasError(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/policies", http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected")
	h := policyTestHarness(t, fake)

	resp := h.readDataSourceResponse(t, "kopia_policies", h.dataSourceValue(t, "kopia_policies", map[string]any{}))
	requireProtocolErrorContaining(t, resp.Diagnostics, "Unable to list Kopia policies")
	requireProtocolErrorContaining(t, resp.Diagnostics, "no repository connected")
}

// --- acceptance -----------------------------------------------------------------

func TestAccPoliciesDataSource(t *testing.T) {
	target := "tfacc@" + testAccPolicyHost + "-list"
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckPolicyStored(target, ""),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
resource "kopia_policy" "listed" {
  target = %[1]q

  retention = {
    keep_daily = 9
  }
}

data "kopia_policies" "host" {
  host = %[2]q

  depends_on = [kopia_policy.listed]
}

data "kopia_policies" "all" {
  depends_on = [kopia_policy.listed]
}
`, target, testAccPolicyHost+"-list"),
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("data.kopia_policies.host", "policies.#", "1"),
					acc.TestCheckResourceAttr("data.kopia_policies.host", "policies.0.target", target),
					acc.TestCheckResourceAttr("data.kopia_policies.host", "policies.0.user_name", "tfacc"),
					acc.TestCheckNoResourceAttr("data.kopia_policies.host", "policies.0.path"),
					acc.TestCheckResourceAttr("data.kopia_policies.host", "policies.0.policy_json", `{"retention":{"keepDaily":9}}`),
					acc.TestMatchResourceAttr("data.kopia_policies.host", "policies.0.manifest_id", regexp.MustCompile(`^[0-9a-f]+$`)),
					// The global policy exists on every repository, and sorts
					// first.
					acc.TestCheckResourceAttr("data.kopia_policies.all", "policies.0.target", "(global)"),
				),
			},
		},
	})
}
