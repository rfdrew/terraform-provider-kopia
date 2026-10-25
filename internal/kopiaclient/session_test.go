package kopiaclient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"testing"
)

// fakeRepoStatusJSON is GET /api/v1/repo/status from a connected 0.23.1 server,
// abridged, in the releases' indented formatting.
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
  "enableActions": false
}
`

// The fake must derive tokens exactly as Kopia does, or the session tests
// would prove nothing. These are Kopia's own test vectors
// (internal/server/server_authz_checks_test.go:29-32).
func TestFakeCSRFTokenMatchesKopiasTestVectors(t *testing.T) {
	vectors := []struct{ key, session, token string }{
		{"some-key", "session1", "557c279a9203afbd5e1edd8a3b091fbcaf699841cd95058954e11886f0a3e6d0"},
		{"some-other-key", "session1", "7fd10608493e844581247d44e61de56b80df83ecdae49891f150823f73524ef7"},
		{"some-key", "session2", "e3aeba64243485ac4664e27445f48711b987c3bf5c7e58d1b89eb1e2722fedcd"},
		{"some-other-key", "session2", "714a124df0f6b6e79500fb06a900e7870a94f50b6d1e532c92a0abc0c63146f8"},
	}
	for _, v := range vectors {
		if got := fakeCSRFToken(v.key, v.session); got != v.token {
			t.Errorf("fakeCSRFToken(%q, %q) = %s, want %s", v.key, v.session, got, v.token)
		}
	}
}

// The session starts lazily with one GET /, which carries the client's own
// session id, and every API call then carries Basic auth, that session cookie
// and the token GET / issued for it.
func TestFirstRequestStartsTheSessionAndEveryCallCarriesIt(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	c := fake.client(t)

	if len(fake.routes()) != 0 {
		t.Fatalf("New() made requests: %v", fake.routes())
	}
	for i := 0; i < 3; i++ {
		connected, err := c.RepositoryConnected(context.Background())
		if err != nil {
			t.Fatalf("RepositoryConnected() error = %v", err)
		}
		if !connected {
			t.Fatal("RepositoryConnected() = false, want true")
		}
	}

	if got := fake.count("GET /"); got != 1 {
		t.Fatalf("GET / was requested %d times, want once; calls: %v", got, fake.routes())
	}
	index := fake.lastCall(t, "GET /")
	sessionCookie := cookieValue(t, index.header, sessionCookieName)
	if len(sessionCookie) != 64 {
		t.Errorf("GET / carried session id %q, want the client's own 64-hex-digit id", sessionCookie)
	}

	for _, call := range fake.callsTo("GET /api/v1/repo/status") {
		if got := cookieValue(t, call.header, sessionCookieName); got != sessionCookie {
			t.Errorf("API call carried session %q, want %q", got, sessionCookie)
		}
		if got, want := call.header.Get(csrfHeader), fakeCSRFToken("signing-key-0", sessionCookie); got != want {
			t.Errorf("API call carried CSRF token %q, want %q", got, want)
		}
		req := &http.Request{Header: call.header}
		if user, password, ok := req.BasicAuth(); !ok || user != fakeUsername || password != fakePassword {
			t.Errorf("API call carried Basic auth %q/%q (%v), want the configured credentials", user, password, ok)
		}
	}
}

func cookieValue(t *testing.T, header http.Header, name string) string {
	t.Helper()
	req := &http.Request{Header: header}
	cookie, err := req.Cookie(name)
	if err != nil {
		t.Fatalf("request carried no %s cookie: %v", name, err)
	}
	return cookie.Value
}

func TestServerVersionIsReadFromThePageTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"release build", "KopiaUI v0.23.1", "0.23.1"},
		{"go install build", "KopiaUI vv0.22.3", "0.22.3"},
		{"title prefix", "Blah: &lt;script&gt;bleh&lt;/script&gt; KopiaUI v0.23.1", "0.23.1"},
		{"release candidate", "KopiaUI v0.24.0-rc1", "0.24.0-rc1"},
		{"no version information", "KopiaUI vv0-unofficial", "0-unofficial"},
		{"no version at all", "Kopia UI not supported.", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.title = tc.title
			got, err := fake.client(t).ServerVersion(context.Background())
			if err != nil {
				t.Fatalf("ServerVersion() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("ServerVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMajorMinorAndVersionAtLeast(t *testing.T) {
	tests := []struct {
		version      string
		major, minor int
		ok           bool
		atLeast023   bool
	}{
		{"0.23.1", 0, 23, true, true},
		{"0.22.3", 0, 22, true, false},
		{"0.24.0-rc1", 0, 24, true, true},
		{"1.0", 1, 0, true, true},
		{"0.23", 0, 23, true, true},
		{"0-unofficial", 0, 0, false, false},
		{"", 0, 0, false, false},
		{"0.23x", 0, 0, false, false},
	}
	for _, tc := range tests {
		major, minor, ok := MajorMinor(tc.version)
		if major != tc.major || minor != tc.minor || ok != tc.ok {
			t.Errorf("MajorMinor(%q) = %d, %d, %v, want %d, %d, %v", tc.version, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
		atLeast, known := VersionAtLeast(tc.version, 0, 23)
		if atLeast != tc.atLeast023 || known != tc.ok {
			t.Errorf("VersionAtLeast(%q, 0, 23) = %v, %v, want %v, %v", tc.version, atLeast, known, tc.atLeast023, tc.ok)
		}
	}
}

// Terraform calls resources in parallel, so the first requests of a run arrive
// together. They must share one GET /: the first starts it, and the others
// wait for its session.
func TestConcurrentRequestsShareOneBootstrap(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	gate := make(chan struct{})
	fake.bootstrapGate = gate
	fake.bootstrapStarted = make(chan struct{}, 16)
	c := fake.client(t)

	const callers = 8
	errs := make(chan error, callers)
	run := func() {
		_, err := c.RepositoryConnected(context.Background())
		errs <- err
	}

	go run()
	<-fake.bootstrapStarted // The first caller's GET / is now in flight.
	for i := 1; i < callers; i++ {
		go run()
	}
	waitForWaiters(t, c, callers-1)
	close(gate)

	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("RepositoryConnected() error = %v", err)
		}
	}
	if got := fake.count("GET /"); got != 1 {
		t.Fatalf("GET / was requested %d times, want once", got)
	}
	if got := fake.count("GET /api/v1/repo/status"); got != callers {
		t.Fatalf("GET /api/v1/repo/status was requested %d times, want %d", got, callers)
	}
}

// waitForWaiters blocks until n callers have joined the GET / in flight.
func waitForWaiters(t *testing.T, c *Client, n int) {
	t.Helper()
	for {
		c.mu.Lock()
		joined := 0
		if c.inflight != nil {
			joined = c.inflight.waiters
		}
		c.mu.Unlock()
		if joined >= n {
			return
		}
		if t.Context().Err() != nil {
			t.Fatal("test cancelled while waiting for callers to join the bootstrap")
		}
		runtime.Gosched()
	}
}

// A caller waiting for someone else's GET / gives up when its own context
// ends, without disturbing the GET / itself.
func TestWaitingForTheBootstrapHonoursTheContext(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	gate := make(chan struct{})
	fake.bootstrapGate = gate
	fake.bootstrapStarted = make(chan struct{}, 4)
	c := fake.client(t)

	leader := make(chan error, 1)
	go func() {
		_, err := c.RepositoryConnected(context.Background())
		leader <- err
	}()
	<-fake.bootstrapStarted

	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, err := c.RepositoryConnected(ctx)
		waiter <- err
	}()
	waitForWaiters(t, c, 1)
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting caller error = %v, want context.Canceled", err)
	}

	close(gate)
	if err := <-leader; err != nil {
		t.Fatalf("leading caller error = %v", err)
	}
}

// After a restart the server signs tokens with a new key and rejects the old
// one with a 401 that carries no WWW-Authenticate. The client fetches GET /
// again with the same session id and retries the call once.
func TestCSRFRejectionAfterARestartRenewsTheSessionAndRetries(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	c := fake.client(t)

	if _, err := c.RepositoryConnected(context.Background()); err != nil {
		t.Fatalf("RepositoryConnected() before the restart: %v", err)
	}
	fake.restart()
	if _, err := c.RepositoryConnected(context.Background()); err != nil {
		t.Fatalf("RepositoryConnected() after the restart: %v", err)
	}

	want := []string{"GET /", "GET /api/v1/repo/status", "GET /api/v1/repo/status", "GET /", "GET /api/v1/repo/status"}
	requireRoutes(t, fake, want)

	indexes := fake.callsTo("GET /")
	if first, second := cookieValue(t, indexes[0].header, sessionCookieName), cookieValue(t, indexes[1].header, sessionCookieName); first != second {
		t.Errorf("the renewal used session %q, want the original %q", second, first)
	}

	// The renewed session is kept: later calls need no further GET /.
	if _, err := c.RepositoryConnected(context.Background()); err != nil {
		t.Fatalf("RepositoryConnected() after the renewal: %v", err)
	}
	if got := fake.count("GET /"); got != 2 {
		t.Fatalf("GET / was requested %d times, want 2", got)
	}
}

func requireRoutes(t *testing.T, fake *fakeKopia, want []string) {
	t.Helper()
	got := fake.routes()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}

// The CSRF check runs before Kopia reads the body, so a retried PUT must send
// the same body again rather than an empty one.
func TestCSRFRetryResendsTheBody(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	fake.reply("PUT /api/v1/repo/throttle", http.StatusOK, "{}\n")
	c := fake.client(t)

	if _, err := c.RepositoryConnected(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.restart()

	body := map[string]any{"maxUploadSpeedBytesPerSecond": 1048576}
	if err := c.do(context.Background(), http.MethodPut, "/api/v1/repo/throttle", nil, body, nil); err != nil {
		t.Fatalf("do(PUT) error = %v", err)
	}

	puts := fake.callsTo("PUT /api/v1/repo/throttle")
	if len(puts) != 2 {
		t.Fatalf("PUT was sent %d times, want twice (rejected, then retried)", len(puts))
	}
	for i, put := range puts {
		if !bytes.Equal(put.body, []byte(`{"maxUploadSpeedBytesPerSecond":1048576}`)) {
			t.Errorf("attempt %d sent body %q, want the encoded request", i+1, put.body)
		}
		if got := put.header.Get("Content-Type"); got != "application/json" {
			t.Errorf("attempt %d sent Content-Type %q, want application/json", i+1, got)
		}
	}
}

// When GET / issues the very token Kopia just rejected, the signing key has not
// changed and a retry cannot help: something drops the cookie or the header.
// The client says so instead of retrying.
func TestCSRFRejectionWithAnUnchangedTokenIsNotRetried(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	fake.rejectCSRF = true

	_, err := fake.client(t).RepositoryConnected(context.Background())
	if !errors.Is(err, ErrCSRFRejected) || !IsCSRFRejected(err) {
		t.Fatalf("RepositoryConnected() error = %v, want ErrCSRFRejected", err)
	}
	requireRoutes(t, fake, []string{"GET /", "GET /api/v1/repo/status", "GET /"})
}

// Even when every GET / issues a new token, a call is retried only once.
func TestCSRFRejectionIsRetriedOnlyOnce(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	fake.rejectCSRF = true
	fake.rotateOnBootstrap = true

	_, err := fake.client(t).RepositoryConnected(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || !IsCSRFRejected(err) {
		t.Fatalf("RepositoryConnected() error = %v, want the second 401 from the CSRF check", err)
	}
	if IsUnauthorized(err) {
		t.Errorf("IsUnauthorized(%v) = true, want false: a CSRF failure is not a credentials failure", err)
	}
	requireRoutes(t, fake, []string{"GET /", "GET /api/v1/repo/status", "GET /", "GET /api/v1/repo/status"})
}

// Many calls rejected at once after a restart renew the session once between
// them.
func TestConcurrentCSRFRejectionsRenewTheSessionOnce(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	c := fake.client(t)
	if _, err := c.RepositoryConnected(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.restart()

	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.RepositoryConnected(context.Background())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("RepositoryConnected() error = %v", err)
		}
	}
	if got := fake.count("GET /"); got != 2 {
		t.Fatalf("GET / was requested %d times, want 2: the first session and one renewal", got)
	}
}

// Each way GET / can fail points at a different configuration mistake.
func TestBootstrapFailuresAreClassified(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(*fakeKopia)
		sentinel  error
		predicate func(error) bool
	}{
		{
			name:      "wrong password",
			setup:     func(f *fakeKopia) { f.password = "something-else" },
			sentinel:  ErrBadCredentials,
			predicate: IsUnauthorized,
		},
		{
			name:      "not the UI user",
			setup:     func(f *fakeKopia) { f.uiUser = "kopia" },
			sentinel:  ErrNotUIUser,
			predicate: IsAccessDenied,
		},
		{
			name:     "server started with --no-ui",
			setup:    func(f *fakeKopia) { f.noUI = true },
			sentinel: ErrUINotServed,
			// The 404 of a missing UI is not the 404 of a missing endpoint,
			// which callers read as an older Kopia release.
			predicate: func(err error) bool { return !IsEndpointMissing(err) },
		},
		{
			name:      "not a kopia server",
			setup:     func(f *fakeKopia) { f.omitToken = true },
			sentinel:  ErrNotKopiaServer,
			predicate: func(err error) bool { return !IsUnauthorized(err) && !IsAccessDenied(err) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			tc.setup(fake)
			_, err := fake.client(t).RepositoryConnected(context.Background())
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("RepositoryConnected() error = %v, want %v", err, tc.sentinel)
			}
			if !tc.predicate(err) {
				t.Errorf("predicate does not hold for %v", err)
			}
			if bytes.Contains([]byte(err.Error()), []byte(fakePassword)) {
				t.Errorf("error %q contains the password", err)
			}
			// Nothing is sent to the API without a session.
			requireRoutes(t, fake, []string{"GET /"})
		})
	}
}

// A failed GET / is not remembered: once the cause is fixed, the next call
// starts a session.
func TestBootstrapFailureIsNotCached(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	fake.password = "rotated"
	c := fake.client(t)

	if _, err := c.RepositoryConnected(context.Background()); !IsUnauthorized(err) {
		t.Fatalf("RepositoryConnected() error = %v, want bad credentials", err)
	}
	fake.mu.Lock()
	fake.password = fakePassword
	fake.mu.Unlock()
	if _, err := c.RepositoryConnected(context.Background()); err != nil {
		t.Fatalf("RepositoryConnected() after fixing the password: %v", err)
	}
}

// Credentials that stop working mid-run produce a 401 with WWW-Authenticate,
// which no new session can fix, so it is returned without another GET /.
func TestBadCredentialsOnAnAPICallAreNotRetried(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	c := fake.client(t)
	if _, err := c.RepositoryConnected(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	fake.password = "rotated"
	fake.mu.Unlock()
	_, err := c.RepositoryConnected(context.Background())
	if !IsUnauthorized(err) || IsCSRFRejected(err) {
		t.Fatalf("RepositoryConnected() error = %v, want bad credentials", err)
	}
	if got := fake.count("GET /"); got != 1 {
		t.Fatalf("GET / was requested %d times, want once", got)
	}
}

// Kopia keeps a session cookie the request carries, but should a server set
// its own, the token it issued belongs to that session, so the client adopts
// it.
func TestASessionTheServerChoosesIsAdopted(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, fakeRepoStatusJSON)
	fake.ignoreClientSession = true
	c := fake.client(t)

	if _, err := c.RepositoryConnected(context.Background()); err != nil {
		t.Fatalf("RepositoryConnected() error = %v", err)
	}
	call := fake.lastCall(t, "GET /api/v1/repo/status")
	if got := cookieValue(t, call.header, sessionCookieName); got != "server-session-1" {
		t.Fatalf("API call carried session %q, want the server's server-session-1", got)
	}
}
