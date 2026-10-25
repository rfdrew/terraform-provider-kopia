package kopiaclient

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		secrets []string
		want    string
	}{
		{
			name:    "verbatim",
			text:    "smtp: 535 authentication failed for p4ss",
			secrets: []string{"p4ss"},
			want:    "smtp: 535 authentication failed for <redacted>",
		},
		{
			name:    "every occurrence",
			text:    "tok tok tok",
			secrets: []string{"tok"},
			want:    "<redacted> <redacted> <redacted>",
		},
		{
			// Kopia's error bodies are JSON, so a quote or backslash in a
			// secret arrives escaped.
			name:    "JSON-escaped",
			text:    `{"code":"INTERNAL","error":"bad value \"pa\\ss\""}`,
			secrets: []string{`"pa\ss"`},
			want:    `{"code":"INTERNAL","error":"bad value <redacted>"}`,
		},
		{
			name:    "Go-quoted",
			text:    fmt.Sprintf("unexpected token %q", "line1\nline2"),
			secrets: []string{"line1\nline2"},
			want:    `unexpected token "<redacted>"`,
		},
		{
			name:    "URL-encoded",
			text:    "POST https://api.pushover.net/1/messages.json?token=a%2Bb%26c failed",
			secrets: []string{"a+b&c"},
			want:    "POST https://api.pushover.net/1/messages.json?token=<redacted> failed",
		},
		{
			name:    "a secret containing another",
			text:    "user=abc password=abcdef",
			secrets: []string{"abc", "abcdef"},
			want:    "user=<redacted> password=<redacted>",
		},
		{
			name:    "empty secrets are ignored",
			text:    "nothing to hide",
			secrets: []string{"", ""},
			want:    "nothing to hide",
		},
		{
			name: "no secrets",
			text: "nothing to hide",
			want: "nothing to hide",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.text, tc.secrets...); got != tc.want {
				t.Fatalf("Redact(%q, %q) = %q, want %q", tc.text, tc.secrets, got, tc.want)
			}
		})
	}
}

// The predicates see through wrapping, so callers can add context with %w.
func TestPredicatesSeeThroughWrapping(t *testing.T) {
	notFound := &Error{StatusCode: http.StatusBadRequest, Code: CodeNotFound, Message: "policy not found"}
	if !IsNotFound(fmt.Errorf("reading policy: %w", notFound)) {
		t.Error("IsNotFound does not see through %w")
	}
	if IsNotFound(errors.New("policy not found")) || IsNotFound(nil) {
		t.Error("IsNotFound accepted something that is not a kopia error")
	}

	badCredentials := fmt.Errorf("%w: %w", ErrBadCredentials, &Error{StatusCode: http.StatusUnauthorized, Authenticate: `Basic realm="Kopia"`})
	if !IsUnauthorized(fmt.Errorf("configure: %w", badCredentials)) {
		t.Error("IsUnauthorized does not see through %w")
	}
}

func TestErrorMessagesAreTruncated(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	err := &Error{Method: http.MethodGet, Path: "/api/v1/example", StatusCode: http.StatusInternalServerError, Message: long}
	got := err.Error()
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Fatalf("Error() = %q, want it truncated", got)
	}
	if !strings.Contains(got, strings.Repeat("é", 256)) || strings.Contains(got, strings.Repeat("é", 257)) {
		t.Errorf("Error() kept the wrong amount of the message: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("Error() split a UTF-8 sequence: %q", got)
	}
}
