package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// fakeKopia emulates a kopia server in process, through the client's
// Transport, so provider tests need no listening socket. It is the provider
// package's copy of the client package's fake (internal/kopiaclient/
// fake_transport_test.go), with the same behaviour:
//
//   - GET / serves the UI page with the version in its title and the CSRF
//     token for the request's Kopia-Session-Cookie, after Basic auth and the
//     UI-user check;
//   - every /api/v1/ route requires Basic auth (401 with WWW-Authenticate),
//     then the session cookie and a matching X-Kopia-Csrf-Token (401 without
//     WWW-Authenticate), then the UI user (403 ACCESS_DENIED);
//   - routes are matched by "METHOD /path", without the query string, which
//     handlers read from the request and tests from the recorded calls; an
//     unrouted API path gets the plain-text 404 of Kopia's catch-all.
//
// GET /api/v1/repo/status answers "connected": true until a test routes it
// differently, because the provider's Configure reads it.
//
// The reply helpers send successes as application/json and errors as
// indented JSON with Content-Type text/plain, as the 0.22 and 0.23 releases
// do.
type fakeKopia struct {
	mu sync.Mutex

	// username and password are the credentials Basic auth accepts.
	username string
	password string
	// uiUser is --server-username; it defaults to username.
	uiUser string
	// title is the UI page's title, which carries the server version.
	title string
	// noUI emulates --no-ui: every path answers 404 before authentication.
	noUI bool
	// omitToken serves a page without the CSRF meta tag.
	omitToken bool
	// rejectCSRF fails every CSRF check, as when a proxy strips the cookie.
	rejectCSRF bool

	signingKey string
	restarts   int

	handlers map[string]http.HandlerFunc
	calls    []fakeCall
}

// fakeCall is one request the fake received.
type fakeCall struct {
	route  string
	query  url.Values
	body   []byte
	header http.Header
}

const (
	fakeURL           = "http://kopia.test"
	fakeUsername      = "tfacc"
	fakePassword      = "correct-horse-battery"
	fakeSessionCookie = "Kopia-Session-Cookie"
	fakeCSRFHeader    = "X-Kopia-Csrf-Token"
)

// fakeRepoStatusJSON is GET /api/v1/repo/status from a connected 0.23.1
// server, abridged.
const fakeRepoStatusJSON = `{
  "connected": true,
  "configFile": "/app/config/repository.config",
  "formatVersion": 3,
  "hash": "BLAKE2B-256-128",
  "encryption": "AES256-GCM-HMAC-SHA256",
  "splitter": "DYNAMIC-4M-BUZHASH",
  "maxPackSize": 20971520,
  "storage": "filesystem",
  "supportsContentCompression": true,
  "hostname": "kopia-acc",
  "username": "root",
  "description": "Repository in Filesystem: /app/repo",
  "enableActions": false,
  "formatBlobCacheDuration": 900000000000
}
`

func newFakeKopia() *fakeKopia {
	f := &fakeKopia{
		username:   fakeUsername,
		password:   fakePassword,
		title:      "KopiaUI v" + testedKopiaVersion,
		signingKey: "signing-key-0",
		handlers:   map[string]http.HandlerFunc{},
	}
	f.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	return f
}

// client returns a client of the fake, as Configure would build it.
func (f *fakeKopia) client(t *testing.T) *kopiaclient.Client {
	t.Helper()
	c, err := kopiaclient.New(kopiaclient.Options{URL: fakeURL, Username: fakeUsername, Password: fakePassword, Transport: f})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// restart emulates a server restart: a new signing key, so every CSRF token
// issued before is rejected once.
func (f *fakeKopia) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	f.signingKey = fmt.Sprintf("signing-key-%d", f.restarts)
}

// handle routes "METHOD /path" to h.
func (f *fakeKopia) handle(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[route] = h
}

// reply answers route with a fixed status and body.
func (f *fakeKopia) reply(route string, status int, body string) {
	f.replies(route, status, body)
}

// replies answers successive requests to route with bodies in turn, repeating
// the last one once they run out.
func (f *fakeKopia) replies(route string, status int, bodies ...string) {
	var mu sync.Mutex
	next := 0
	f.handle(route, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		body := bodies[next]
		if next < len(bodies)-1 {
			next++
		}
		mu.Unlock()
		writeFakeResponse(w, status, body)
	})
}

// replyError answers route with Kopia's error envelope.
func (f *fakeKopia) replyError(route string, status int, code, message string) {
	f.reply(route, status, fakeErrorBody(code, message))
}

// fakeErrorBody is {"code": code, "error": message} as the releases encode
// it: indented, with a trailing newline.
func fakeErrorBody(code, message string) string {
	encoded, _ := json.MarshalIndent(map[string]string{"code": code, "error": message}, "", "  ")
	return string(encoded) + "\n"
}

// writeFakeResponse writes body with Kopia's content type for status.
func writeFakeResponse(w http.ResponseWriter, status int, body string) {
	if status >= 200 && status < 300 {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// writeFakeJSON encodes v as a successful JSON response, indented as the
// releases do.
func writeFakeJSON(w http.ResponseWriter, v any) {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		writeFakeResponse(w, http.StatusInternalServerError, fakeErrorBody(kopiaclient.CodeInternal, err.Error()))
		return
	}
	writeFakeResponse(w, http.StatusOK, string(encoded)+"\n")
}

func fakePlainError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, message)
}

func fakeCSRFToken(signingKey, sessionID string) string {
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (f *fakeKopia) RoundTrip(req *http.Request) (*http.Response, error) {
	call := fakeCall{route: req.Method + " " + req.URL.Path, query: req.URL.Query(), header: req.Header.Clone()}
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		call.body = raw
		req.Body = io.NopCloser(strings.NewReader(string(raw)))
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	rec := httptest.NewRecorder()
	f.serve(rec, req)
	return rec.Result(), nil
}

func (f *fakeKopia) serve(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	noUI := f.noUI
	username, password := f.username, f.password
	uiUser := f.uiUser
	f.mu.Unlock()
	if uiUser == "" {
		uiUser = username
	}

	if noUI {
		fakePlainError(w, http.StatusNotFound, "404 page not found")
		return
	}
	user, pass, ok := req.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Kopia"`)
		fakePlainError(w, http.StatusUnauthorized, "Missing credentials.\n")
		return
	}
	if user != username || pass != password {
		w.Header().Set("WWW-Authenticate", `Basic realm="Kopia"`)
		fakePlainError(w, http.StatusUnauthorized, "Access denied.\n")
		return
	}

	if req.URL.Path == "/" {
		if user != uiUser {
			fakePlainError(w, http.StatusForbidden, "UI Access denied. See https://github.com/kopia/kopia/issues/880#issuecomment-798421751 for more information.")
			return
		}
		f.serveIndex(w, req)
		return
	}
	if !strings.HasPrefix(req.URL.Path, "/api/v1/") {
		fakePlainError(w, http.StatusNotFound, "404 page not found")
		return
	}
	if !f.validCSRF(req) {
		fakePlainError(w, http.StatusUnauthorized, "Invalid or missing CSRF token.\n")
		return
	}
	if user != uiUser {
		writeFakeResponse(w, http.StatusForbidden, fakeErrorBody(kopiaclient.CodeAccessDenied, "access is denied"))
		return
	}

	f.mu.Lock()
	h := f.handlers[req.Method+" "+req.URL.Path]
	f.mu.Unlock()
	if h == nil {
		fakePlainError(w, http.StatusNotFound, "404 page not found")
		return
	}
	h(w, req)
}

func (f *fakeKopia) serveIndex(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sessionID := "server-session"
	if cookie, err := req.Cookie(fakeSessionCookie); err == nil {
		sessionID = cookie.Value
	} else {
		http.SetCookie(w, &http.Cookie{Name: fakeSessionCookie, Value: sessionID, Path: "/"})
	}
	head := "<title>" + f.title + "</title>"
	if !f.omitToken {
		head += `<meta name="kopia-csrf-token" content="` + fakeCSRFToken(f.signingKey, sessionID) + `" />`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">`+head+`</head><body></body></html>`)
}

func (f *fakeKopia) validCSRF(req *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejectCSRF {
		return false
	}
	cookie, err := req.Cookie(fakeSessionCookie)
	if err != nil {
		return false
	}
	token := req.Header.Get(fakeCSRFHeader)
	return token != "" && hmac.Equal([]byte(token), []byte(fakeCSRFToken(f.signingKey, cookie.Value)))
}

// routes returns the calls made so far, in order, as "METHOD /path".
func (f *fakeKopia) routes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	routes := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		routes = append(routes, c.route)
	}
	return routes
}

// apiRoutes is routes without the GET / of the session bootstrap, which most
// tests do not care about.
func (f *fakeKopia) apiRoutes() []string {
	var routes []string
	for _, r := range f.routes() {
		if r != "GET /" {
			routes = append(routes, r)
		}
	}
	return routes
}

// count returns how many requests were made to route.
func (f *fakeKopia) count(route string) int {
	n := 0
	for _, r := range f.routes() {
		if r == route {
			n++
		}
	}
	return n
}

// called reports whether any request was made to route.
func (f *fakeKopia) called(route string) bool { return f.count(route) > 0 }

// callsTo returns every request made to route, in order.
func (f *fakeKopia) callsTo(route string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []fakeCall
	for _, c := range f.calls {
		if c.route == route {
			calls = append(calls, c)
		}
	}
	return calls
}

// lastCall returns the most recent request to route, failing the test when
// there was none.
func (f *fakeKopia) lastCall(t *testing.T, route string) fakeCall {
	t.Helper()
	calls := f.callsTo(route)
	if len(calls) == 0 {
		t.Fatalf("no request was made to %s; calls: %v", route, f.routes())
	}
	return calls[len(calls)-1]
}

// query returns the query string of the most recent request to route.
func (f *fakeKopia) query(t *testing.T, route string) url.Values {
	t.Helper()
	return f.lastCall(t, route).query
}

// body decodes the last JSON object sent to route.
func (f *fakeKopia) body(t *testing.T, route string) map[string]any {
	t.Helper()
	raw := f.lastCall(t, route).body
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("request body for %s is not a JSON object: %v (body: %s)", route, err, raw)
	}
	return decoded
}
