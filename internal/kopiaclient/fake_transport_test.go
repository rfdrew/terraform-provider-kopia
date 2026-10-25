package kopiaclient

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
)

// fakeKopia emulates a kopia server in process, through Options.Transport, so
// tests need no listening socket. It reproduces what the client depends on:
//
//   - GET / serves the UI page with the CSRF token for the request's
//     Kopia-Session-Cookie (or for a session it picks and sets, when the
//     request carries none) and the version in the title, after Basic auth and
//     the UI-user check, exactly as internal/server/server.go does;
//   - every /api/v1/ route requires Basic auth (401 with WWW-Authenticate),
//     then the session cookie and a matching X-Kopia-Csrf-Token (401 without
//     WWW-Authenticate), then the UI user (403 ACCESS_DENIED), in that order;
//   - routes are matched by "METHOD /path"; an unrouted API path gets the
//     plain-text 404 of Kopia's catch-all file server.
//
// restart rotates the signing key, which invalidates every token issued so
// far, as a real restart does. Every request is recorded in order with its
// query string and body.
//
// Response helpers mirror Kopia's formatting in the 0.22 and 0.23 releases:
// successes are application/json, errors are indented JSON sent as
// text/plain.
type fakeKopia struct {
	mu sync.Mutex

	// username and password are the credentials Basic auth accepts.
	username string
	password string
	// uiUser is --server-username; it defaults to username. A different
	// value makes the configured user a non-UI user.
	uiUser string
	// title is the page title, which carries the server version.
	title string
	// noUI emulates --no-ui: every path answers 404 before authentication.
	noUI bool
	// omitToken serves a page without the CSRF meta tag, as a server that is
	// not kopia would.
	omitToken bool
	// ignoreClientSession makes GET / pick its own session id and set it,
	// instead of reusing the cookie the request carries.
	ignoreClientSession bool
	// rejectCSRF fails every CSRF check, as when a proxy strips the cookie.
	rejectCSRF bool
	// rotateOnBootstrap changes the signing key on every GET /, so that no
	// token is ever accepted twice.
	rotateOnBootstrap bool
	// bootstrapGate, when set, holds every GET / until it is closed;
	// bootstrapStarted is signalled as each one arrives.
	bootstrapGate    chan struct{}
	bootstrapStarted chan struct{}

	signingKey string
	restarts   int
	sessions   int

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
	fakeUsername = "tfacc"
	fakePassword = "correct-horse-battery"
	fakeTitle    = "KopiaUI v0.23.1"
)

func newFakeKopia() *fakeKopia {
	return &fakeKopia{
		username:   fakeUsername,
		password:   fakePassword,
		title:      fakeTitle,
		signingKey: "signing-key-0",
		handlers:   map[string]http.HandlerFunc{},
	}
}

// fakeCSRFToken is Kopia's token derivation
// (internal/server/server_authz_checks.go:19-27).
func fakeCSRFToken(signingKey, sessionID string) string {
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

// restart emulates a server restart without a fixed --auth-cookie-signing-key:
// a new signing key, so every token issued before is rejected.
func (f *fakeKopia) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	f.signingKey = fmt.Sprintf("signing-key-%d", f.restarts)
}

func (f *fakeKopia) handle(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[route] = h
}

// reply answers route with a fixed status and body: as JSON for a 2xx, as
// text/plain otherwise, the way Kopia sends both.
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

// replyError answers route with Kopia's error envelope, indented as the 0.22
// and 0.23 releases write it.
func (f *fakeKopia) replyError(route string, status int, code, message string) {
	f.reply(route, status, fakeErrorBody(code, message))
}

// fakeErrorBody is {"code": code, "error": message} as the releases encode it:
// two-space indent and a trailing newline.
func fakeErrorBody(code, message string) string {
	encoded, _ := json.MarshalIndent(map[string]string{"code": code, "error": message}, "", "  ")
	return string(encoded) + "\n"
}

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

// plainError writes a plain-text error the way http.Error does, which appends
// a newline even to a message that already ends in one.
func plainError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, message)
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
	f.mu.Unlock()
	if noUI {
		// gorilla/mux's default 404, before any authentication.
		plainError(w, http.StatusNotFound, "404 page not found")
		return
	}

	user, password, ok := req.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Kopia"`)
		plainError(w, http.StatusUnauthorized, "Missing credentials.\n")
		return
	}
	f.mu.Lock()
	validUser := user == f.username && password == f.password
	uiUser := f.uiUser
	if uiUser == "" {
		uiUser = f.username
	}
	f.mu.Unlock()
	if !validUser {
		w.Header().Set("WWW-Authenticate", `Basic realm="Kopia"`)
		plainError(w, http.StatusUnauthorized, "Access denied.\n")
		return
	}

	if req.URL.Path == "/" {
		f.serveIndex(w, req, user == uiUser)
		return
	}
	if !strings.HasPrefix(req.URL.Path, "/api/v1/") {
		plainError(w, http.StatusNotFound, "404 page not found")
		return
	}

	if !f.validCSRF(req) {
		plainError(w, http.StatusUnauthorized, "Invalid or missing CSRF token.\n")
		return
	}
	if user != uiUser {
		writeFakeResponse(w, http.StatusForbidden, fakeErrorBody(CodeAccessDenied, "access is denied"))
		return
	}

	f.mu.Lock()
	h := f.handlers[req.Method+" "+req.URL.Path]
	f.mu.Unlock()
	if h == nil {
		// The UI's catch-all file server answers unknown API routes.
		plainError(w, http.StatusNotFound, "404 page not found")
		return
	}
	h(w, req)
}

func (f *fakeKopia) serveIndex(w http.ResponseWriter, req *http.Request, isUIUser bool) {
	f.mu.Lock()
	gate, started := f.bootstrapGate, f.bootstrapStarted
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		<-gate
	}

	if !isUIUser {
		plainError(w, http.StatusForbidden, "UI Access denied. See https://github.com/kopia/kopia/issues/880#issuecomment-798421751 for more information.")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rotateOnBootstrap {
		f.restarts++
		f.signingKey = fmt.Sprintf("signing-key-%d", f.restarts)
	}

	sessionID := ""
	if cookie, err := req.Cookie(sessionCookieName); err == nil && !f.ignoreClientSession {
		sessionID = cookie.Value
	} else {
		f.sessions++
		sessionID = fmt.Sprintf("server-session-%d", f.sessions)
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sessionID, Path: "/"})
	}

	head := "<title>" + f.title + "</title>"
	if !f.omitToken {
		head += `<meta name="kopia-csrf-token" content="` + fakeCSRFToken(f.signingKey, sessionID) + `" />`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">`+head+`</head><body><div id="root"></div></body></html>`)
}

// validCSRF is validateCSRFToken (server_authz_checks.go:29-59): the session
// cookie must be present and the header must equal its HMAC byte for byte.
func (f *fakeKopia) validCSRF(req *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejectCSRF {
		return false
	}
	cookie, err := req.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	token := req.Header.Get(csrfHeader)
	return token != "" && hmac.Equal([]byte(token), []byte(fakeCSRFToken(f.signingKey, cookie.Value)))
}

// routes returns the calls made so far, in order.
func (f *fakeKopia) routes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	routes := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		routes = append(routes, c.route)
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

// lastCall returns the most recent request to route.
func (f *fakeKopia) lastCall(t *testing.T, route string) fakeCall {
	t.Helper()
	calls := f.callsTo(route)
	if len(calls) == 0 {
		t.Fatalf("no request was made to %s; calls: %v", route, f.routes())
	}
	return calls[len(calls)-1]
}

// query returns the query string of the most recent request to route.
// Routes match on the path alone, so this is where a test checks, say, the
// target of a policy request.
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

func (f *fakeKopia) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{URL: "http://kopia.test", Username: fakeUsername, Password: fakePassword, Transport: f})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
