// Package kopiaclient is a thin HTTP client for the API behind a Kopia
// server's web UI.
//
// Kopia has no separately versioned public API: `kopia server start` serves the
// JSON endpoints its own web UI calls, under /api/v1/..., and this client
// speaks exactly those. Every call authenticates with HTTP Basic auth as the UI
// user and carries a CSRF token bound to a session cookie; session.go explains
// how the client obtains and renews that token. The package depends on nothing
// from Kopia's Go module: it is written against the wire behaviour of the
// 0.22 and 0.23 releases.
package kopiaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// DefaultUsername is Kopia's default --server-username, the UI user.
const DefaultUsername = "kopia"

// DefaultTimeout bounds a single HTTP request when Options.Timeout is unset.
const DefaultTimeout = 30 * time.Second

// Client talks to a single Kopia server. It is safe for concurrent use, which
// matters because Terraform calls resources in parallel.
type Client struct {
	baseURL    string
	username   string
	password   string
	userAgent  string
	httpClient *http.Client

	// The CSRF session; see session.go. mu guards every field below it.
	mu        sync.Mutex
	sessionID string
	current   *session
	gen       uint64
	inflight  *bootstrapCall

	// configFileMu serialises writes to the server's repository.config; see
	// LockConfigFile.
	configFileMu sync.Mutex
}

// Options configures a Client.
type Options struct {
	// URL is the server's root address, e.g. https://kopia.example.com:51515.
	// A trailing slash is trimmed. A path is kept, for a server behind a
	// reverse proxy that serves it under a prefix.
	URL string
	// Username and Password are the UI user's credentials: --server-username
	// (default "kopia") and --server-password of `kopia server start`.
	Username string
	Password string
	// CertFingerprint pins the server's certificate by the SHA-256 of its DER
	// encoding, as NormaliseFingerprint accepts it.
	CertFingerprint string
	// CAFile is a PEM bundle used instead of the system roots.
	CAFile string
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool
	// Timeout bounds a single HTTP request. Defaults to DefaultTimeout.
	Timeout time.Duration
	// UserAgent is sent with every request.
	UserAgent string
	// Transport replaces the HTTP transport, and with it every TLS setting
	// above, which is still validated. Tests use it to answer requests in
	// process instead of over a socket.
	Transport http.RoundTripper
}

// New builds a Client from Options. It does not contact the server: the
// session starts with the first request, or with ServerVersion.
func New(opts Options) (*Client, error) {
	base := strings.TrimRight(opts.URL, "/")
	if base == "" {
		return nil, errors.New("the kopia server URL must not be empty")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		// url.Error repeats the URL, which may hold a password, so only
		// the reason is kept.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("the kopia server URL is not a valid URL: %w", err)
	}
	if parsed.User != nil {
		return nil, errors.New("the kopia server URL must not contain credentials; set the username and password separately")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("the kopia server URL must be an http:// or https:// URL with a host, got %q", opts.URL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("the kopia server URL must not contain a query or fragment, got %q", opts.URL)
	}
	if opts.Username == "" {
		return nil, errors.New("the username must not be empty")
	}
	if opts.Password == "" {
		return nil, errors.New("the password must not be empty")
	}
	// Over plain HTTP these settings would silently do nothing, leaving the
	// impression that the connection is verified when it is not even
	// encrypted. Kopia's own client ignores a fingerprint in that case.
	if parsed.Scheme == "http" && (opts.CertFingerprint != "" || opts.CAFile != "") {
		return nil, errors.New("a certificate fingerprint or CA file only applies to an https:// URL, and the kopia server URL uses http://")
	}

	tlsConf, err := tlsConfig(opts)
	if err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	transport := opts.Transport
	if transport == nil {
		defaultTransport := http.DefaultTransport.(*http.Transport).Clone()
		if tlsConf != nil {
			defaultTransport.TLSClientConfig = tlsConf
		}
		transport = defaultTransport
	}

	sessionID, err := newSessionID()
	if err != nil {
		return nil, err
	}

	ua := opts.UserAgent
	if ua == "" {
		ua = "terraform-provider-kopia"
	}

	return &Client{
		baseURL:   base,
		username:  opts.Username,
		password:  opts.Password,
		userAgent: ua,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			// Kopia never redirects an API call. A redirect means the URL
			// points somewhere else — an http:// address a proxy upgrades to
			// https://, say — and following it would replay a PUT or POST as
			// a GET, so it is reported instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sessionID: sessionID,
	}, nil
}

// BaseURL returns the server's root address without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// Username returns the user the client authenticates as.
func (c *Client) Username() string { return c.username }

// LockConfigFile and UnlockConfigFile serialise the calls that rewrite the
// server's repository.config. PUT /api/v1/repo/throttle and POST
// /api/v1/repo/description both read, modify and write that file without any
// lock on the server side, so two of them running in parallel can lose one
// update (spec: repo.md §4.5 (f)); the throttle and description resources take
// this lock around their writes.
//
// The lock belongs to the Client, so it covers every resource of one provider
// configuration. Two provider configurations pointing at the same server, or
// any writer outside Terraform, are not covered.
func (c *Client) LockConfigFile() { c.configFileMu.Lock() }

// UnlockConfigFile releases the lock taken by LockConfigFile.
func (c *Client) UnlockConfigFile() { c.configFileMu.Unlock() }

// WithConfigFileLock runs fn while holding the lock of LockConfigFile.
func (c *Client) WithConfigFileLock(fn func() error) error {
	c.LockConfigFile()
	defer c.UnlockConfigFile()
	return fn()
}

// RepositoryConnected reports whether the server has a repository open. GET
// /api/v1/repo/status answers even without one, with "connected": false;
// every other API route then fails with NOT_CONNECTED. The provider's
// Configure uses it to warn early.
func (c *Client) RepositoryConnected(ctx context.Context) (bool, error) {
	var status struct {
		Connected bool `json:"connected"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/repo/status", nil, nil, &status); err != nil {
		return false, err
	}
	return status.Connected, nil
}

// do issues an API request and decodes a JSON response body into out when out
// is non-nil. path is the full path, starting with /api/v1/; query, when not
// empty, is appended as the query string. body, when non-nil, is sent as JSON.
//
// A 401 without WWW-Authenticate is Kopia's CSRF check failing, which happens
// after every server restart because the token's signing key is regenerated
// then. The handler has not run at that point, so the request is retried once
// with a fresh session, whatever its method; that is why the body is encoded
// once up front and sent again from the same bytes.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body for %s %s: %w", method, path, err)
		}
		payload = encoded
	}

	sess, err := c.sessionNewerThan(ctx, 0)
	if err != nil {
		return err
	}

	for attempt := 0; ; attempt++ {
		resp, raw, err := c.send(ctx, method, path, query, payload, sess)
		if err != nil {
			return err
		}

		if resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" && attempt == 0 {
			tflog.Debug(ctx, "Kopia rejected the CSRF token; renewing the session", map[string]any{"method": method, "path": path})
			fresh, err := c.sessionNewerThan(ctx, sess.gen)
			if err != nil {
				return fmt.Errorf("%s %s: kopia rejected the CSRF token, and renewing the session failed: %w", method, path, err)
			}
			if fresh.token == sess.token {
				return fmt.Errorf("%s %s: %w, although GET / issued the same token again, so the server's signing key "+
					"has not changed; something between the provider and the kopia server probably drops the %s "+
					"cookie or the %s header", method, path, ErrCSRFRejected, sessionCookieName, csrfHeader)
			}
			sess = fresh
			continue
		}

		return c.decode(method, path, resp, raw, out)
	}
}

// send makes one attempt at an API request with the given session and returns
// the response, whose body has been read into raw and closed.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, payload []byte, sess *session) (*http.Response, []byte, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("building request for %s %s: %w", method, path, err)
	}

	req.SetBasicAuth(c.username, c.password)
	// The cookie is set by hand rather than through a cookie jar: Kopia
	// master marks it Secure when it terminates TLS itself, and a jar would
	// then withhold it from a plain-HTTP hop in front of the server.
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.id})
	req.Header.Set(csrfHeader, sess.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading response body for %s %s: %w", method, path, err)
	}
	return resp, raw, nil
}

// decode turns a response into an *Error, or decodes its JSON body into out.
func (c *Client) decode(method, path string, resp *http.Response, raw []byte, out any) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := newError(method, path, resp.StatusCode, resp.Header, raw)
		// Kopia never echoes the Basic credentials, but an error body is
		// shown to the user verbatim, so make sure.
		apiErr.Message = Redact(apiErr.Message, c.password)
		apiErr.Body = Redact(apiErr.Body, c.password)
		return apiErr
	}

	// Kopia answers every API call with application/json. Anything else came
	// from somewhere other than the API: paths such as /repo... or
	// /policies... without the /api/v1 prefix are rewritten to the UI page and
	// answer 200 text/html, so a URL that does not point at the server's root
	// would otherwise "succeed" with an empty result.
	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		return fmt.Errorf("%s %s: expected a JSON response but got Content-Type %q; check that the URL is the kopia server's root address",
			method, path, resp.Header.Get("Content-Type"))
	}

	if out == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("%s %s: expected a JSON response body but got none", method, path)
	}
	if err := json.Unmarshal(trimmed, out); err != nil {
		return fmt.Errorf("decoding response body for %s %s: %w (body: %s)", method, path, err, truncate(string(trimmed), 512))
	}
	return nil
}
