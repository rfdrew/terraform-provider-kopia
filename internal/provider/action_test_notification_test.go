package provider

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// These tests invoke kopia_notification_test through the protocol server,
// against the notification store of notification_profile_store_test.go, which
// records what reaches Kopia's test endpoint.

const notificationTestActionType = "kopia_notification_test"

// notificationTestActionStore stores the webhook profile "ops", whose URL and
// headers carry secrets.
func notificationTestActionStore(t *testing.T, fake *fakeKopia) *notificationTestStore {
	t.Helper()
	store := newNotificationTestStore(fake, "ops")
	store.put(t, "ops", `{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"`+notificationTestHookURL+
		`","headers":"Authorization: `+notificationTestBearer+`","method":"PUT","format":"html"}},"minSeverity":20}`)
	return store
}

// invokeNotificationTest invokes kopia_notification_test for the named profile.
func invokeNotificationTest(t *testing.T, h *protocolHarness, name string) ([]string, []*tfprotov6.Diagnostic) {
	t.Helper()
	return h.invokeAction(t, notificationTestActionType, h.actionValue(t, notificationTestActionType, map[string]any{"name": name}))
}

// Kopia's test endpoint takes a whole profile, so the stored one is read and
// sent, every option written out; the severity goes along although Kopia
// ignores it.
func TestNotificationTestActionSendsTheStoredProfile(t *testing.T) {
	fake := newFakeKopia()
	store := notificationTestActionStore(t, fake)
	h := newProtocolHarness(t, fake)

	progress, diags := invokeNotificationTest(t, h, "ops")
	requireNoProtocolErrors(t, diags)

	sent := store.testsSent()
	if len(sent) != 1 {
		t.Fatalf("tests sent = %d, want 1", len(sent))
	}
	method, _ := sent[0]["method"].(map[string]any)
	config, _ := method["config"].(map[string]any)
	if sent[0]["profile"] != "ops" || method["type"] != "webhook" || sent[0]["minSeverity"] != float64(20) {
		t.Errorf("test body = %v", sent[0])
	}
	want := map[string]any{"endpoint": notificationTestHookURL, "headers": "Authorization: " + notificationTestBearer,
		"method": "PUT", "format": "html"}
	for k, v := range want {
		if config[k] != v {
			t.Errorf("config[%s] = %v, want %v", k, config[k], v)
		}
	}
	if len(progress) != 2 || !strings.Contains(progress[0], `webhook profile "ops"`) ||
		!strings.Contains(progress[1], "delivered") {
		t.Errorf("progress = %q", progress)
	}
	if store.stored("ops") == nil || len(store.testsSent()) != 1 {
		t.Error("the action changed the stored profile or sent more than one test")
	}
}

// Kopia reports a failed delivery as 400 MALFORMED_REQUEST, quoting the
// webhook URL, and an invalid request with the whole body; every secret of
// the profile is redacted from the error.
func TestNotificationTestActionReportsFailuresWithoutSecrets(t *testing.T) {
	// Kopia echoes the request body as the client wrote it, through
	// json.Marshal, which writes the ampersand of the webhook URL as a
	// Unicode escape: the form the redaction must catch besides the plain
	// one.
	echoedURL, err := json.Marshal(notificationTestHookURL)
	if err != nil {
		t.Fatal(err)
	}
	if string(echoedURL) == strconv.Quote(notificationTestHookURL) {
		t.Fatalf("the echoed webhook URL %s holds no escaped character, so the case below tests nothing new", echoedURL)
	}
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			"failed delivery",
			"unable to send notification: unable to send notification message: error sending webhook notification: " +
				`Post "` + notificationTestHookURL + `": dial tcp 192.0.2.1:443: connect: connection refused`,
			"sending the message failed",
		},
		{
			"refused by the service",
			"unable to send notification: unable to send notification message: error sending webhook notification: 401 Unauthorized",
			"accepts the profile's credentials",
		},
		{
			"no sender",
			"unable to construct sender: unable to create sender: invalid notification configuration: invalid format: md",
			"cannot build a sender",
		},
		{
			"echoed body",
			`malformed request body: {"profile":"ops","method":{"type":"webhook","config":{"endpoint":` +
				string(echoedURL) + `,"headers":"Authorization: ` + notificationTestBearer + `"}}}`,
			"malformed request body",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			store := notificationTestActionStore(t, fake)
			store.failTests(tc.message)
			h := newProtocolHarness(t, fake)

			_, diags := invokeNotificationTest(t, h, "ops")
			requireProtocolErrorContaining(t, diags, "Kopia could not send the test notification")
			requireProtocolErrorContaining(t, diags, tc.want)
			requireNoNotificationSecrets(t, diags)
		})
	}

	fake := newFakeKopia()
	notificationTestActionStore(t, fake).failTests(tests[0].message)
	_, diags := invokeNotificationTest(t, newProtocolHarness(t, fake), "ops")
	requireProtocolErrorContaining(t, diags, `Post "<redacted>"`)
}

// Kopia waits for a delivery without a time limit of its own, so the
// provider's request_timeout can end the test first; the error then says the
// message may still arrive, and still quotes no secret.
func TestNotificationTestActionReportsATimeout(t *testing.T) {
	fake := newFakeKopia()
	store := notificationTestActionStore(t, fake)
	client := fake.clientLosingAnswer(t, "POST /api/v1/testNotificationProfile", 200*time.Millisecond, awaitRequestTimeout)
	h := newProtocolHarness(t, fake, withClient(client))

	_, diags := invokeNotificationTest(t, h, "ops")
	requireProtocolErrorContaining(t, diags, "Kopia could not send the test notification")
	requireProtocolErrorContaining(t, diags, "deadline exceeded")
	requireProtocolErrorContaining(t, diags, notificationTestTimeoutNote)
	requireNoNotificationSecrets(t, diags)
	if len(store.testsSent()) != 1 {
		t.Errorf("tests sent = %d, want the one that timed out", len(store.testsSent()))
	}
}

// Kopia answers a missing profile with 500 INTERNAL "profile not found"; the
// action names the profile and says how to test one created in the same
// apply, and sends nothing.
func TestNotificationTestActionWithoutTheProfile(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "ops")
	h := newProtocolHarness(t, fake)

	_, diags := invokeNotificationTest(t, h, "ops")
	requireProtocolErrorContaining(t, diags, "Kopia notification profile not found")
	requireProtocolErrorContaining(t, diags, `no notification profile named "ops"`)
	requireProtocolErrorContaining(t, diags, "after_create")
	requireNotificationAttributeError(t, diags, "name")
	if len(store.testsSent()) != 0 {
		t.Error("a test was sent without a profile")
	}
}

// A profile the provider cannot represent cannot be sent to the test either.
func TestNotificationTestActionRefusesAnUnsupportedProfile(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "ops")
	store.put(t, "ops", `{"profile":"ops","method":{"type":"testsender","config":{}},"minSeverity":0}`)
	h := newProtocolHarness(t, fake)

	_, diags := invokeNotificationTest(t, h, "ops")
	requireProtocolErrorContaining(t, diags, "Unsupported Kopia notification profile")
	requireNotificationAttributeError(t, diags, "name")
	if len(store.testsSent()) != 0 {
		t.Error("a test was sent for an unsupported profile")
	}
}

// A port stored as a string cannot be used by Kopia, but is sent to the test
// as a number, so the action warns that a successful test proves less.
func TestNotificationTestActionWarnsAboutAProfileKopiaCannotUse(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "mail")
	store.put(t, "mail", `{"profile":"mail","method":{"type":"email","config":{"smtpServer":"smtp.example.com",`+
		`"smtpPort":"2525","smtpPassword":"`+notificationTestPassword+`","from":"kopia@example.com","to":"ops@example.com"}},"minSeverity":0}`)
	h := newProtocolHarness(t, fake)

	_, diags := invokeNotificationTest(t, h, "mail")
	requireNoProtocolErrors(t, diags)
	requireProtocolWarningContaining(t, diags, "may succeed all the same")
	sent := store.testsSent()
	if len(sent) != 1 {
		t.Fatalf("tests sent = %d, want 1", len(sent))
	}
	config := sent[0]["method"].(map[string]any)["config"].(map[string]any)
	if config["smtpPort"] != float64(2525) {
		t.Errorf("smtpPort = %#v, want the number 2525", config["smtpPort"])
	}
	requireNoNotificationSecrets(t, diags)
}

func TestNotificationTestActionWithoutARepository(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/notificationProfiles/ops", http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected")
	h := newProtocolHarness(t, fake)

	_, diags := invokeNotificationTest(t, h, "ops")
	requireProtocolErrorContaining(t, diags, "no repository connected")
}

// A name Kopia's API cannot address is refused before anything is sent.
func TestNotificationTestActionValidatesTheName(t *testing.T) {
	h := newProtocolHarness(t, newFakeKopia())
	for value, want := range map[string]string{"": "length must be at least 1", "ops/eu": `must not contain "/"`, "..": `be "." or ".."`} {
		diags := h.validateAction(t, notificationTestActionType, h.actionValue(t, notificationTestActionType, map[string]any{"name": value}))
		requireProtocolErrorContaining(t, diags, want)
	}
	requireNoProtocolErrors(t, h.validateAction(t, notificationTestActionType,
		h.actionValue(t, notificationTestActionType, map[string]any{"name": "ops"})))
	// A name known only at apply time is checked when it is.
	requireNoProtocolErrors(t, h.validateAction(t, notificationTestActionType,
		h.actionValue(t, notificationTestActionType, map[string]any{"name": tfUnknown})))
}

func TestNotificationTestActionWithoutAConfiguredProvider(t *testing.T) {
	resp := invokeActionDirectly(t, NewNotificationTestAction(), map[string]any{"name": "ops"})
	requireErrorContaining(t, resp.Diagnostics, "Unconfigured Kopia provider")
}

// --- acceptance ---------------------------------------------------------------

// The profile points at a port nothing listens on, inside the Kopia
// container, so the test fails at once and delivers nothing. Kopia quotes the
// whole URL in its error, token included; the action must replace it.
func TestAccNotificationTestAction(t *testing.T) {
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			// Actions arrived in Terraform 1.14.
			tfversion.SkipBelow(tfversion.Version1_14_0),
		},
		CheckDestroy: testAccCheckNotificationProfilesDestroyed(t, "tfacc-test-action"),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
resource "kopia_notification_profile" "unreachable" {
  name = "tfacc-test-action"

  webhook = {
    endpoint = "http://127.0.0.1:9/tfacc?token=tk_tfacc_action"
  }

  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.kopia_notification_test.unreachable]
    }
  }
}

action "kopia_notification_test" "unreachable" {
  config {
    name = kopia_notification_profile.unreachable.name
  }
}
`,
				// Terraform wraps diagnostics, so words may be separated by a
				// line break. The URL Kopia quotes must come out as <redacted>,
				// quotes and all, which also proves the token is gone.
				ExpectError: regexp.MustCompile(`(?s)Kopia\s+could\s+not\s+send\s+(the|its)\s+test\s+notification.*` +
					`unable\s+to\s+send\s+notification.*Post\s+"<redacted>"`),
			},
		},
	})
}
