package kopiaclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kopia's API error codes (internal/serverapi/serverapi.go:83-96, identical in
// 0.22.0 to master). The code, not the HTTP status, is what identifies an
// error: NOT_FOUND, for one, comes with 400 from some handlers and 404 from
// others.
const (
	CodeInternal           = "INTERNAL"
	CodeAlreadyConnected   = "ALREADY_CONNECTED"
	CodeAlreadyInitialized = "ALREADY_INITIALIZED"
	CodeInvalidPassword    = "INVALID_PASSWORD"
	CodeInvalidToken       = "INVALID_TOKEN"
	CodeMalformedRequest   = "MALFORMED_REQUEST"
	CodeNotConnected       = "NOT_CONNECTED"
	CodeNotFound           = "NOT_FOUND"
	CodeNotInitialized     = "NOT_INITIALIZED"
	CodePathNotFound       = "PATH_NOT_FOUND"
	CodeStorageConnection  = "STORAGE_CONNECTION"
	CodeAccessDenied       = "ACCESS_DENIED"
)

// The session bootstrap (GET /) fails in a handful of ways that each point at
// a different configuration mistake, so each has a sentinel the provider can
// test with errors.Is and turn into specific advice. The errors returned wrap
// them with what the server actually answered.
var (
	// ErrBadCredentials is a 401 carrying WWW-Authenticate: Kopia did not
	// accept the username and password at all.
	ErrBadCredentials = errors.New("kopia rejected the username or password")

	// ErrNotUIUser is a 403 from GET /: the credentials are valid, but they
	// belong to a user other than the one `kopia server start` names with
	// --server-username, such as the server-control user or a repository
	// user, and only that one may use the UI API.
	ErrNotUIUser = errors.New("the user is not the kopia server's UI user (--server-username)")

	// ErrUINotServed is a 404 from GET /. A server started with --no-ui
	// registers neither the page nor the API behind it, and answers 404 before
	// it even checks credentials; a URL that is not the server's root address
	// produces the same.
	ErrUINotServed = errors.New("the server does not serve the kopia UI at this address")

	// ErrNotKopiaServer is a successful GET / whose page carries no
	// kopia-csrf-token meta tag, which every kopia server injects into its UI
	// page.
	ErrNotKopiaServer = errors.New("the server's GET / response carries no kopia CSRF token")

	// ErrCSRFRejected means Kopia rejected the CSRF token although a fresh
	// GET / issued the very same token again, so the signing key did not
	// change and the rejection has another cause: usually something between
	// the provider and Kopia drops the Kopia-Session-Cookie cookie or the
	// X-Kopia-Csrf-Token header.
	ErrCSRFRejected = errors.New("kopia rejected the session's CSRF token")
)

// Error is a non-2xx response from a Kopia API call.
//
// Kopia reports API failures as {"code": ..., "error": ...}
// (internal/serverapi/serverapi.go:98-102), but sends that JSON with
// Content-Type text/plain, pretty-printed with a two-space indent in the
// 0.22 and 0.23 releases and compact on master. Failures from outside the
// handlers — authentication, the CSRF check, unknown routes — are plain text.
// newError therefore recognises JSON by its content, and leaves Code empty for
// everything else.
type Error struct {
	// Method and Path name the request, without its query string.
	Method string
	Path   string
	// StatusCode is the HTTP status.
	StatusCode int
	// Code is Kopia's error code, such as NOT_FOUND, or "" for a plain-text
	// response.
	Code string
	// Message is Kopia's error text, or the trimmed plain-text body.
	Message string
	// Body is the raw response body.
	Body string
	// Authenticate is the WWW-Authenticate header. Kopia sends it with a 401
	// for missing or wrong credentials, and never with the 401 of a failed
	// CSRF check.
	Authenticate string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("kopia API error: ")
	if e.Method != "" {
		fmt.Fprintf(&b, "%s %s: ", e.Method, e.Path)
	}
	fmt.Fprintf(&b, "HTTP %d", e.StatusCode)
	if e.Code != "" {
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", truncate(e.Message, 512))
	}
	return b.String()
}

// newError builds the Error for a non-2xx response. The body has already been
// read; header is consulted for WWW-Authenticate and, on a redirect, Location.
func newError(method, path string, status int, header http.Header, raw []byte) *Error {
	apiErr := &Error{
		Method:       method,
		Path:         path,
		StatusCode:   status,
		Body:         string(raw),
		Authenticate: header.Get("WWW-Authenticate"),
	}

	trimmed := bytes.TrimSpace(raw)
	var envelope struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if len(trimmed) > 0 && trimmed[0] == '{' && json.Unmarshal(trimmed, &envelope) == nil && envelope.Code != "" {
		apiErr.Code = envelope.Code
		apiErr.Message = envelope.Error
		return apiErr
	}

	apiErr.Message = string(trimmed)
	// The client never follows redirects (see New), so a 3xx is reported as an
	// error; where it pointed is the useful part.
	if status >= 300 && status < 400 {
		if location := header.Get("Location"); location != "" {
			apiErr.Message = "redirected to " + location + "; set the URL to the address the kopia server answers on"
		}
	}
	return apiErr
}

// asError extracts the *Error from err, if there is one.
func asError(err error) (*Error, bool) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// IsNotFound reports whether Kopia answered with code NOT_FOUND, whatever the
// status: the policy handler, for one, answers a missing policy with 400
// (internal/server/api_policies.go:59).
func IsNotFound(err error) bool {
	apiErr, ok := asError(err)
	return ok && apiErr.Code == CodeNotFound
}

// IsNotConnected reports whether the server has no repository open (400
// NOT_CONNECTED). Every API route except a few status endpoints needs one.
func IsNotConnected(err error) bool {
	apiErr, ok := asError(err)
	return ok && apiErr.Code == CodeNotConnected
}

// IsAccessDenied reports whether Kopia authenticated the user but refused it
// the UI API: a JSON 403 ACCESS_DENIED from an API route, the plain-text 403
// from the UI catch-all, or ErrNotUIUser from the session bootstrap.
func IsAccessDenied(err error) bool {
	if errors.Is(err, ErrNotUIUser) {
		return true
	}
	apiErr, ok := asError(err)
	return ok && (apiErr.Code == CodeAccessDenied || (apiErr.Code == "" && apiErr.StatusCode == http.StatusForbidden))
}

// IsUnauthorized reports whether Kopia rejected the username or password: a
// 401 with WWW-Authenticate, or ErrBadCredentials from the session bootstrap.
// A 401 without WWW-Authenticate is a CSRF failure instead; see
// IsCSRFRejected.
func IsUnauthorized(err error) bool {
	if errors.Is(err, ErrBadCredentials) {
		return true
	}
	apiErr, ok := asError(err)
	return ok && apiErr.StatusCode == http.StatusUnauthorized && apiErr.Authenticate != ""
}

// IsCSRFRejected reports whether Kopia rejected the session's CSRF token even
// after the client fetched a fresh one.
func IsCSRFRejected(err error) bool {
	if errors.Is(err, ErrCSRFRejected) {
		return true
	}
	apiErr, ok := asError(err)
	return ok && apiErr.StatusCode == http.StatusUnauthorized && apiErr.Authenticate == ""
}

// IsEndpointMissing reports whether an API path does not exist on this
// server: a plain-text 404, which is what the UI's catch-all file server
// answers for a route the running Kopia release does not register (or for a
// method the route does not accept). Handler-level "not found" errors carry
// JSON and are reported by IsNotFound instead.
func IsEndpointMissing(err error) bool {
	// GET / answers 404 when the server runs with --no-ui, which is a
	// configuration problem rather than a missing endpoint.
	if errors.Is(err, ErrUINotServed) {
		return false
	}
	apiErr, ok := asError(err)
	return ok && apiErr.Code == "" && apiErr.StatusCode == http.StatusNotFound
}

// redactedMarker replaces every secret Redact finds.
const redactedMarker = "<redacted>"

// Redact replaces every occurrence of each non-empty secret in text with
// "<redacted>". Kopia echoes parts of a request body in some error messages —
// a notification profile's SMTP password can come back from a failed test
// send — so error texts that may contain secrets go through this before they
// reach a diagnostic or a log.
//
// Besides the secret as given, it replaces the forms in which an error message
// is likely to quote it: JSON-escaped (Kopia's own error bodies), Go-quoted
// (%q in a Go error) and URL-encoded. Longer secrets are replaced first, so a
// secret that contains a shorter one is not left half-redacted.
func Redact(text string, secrets ...string) string {
	var forms []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			forms = append(forms, s)
		}
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		add(secret)
		if encoded, err := json.Marshal(secret); err == nil {
			add(string(encoded[1 : len(encoded)-1]))
		}
		quoted := strconv.Quote(secret)
		add(quoted[1 : len(quoted)-1])
		add(url.QueryEscape(secret))
		add(url.PathEscape(secret))
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	for _, form := range forms {
		text = strings.ReplaceAll(text, form, redactedMarker)
	}
	return text
}

// truncate shortens s to at most max bytes for an error message, without
// splitting a UTF-8 sequence.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}
