package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// notificationTestType is the resource under test.
const notificationTestType = "kopia_notification_profile"

// Secrets used by the notification profile tests, distinctive enough that a
// leak shows in any text.
const (
	notificationTestPassword = "smtp-pa55word&<x>"
	notificationTestAppToken = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
	notificationTestUserKey  = "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"
	notificationTestHookPath = "XXXXsecretXXXX"
	notificationTestHookURL  = "https://hooks.example.com/services/T000/B000/" + notificationTestHookPath + "?token=tk_query&x=1"
	notificationTestBearer   = "Bearer tk_header_secret"
)

// notificationTestSecrets are the parts of the secrets above that must never
// appear in a diagnostic, in whichever form.
var notificationTestSecrets = []string{
	notificationTestPassword, notificationTestAppToken, notificationTestUserKey, notificationTestHookPath,
	"tk_query", "tk_header_secret",
}

// requireNoNotificationSecrets fails the test when any diagnostic quotes a
// test secret.
func requireNoNotificationSecrets(t *testing.T, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		for _, secret := range notificationTestSecrets {
			for _, text := range []string{d.Summary, d.Detail} {
				if kopiaclient.Redact(text, secret) != text {
					t.Errorf("diagnostic %q quotes the secret %q: %s", d.Summary, secret, d.Detail)
				}
			}
		}
	}
}

// requireNotificationAttributeError fails the test unless an error is attached
// to the attribute at steps, such as "webhook", "headers".
func requireNotificationAttributeError(t *testing.T, diags []*tfprotov6.Diagnostic, steps ...string) {
	t.Helper()
	want := tftypes.NewAttributePath()
	for _, step := range steps {
		want = want.WithAttributeName(step)
	}
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError && d.Attribute != nil && d.Attribute.Equal(want) {
			return
		}
	}
	var got []string
	for _, d := range diags {
		got = append(got, fmt.Sprintf("%s on %v: %s: %s", d.Severity, d.Attribute, d.Summary, d.Detail))
	}
	t.Fatalf("expected an error on %s, got %v", want, got)
}

// notificationTestHarness serves only the notification profile resource and
// data source.
func notificationTestHarness(t *testing.T, fake *fakeKopia) *protocolHarness {
	t.Helper()
	return newProtocolHarness(t, fake,
		withResources(NewNotificationProfileResource),
		withDataSources(NewNotificationProfilesDataSource),
	)
}

// notificationTestConfig builds a configuration of the profile "ops", with
// attrs added; attrs may name it differently.
func notificationTestConfig(t *testing.T, h *protocolHarness, attrs map[string]any) tftypes.Value {
	t.Helper()
	config := map[string]any{"name": "ops"}
	for k, v := range attrs {
		config[k] = v
	}
	return h.resourceValue(t, notificationTestType, config)
}

// notificationTestWebhook is a webhook method whose URL and headers carry
// secrets.
func notificationTestWebhook() map[string]any {
	return map[string]any{
		"endpoint": notificationTestHookURL,
		"headers":  map[string]any{"Authorization": notificationTestBearer},
	}
}

// requireNotificationPlanUnchanged plans config against inst and fails the
// test unless the plan reproduces the state exactly, as `terraform plan`
// reporting no changes requires.
func requireNotificationPlanUnchanged(t *testing.T, h *protocolHarness, inst *resourceInstance, config tftypes.Value) {
	t.Helper()
	planned := h.plan(t, inst.typeName, inst, config)
	requireNoProtocolErrors(t, planned.Diagnostics)
	if got := h.decodeResource(t, inst.typeName, planned.PlannedState); !got.Equal(inst.state) {
		diffs, _ := got.Diff(inst.state)
		t.Fatalf("the plan changes %v:\nplanned %s\nstate   %s", diffs, got, inst.state)
	}
}

// storedNotificationOptions returns the options of the profile stored under
// name, failing the test when there is none.
func storedNotificationOptions(t *testing.T, store *notificationTestStore, name string) map[string]any {
	t.Helper()
	method, _ := store.stored(name)["method"].(map[string]any)
	options, ok := method["config"].(map[string]any)
	if !ok {
		t.Fatalf("no options stored for profile %q: %v", name, store.stored(name))
	}
	return options
}

// notificationStateHeaders returns the webhook headers of a state.
func notificationStateHeaders(t *testing.T, state tftypes.Value) map[string]string {
	t.Helper()
	var elements map[string]tftypes.Value
	if err := knownAt(t, state, "webhook", "headers").As(&elements); err != nil {
		t.Fatal(err)
	}
	headers := make(map[string]string, len(elements))
	for name, element := range elements {
		var value string
		if err := element.As(&value); err != nil {
			t.Fatal(err)
		}
		headers[name] = value
	}
	return headers
}

// requireNotificationJSONBody compares a request body with the JSON document want.
func requireNotificationJSONBody(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected body is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("request body = %s, want %s", got, want)
	}
}

func TestNotificationProfileSchemasAreValid(t *testing.T) {
	r := NewNotificationProfileResource()
	requireValidResourceSchema(t, r)
	requireValidDataSourceSchema(t, NewNotificationProfilesDataSource())

	// The attribute type maps that build method values must match the
	// schema, or every state write with a method would fail.
	s := resourceSchema(t, r)
	for name, want := range map[string]map[string]attr.Type{
		"email":    notificationEmailAttrTypes,
		"pushover": notificationPushoverAttrTypes,
		"webhook":  notificationWebhookAttrTypes,
	} {
		if got := s.Attributes[name].GetType(); !got.Equal(types.ObjectType{AttrTypes: want}) {
			t.Errorf("%s has type %s, but its attribute type map says %s", name, got, types.ObjectType{AttrTypes: want})
		}
	}

	// The secrets Kopia returns must be hidden in plans and output.
	for _, p := range [][2]string{{"email", "smtp_password"}, {"pushover", "app_token"}, {"pushover", "user_key"},
		{"webhook", "endpoint"}, {"webhook", "headers"}} {
		method, ok := s.Attributes[p[0]].(schema.SingleNestedAttribute)
		if !ok {
			t.Fatalf("%s is a %T, want a single nested attribute", p[0], s.Attributes[p[0]])
		}
		if !method.Attributes[p[1]].IsSensitive() {
			t.Errorf("%s.%s is not sensitive", p[0], p[1])
		}
	}
	identity := resourceIdentitySchema(t, r)
	if _, ok := identity.Attributes["name"]; !ok || len(identity.Attributes) != 1 {
		t.Errorf("identity attributes = %v, want only name", harnessSortedKeys(identity.Attributes))
	}
}

// Each method round-trips: Kopia's defaults are written out explicitly, what
// Kopia stores reads back as the state the apply produced, and the same
// configuration then plans no change — with the defaults left out or spelt
// out alike.
func TestNotificationProfileRoundTripsEachMethod(t *testing.T) {
	tests := []struct {
		name     string
		minimal  map[string]any
		explicit map[string]any
		wantBody string
	}{
		{
			name: "email",
			minimal: map[string]any{"email": map[string]any{
				"smtp_server": "smtp.example.com", "from": "kopia@example.com", "to": "ops@example.com,oncall@example.com",
			}},
			explicit: map[string]any{"min_severity": "report", "email": map[string]any{
				"smtp_server": "smtp.example.com", "smtp_port": 587, "from": "kopia@example.com",
				"to": "ops@example.com,oncall@example.com", "format": "html",
			}},
			wantBody: `{"profile":"ops","minSeverity":0,"method":{"type":"email","config":{"smtpServer":"smtp.example.com",
				"smtpPort":587,"smtpIdentity":"","smtpUsername":"","smtpPassword":"","from":"kopia@example.com",
				"to":"ops@example.com,oncall@example.com","cc":"","format":"html"}}}`,
		},
		{
			name: "email with every option",
			minimal: map[string]any{"min_severity": "warning", "email": map[string]any{
				"smtp_server": "[2001:db8::25]", "smtp_port": 2525, "smtp_identity": "id", "smtp_username": "kopia",
				"smtp_password": notificationTestPassword, "from": "kopia@example.com", "to": "ops@example.com",
				"cc": "boss@example.com", "format": "txt",
			}},
			wantBody: `{"profile":"ops","minSeverity":10,"method":{"type":"email","config":{"smtpServer":"[2001:db8::25]",
				"smtpPort":2525,"smtpIdentity":"id","smtpUsername":"kopia","smtpPassword":"` + notificationTestPassword + `",
				"from":"kopia@example.com","to":"ops@example.com","cc":"boss@example.com","format":"txt"}}}`,
		},
		{
			name: "pushover",
			minimal: map[string]any{"pushover": map[string]any{
				"app_token": notificationTestAppToken, "user_key": notificationTestUserKey,
			}},
			explicit: map[string]any{"pushover": map[string]any{
				"app_token": notificationTestAppToken, "user_key": notificationTestUserKey, "format": "txt",
			}, "send_test_on_apply": false},
			wantBody: `{"profile":"ops","minSeverity":0,"method":{"type":"pushover","config":{
				"appToken":"` + notificationTestAppToken + `","userKey":"` + notificationTestUserKey + `","format":"txt"}}}`,
		},
		{
			name: "pushover with an endpoint",
			minimal: map[string]any{"min_severity": "verbose", "pushover": map[string]any{
				"app_token": "a", "user_key": "u", "format": "html", "endpoint": "https://pushover.example.com/1/messages.json",
			}},
			wantBody: `{"profile":"ops","minSeverity":-100,"method":{"type":"pushover","config":{
				"appToken":"a","userKey":"u","format":"html","endpoint":"https://pushover.example.com/1/messages.json"}}}`,
		},
		{
			name:    "webhook",
			minimal: map[string]any{"webhook": map[string]any{"endpoint": notificationTestHookURL}},
			explicit: map[string]any{"webhook": map[string]any{
				"endpoint": notificationTestHookURL, "method": "POST", "format": "txt",
			}},
			wantBody: `{"profile":"ops","minSeverity":0,"method":{"type":"webhook","config":{
				"endpoint":"` + notificationTestHookURL + `","method":"POST","format":"txt","headers":""}}}`,
		},
		{
			name: "webhook with every option",
			minimal: map[string]any{"min_severity": "error", "webhook": map[string]any{
				"endpoint": "http://hooks.internal:8080/kopia?x=<1>&y=2", "method": "PUT", "format": "html",
				"headers": map[string]any{"Priority": "high", "Authorization": notificationTestBearer, "X-Tags": "a:b, c", "X-Empty": ""},
			}},
			wantBody: `{"profile":"ops","minSeverity":20,"method":{"type":"webhook","config":{
				"endpoint":"http://hooks.internal:8080/kopia?x=<1>&y=2","method":"PUT","format":"html",
				"headers":"Authorization: ` + notificationTestBearer + `\nPriority: high\nX-Empty: \nX-Tags: a:b, c"}}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			store := newNotificationTestStore(fake, "ops")
			h := notificationTestHarness(t, fake)

			config := notificationTestConfig(t, h, tc.minimal)
			inst := h.create(t, notificationTestType, config)

			requireNotificationJSONBody(t, fake.lastCall(t, "POST /api/v1/notificationProfiles").body, tc.wantBody)
			if stringAt(t, inst.state, "id") != "ops" || stringAt(t, inst.state, "name") != "ops" {
				t.Errorf("id and name = %q, %q, want ops", stringAt(t, inst.state, "id"), stringAt(t, inst.state, "name"))
			}
			if boolAt(t, inst.state, "send_test_on_apply") {
				t.Error("send_test_on_apply = true, want the default false")
			}
			if len(store.testsSent()) != 0 {
				t.Errorf("tests sent = %d, want none", len(store.testsSent()))
			}

			refreshed := h.refresh(t, inst)
			if !refreshed.state.Equal(inst.state) {
				diffs, _ := refreshed.state.Diff(inst.state)
				t.Fatalf("a refresh changes %v", diffs)
			}
			requireNotificationPlanUnchanged(t, h, refreshed, config)
			if tc.explicit != nil {
				requireNotificationPlanUnchanged(t, h, refreshed, notificationTestConfig(t, h, tc.explicit))
			}

			h.destroy(t, refreshed)
			if store.stored("ops") != nil {
				t.Error("the profile still exists after destroy")
			}
		})
	}
}

// Headers are a map in Terraform and one "Name: value" line per header in
// Kopia. The provider writes them sorted; it reads any string Kopia's webhook
// sender would accept, the way the sender parses it.
func TestNotificationProfileWebhookHeaders(t *testing.T) {
	t.Run("read from another client's string", func(t *testing.T) {
		fake := newFakeKopia()
		store := newNotificationTestStore(fake, "ops")
		store.put(t, "ops", `{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"https://ntfy.example.org/kopia",
			"headers":"x-lower: l\r\n  Priority:high  \nnot a header\n\nX-Url: https://a.example/b?c=d"}},"minSeverity":0}`)
		h := notificationTestHarness(t, fake)

		inst := h.importByID(t, notificationTestType, "ops")
		want := map[string]string{"x-lower": "l", "Priority": "high", "X-Url": "https://a.example/b?c=d"}
		if got := notificationStateHeaders(t, inst.state); !reflect.DeepEqual(got, want) {
			t.Fatalf("headers = %q, want %q", got, want)
		}

		// The same headers configured as a map plan no change, although the
		// provider would write the string differently.
		config := notificationTestConfig(t, h, map[string]any{"webhook": map[string]any{
			"endpoint": "https://ntfy.example.org/kopia",
			"headers":  map[string]any{"x-lower": "l", "Priority": "high", "X-Url": "https://a.example/b?c=d"},
		}})
		requireNotificationPlanUnchanged(t, h, inst, config)

		// A string without a single header reads as no headers at all.
		store.put(t, "ops", `{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"https://ntfy.example.org/kopia",
			"headers":"\nno colon here\n"}},"minSeverity":0}`)
		if refreshed := h.refresh(t, inst); !isNullAt(t, refreshed.state, "webhook", "headers") {
			t.Errorf("headers = %s, want null", valueAt(t, refreshed.state, "webhook", "headers"))
		}
	})

	t.Run("written sorted and read back unchanged", func(t *testing.T) {
		fake := newFakeKopia()
		store := newNotificationTestStore(fake, "ops")
		h := notificationTestHarness(t, fake)

		headers := map[string]any{"X-B": "2", "Authorization": notificationTestBearer, "x-a": "1:2", "Content-Type": "text/plain"}
		inst := h.create(t, notificationTestType, notificationTestConfig(t, h, map[string]any{"webhook": map[string]any{
			"endpoint": notificationTestHookURL, "headers": headers,
		}}))

		stored := storedNotificationOptions(t, store, "ops")
		if want := "Authorization: " + notificationTestBearer + "\nContent-Type: text/plain\nX-B: 2\nx-a: 1:2"; stored["headers"] != want {
			t.Errorf("stored headers = %q, want %q", stored["headers"], want)
		}
		if got := notificationStateHeaders(t, inst.state); len(got) != len(headers) || got["x-a"] != "1:2" {
			t.Errorf("headers in state = %q, want the %d configured", got, len(headers))
		}

		// Removing the headers writes an empty string.
		inst = h.update(t, inst, notificationTestConfig(t, h, map[string]any{"webhook": map[string]any{"endpoint": notificationTestHookURL}}))
		if got := storedNotificationOptions(t, store, "ops")["headers"]; got != "" {
			t.Errorf("stored headers = %q, want empty", got)
		}
		if !isNullAt(t, inst.state, "webhook", "headers") {
			t.Errorf("headers = %s, want null", valueAt(t, inst.state, "webhook", "headers"))
		}
	})
}

// Severities are names in Terraform and numbers in Kopia.
func TestNotificationProfileSeverities(t *testing.T) {
	for name, want := range map[string]float64{"verbose": -100, "success": -10, "report": 0, "warning": 10, "error": 20} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeKopia()
			newNotificationTestStore(fake, "ops")
			h := notificationTestHarness(t, fake)

			inst := h.create(t, notificationTestType, notificationTestConfig(t, h, map[string]any{
				"min_severity": name, "webhook": map[string]any{"endpoint": "https://example.com/hook"},
			}))
			if got := fake.body(t, "POST /api/v1/notificationProfiles")["minSeverity"]; got != want {
				t.Errorf("minSeverity = %v, want %v", got, want)
			}
			if got := stringAt(t, inst.state, "min_severity"); got != name {
				t.Errorf("min_severity = %q, want %q", got, name)
			}
		})
	}

	// Kopia accepts any int32; a number with no name is shown as its decimal
	// string, so that it plans a change back to the configured name.
	t.Run("a number without a name", func(t *testing.T) {
		fake := newFakeKopia()
		store := newNotificationTestStore(fake, "ops")
		store.put(t, "ops", `{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"https://example.com/hook",
			"format":"txt","headers":"","method":"POST"}},"minSeverity":3}`)
		h := notificationTestHarness(t, fake)

		inst := h.importByID(t, notificationTestType, "ops")
		if got := stringAt(t, inst.state, "min_severity"); got != "3" {
			t.Fatalf("min_severity = %q, want \"3\"", got)
		}

		config := notificationTestConfig(t, h, map[string]any{"webhook": map[string]any{"endpoint": "https://example.com/hook"}})
		planned := h.plan(t, notificationTestType, inst, config)
		requireNoProtocolErrors(t, planned.Diagnostics)
		if got := stringAt(t, h.decodeResource(t, notificationTestType, planned.PlannedState), "min_severity"); got != "report" {
			t.Errorf("planned min_severity = %q, want the default report", got)
		}
		inst = h.update(t, inst, config)
		if got := fake.body(t, "POST /api/v1/notificationProfiles")["minSeverity"]; got != float64(0) {
			t.Errorf("minSeverity = %v, want 0", got)
		}
		requireNotificationPlanUnchanged(t, h, inst, config)
	})
}

// Kopia's POST overwrites a profile of the same name without a word, so
// Create refuses a name that is taken, even by a profile the provider could
// not manage.
func TestNotificationProfileCreateRefusesAnExistingName(t *testing.T) {
	for name, stored := range map[string]string{
		"webhook":    `{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"https://example.com/"}},"minSeverity":0}`,
		"testsender": `{"profile":"ops","method":{"type":"testsender","config":{"format":"html"}},"minSeverity":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeKopia()
			store := newNotificationTestStore(fake, "ops")
			store.put(t, "ops", stored)
			h := notificationTestHarness(t, fake)

			config := notificationTestConfig(t, h, map[string]any{"webhook": notificationTestWebhook()})
			planned := h.plan(t, notificationTestType, nil, config)
			requireNoProtocolErrors(t, planned.Diagnostics)
			applied := h.apply(t, notificationTestType, nil, config, planned)

			requireProtocolErrorContaining(t, applied.Diagnostics, `already has a notification profile named "ops"`)
			requireNotificationAttributeError(t, applied.Diagnostics, "name")
			if fake.called("POST /api/v1/notificationProfiles") {
				t.Error("the existing profile was overwritten")
			}
			if state := h.decodeResource(t, notificationTestType, applied.NewState); !state.IsNull() {
				t.Errorf("state = %s, want none", state)
			}
		})
	}

	t.Run("a failed check creates nothing", func(t *testing.T) {
		fake := newFakeKopia()
		newNotificationTestStore(fake, "ops")
		fake.replyError("GET /api/v1/notificationProfiles/ops", http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected")
		h := notificationTestHarness(t, fake)

		config := notificationTestConfig(t, h, map[string]any{"webhook": notificationTestWebhook()})
		planned := h.plan(t, notificationTestType, nil, config)
		applied := h.apply(t, notificationTestType, nil, config, planned)
		requireProtocolErrorContaining(t, applied.Diagnostics, "not connected")
		if fake.called("POST /api/v1/notificationProfiles") {
			t.Error("the profile was saved although the check failed")
		}
	})
}

// A profile deleted outside Terraform leaves the state on the next refresh,
// and destroying one that is already gone succeeds.
func TestNotificationProfileGoneOutsideTerraform(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "ops")
	h := notificationTestHarness(t, fake)

	inst := h.create(t, notificationTestType, notificationTestConfig(t, h, map[string]any{"webhook": notificationTestWebhook()}))
	store.drop("ops")

	refreshed := h.refresh(t, inst)
	if !refreshed.state.IsNull() {
		t.Fatalf("state = %s, want it removed", refreshed.state)
	}
	// Read records the identity before the lookup, so a state without one
	// still gets one when the profile turns out to be gone.
	if got := h.identityValue(t, notificationTestType, refreshed.identity); got.IsNull() {
		t.Error("no identity after a refresh found the profile gone")
	}

	h.destroy(t, inst)
	if got := fake.count("DELETE /api/v1/notificationProfiles/ops"); got != 1 {
		t.Errorf("DELETE requests = %d, want 1", got)
	}
}

// send_test_on_apply has Kopia deliver its test message after an apply that
// creates or changes the profile, and only then.
func TestNotificationProfileSendTestOnApply(t *testing.T) {
	t.Run("sent after a create or a change", func(t *testing.T) {
		fake := newFakeKopia()
		store := newNotificationTestStore(fake, "ops")
		h := notificationTestHarness(t, fake)

		webhook := notificationTestWebhook()
		inst := h.create(t, notificationTestType, notificationTestConfig(t, h, map[string]any{
			"webhook": webhook, "send_test_on_apply": true,
		}))
		if got := len(store.testsSent()); got != 1 {
			t.Fatalf("tests sent = %d, want 1", got)
		}
		// The test sends exactly what was saved.
		requireNotificationJSONBody(t, fake.lastCall(t, "POST /api/v1/testNotificationProfile").body,
			string(fake.lastCall(t, "POST /api/v1/notificationProfiles").body))
		if !boolAt(t, inst.state, "send_test_on_apply") {
			t.Error("send_test_on_apply = false, want true")
		}

		inst = h.update(t, inst, notificationTestConfig(t, h, map[string]any{
			"webhook": webhook, "send_test_on_apply": true, "min_severity": "warning",
		}))
		if got := len(store.testsSent()); got != 2 {
			t.Fatalf("tests sent = %d, want 2 after a change", got)
		}

		// Turning the setting off and on again changes nothing in Kopia, so
		// nothing is sent.
		inst = h.update(t, inst, notificationTestConfig(t, h, map[string]any{"webhook": webhook, "min_severity": "warning"}))
		h.update(t, inst, notificationTestConfig(t, h, map[string]any{
			"webhook": webhook, "send_test_on_apply": true, "min_severity": "warning",
		}))
		if got := len(store.testsSent()); got != 2 {
			t.Errorf("tests sent = %d, want still 2", got)
		}
	})

	// The failure Kopia reports quotes the full webhook URL, which carries a
	// token here.
	failure := `unable to send notification: unable to send notification message: error sending webhook notification: Post "` +
		notificationTestHookURL + `": dial tcp: lookup hooks.example.com: no such host`

	t.Run("a failed create keeps the profile in state", func(t *testing.T) {
		fake := newFakeKopia()
		store := newNotificationTestStore(fake, "ops")
		store.failTests(failure)
		h := notificationTestHarness(t, fake)

		config := notificationTestConfig(t, h, map[string]any{"webhook": notificationTestWebhook(), "send_test_on_apply": true})
		planned := h.plan(t, notificationTestType, nil, config)
		requireNoProtocolErrors(t, planned.Diagnostics)
		applied := h.apply(t, notificationTestType, nil, config, planned)

		requireProtocolErrorContaining(t, applied.Diagnostics, "lookup hooks.example.com: no such host")
		requireProtocolErrorContaining(t, applied.Diagnostics, "marked tainted")
		requireNotificationAttributeError(t, applied.Diagnostics, "webhook")
		requireNoNotificationSecrets(t, applied.Diagnostics)

		state := h.decodeResource(t, notificationTestType, applied.NewState)
		if state.IsNull() || !state.IsFullyKnown() || stringAt(t, state, "id") != "ops" {
			t.Fatalf("state = %s, want the saved profile", state)
		}
		if store.stored("ops") == nil {
			t.Error("the profile was not saved")
		}
	})

	t.Run("a failed update keeps the change in state", func(t *testing.T) {
		fake := newFakeKopia()
		store := newNotificationTestStore(fake, "ops")
		h := notificationTestHarness(t, fake)

		inst := h.create(t, notificationTestType, notificationTestConfig(t, h, map[string]any{"webhook": notificationTestWebhook()}))
		store.failTests(failure)

		config := notificationTestConfig(t, h, map[string]any{
			"webhook": notificationTestWebhook(), "send_test_on_apply": true, "min_severity": "error",
		})
		planned := h.plan(t, notificationTestType, inst, config)
		requireNoProtocolErrors(t, planned.Diagnostics)
		applied := h.apply(t, notificationTestType, inst, config, planned)

		requireProtocolErrorContaining(t, applied.Diagnostics, "could not deliver its test notification")
		requireProtocolErrorContaining(t, applied.Diagnostics, "only if it changes again")
		requireNoNotificationSecrets(t, applied.Diagnostics)
		if got := stringAt(t, h.decodeResource(t, notificationTestType, applied.NewState), "min_severity"); got != "error" {
			t.Errorf("min_severity in state = %q, want the change", got)
		}
	})
}

// Kopia echoes the request body, secrets included, in the errors of its
// writes, and its delivery errors quote the webhook URL; no diagnostic may
// repeat them.
func TestNotificationProfileKeepsSecretsOutOfDiagnostics(t *testing.T) {
	// Kopia's answer to a body it cannot decode: the raw body, appended to
	// the message (internal/server/api_notification_profile.go:17-18).
	echo := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "malformed request body: "+string(raw)))
	}

	methods := map[string]map[string]any{
		"email": {"email": map[string]any{
			"smtp_server": "smtp.example.com", "smtp_username": "kopia", "smtp_password": notificationTestPassword,
			"from": "kopia@example.com", "to": "ops@example.com",
		}},
		"pushover": {"pushover": map[string]any{"app_token": notificationTestAppToken, "user_key": notificationTestUserKey}},
		"webhook":  {"webhook": notificationTestWebhook()},
	}
	for name, method := range methods {
		t.Run("create of "+name, func(t *testing.T) {
			fake := newFakeKopia()
			newNotificationTestStore(fake, "ops")
			fake.handle("POST /api/v1/notificationProfiles", echo)
			h := notificationTestHarness(t, fake)

			config := notificationTestConfig(t, h, method)
			planned := h.plan(t, notificationTestType, nil, config)
			applied := h.apply(t, notificationTestType, nil, config, planned)
			requireProtocolErrorContaining(t, applied.Diagnostics, "malformed request body")
			requireProtocolErrorContaining(t, applied.Diagnostics, "<redacted>")
			requireNoNotificationSecrets(t, applied.Diagnostics)
		})

		t.Run("update of "+name, func(t *testing.T) {
			fake := newFakeKopia()
			newNotificationTestStore(fake, "ops")
			h := notificationTestHarness(t, fake)
			inst := h.create(t, notificationTestType, notificationTestConfig(t, h, method))
			fake.handle("POST /api/v1/notificationProfiles", echo)

			changed := map[string]any{"min_severity": "error"}
			for k, v := range method {
				changed[k] = v
			}
			config := notificationTestConfig(t, h, changed)
			planned := h.plan(t, notificationTestType, inst, config)
			applied := h.apply(t, notificationTestType, inst, config, planned)
			requireProtocolErrorContaining(t, applied.Diagnostics, "Unable to update Kopia notification profile")
			requireNoNotificationSecrets(t, applied.Diagnostics)
		})
	}

	t.Run("an unsupported profile", func(t *testing.T) {
		fake := newFakeKopia()
		store := newNotificationTestStore(fake, "ops")
		store.put(t, "ops", `{"profile":"ops","method":{"type":"email","config":{"smtpPassword":"`+notificationTestPassword+
			`","smtpPort":"`+notificationTestAppToken+`"}},"minSeverity":0}`)
		h := notificationTestHarness(t, fake)

		imported := h.importResource(t, notificationTestType, "ops", nil)
		requireNoProtocolErrors(t, imported.Diagnostics)
		read := h.read(t, &resourceInstance{
			typeName: notificationTestType,
			state:    h.decodeResource(t, notificationTestType, imported.ImportedResources[0].State),
			identity: imported.ImportedResources[0].Identity,
		})
		requireProtocolErrorContaining(t, read.Diagnostics, "cannot be managed with Terraform")
		requireNoNotificationSecrets(t, read.Diagnostics)
	})
}

// The validators repeat and tighten the checks Kopia's senders make only at
// send time, and never quote a sensitive value.
func TestNotificationProfileValidation(t *testing.T) {
	email := func(overrides map[string]any) map[string]any {
		o := map[string]any{"smtp_server": "smtp.example.com", "from": "kopia@example.com", "to": "ops@example.com"}
		for k, v := range overrides {
			o[k] = v
		}
		return map[string]any{"email": o}
	}
	webhook := func(overrides map[string]any) map[string]any {
		o := map[string]any{"endpoint": "https://example.com/hook"}
		for k, v := range overrides {
			o[k] = v
		}
		return map[string]any{"webhook": o}
	}
	with := func(base map[string]any, extra map[string]any) map[string]any {
		for k, v := range extra {
			base[k] = v
		}
		return base
	}

	tests := []struct {
		name  string
		attrs map[string]any
		path  []string
		want  string
	}{
		{"empty name", with(webhook(nil), map[string]any{"name": ""}), []string{"name"}, "at least 1"},
		{"name with a slash", with(webhook(nil), map[string]any{"name": "ops/team"}), []string{"name"}, `must not contain "/"`},
		{"name .", with(webhook(nil), map[string]any{"name": "."}), []string{"name"}, "neither read nor delete"},
		{"name ..", with(webhook(nil), map[string]any{"name": ".."}), []string{"name"}, "neither read nor delete"},
		{"unknown severity", with(webhook(nil), map[string]any{"min_severity": "info"}), []string{"min_severity"}, "value must be one of"},
		{"numeric severity", with(webhook(nil), map[string]any{"min_severity": "3"}), []string{"min_severity"}, "value must be one of"},

		{"md format", email(map[string]any{"format": "md"}), []string{"email", "format"}, "value must be one of"},
		{"port 0", email(map[string]any{"smtp_port": 0}), []string{"email", "smtp_port"}, "between 1 and 65535"},
		{"port 65536", email(map[string]any{"smtp_port": 65536}), []string{"email", "smtp_port"}, "between 1 and 65535"},
		{"server with a port", email(map[string]any{"smtp_server": "smtp.example.com:465"}), []string{"email", "smtp_server"}, "without a port"},
		{"bare IPv6 server", email(map[string]any{"smtp_server": "2001:db8::25"}), []string{"email", "smtp_server"}, "in brackets"},
		{"display name", email(map[string]any{"from": "Kopia <kopia@example.com>"}), []string{"email", "from"}, "bare email address"},
		{"space after comma", email(map[string]any{"to": "a@example.com, b@example.com"}), []string{"email", "to"}, "without spaces"},
		{"trailing comma", email(map[string]any{"to": "a@example.com,"}), []string{"email", "to"}, "without spaces"},
		{"display name in cc", email(map[string]any{"cc": "Boss <boss@example.com>"}), []string{"email", "cc"}, "without spaces"},
		{"empty identity", email(map[string]any{"smtp_identity": "", "smtp_username": "u"}), []string{"email", "smtp_identity"}, "at least 1"},
		{"password without a user", email(map[string]any{"smtp_password": notificationTestPassword}), []string{"email", "smtp_password"}, "smtp_username"},
		{"identity without a user", email(map[string]any{"smtp_identity": "id"}), []string{"email", "smtp_identity"}, "smtp_username"},

		{"empty app token", map[string]any{"pushover": map[string]any{"app_token": "", "user_key": "u"}}, []string{"pushover", "app_token"}, "at least 1"},
		{"empty user key", map[string]any{"pushover": map[string]any{"app_token": "a", "user_key": ""}}, []string{"pushover", "user_key"}, "at least 1"},
		{"pushover endpoint scheme", map[string]any{"pushover": map[string]any{"app_token": "a", "user_key": "u", "endpoint": "ftp://x"}}, []string{"pushover", "endpoint"}, "http:// or https://"},

		{"endpoint scheme", webhook(map[string]any{"endpoint": "ftp://hooks.example.com/" + notificationTestHookPath}), []string{"webhook", "endpoint"}, "http:// or https://"},
		{"endpoint without a host", webhook(map[string]any{"endpoint": "http://"}), []string{"webhook", "endpoint"}, "with a host"},
		{"endpoint without a scheme", webhook(map[string]any{"endpoint": "hooks.example.com/" + notificationTestHookPath}), []string{"webhook", "endpoint"}, "absolute"},
		{"lower-case method", webhook(map[string]any{"method": "post"}), []string{"webhook", "method"}, "upper case"},
		{"upper-case format", webhook(map[string]any{"format": "HTML"}), []string{"webhook", "format"}, "value must be one of"},
		{"no headers", webhook(map[string]any{"headers": map[string]any{}}), []string{"webhook", "headers"}, "at least 1"},
		{"header name with a space", webhook(map[string]any{"headers": map[string]any{"X " + notificationTestHookPath: "v"}}), []string{"webhook", "headers"}, "not HTTP tokens"},
		{"header name with a colon", webhook(map[string]any{"headers": map[string]any{"X-A:": "v"}}), []string{"webhook", "headers"}, "not HTTP tokens"},
		{"header names differing in case", webhook(map[string]any{"headers": map[string]any{"Authorization": "a", "authorization": "b"}}), []string{"webhook", "headers"}, "Duplicate webhook header"},
		{"header value with a leading space", webhook(map[string]any{"headers": map[string]any{"Authorization": " " + notificationTestBearer}}), []string{"webhook", "headers"}, "Invalid webhook header value"},
		{"header value with a line break", webhook(map[string]any{"headers": map[string]any{"Authorization": notificationTestBearer + "\nX-B: 1"}}), []string{"webhook", "headers"}, "Invalid webhook header value"},
		{"header value with a tab", webhook(map[string]any{"headers": map[string]any{"Authorization": "Bearer\t" + notificationTestBearer}}), []string{"webhook", "headers"}, "Invalid webhook header value"},
	}
	fake := newFakeKopia()
	h := notificationTestHarness(t, fake)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diags := h.validateResource(t, notificationTestType, notificationTestConfig(t, h, tc.attrs))
			requireNotificationAttributeError(t, diags, tc.path...)
			if tc.want != "" {
				requireProtocolErrorContaining(t, diags, tc.want)
			}
			requireNoNotificationSecrets(t, diags)
		})
	}
	if routes := fake.apiRoutes(); len(routes) != 0 {
		t.Errorf("validation made requests: %v", routes)
	}
}

// Exactly one method must be set; switching from one to another updates the
// profile in place.
func TestNotificationProfileMethods(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "ops")
	h := notificationTestHarness(t, fake)

	pushover := map[string]any{"app_token": notificationTestAppToken, "user_key": notificationTestUserKey}
	for name, attrs := range map[string]map[string]any{
		"none": {},
		"two":  {"webhook": notificationTestWebhook(), "pushover": pushover},
	} {
		t.Run(name, func(t *testing.T) {
			diags := h.validateResource(t, notificationTestType, notificationTestConfig(t, h, attrs))
			requireProtocolErrorContaining(t, diags, "Exactly one of these attributes must be configured")

			r := NewNotificationProfileResource()
			direct := validateResourceConfig(t, r, resourceValue(t, r, map[string]any{"name": "ops", "webhook": attrs["webhook"], "pushover": attrs["pushover"]}))
			requireErrorContaining(t, direct, "Exactly one of these attributes must be configured")
		})
	}

	inst := h.create(t, notificationTestType, notificationTestConfig(t, h, map[string]any{"webhook": notificationTestWebhook()}))
	email := map[string]any{"email": map[string]any{"smtp_server": "smtp.example.com", "from": "kopia@example.com", "to": "ops@example.com"}}
	for _, step := range []struct {
		method string
		attrs  map[string]any
	}{
		{"email", email},
		{"pushover", map[string]any{"pushover": pushover}},
		{"webhook", map[string]any{"webhook": notificationTestWebhook()}},
	} {
		config := notificationTestConfig(t, h, step.attrs)
		planned := h.plan(t, notificationTestType, inst, config)
		requireNoProtocolErrors(t, planned.Diagnostics)
		if len(planned.RequiresReplace) != 0 {
			t.Fatalf("switching to %s replaces the profile: %v", step.method, planned.RequiresReplace)
		}
		inst = h.update(t, inst, config)

		method, _ := store.stored("ops")["method"].(map[string]any)
		if method["type"] != step.method {
			t.Errorf("stored method = %v, want %s", method["type"], step.method)
		}
		for _, other := range []string{"email", "pushover", "webhook"} {
			if isNullAt(t, inst.state, other) != (other != step.method) {
				t.Errorf("after switching to %s, %s = %s", step.method, other, valueAt(t, inst.state, other))
			}
		}
		requireNotificationPlanUnchanged(t, h, inst, config)
	}
}

// The identity is the name, and import works by name or identity, with the
// state an apply of the same configuration produces.
func TestNotificationProfileIdentityAndImport(t *testing.T) {
	fake := newFakeKopia()
	newNotificationTestStore(fake, "ops", "missing")
	h := notificationTestHarness(t, fake)

	config := notificationTestConfig(t, h, map[string]any{"min_severity": "warning", "webhook": notificationTestWebhook()})
	created := h.create(t, notificationTestType, config)
	identity := h.identityValue(t, notificationTestType, created.identity)
	if got := stringAt(t, identity, "name"); got != "ops" {
		t.Fatalf("identity name = %q, want ops", got)
	}

	for kind, imported := range map[string]*resourceInstance{
		"id":       h.importByID(t, notificationTestType, "ops"),
		"identity": h.importByIdentity(t, notificationTestType, map[string]any{"name": "ops"}),
	} {
		if !imported.state.Equal(created.state) {
			diffs, _ := imported.state.Diff(created.state)
			t.Errorf("import by %s differs from the created state: %v", kind, diffs)
		}
		if got := h.identityValue(t, notificationTestType, imported.identity); !got.Equal(identity) {
			t.Errorf("import by %s gives identity %s, want %s", kind, got, identity)
		}
		requireNotificationPlanUnchanged(t, h, imported, config)
	}

	// Importing a profile that does not exist leaves nothing, which
	// Terraform reports as a missing object.
	if missing := h.importByID(t, notificationTestType, "missing"); !missing.state.IsNull() {
		t.Errorf("import of a missing profile = %s, want no state", missing.state)
	}

	for id, want := range map[string]string{"a/b": "contains a slash", "..": "cannot be addressed"} {
		resp := h.importResource(t, notificationTestType, id, nil)
		requireProtocolErrorContaining(t, resp.Diagnostics, "Unexpected import identifier")
		requireProtocolErrorContaining(t, resp.Diagnostics, want)
	}
	resp := h.importResource(t, notificationTestType, "", h.identityData(t, notificationTestType, map[string]any{"name": ""}))
	requireProtocolErrorContaining(t, resp.Diagnostics, "Missing import identity")
}

// A profile this provider cannot represent cannot be imported or refreshed.
func TestNotificationProfileRefusesUnsupportedProfiles(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "ops")
	store.put(t, "ops", `{"profile":"ops","method":{"type":"testsender","config":{"format":"html"}},"minSeverity":0}`)
	h := notificationTestHarness(t, fake)

	imported := h.importResource(t, notificationTestType, "ops", nil)
	requireNoProtocolErrors(t, imported.Diagnostics)
	read := h.read(t, &resourceInstance{
		typeName: notificationTestType,
		state:    h.decodeResource(t, notificationTestType, imported.ImportedResources[0].State),
		identity: imported.ImportedResources[0].Identity,
	})
	requireProtocolErrorContaining(t, read.Diagnostics, "Unsupported Kopia notification profile")
	requireProtocolErrorContaining(t, read.Diagnostics, `"testsender"`)
}

// A port stored as a string, as another client may have written it, is read
// with a warning: Kopia cannot send through the profile until it is saved
// again.
func TestNotificationProfileWarnsAboutAPortStoredAsAString(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "ops")
	store.put(t, "ops", `{"profile":"ops","method":{"type":"email","config":{"smtpServer":"smtp.example.com",
		"smtpPort":"2525","from":"kopia@example.com","to":"ops@example.com"}},"minSeverity":0}`)
	h := notificationTestHarness(t, fake)

	imported := h.importResource(t, notificationTestType, "ops", nil)
	requireNoProtocolErrors(t, imported.Diagnostics)
	read := h.read(t, &resourceInstance{
		typeName: notificationTestType,
		state:    h.decodeResource(t, notificationTestType, imported.ImportedResources[0].State),
		identity: imported.ImportedResources[0].Identity,
	})
	requireNoProtocolErrors(t, read.Diagnostics)
	requireProtocolWarningContaining(t, read.Diagnostics, "stored as a string")
	if got := int64At(t, h.decodeResource(t, notificationTestType, read.NewState), "email", "smtp_port"); got != 2525 {
		t.Errorf("smtp_port = %d, want 2525", got)
	}
}

// When reading the profile back after a create fails, the profile exists all
// the same, so it is recorded in state and marked tainted rather than lost.
func TestNotificationProfileCreateReadBackFailure(t *testing.T) {
	fake := newFakeKopia()
	store := newNotificationTestStore(fake, "ops")
	var mu sync.Mutex
	gets := 0
	fake.handle("GET /api/v1/notificationProfiles/ops", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		gets++
		first := gets == 1
		mu.Unlock()
		if first {
			writeFakeResponse(w, http.StatusInternalServerError, fakeErrorBody(kopiaclient.CodeInternal, "internal server error: profile not found"))
			return
		}
		writeFakeResponse(w, http.StatusInternalServerError, fakeErrorBody(kopiaclient.CodeInternal, "internal server error: repository is read-only"))
	})
	h := notificationTestHarness(t, fake)

	config := notificationTestConfig(t, h, map[string]any{"webhook": notificationTestWebhook(), "send_test_on_apply": true})
	planned := h.plan(t, notificationTestType, nil, config)
	applied := h.apply(t, notificationTestType, nil, config, planned)

	requireProtocolErrorContaining(t, applied.Diagnostics, "Unable to read Kopia notification profile back after creation")
	requireProtocolErrorContaining(t, applied.Diagnostics, "repository is read-only")
	state := h.decodeResource(t, notificationTestType, applied.NewState)
	if state.IsNull() || !state.IsFullyKnown() || stringAt(t, state, "id") != "ops" {
		t.Fatalf("state = %s, want the planned profile with its id", state)
	}
	if store.stored("ops") == nil {
		t.Error("the profile was not saved")
	}
	// Without a profile read back, no test is sent.
	if got := len(store.testsSent()); got != 0 {
		t.Errorf("tests sent = %d, want none", got)
	}
}

// --- acceptance ---------------------------------------------------------------

// testAccCheckNotificationProfilesDestroyed checks that destroying the
// configuration deleted the named profiles from the server.
func testAccCheckNotificationProfilesDestroyed(t *testing.T, names ...string) acc.TestCheckFunc {
	return func(*terraform.State) error {
		c, err := testAccClient()
		if err != nil {
			return err
		}
		for _, name := range names {
			_, err := c.GetNotificationProfile(t.Context(), name)
			if err == nil {
				return fmt.Errorf("notification profile %q still exists", name)
			}
			if !kopiaclient.IsNotFound(err) {
				return fmt.Errorf("checking notification profile %q: %w", name, err)
			}
		}
		return nil
	}
}

func TestAccNotificationProfileResource_webhook(t *testing.T) {
	const address = "kopia_notification_profile.test"
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckNotificationProfilesDestroyed(t, "tfacc-webhook"),
		Steps: []acc.TestStep{
			{
				// Create with the defaults left to the provider. The endpoint is
				// never contacted: send_test_on_apply stays off.
				Config: providerConfig + `
resource "kopia_notification_profile" "test" {
  name         = "tfacc-webhook"
  min_severity = "warning"

  webhook = {
    endpoint = "https://ntfy.example.invalid/tfacc?token=tk_tfacc"
    headers = {
      Authorization = "Bearer tk_tfacc"
      Priority      = "high"
    }
  }
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "id", "tfacc-webhook"),
					acc.TestCheckResourceAttr(address, "min_severity", "warning"),
					acc.TestCheckResourceAttr(address, "webhook.method", "POST"),
					acc.TestCheckResourceAttr(address, "webhook.format", "txt"),
					acc.TestCheckResourceAttr(address, "webhook.headers.%", "2"),
					acc.TestCheckResourceAttr(address, "webhook.headers.Priority", "high"),
					acc.TestCheckResourceAttr(address, "send_test_on_apply", "false"),
					acc.TestCheckNoResourceAttr(address, "email.smtp_server"),
				),
			},
			{
				// Import must round-trip without producing a diff: Kopia
				// returns every option, secrets included.
				ResourceName:      address,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Every option changes in place.
				Config: providerConfig + `
resource "kopia_notification_profile" "test" {
  name         = "tfacc-webhook"
  min_severity = "error"

  webhook = {
    endpoint = "https://ntfy.example.invalid/tfacc-changed"
    method   = "PUT"
    format   = "html"
    headers = {
      "Content-Type" = "text/html"
    }
  }
}
`,
				ConfigPlanChecks: acc.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
				},
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "min_severity", "error"),
					acc.TestCheckResourceAttr(address, "webhook.method", "PUT"),
					acc.TestCheckResourceAttr(address, "webhook.format", "html"),
					acc.TestCheckResourceAttr(address, "webhook.headers.%", "1"),
					acc.TestCheckResourceAttr(address, "webhook.headers.Content-Type", "text/html"),
				),
			},
			{
				// Moving to another method keeps the profile and its name.
				Config: providerConfig + `
resource "kopia_notification_profile" "test" {
  name = "tfacc-webhook"

  email = {
    smtp_server = "smtp.example.invalid"
    from        = "kopia@example.invalid"
    to          = "ops@example.invalid"
  }
}
`,
				ConfigPlanChecks: acc.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
				},
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "min_severity", "report"),
					acc.TestCheckResourceAttr(address, "email.smtp_port", "587"),
					acc.TestCheckResourceAttr(address, "email.format", "html"),
					acc.TestCheckNoResourceAttr(address, "webhook.endpoint"),
				),
			},
		},
	})
}

func TestAccNotificationProfileResource_email(t *testing.T) {
	const address = "kopia_notification_profile.test"
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckNotificationProfilesDestroyed(t, "tfacc-email"),
		Steps: []acc.TestStep{
			{
				// Every option set.
				Config: providerConfig + `
resource "kopia_notification_profile" "test" {
  name         = "tfacc-email"
  min_severity = "error"

  email = {
    smtp_server   = "smtp.example.invalid"
    smtp_port     = 2525
    smtp_identity = "tfacc"
    smtp_username = "kopia@example.invalid"
    smtp_password = "tfacc-smtp-password"
    from          = "kopia@example.invalid"
    to            = "ops@example.invalid,oncall@example.invalid"
    cc            = "boss@example.invalid"
    format        = "txt"
  }
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "email.smtp_port", "2525"),
					acc.TestCheckResourceAttr(address, "email.smtp_password", "tfacc-smtp-password"),
					acc.TestCheckResourceAttr(address, "email.cc", "boss@example.invalid"),
					acc.TestCheckResourceAttr(address, "email.format", "txt"),
				),
			},
			{
				// Import must round-trip without producing a diff.
				ResourceName:      address,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Leaving the optional options out clears them in Kopia: they
				// read back as null, not as empty strings.
				Config: providerConfig + `
resource "kopia_notification_profile" "test" {
  name = "tfacc-email"

  email = {
    smtp_server = "smtp.example.invalid"
    from        = "kopia@example.invalid"
    to          = "ops@example.invalid"
  }
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "email.smtp_port", "587"),
					acc.TestCheckResourceAttr(address, "email.format", "html"),
					acc.TestCheckNoResourceAttr(address, "email.smtp_identity"),
					acc.TestCheckNoResourceAttr(address, "email.smtp_username"),
					acc.TestCheckNoResourceAttr(address, "email.smtp_password"),
					acc.TestCheckNoResourceAttr(address, "email.cc"),
				),
			},
		},
	})
}

func TestAccNotificationProfileResource_pushover(t *testing.T) {
	const address = "kopia_notification_profile.test"
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckNotificationProfilesDestroyed(t, "tfacc-pushover"),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
resource "kopia_notification_profile" "test" {
  name = "tfacc-pushover"

  pushover = {
    app_token = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
    user_key  = "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"
  }
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "pushover.format", "txt"),
					acc.TestCheckNoResourceAttr(address, "pushover.endpoint"),
				),
			},
			{
				// Import must round-trip without producing a diff.
				ResourceName:      address,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: providerConfig + `
resource "kopia_notification_profile" "test" {
  name         = "tfacc-pushover"
  min_severity = "warning"

  pushover = {
    app_token = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
    user_key  = "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"
    format    = "html"
    endpoint  = "https://pushover.example.invalid/1/messages.json"
  }
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr(address, "min_severity", "warning"),
					acc.TestCheckResourceAttr(address, "pushover.format", "html"),
					acc.TestCheckResourceAttr(address, "pushover.endpoint", "https://pushover.example.invalid/1/messages.json"),
				),
			},
		},
	})
}

// Creating a profile under a name that is taken fails instead of overwriting
// it.
func TestAccNotificationProfileResource_existingName(t *testing.T) {
	config := providerConfig + `
resource "kopia_notification_profile" "first" {
  name    = "tfacc-taken"
  webhook = { endpoint = "https://ntfy.example.invalid/first" }
}
`
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckNotificationProfilesDestroyed(t, "tfacc-taken"),
		Steps: []acc.TestStep{
			{Config: config},
			{
				Config: config + `
resource "kopia_notification_profile" "second" {
  name    = "tfacc-taken"
  webhook = { endpoint = "https://ntfy.example.invalid/second" }

  depends_on = [kopia_notification_profile.first]
}
`,
				ExpectError: regexp.MustCompile(`already has a notification profile named "tfacc-taken"`),
			},
			{
				// The first profile is untouched.
				Config: config,
				Check:  acc.TestCheckResourceAttr("kopia_notification_profile.first", "webhook.endpoint", "https://ntfy.example.invalid/first"),
			},
		},
	})
}

// Import by identity, and the identity recorded in state, need Terraform 1.12.
func TestAccNotificationProfileResource_identity(t *testing.T) {
	const address = "kopia_notification_profile.test"
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			// Resource identity, and import blocks that use one, arrived in
			// Terraform 1.12.
			tfversion.SkipBelow(tfversion.Version1_12_0),
		},
		CheckDestroy: testAccCheckNotificationProfilesDestroyed(t, "tfacc-identity"),
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
resource "kopia_notification_profile" "test" {
  name    = "tfacc-identity"
  webhook = { endpoint = "https://ntfy.example.invalid/identity" }
}
`,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectIdentityValueMatchesState(address, tfjsonpath.New("name")),
				},
			},
			{
				ResourceName:    address,
				ImportState:     true,
				ImportStateKind: acc.ImportBlockWithResourceIdentity,
			},
			{
				ResourceName:    address,
				ImportState:     true,
				ImportStateKind: acc.ImportBlockWithID,
			},
		},
	})
}
