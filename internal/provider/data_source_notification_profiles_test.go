package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// notificationProfilesTestType is the data source under test.
const notificationProfilesTestType = "kopia_notification_profiles"

// notificationProfilesTestList reads the data source and returns its profiles
// as name, method and min_severity triples.
func notificationProfilesTestList(t *testing.T, h *protocolHarness) ([][3]string, tftypes.Value) {
	t.Helper()
	state := h.readDataSource(t, notificationProfilesTestType, h.dataSourceValue(t, notificationProfilesTestType, map[string]any{}))
	var elements []tftypes.Value
	if err := knownAt(t, state, "profiles").As(&elements); err != nil {
		t.Fatal(err)
	}
	listed := make([][3]string, 0, len(elements))
	for i := range elements {
		listed = append(listed, [3]string{
			stringAt(t, state, "profiles", i, "name"),
			stringAt(t, state, "profiles", i, "method"),
			stringAt(t, state, "profiles", i, "min_severity"),
		})
	}
	return listed, state
}

// Kopia answers null when there are no profiles, which is an empty list, not
// a null one, so that `length()` works on it.
func TestNotificationProfilesDataSourceEmpty(t *testing.T) {
	fake := newFakeKopia()
	newNotificationTestStore(fake)
	h := notificationTestHarness(t, fake)

	listed, _ := notificationProfilesTestList(t, h)
	if len(listed) != 0 {
		t.Fatalf("profiles = %v, want none", listed)
	}
}

// Profiles are listed by name, once each, with their methods and severities
// and none of their options, so no secret reaches the state.
func TestNotificationProfilesDataSourceListsWithoutSecrets(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake)
	store.put(t, "phone", `{"profile":"phone","method":{"type":"pushover","config":{"appToken":"`+notificationTestAppToken+
		`","userKey":"`+notificationTestUserKey+`"}},"minSeverity":10}`)
	store.put(t, "ops", `{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"`+notificationTestHookURL+
		`","headers":"Authorization: `+notificationTestBearer+`"}},"minSeverity":3}`)
	store.put(t, "mail", `{"profile":"mail","method":{"type":"email","config":{"smtpPassword":"`+notificationTestPassword+
		`"}},"minSeverity":-100}`)
	store.put(t, "test", `{"profile":"test","method":{"type":"testsender","config":null},"minSeverity":0}`)
	h := notificationTestHarness(t, fake)

	listed, state := notificationProfilesTestList(t, h)
	want := [][3]string{
		{"mail", "email", "verbose"},
		{"ops", "webhook", "3"},
		{"phone", "pushover", "warning"},
		{"test", "testsender", "report"},
	}
	if len(listed) != len(want) {
		t.Fatalf("profiles = %v, want %v", listed, want)
	}
	for i := range want {
		if listed[i] != want[i] {
			t.Errorf("profiles[%d] = %v, want %v", i, listed[i], want[i])
		}
	}

	text := state.String()
	for _, secret := range notificationTestSecrets {
		if strings.Contains(text, secret) {
			t.Errorf("the state holds the secret %q: %s", secret, text)
		}
	}
}

// Kopia lists the manifests of a name that two writers saved at once twice;
// the data source lists the name once, as the last entry describes it, which
// is the one Kopia's GET returns.
func TestNotificationProfilesDataSourceListsANameOnce(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/notificationProfiles", http.StatusOK, `[
  {"profile": "ops", "method": {"type": "webhook", "config": {}}, "minSeverity": 0},
  {"profile": "b", "method": {"type": "email", "config": {}}, "minSeverity": 20},
  {"profile": "ops", "method": {"type": "pushover", "config": {}}, "minSeverity": 10}
]
`)
	h := notificationTestHarness(t, fake)

	listed, _ := notificationProfilesTestList(t, h)
	want := [][3]string{{"b", "email", "error"}, {"ops", "pushover", "warning"}}
	if len(listed) != len(want) || listed[0] != want[0] || listed[1] != want[1] {
		t.Fatalf("profiles = %v, want %v", listed, want)
	}
}

// A profile Kopia cannot decode fails its whole list, which is reported.
func TestNotificationProfilesDataSourceReportsFailures(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/notificationProfiles", http.StatusInternalServerError, kopiaclient.CodeInternal,
		`internal server error: unable to get notification profile: unable to unmarshal "k1": sender type '' not registered`)
	h := notificationTestHarness(t, fake)

	resp := h.readDataSourceResponse(t, notificationProfilesTestType, h.dataSourceValue(t, notificationProfilesTestType, map[string]any{}))
	requireProtocolErrorContaining(t, resp.Diagnostics, "Unable to list Kopia notification profiles")
	requireProtocolErrorContaining(t, resp.Diagnostics, "sender type '' not registered")
}

// --- acceptance ---------------------------------------------------------------

func TestAccNotificationProfilesDataSource(t *testing.T) {
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckNotificationProfilesDestroyed(t, "tfacc-listed"),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
resource "kopia_notification_profile" "listed" {
  name         = "tfacc-listed"
  min_severity = "error"
  webhook      = { endpoint = "https://ntfy.example.invalid/listed" }
}

data "kopia_notification_profiles" "all" {
  depends_on = [kopia_notification_profile.listed]
}
`,
				Check: acc.TestCheckTypeSetElemNestedAttrs("data.kopia_notification_profiles.all", "profiles.*", map[string]string{
					"name":         "tfacc-listed",
					"method":       "webhook",
					"min_severity": "error",
				}),
			},
		},
	})
}
