package kopiaclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewValidatesOptions(t *testing.T) {
	base := Options{URL: "https://kopia.example.com:51515", Username: "kopia", Password: "secret"}
	tests := []struct {
		name   string
		modify func(*Options)
		want   string
	}{
		{"empty URL", func(o *Options) { o.URL = "" }, "must not be empty"},
		{"only a slash", func(o *Options) { o.URL = "/" }, "must not be empty"},
		{"another scheme", func(o *Options) { o.URL = "ftp://kopia.example.com" }, "http:// or https://"},
		{"no scheme", func(o *Options) { o.URL = "kopia.example.com:51515" }, "http:// or https://"},
		{"no host", func(o *Options) { o.URL = "https://" }, "http:// or https://"},
		{"credentials in the URL", func(o *Options) { o.URL = "https://kopia:secret@kopia.example.com" }, "must not contain credentials"},
		{"credentials in an unparsable URL", func(o *Options) { o.URL = "https://kopia:secret@kopia.example.com:port" }, "not a valid URL"},
		{"query", func(o *Options) { o.URL = "https://kopia.example.com/?x=1" }, "query or fragment"},
		{"empty username", func(o *Options) { o.Username = "" }, "username must not be empty"},
		{"empty password", func(o *Options) { o.Password = "" }, "password must not be empty"},
		{"fingerprint over http", func(o *Options) {
			o.URL = "http://kopia.example.com"
			o.CertFingerprint = strings.Repeat("ab", 32)
		}, "only applies to an https:// URL"},
		{"CA file over http", func(o *Options) {
			o.URL = "http://kopia.example.com"
			o.CAFile = "/etc/kopia/ca.pem"
		}, "only applies to an https:// URL"},
		{"fingerprint and insecure", func(o *Options) {
			o.CertFingerprint = strings.Repeat("ab", 32)
			o.InsecureSkipVerify = true
		}, "mutually exclusive"},
		{"fingerprint and CA file", func(o *Options) {
			o.CertFingerprint = strings.Repeat("ab", 32)
			o.CAFile = "/etc/kopia/ca.pem"
		}, "mutually exclusive"},
		{"CA file and insecure", func(o *Options) {
			o.CAFile = "/etc/kopia/ca.pem"
			o.InsecureSkipVerify = true
		}, "mutually exclusive"},
		{"malformed fingerprint", func(o *Options) { o.CertFingerprint = "abc" }, "invalid SHA-256 certificate fingerprint"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			tc.modify(&opts)
			_, err := New(opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New() error = %v, want one containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("New() error %q contains the password", err)
			}
		})
	}
}

func TestNewNormalisesTheURLAndDefaults(t *testing.T) {
	c, err := New(Options{URL: "https://kopia.example.com:51515/", Username: "kopia", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.BaseURL(), "https://kopia.example.com:51515"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	if got := c.httpClient.Timeout; got != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", got, DefaultTimeout)
	}
	if c.userAgent != "terraform-provider-kopia" {
		t.Errorf("user agent = %q, want the default", c.userAgent)
	}

	// A path prefix, for a server behind a reverse proxy, is kept.
	c, err = New(Options{URL: "https://proxy.example.com/kopia/", Username: "kopia", Password: "secret", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.BaseURL(), "https://proxy.example.com/kopia"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	if got := c.httpClient.Timeout; got != time.Minute {
		t.Errorf("timeout = %v, want 1m", got)
	}
}

// Every client picks its own random session id.
func TestEachClientHasItsOwnSession(t *testing.T) {
	a := newFakeKopia().client(t)
	b := newFakeKopia().client(t)
	if a.sessionID == b.sessionID || len(a.sessionID) != 64 {
		t.Fatalf("session ids %q and %q, want two different 64-digit ids", a.sessionID, b.sessionID)
	}
}

func TestDoSendsTheQueryAndBodyAndDecodesTheResponse(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("POST /api/v1/example", http.StatusOK, "{\n  \"answer\": 42\n}\n")
	c := fake.client(t)

	query := url.Values{"host": {"host1"}, "userName": {"docker"}, "path": {"/srv/data"}}
	var out struct {
		Answer int `json:"answer"`
	}
	if err := c.do(context.Background(), http.MethodPost, "/api/v1/example", query, map[string]any{"q": "v"}, &out); err != nil {
		t.Fatalf("do() error = %v", err)
	}
	if out.Answer != 42 {
		t.Errorf("decoded answer = %d, want 42", out.Answer)
	}

	call := fake.lastCall(t, "POST /api/v1/example")
	if got := fake.query(t, "POST /api/v1/example").Encode(); got != query.Encode() {
		t.Errorf("query = %q, want %q", got, query.Encode())
	}
	if got := fake.body(t, "POST /api/v1/example")["q"]; got != "v" {
		t.Errorf("body q = %v, want v", got)
	}
	if got := call.header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
}

// A call without a body sends none, and a response it does not decode needs
// no body either.
func TestDoWithoutBodies(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("DELETE /api/v1/example", http.StatusOK, "{}\n")
	if err := fake.client(t).do(context.Background(), http.MethodDelete, "/api/v1/example", nil, nil, nil); err != nil {
		t.Fatalf("do() error = %v", err)
	}
	call := fake.lastCall(t, "DELETE /api/v1/example")
	if len(call.body) != 0 || call.header.Get("Content-Type") != "" {
		t.Errorf("request carried body %q with Content-Type %q, want neither", call.body, call.header.Get("Content-Type"))
	}
}

// Kopia sends its JSON errors as text/plain, indented in the releases and
// compact on master, so they are recognised by content; plain-text failures
// keep their text as the message.
func TestDoParsesKopiaErrors(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		code        string
		message     string
		notFound    bool
		notConn     bool
		denied      bool
		missing     bool
		wantInError string
	}{
		{
			name: "release NOT_FOUND with 400", status: 400,
			body: "{\n  \"code\": \"NOT_FOUND\",\n  \"error\": \"policy not found\"\n}\n",
			code: CodeNotFound, message: "policy not found", notFound: true,
			wantInError: "kopia API error: GET /api/v1/example: HTTP 400 (NOT_FOUND): policy not found",
		},
		{
			name: "master NOT_FOUND with 404", status: 404,
			body: `{"code":"NOT_FOUND","error":"no source matching the provided filters"}` + "\n",
			code: CodeNotFound, message: "no source matching the provided filters", notFound: true,
		},
		{
			name: "not connected", status: 400,
			body: "{\n  \"code\": \"NOT_CONNECTED\",\n  \"error\": \"not connected\"\n}\n",
			code: CodeNotConnected, message: "not connected", notConn: true,
		},
		{
			name: "access denied", status: 403,
			body: "{\n  \"code\": \"ACCESS_DENIED\",\n  \"error\": \"access is denied\"\n}\n",
			code: CodeAccessDenied, message: "access is denied", denied: true,
		},
		{
			name: "internal", status: 500,
			body: "{\n  \"code\": \"INTERNAL\",\n  \"error\": \"internal server error: boom\"\n}\n",
			code: CodeInternal, message: "internal server error: boom",
		},
		{
			name: "unknown route", status: 404, body: "404 page not found\n",
			message: "404 page not found", missing: true,
			wantInError: "HTTP 404: 404 page not found",
		},
		{
			name: "plain-text UI denial", status: 403,
			body:    "UI Access denied. See https://github.com/kopia/kopia/issues/880#issuecomment-798421751 for more information.\n",
			message: "UI Access denied. See https://github.com/kopia/kopia/issues/880#issuecomment-798421751 for more information.",
			denied:  true,
		},
		{
			name: "JSON without a code", status: 500, body: `{"unexpected": true}`,
			message: `{"unexpected": true}`,
		},
		{
			name: "master body limit", status: 413, body: "error reading request body\n",
			message: "error reading request body",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.reply("GET /api/v1/example", tc.status, tc.body)
			err := fake.client(t).do(context.Background(), http.MethodGet, "/api/v1/example", nil, nil, &struct{}{})

			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("do() error = %v, want an *Error", err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Code != tc.code || apiErr.Message != tc.message || apiErr.Body != tc.body {
				t.Errorf("error = %+v, want status %d, code %q, message %q and the raw body", apiErr, tc.status, tc.code, tc.message)
			}
			if apiErr.Method != http.MethodGet || apiErr.Path != "/api/v1/example" {
				t.Errorf("error names %s %s, want GET /api/v1/example", apiErr.Method, apiErr.Path)
			}
			if IsNotFound(err) != tc.notFound || IsNotConnected(err) != tc.notConn || IsAccessDenied(err) != tc.denied || IsEndpointMissing(err) != tc.missing {
				t.Errorf("predicates: IsNotFound=%v IsNotConnected=%v IsAccessDenied=%v IsEndpointMissing=%v, want %v %v %v %v",
					IsNotFound(err), IsNotConnected(err), IsAccessDenied(err), IsEndpointMissing(err),
					tc.notFound, tc.notConn, tc.denied, tc.missing)
			}
			if IsUnauthorized(err) || IsCSRFRejected(err) {
				t.Errorf("%v reads as an authentication failure", err)
			}
			if tc.wantInError != "" && !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("Error() = %q, want it to contain %q", err.Error(), tc.wantInError)
			}
		})
	}
}

// Paths such as /repo/status without the /api/v1 prefix are rewritten to the
// UI page, so a URL that does not point at the server's root gets HTML where
// JSON was expected. That must fail rather than decode as an empty result.
func TestDoRejectsASuccessThatIsNotJSON(t *testing.T) {
	fake := newFakeKopia()
	fake.handle("GET /api/v1/example", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html></html>"))
	})
	err := fake.client(t).do(context.Background(), http.MethodGet, "/api/v1/example", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "root address") {
		t.Fatalf("do() error = %v, want a complaint about the response type", err)
	}
}

func TestDoRequiresABodyWhenOneIsExpected(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/example", http.StatusOK, "\n")
	var out map[string]any
	err := fake.client(t).do(context.Background(), http.MethodGet, "/api/v1/example", nil, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "expected a JSON response body") {
		t.Fatalf("do() error = %v, want a missing-body error", err)
	}
}

func TestDoReportsUndecodableBodies(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/example", http.StatusOK, `{"answer":"forty-two"}`)
	var out struct {
		Answer int `json:"answer"`
	}
	err := fake.client(t).do(context.Background(), http.MethodGet, "/api/v1/example", nil, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "decoding response body for GET /api/v1/example") {
		t.Fatalf("do() error = %v, want a decoding error", err)
	}
}

// A redirect is reported, never followed: following it would replay a PUT as
// a GET.
func TestDoDoesNotFollowRedirects(t *testing.T) {
	fake := newFakeKopia()
	fake.handle("PUT /api/v1/example", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://kopia.example.com/api/v1/example")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	err := fake.client(t).do(context.Background(), http.MethodPut, "/api/v1/example", nil, map[string]any{}, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("do() error = %v, want the 301", err)
	}
	if !strings.Contains(apiErr.Message, "redirected to https://kopia.example.com/api/v1/example") {
		t.Errorf("message = %q, want it to name the redirect target", apiErr.Message)
	}
	if got := fake.count("PUT /api/v1/example"); got != 1 {
		t.Errorf("PUT was sent %d times, want once", got)
	}
}

// Kopia never echoes credentials, but should an error body ever contain the
// password, it must not reach a diagnostic.
func TestErrorsNeverContainThePassword(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/example", http.StatusInternalServerError, CodeInternal, "internal server error: bad password "+fakePassword)
	err := fake.client(t).do(context.Background(), http.MethodGet, "/api/v1/example", nil, nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("do() error = %v, want an *Error", err)
	}
	if strings.Contains(err.Error(), fakePassword) || strings.Contains(apiErr.Body, fakePassword) {
		t.Fatalf("error %q (body %q) contains the password", err, apiErr.Body)
	}
	if !strings.Contains(err.Error(), "bad password <redacted>") {
		t.Errorf("Error() = %q, want the password replaced", err)
	}
}

func TestRepositoryConnected(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, "{\n  \"connected\": false,\n  \"supportsContentCompression\": false,\n  \"hostname\": \"\",\n  \"username\": \"\",\n  \"enableActions\": false\n}\n")
	connected, err := fake.client(t).RepositoryConnected(context.Background())
	if err != nil {
		t.Fatalf("RepositoryConnected() error = %v", err)
	}
	if connected {
		t.Fatal("RepositoryConnected() = true, want false")
	}
	if !fake.called("GET /api/v1/repo/status") {
		t.Fatalf("calls = %v, want GET /api/v1/repo/status", fake.routes())
	}
}

func TestConfigFileLock(t *testing.T) {
	c := newFakeKopia().client(t)

	c.LockConfigFile()
	if c.configFileMu.TryLock() {
		t.Fatal("the config file lock was free while held")
	}
	c.UnlockConfigFile()

	sentinel := errors.New("from fn")
	err := c.WithConfigFileLock(func() error {
		if c.configFileMu.TryLock() {
			t.Error("WithConfigFileLock ran fn without holding the lock")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithConfigFileLock() = %v, want fn's error", err)
	}
	if !c.configFileMu.TryLock() {
		t.Fatal("WithConfigFileLock did not release the lock")
	}
}
