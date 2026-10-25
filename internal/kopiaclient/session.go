package kopiaclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Every UI API request needs, besides Basic auth, the cookie
// Kopia-Session-Cookie and the header X-Kopia-Csrf-Token, whose value is
// hex(HMAC-SHA256(signing key, session cookie value))
// (internal/server/server_authz_checks.go:19-59). The only way to learn the
// token is GET /, which serves the UI page with the token injected as
// <meta name="kopia-csrf-token" content="..." /> (server.go:762-765 in the
// releases). GET / reuses a session cookie the client sends, so the client
// picks its own random session id and never has to parse Set-Cookie.
//
// The token has no expiry and no server-side state; it changes only with the
// signing key, which Kopia regenerates on every start unless
// --auth-cookie-signing-key is fixed. After a restart the next API call fails
// with a 401 that, unlike a credentials failure, carries no WWW-Authenticate;
// do then fetches GET / once more with the same session id and retries.
//
// Terraform calls resources in parallel, so the first request of a run, or the
// first after a restart, can arrive many times at once. Sessions carry a
// generation number, and at most one GET / is in flight: callers that need a
// session newer than the one they hold join it instead of starting another.

const (
	// sessionCookieName is serverSessionCookieName
	// (internal/server/server_authz_checks.go:17).
	sessionCookieName = "Kopia-Session-Cookie"
	// csrfHeader is apiclient.CSRFTokenHeader
	// (internal/apiclient/apiclient.go:29).
	csrfHeader = "X-Kopia-Csrf-Token"
	// maxIndexBytes bounds how much of the UI page is read. The official page
	// is a few kilobytes; a server that sends more is not a kopia server.
	maxIndexBytes = 4 << 20
)

// csrfTokenPattern matches the meta tag Kopia injects before </head>. The
// token is always 64 lower-case hex digits, and Kopia compares it byte for
// byte, so nothing else is accepted.
var csrfTokenPattern = regexp.MustCompile(`<meta name="kopia-csrf-token" content="([0-9a-f]{64})"`)

// versionPattern finds the server version in the page title. Kopia appends
// " v" + its build version to every </title> (server.go:755-758 in the
// releases). Release builds set the version without a "v", so the title ends
// in " v0.23.1"; a `go install` build falls back to the module version, which
// has one, giving " vv0.23.1".
var versionPattern = regexp.MustCompile(`\sv(v?[0-9][^<\s]*)</title>`)

// majorMinorPattern reads the release line from a version such as 0.23.1 or
// 0.24.0-rc1.
var majorMinorPattern = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(?:[.+-]|$)`)

// session is one CSRF session: the cookie value, the token GET / issued for
// it, and the server version the same page reported. Sessions are immutable
// once published; a renewal creates a new one with a higher generation.
type session struct {
	gen     uint64
	id      string
	token   string
	version string
}

// bootstrapCall is a GET / in flight. Callers that need a session while one is
// running wait for it instead of starting their own.
type bootstrapCall struct {
	done    chan struct{}
	session *session
	err     error
	// waiters counts the callers that joined the call rather than starting
	// it. Only the tests read it, to know when every caller is waiting.
	waiters int
}

// sessionNewerThan returns a session with a generation above stale, starting
// one with GET / when there is none. Pass 0 for any session at all, and the
// generation of a session the server rejected to get a renewed one.
func (c *Client) sessionNewerThan(ctx context.Context, stale uint64) (*session, error) {
	for {
		c.mu.Lock()
		if c.current != nil && c.current.gen > stale {
			s := c.current
			c.mu.Unlock()
			return s, nil
		}
		call := c.inflight
		if call == nil {
			call = &bootstrapCall{done: make(chan struct{})}
			c.inflight = call
			id := c.sessionID
			c.mu.Unlock()
			return c.runBootstrap(ctx, call, id)
		}
		call.waiters++
		c.mu.Unlock()

		select {
		case <-call.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if call.err != nil {
			// The caller that ran the GET / may have been cancelled while
			// this one still has time; it then tries again itself rather
			// than inherit someone else's cancellation.
			if isContextError(call.err) && ctx.Err() == nil {
				continue
			}
			return nil, call.err
		}
		if call.session.gen > stale {
			return call.session, nil
		}
	}
}

// runBootstrap performs the GET / of call, publishes the session it yields and
// releases everyone waiting for it.
func (c *Client) runBootstrap(ctx context.Context, call *bootstrapCall, id string) (*session, error) {
	s, err := c.bootstrap(ctx, id)

	c.mu.Lock()
	if err == nil {
		c.gen++
		s.gen = c.gen
		c.sessionID = s.id
		c.current = s
	}
	call.session, call.err = s, err
	c.inflight = nil
	c.mu.Unlock()
	close(call.done)

	if err == nil {
		tflog.Debug(ctx, "started a Kopia session", map[string]any{"url": c.baseURL, "server_version": s.version})
	}
	return s, err
}

// bootstrap fetches the UI page for session id and extracts the CSRF token
// and the server version from it. Each way it can fail points at a different
// mistake, so each gets its own sentinel error (errors.go).
func (c *Client) bootstrap(ctx context.Context, id string) (*session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("building request for GET /: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
	// No Range or If-Modified-Since: the page is served by
	// http.ServeContent, which would honour them and answer 206 or 304
	// without the token.
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to the kopia server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	page, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes))
	if err != nil {
		return nil, fmt.Errorf("reading response body for GET /: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := newError(http.MethodGet, "/", resp.StatusCode, resp.Header, page)
		apiErr.Message = Redact(apiErr.Message, c.password)
		apiErr.Body = Redact(apiErr.Body, c.password)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && apiErr.Authenticate != "":
			return nil, fmt.Errorf("%w: %w", ErrBadCredentials, apiErr)
		case resp.StatusCode == http.StatusForbidden:
			return nil, fmt.Errorf("%w: %w", ErrNotUIUser, apiErr)
		case resp.StatusCode == http.StatusNotFound:
			return nil, fmt.Errorf("%w: %w", ErrUINotServed, apiErr)
		}
		return nil, apiErr
	}

	match := csrfTokenPattern.FindSubmatch(page)
	if match == nil {
		return nil, fmt.Errorf("%w: GET %s/ answered HTTP 200 without a kopia-csrf-token meta tag", ErrNotKopiaServer, c.baseURL)
	}

	// Kopia keeps a session cookie the request carries, so this is only a
	// safeguard: if the server chose its own session anyway, the token
	// belongs to that one.
	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			id = cookie.Value
		}
	}

	return &session{id: id, token: string(match[1]), version: parseServerVersion(page)}, nil
}

// ServerVersion returns the Kopia release the server reports, such as
// "0.23.1", starting the session if there is none yet. It is "" when the page
// title carries no recognisable version.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	s, err := c.sessionNewerThan(ctx, 0)
	if err != nil {
		return "", err
	}
	return s.version, nil
}

// parseServerVersion extracts the build version from the UI page's title, with
// at most one leading "v" removed: the first one belongs to the " v" Kopia
// inserts, a second one to a module version.
func parseServerVersion(page []byte) string {
	match := versionPattern.FindSubmatch(page)
	if match == nil {
		return ""
	}
	return string(bytes.TrimPrefix(match[1], []byte("v")))
}

// MajorMinor parses the release line of a server version: 0 and 23 for
// "0.23.1". ok is false for a version that does not start with two numbers,
// such as "" or the "0-unofficial" of a build without version information.
func MajorMinor(version string) (major, minor int, ok bool) {
	match := majorMinorPattern.FindStringSubmatch(version)
	if match == nil {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(match[1])
	minor, errMinor := strconv.Atoi(match[2])
	if errMajor != nil || errMinor != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// VersionAtLeast reports whether version is major.minor or a later release.
// known is false when the version cannot be parsed; callers should then
// assume nothing about the server.
func VersionAtLeast(version string, major, minor int) (atLeast, known bool) {
	gotMajor, gotMinor, ok := MajorMinor(version)
	if !ok {
		return false, false
	}
	if gotMajor != major {
		return gotMajor > major, true
	}
	return gotMinor >= minor, true
}

// newSessionID returns a random session cookie value: 32 random bytes in hex.
func newSessionID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a session id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// isContextError reports whether err stems from a cancelled or expired
// context.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
