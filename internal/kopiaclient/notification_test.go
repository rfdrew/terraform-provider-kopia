package kopiaclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Secrets used by the notification tests, chosen so that a leak is easy to
// spot in any error text.
const (
	notificationTestSMTPPassword = "smtp-pa55word&<x>"
	notificationTestAppToken     = "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
	notificationTestUserKey      = "uQiRzpo4DXghDmr9QzzfQu27cmVRsG"
	notificationTestWebhookURL   = "https://hooks.example.com/services/T000/B000/XXXXsecretXXXX?token=tk_query&x=1"
	notificationTestBearer       = "Bearer tk_header_secret"
)

// notificationTestSecrets are every secret above, for leak checks.
var notificationTestSecrets = []string{
	notificationTestSMTPPassword, notificationTestAppToken, notificationTestUserKey,
	"XXXXsecretXXXX", "tk_query", "tk_header_secret",
}

// requireNoNotificationSecret fails the test when text contains any of the
// test secrets, in any form Redact covers.
func requireNoNotificationSecret(t *testing.T, what, text string) {
	t.Helper()
	for _, secret := range notificationTestSecrets {
		if Redact(text, secret) != text {
			t.Errorf("%s contains the secret %q: %s", what, secret, text)
		}
	}
}

// notificationReleaseJSON re-encodes a JSON document the way a 0.22 or 0.23 server
// returns a stored profile: through a generic map, so with sorted keys and
// numbers as float64, indented by two spaces, with HTML escaped.
func notificationReleaseJSON(t *testing.T, document string) string {
	t.Helper()
	var generic any
	if err := json.Unmarshal([]byte(document), &generic); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	encoded, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

// requireNotificationJSONEqual compares two JSON documents by value.
func requireNotificationJSONEqual(t *testing.T, what string, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s is not JSON: %v (%s)", what, err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected %s is not JSON: %v", what, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s = %s, want %s", what, got, want)
	}
}

func TestNotificationSeverityNames(t *testing.T) {
	want := map[string]int32{"verbose": -100, "success": -10, "report": 0, "warning": 10, "error": 20}
	names := NotificationSeverityNames()
	if got := strings.Join(names, ","); got != "verbose,success,report,warning,error" {
		t.Errorf("NotificationSeverityNames() = %s, want them from the lowest to the highest", got)
	}
	for name, value := range want {
		if got, ok := ParseNotificationSeverity(name); !ok || got != value {
			t.Errorf("ParseNotificationSeverity(%q) = %d, %v, want %d", name, got, ok, value)
		}
		if got, ok := NotificationSeverityName(value); !ok || got != name {
			t.Errorf("NotificationSeverityName(%d) = %q, %v, want %q", value, got, ok, name)
		}
	}
	// Kopia accepts any int32, and the CLI's names are case-sensitive.
	for _, value := range []int32{3, -1, 21, -2147483648} {
		if name, ok := NotificationSeverityName(value); ok {
			t.Errorf("NotificationSeverityName(%d) = %q, want no name", value, name)
		}
	}
	for _, name := range []string{"", "Error", "info", "10"} {
		if _, ok := ParseNotificationSeverity(name); ok {
			t.Errorf("ParseNotificationSeverity(%q) succeeded, want no severity", name)
		}
	}
}

// Kopia stores the posted options as they are and applies its defaults only
// in memory, so every option is written out, defaults included, and what GET
// returns is then exactly what was sent (spec: notification.md §7.1).
func TestSaveNotificationProfileWritesEveryOption(t *testing.T) {
	tests := []struct {
		name    string
		profile NotificationProfile
		want    string
	}{
		{
			name: "email with defaults",
			profile: NotificationProfile{Name: "mail", MinSeverity: NotificationSeverityError, Email: &EmailNotificationOptions{
				SMTPServer: "smtp.example.com", SMTPUsername: "kopia@example.com", SMTPPassword: "s3cret",
				From: "kopia@example.com", To: "ops@example.com,oncall@example.com",
			}},
			want: `{"profile":"mail","minSeverity":20,"method":{"type":"email","config":{
				"smtpServer":"smtp.example.com","smtpPort":587,"smtpIdentity":"","smtpUsername":"kopia@example.com",
				"smtpPassword":"s3cret","from":"kopia@example.com","to":"ops@example.com,oncall@example.com",
				"cc":"","format":"html"}}}`,
		},
		{
			name: "pushover without an endpoint",
			profile: NotificationProfile{Name: "phone", Pushover: &PushoverNotificationOptions{
				AppToken: notificationTestAppToken, UserKey: notificationTestUserKey,
			}},
			want: `{"profile":"phone","minSeverity":0,"method":{"type":"pushover","config":{
				"appToken":"` + notificationTestAppToken + `","userKey":"` + notificationTestUserKey + `","format":"txt"}}}`,
		},
		{
			name: "pushover with an endpoint",
			profile: NotificationProfile{Name: "phone", MinSeverity: NotificationSeverityVerbose, Pushover: &PushoverNotificationOptions{
				AppToken: "a", UserKey: "u", Format: "html", Endpoint: "https://pushover.example.com/1/messages.json",
			}},
			want: `{"profile":"phone","minSeverity":-100,"method":{"type":"pushover","config":{
				"appToken":"a","userKey":"u","format":"html","endpoint":"https://pushover.example.com/1/messages.json"}}}`,
		},
		{
			name: "webhook with defaults",
			profile: NotificationProfile{Name: "ops", MinSeverity: NotificationSeverityWarning, Webhook: &WebhookNotificationOptions{
				Endpoint: "https://ntfy.example.org/kopia", Headers: "Authorization: Bearer tk\nPriority: high",
			}},
			want: `{"profile":"ops","minSeverity":10,"method":{"type":"webhook","config":{
				"endpoint":"https://ntfy.example.org/kopia","method":"POST","format":"txt",
				"headers":"Authorization: Bearer tk\nPriority: high"}}}`,
		},
		{
			name: "webhook with every option",
			profile: NotificationProfile{Name: "ops", Webhook: &WebhookNotificationOptions{
				Endpoint: "http://hooks.internal/kopia", Method: "PUT", Format: "html",
			}},
			want: `{"profile":"ops","minSeverity":0,"method":{"type":"webhook","config":{
				"endpoint":"http://hooks.internal/kopia","method":"PUT","format":"html","headers":""}}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.reply("POST /api/v1/notificationProfiles", http.StatusOK, "{}\n")

			if err := fake.client(t).SaveNotificationProfile(context.Background(), tc.profile); err != nil {
				t.Fatalf("SaveNotificationProfile() error = %v", err)
			}
			requireNotificationJSONEqual(t, "request body", fake.lastCall(t, "POST /api/v1/notificationProfiles").body, tc.want)
		})
	}
}

// Kopia stores a body without a method, or under a name its routes cannot
// address, and both leave a profile that cannot be read or deleted (spec:
// notification.md §2.4, §5.1), so the client refuses them without a request.
func TestSaveNotificationProfileRefusesWhatKopiaCannotUse(t *testing.T) {
	webhook := &WebhookNotificationOptions{Endpoint: "https://example.com/hook"}
	tests := []struct {
		name    string
		profile NotificationProfile
		want    string
	}{
		{"empty name", NotificationProfile{Webhook: webhook}, "must not be empty"},
		{"slash", NotificationProfile{Name: "ops/team", Webhook: webhook}, "contains a slash"},
		{"dot", NotificationProfile{Name: ".", Webhook: webhook}, "cannot be addressed"},
		{"dot dot", NotificationProfile{Name: "..", Webhook: webhook}, "cannot be addressed"},
		{"no method", NotificationProfile{Name: "ops"}, "exactly one method"},
		{"two methods", NotificationProfile{Name: "ops", Webhook: webhook, Pushover: &PushoverNotificationOptions{}}, "exactly one method"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			err := fake.client(t).SaveNotificationProfile(context.Background(), tc.profile)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SaveNotificationProfile() error = %v, want one containing %q", err, tc.want)
			}
			if routes := fake.routes(); len(routes) != 0 {
				t.Errorf("requests were made: %v", routes)
			}
		})
	}
}

// Profiles come back as a 0.22 or 0.23 server sends them: config keys
// sorted, the body indented.
func TestGetNotificationProfileDecodesEachMethod(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		want   NotificationProfile
	}{
		{
			name: "email",
			stored: `{"profile":"mail","method":{"type":"email","config":{"cc":"boss@example.com","format":"txt",
				"from":"kopia@example.com","smtpIdentity":"id","smtpPassword":"s3cret","smtpPort":2525,
				"smtpServer":"smtp.example.com","smtpUsername":"kopia","to":"ops@example.com"}},"minSeverity":20}`,
			want: NotificationProfile{Name: "mail", MinSeverity: 20, Email: &EmailNotificationOptions{
				SMTPServer: "smtp.example.com", SMTPPort: 2525, SMTPIdentity: "id", SMTPUsername: "kopia",
				SMTPPassword: "s3cret", From: "kopia@example.com", To: "ops@example.com", CC: "boss@example.com", Format: "txt",
			}},
		},
		{
			name: "pushover",
			stored: `{"profile":"phone","method":{"type":"pushover","config":{"appToken":"app","format":"html",
				"userKey":"user"}},"minSeverity":-10}`,
			want: NotificationProfile{Name: "phone", MinSeverity: -10, Pushover: &PushoverNotificationOptions{
				AppToken: "app", UserKey: "user", Format: "html",
			}},
		},
		{
			name: "webhook",
			stored: `{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"https://ntfy.example.org/kopia?a=1&b=<2>",
				"format":"txt","headers":"Authorization: Bearer tk\nPriority: high","method":"PUT"}},"minSeverity":3}`,
			want: NotificationProfile{Name: "ops", MinSeverity: 3, Webhook: &WebhookNotificationOptions{
				Endpoint: "https://ntfy.example.org/kopia?a=1&b=<2>", Method: "PUT", Format: "txt",
				Headers: "Authorization: Bearer tk\nPriority: high",
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			route := "GET /api/v1/notificationProfiles/" + tc.want.Name
			fake.reply(route, http.StatusOK, notificationReleaseJSON(t, tc.stored))

			got, err := fake.client(t).GetNotificationProfile(context.Background(), tc.want.Name)
			if err != nil {
				t.Fatalf("GetNotificationProfile() error = %v", err)
			}
			if !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("GetNotificationProfile() = %+v, want %+v", *got, tc.want)
			}
			if got.MethodType() != tc.name {
				t.Errorf("MethodType() = %q, want %q", got.MethodType(), tc.name)
			}
		})
	}
}

// Profiles written by the CLI, the web UI or another client need not look
// like the ones this package writes. They are read the way Kopia's sender
// reads them (spec: notification.md §7.3).
func TestGetNotificationProfileReadsOtherShapesTolerantly(t *testing.T) {
	tests := []struct {
		name         string
		stored       string
		want         NotificationProfile
		wantWarnings int
	}{
		{
			name: "missing options take Kopia's defaults",
			stored: `{"profile":"x","method":{"type":"email","config":{"smtpServer":"smtp.example.com",
				"from":"a@example.com","to":"b@example.com"}}}`,
			want: NotificationProfile{Name: "x", Email: &EmailNotificationOptions{
				SMTPServer: "smtp.example.com", SMTPPort: 587, From: "a@example.com", To: "b@example.com", Format: "html",
			}},
		},
		{
			name: "zero and null options take Kopia's defaults",
			stored: `{"profile":"x","method":{"type":"webhook","config":{"endpoint":"https://example.com/",
				"method":"","format":null,"headers":null}},"minSeverity":null}`,
			want: NotificationProfile{Name: "x", Webhook: &WebhookNotificationOptions{
				Endpoint: "https://example.com/", Method: "POST", Format: "txt",
			}},
		},
		{
			name:   "an empty config",
			stored: `{"profile":"x","method":{"type":"pushover","config":{}},"minSeverity":0}`,
			want:   NotificationProfile{Name: "x", Pushover: &PushoverNotificationOptions{Format: "txt"}},
		},
		{
			name: "keys in another letter case, the last in sorted order winning",
			stored: `{"profile":"x","method":{"type":"email","config":{"SMTPServer":"loses.example.com",
				"smtpServer":"wins.example.com","SMTPPORT":25,"From":"a@example.com","TO":"b@example.com"}}}`,
			want: NotificationProfile{Name: "x", Email: &EmailNotificationOptions{
				SMTPServer: "wins.example.com", SMTPPort: 25, From: "a@example.com", To: "b@example.com", Format: "html",
			}},
		},
		{
			// A null leaves the field alone when Kopia decodes it, so it
			// does not undo a value an earlier key in sorted order set.
			name: "a null does not override another letter case",
			stored: `{"profile":"x","method":{"type":"webhook","config":{"ENDPOINT":"https://example.com/",
				"endpoint":null}}}`,
			want: NotificationProfile{Name: "x", Webhook: &WebhookNotificationOptions{
				Endpoint: "https://example.com/", Method: "POST", Format: "txt",
			}},
		},
		{
			name:   "unknown keys are ignored",
			stored: `{"profile":"x","method":{"type":"pushover","config":{"appToken":"a","userKey":"u","priority":2,"sound":"siren"}}}`,
			want:   NotificationProfile{Name: "x", Pushover: &PushoverNotificationOptions{AppToken: "a", UserKey: "u", Format: "txt"}},
		},
		{
			name:   "a port sent as a float with no fraction comes back as an integer",
			stored: `{"profile":"x","method":{"type":"email","config":{"smtpPort":465.0}}}`,
			want:   NotificationProfile{Name: "x", Email: &EmailNotificationOptions{SMTPPort: 465, Format: "html"}},
		},
		{
			name:         "a port stored as a string is read, with a warning",
			stored:       `{"profile":"x","method":{"type":"email","config":{"smtpPort":"2525"}}}`,
			want:         NotificationProfile{Name: "x", Email: &EmailNotificationOptions{SMTPPort: 2525, Format: "html"}},
			wantWarnings: 1,
		},
		{
			name:   "invalid values Kopia would refuse are kept, so they show as drift",
			stored: `{"profile":"x","method":{"type":"webhook","config":{"format":"md","method":"post","endpoint":"ftp://x"}},"minSeverity":-5}`,
			want: NotificationProfile{Name: "x", MinSeverity: -5, Webhook: &WebhookNotificationOptions{
				Endpoint: "ftp://x", Method: "post", Format: "md",
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.reply("GET /api/v1/notificationProfiles/x", http.StatusOK, notificationReleaseJSON(t, tc.stored))

			got, err := fake.client(t).GetNotificationProfile(context.Background(), "x")
			if err != nil {
				t.Fatalf("GetNotificationProfile() error = %v", err)
			}
			if len(got.Warnings) != tc.wantWarnings {
				t.Errorf("Warnings = %q, want %d", got.Warnings, tc.wantWarnings)
			}
			got.Warnings = nil
			if !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("GetNotificationProfile() = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// A stored profile this package cannot represent is an error, which says what
// is wrong without quoting the options, because they hold secrets.
func TestGetNotificationProfileRefusesWhatItCannotRepresent(t *testing.T) {
	secretConfig := `"smtpPassword":"` + notificationTestSMTPPassword + `","appToken":"` + notificationTestAppToken + `"`
	tests := []struct {
		name   string
		stored string
		want   string
	}{
		{"testsender", `{"profile":"x","method":{"type":"testsender","config":{"format":"html",` + secretConfig + `}}}`, `unsupported notification profile: its method "testsender" is none of email, pushover and webhook`},
		{"a type in another case", `{"profile":"x","method":{"type":"WEBHOOK","config":{}}}`, `its method "WEBHOOK" is none of`},
		{"no method", `{"profile":"x","minSeverity":0}`, "unsupported notification profile: it has no method"},
		{"null config", `{"profile":"x","method":{"type":"email","config":null}}`, "email options: the stored options are null, not an object"},
		{"string config", `{"profile":"x","method":{"type":"email","config":"` + notificationTestSMTPPassword + `"}}`, "options are a string, not an object"},
		{"array config", `{"profile":"x","method":{"type":"pushover","config":["` + notificationTestAppToken + `"]}}`, "options are an array, not an object"},
		{"fractional port", `{"profile":"x","method":{"type":"email","config":{"smtpPort":587.5,` + secretConfig + `}}}`, "option smtpPort is a number, not a whole number"},
		{"huge port", `{"profile":"x","method":{"type":"email","config":{"smtpPort":1e21}}}`, "option smtpPort is a number, not a whole number"},
		{"non-numeric port string", `{"profile":"x","method":{"type":"email","config":{"smtpPort":"` + notificationTestSMTPPassword + `"}}}`, "option smtpPort is a string that is not a whole number"},
		{"boolean port", `{"profile":"x","method":{"type":"email","config":{"smtpPort":true}}}`, "option smtpPort is a boolean"},
		{"number where a string belongs", `{"profile":"x","method":{"type":"email","config":{"to":123,` + secretConfig + `}}}`, "option to is a number, not a string"},
		{"object where a string belongs", `{"profile":"x","method":{"type":"webhook","config":{"headers":{"Authorization":"` + notificationTestBearer + `"}}}}`, "option headers is an object, not a string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.reply("GET /api/v1/notificationProfiles/x", http.StatusOK, notificationReleaseJSON(t, tc.stored))

			_, err := fake.client(t).GetNotificationProfile(context.Background(), "x")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GetNotificationProfile() error = %v, want one containing %q", err, tc.want)
			}
			if !errors.Is(err, ErrUnsupportedNotificationProfile) {
				t.Errorf("error %v does not wrap ErrUnsupportedNotificationProfile", err)
			}
			requireNoNotificationSecret(t, "error", err.Error())
		})
	}
}

// A missing profile is 500 INTERNAL "profile not found" in every supported
// release (spec: notification.md §5.2), which IsNotFound must recognise; any
// other 500 must not be taken for a missing profile.
func TestGetNotificationProfileMapsProfileNotFound(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/notificationProfiles/gone", http.StatusInternalServerError, CodeInternal, "internal server error: profile not found")
	fake.replyError("GET /api/v1/notificationProfiles/broken", http.StatusInternalServerError, CodeInternal,
		`internal server error: unable to get notification profile: unable to unmarshal "k1234": sender type '' not registered`)
	c := fake.client(t)

	_, err := c.GetNotificationProfile(context.Background(), "gone")
	if !IsNotFound(err) {
		t.Fatalf("GetNotificationProfile(gone) error = %v, want one IsNotFound recognises", err)
	}
	if !strings.Contains(err.Error(), `notification profile "gone" does not exist`) {
		t.Errorf("error = %q, want it to say the profile does not exist", err)
	}

	_, err = c.GetNotificationProfile(context.Background(), "broken")
	if err == nil || IsNotFound(err) {
		t.Fatalf("GetNotificationProfile(broken) error = %v, want an error IsNotFound does not recognise", err)
	}
	if !strings.Contains(err.Error(), "sender type '' not registered") {
		t.Errorf("error = %q, want Kopia's message", err)
	}
}

// Names are one path segment, escaped; what Kopia's router decodes must be
// the name itself.
func TestNotificationProfilePathsAreEscaped(t *testing.T) {
	const name = "ops team?#%&ü"
	const escaped = "/api/v1/notificationProfiles/ops%20team%3F%23%25&%C3%BC"
	stored := notificationReleaseJSON(t, `{"profile":"`+name+`","method":{"type":"webhook","config":{"endpoint":"https://example.com/"}}}`)

	fake := newFakeKopia()
	var paths []string
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		fake.handle(method+" /api/v1/notificationProfiles/"+name, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.EscapedPath())
			if r.Method == http.MethodGet {
				writeFakeResponse(w, http.StatusOK, stored)
				return
			}
			writeFakeResponse(w, http.StatusOK, "{}\n")
		})
	}
	c := fake.client(t)

	profile, err := c.GetNotificationProfile(context.Background(), name)
	if err != nil {
		t.Fatalf("GetNotificationProfile() error = %v", err)
	}
	if profile.Name != name {
		t.Errorf("Name = %q, want %q", profile.Name, name)
	}
	if err := c.DeleteNotificationProfile(context.Background(), name); err != nil {
		t.Fatalf("DeleteNotificationProfile() error = %v", err)
	}
	if want := []string{escaped, escaped}; !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %q, want %q", paths, want)
	}

	for _, bad := range []string{"", "a/b", ".", ".."} {
		if _, err := c.GetNotificationProfile(context.Background(), bad); err == nil {
			t.Errorf("GetNotificationProfile(%q) succeeded, want an error", bad)
		}
		if err := c.DeleteNotificationProfile(context.Background(), bad); err == nil {
			t.Errorf("DeleteNotificationProfile(%q) succeeded, want an error", bad)
		}
	}
	if got := fake.count("GET /api/v1/notificationProfiles/" + name); got != 1 {
		t.Errorf("GET requests = %d, want only the valid one", got)
	}
}

// Kopia answers DELETE with 200 whether or not the profile exists.
func TestDeleteNotificationProfile(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("DELETE /api/v1/notificationProfiles/ops", http.StatusOK, "{}\n")

	if err := fake.client(t).DeleteNotificationProfile(context.Background(), "ops"); err != nil {
		t.Fatalf("DeleteNotificationProfile() error = %v", err)
	}
	if !fake.called("DELETE /api/v1/notificationProfiles/ops") {
		t.Errorf("routes = %v, want the DELETE", fake.routes())
	}
}

func TestListNotificationProfiles(t *testing.T) {
	t.Run("null is an empty list", func(t *testing.T) {
		fake := newFakeKopia()
		fake.reply("GET /api/v1/notificationProfiles", http.StatusOK, "null\n")

		got, err := fake.client(t).ListNotificationProfiles(context.Background())
		if err != nil {
			t.Fatalf("ListNotificationProfiles() error = %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("ListNotificationProfiles() = %#v, want an empty, non-nil slice", got)
		}
	})

	t.Run("every type is listed in Kopia's order", func(t *testing.T) {
		fake := newFakeKopia()
		fake.reply("GET /api/v1/notificationProfiles", http.StatusOK, notificationReleaseJSON(t, `[
			{"profile":"ops","method":{"type":"webhook","config":{"endpoint":"`+notificationTestWebhookURL+`"}},"minSeverity":10},
			{"profile":"mail","method":{"type":"email","config":{"smtpPassword":"`+notificationTestSMTPPassword+`"}},"minSeverity":3},
			{"profile":"test","method":{"type":"testsender","config":null},"minSeverity":-100},
			{"profile":"ops","method":{"type":"pushover","config":"not an object"},"minSeverity":0}
		]`))

		got, err := fake.client(t).ListNotificationProfiles(context.Background())
		if err != nil {
			t.Fatalf("ListNotificationProfiles() error = %v", err)
		}
		want := []NotificationProfileSummary{
			{Name: "ops", MethodType: "webhook", MinSeverity: 10},
			{Name: "mail", MethodType: "email", MinSeverity: 3},
			{Name: "test", MethodType: "testsender", MinSeverity: -100},
			{Name: "ops", MethodType: "pushover", MinSeverity: 0},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ListNotificationProfiles() = %+v, want %+v", got, want)
		}
	})

	t.Run("a failure is reported", func(t *testing.T) {
		fake := newFakeKopia()
		fake.replyError("GET /api/v1/notificationProfiles", http.StatusInternalServerError, CodeInternal,
			`internal server error: unable to get notification profile: unable to unmarshal "k1": sender type '' not registered`)

		_, err := fake.client(t).ListNotificationProfiles(context.Background())
		if err == nil || !strings.Contains(err.Error(), "sender type '' not registered") {
			t.Fatalf("ListNotificationProfiles() error = %v, want Kopia's message", err)
		}
	})
}

// do quotes the body of a successful response that is not valid JSON; for a
// profile, that body would hold its secrets.
func TestNotificationReadsKeepResponseBodiesOutOfErrors(t *testing.T) {
	broken := `{"profile":"x","method":{"type":"email","config":{"smtpPassword":"` + notificationTestSMTPPassword + `"}}` + " trailing"
	fake := newFakeKopia()
	fake.reply("GET /api/v1/notificationProfiles/x", http.StatusOK, broken)
	fake.reply("GET /api/v1/notificationProfiles", http.StatusOK, "["+broken+"]")
	c := fake.client(t)

	_, err := c.GetNotificationProfile(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("GetNotificationProfile() error = %v, want one about invalid JSON", err)
	}
	requireNoNotificationSecret(t, "GetNotificationProfile() error", err.Error())

	_, err = c.ListNotificationProfiles(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("ListNotificationProfiles() error = %v, want one about invalid JSON", err)
	}
	requireNoNotificationSecret(t, "ListNotificationProfiles() error", err.Error())
}

// notificationSecretProfile has every kind of secret a profile can hold, for
// the redaction tests: a webhook whose URL and headers carry tokens.
func notificationSecretProfile() NotificationProfile {
	return NotificationProfile{Name: "ops", Webhook: &WebhookNotificationOptions{
		Endpoint: notificationTestWebhookURL,
		Headers:  "Authorization: " + notificationTestBearer + "\nPriority: high",
	}}
}

// echoNotificationMalformedRequest answers the way Kopia's profile handlers answer a
// body they cannot decode: 400 MALFORMED_REQUEST with the raw body appended
// to the message (internal/server/api_notification_profile.go:17-18), in the
// releases' indented, HTML-escaped envelope.
func echoNotificationMalformedRequest(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(CodeMalformedRequest, "malformed request body: "+string(raw)))
}

func TestNotificationWritesRedactSecretsFromErrors(t *testing.T) {
	tests := []struct {
		name    string
		route   string
		handler http.HandlerFunc
		call    func(*Client, NotificationProfile) error
		want    string
	}{
		{
			name:    "save, body echoed",
			route:   "POST /api/v1/notificationProfiles",
			handler: echoNotificationMalformedRequest,
			call: func(c *Client, p NotificationProfile) error {
				return c.SaveNotificationProfile(context.Background(), p)
			},
			want: "malformed request body",
		},
		{
			name:    "test, body echoed",
			route:   "POST /api/v1/testNotificationProfile",
			handler: echoNotificationMalformedRequest,
			call: func(c *Client, p NotificationProfile) error {
				return c.TestNotificationProfile(context.Background(), p)
			},
			want: "malformed request body",
		},
		{
			// The webhook sender's transport errors quote the full URL,
			// query string included (spec: notification.md §5.5).
			name:  "test, delivery failed",
			route: "POST /api/v1/testNotificationProfile",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(CodeMalformedRequest,
					`unable to send notification: unable to send notification message: error sending webhook notification: Post "`+
						notificationTestWebhookURL+`": dial tcp: lookup hooks.example.com: no such host`))
			},
			call: func(c *Client, p NotificationProfile) error {
				return c.TestNotificationProfile(context.Background(), p)
			},
			want: "lookup hooks.example.com: no such host",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.handle(tc.route, tc.handler)

			err := tc.call(fake.client(t), notificationSecretProfile())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), redactedMarker) {
				t.Errorf("error = %q, want the secrets replaced with %s", err, redactedMarker)
			}
			requireNoNotificationSecret(t, "error", err.Error())

			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %T does not unwrap to *Error", err)
			}
			if apiErr.Code != CodeMalformedRequest || apiErr.StatusCode != http.StatusBadRequest {
				t.Errorf("unwrapped error = %d %s, want 400 %s", apiErr.StatusCode, apiErr.Code, CodeMalformedRequest)
			}
			requireNoNotificationSecret(t, "unwrapped Message", apiErr.Message)
			requireNoNotificationSecret(t, "unwrapped Body", apiErr.Body)
		})
	}
}

func TestNotificationSecretsCoverEveryMethod(t *testing.T) {
	email := NotificationProfile{Email: &EmailNotificationOptions{SMTPUsername: "kopia", SMTPPassword: notificationTestSMTPPassword}}
	pushover := NotificationProfile{Pushover: &PushoverNotificationOptions{AppToken: notificationTestAppToken, UserKey: notificationTestUserKey}}
	webhook := NotificationProfile{Webhook: &WebhookNotificationOptions{
		Endpoint: "https://user:hook-password@example.com/a b?k=v", Headers: "X-Token: " + notificationTestBearer,
	}}

	for _, tc := range []struct {
		profile NotificationProfile
		text    string
	}{
		{email, "auth failed for " + notificationTestSMTPPassword},
		{pushover, "token=" + notificationTestAppToken + " user=" + notificationTestUserKey},
		// The re-encoded URL, as Go quotes it, and the password alone.
		{webhook, `Post "https://user:hook-password@example.com/a%20b?k=v": EOF; password hook-password; header X-Token: ` + notificationTestBearer},
		// net/http masks the password of a URL it quotes in an error.
		{webhook, `Post "https://user:***@example.com/a%20b?k=v": EOF`},
	} {
		redacted := Redact(tc.text, tc.profile.Secrets()...)
		requireNoNotificationSecret(t, "redacted text", redacted)
		if strings.Contains(redacted, "hook-password") || strings.Contains(redacted, "a%20b") {
			t.Errorf("Redact(%q) = %q, want the webhook URL and its password redacted", tc.text, redacted)
		}
	}
	if strings.Contains(Redact("user kopia", email.Secrets()...), redactedMarker) {
		t.Error("the SMTP username is redacted, but it is not a secret")
	}
}

// The test endpoint takes the same body as a save, and answers 200 once the
// message was delivered.
func TestTestNotificationProfile(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("POST /api/v1/testNotificationProfile", http.StatusOK, "{}\n")
	profile := NotificationProfile{Name: "ops", MinSeverity: NotificationSeverityError, Webhook: &WebhookNotificationOptions{
		Endpoint: "https://ntfy.example.org/kopia",
	}}

	if err := fake.client(t).TestNotificationProfile(context.Background(), profile); err != nil {
		t.Fatalf("TestNotificationProfile() error = %v", err)
	}
	requireNotificationJSONEqual(t, "request body", fake.lastCall(t, "POST /api/v1/testNotificationProfile").body,
		`{"profile":"ops","minSeverity":20,"method":{"type":"webhook","config":{
			"endpoint":"https://ntfy.example.org/kopia","method":"POST","format":"txt","headers":""}}}`)

	if err := fake.client(t).TestNotificationProfile(context.Background(), NotificationProfile{Name: "ops"}); err == nil {
		t.Error("TestNotificationProfile() without a method succeeded, want an error")
	}
}

// ParseWebhookHeaders must read the headers string exactly as Kopia's webhook
// sender applies it (notification/sender/webhook/webhook_sender.go:37-42).
func TestParseWebhookHeaders(t *testing.T) {
	tests := []struct {
		headers string
		want    map[string]string
	}{
		{"", nil},
		{"\n\n", nil},
		{"Authorization: Bearer tk\nPriority: high", map[string]string{"Authorization": "Bearer tk", "Priority": "high"}},
		// CRLF line endings and surrounding whitespace are trimmed.
		{"  X-A: 1 \r\nX-B:2\r\n", map[string]string{"X-A": "1", "X-B": "2"}},
		// The value may contain colons; a line without one is ignored.
		{"X-B:  v:w \nnot a header\nX-C:", map[string]string{"X-B": "v:w", "X-C": ""}},
		// The key is not trimmed after the split, and keeps its case.
		{"X-A : v\nx-lower: l", map[string]string{"X-A ": "v", "x-lower": "l"}},
		// A later line overrides an earlier one with the same key.
		{"X-A: 1\nX-A: 2", map[string]string{"X-A": "2"}},
	}
	for _, tc := range tests {
		if got := ParseWebhookHeaders(tc.headers); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseWebhookHeaders(%q) = %q, want %q", tc.headers, got, tc.want)
		}
	}
}

func TestFormatWebhookHeaders(t *testing.T) {
	headers := map[string]string{"Priority": "high", "Authorization": "Bearer tk", "X-Empty": "", "Tags": "a:b,c"}
	formatted := FormatWebhookHeaders(headers)
	if want := "Authorization: Bearer tk\nPriority: high\nTags: a:b,c\nX-Empty: "; formatted != want {
		t.Fatalf("FormatWebhookHeaders() = %q, want %q", formatted, want)
	}
	if got := ParseWebhookHeaders(formatted); !reflect.DeepEqual(got, headers) {
		t.Errorf("ParseWebhookHeaders(FormatWebhookHeaders(h)) = %q, want %q", got, headers)
	}
	if got := FormatWebhookHeaders(nil); got != "" {
		t.Errorf("FormatWebhookHeaders(nil) = %q, want empty", got)
	}
}
