package provider

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// Kopia saves a notification profile without checking a single option, and
// an invalid one only shows when the server fails to build a sender for it at
// send time, which it merely logs (spec: notification.md §5.1). These
// validators repeat, and in places tighten, the checks Kopia's senders make,
// so that a profile that could never work is refused at plan time.
//
// Several of the attributes they check are sensitive. Terraform hides those
// in plans and output, so their errors say what is wrong without quoting the
// value, unlike stringCheck's.

// notificationSensitiveCheck adapts a predicate into a validator for a
// sensitive string. A failing value is reported as "Attribute <path>
// <description>." without the value.
type notificationSensitiveCheck struct {
	description string
	check       func(string) bool
}

func (v notificationSensitiveCheck) Description(_ context.Context) string { return v.description }

func (v notificationSensitiveCheck) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v notificationSensitiveCheck) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() || v.check(req.ConfigValue.ValueString()) {
		return
	}
	resp.Diagnostics.AddAttributeError(
		req.Path,
		"Invalid Attribute Value",
		fmt.Sprintf("Attribute %s %s. The value is not shown because the attribute is sensitive.", req.Path, v.description),
	)
}

// notificationProfileNameValidator refuses the names Kopia stores but whose
// profile its API can then neither read nor delete.
func notificationProfileNameValidator() validator.String {
	return stringCheck{
		description: `must not contain "/" or be "." or "..": Kopia would store such a profile, but could ` +
			`then neither read nor delete it`,
		check: func(s string) bool { return kopiaclient.ValidateNotificationProfileName(s) == nil },
	}
}

// notificationSMTPAuthValidator requires smtp_username next to smtp_identity
// and smtp_password. Kopia authenticates only when the username is set
// (notification/sender/email/email_sender.go:27-29), so an identity or a
// password without one would be stored and never used.
func notificationSMTPAuthValidator() validator.String {
	return stringvalidator.AlsoRequires(path.MatchRelative().AtParent().AtName("smtp_username"))
}

// isNotificationAddress reports whether s can serve as one email address for
// Kopia's email sender. net/smtp passes the address to the server verbatim, as
// MAIL FROM:<s> or RCPT TO:<s>, and refuses CR and LF, so a display name, an
// address in angle brackets, or a space after a comma reaches the server as
// part of the address (spec: notification.md §3.3).
func isNotificationAddress(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("<>,", r) {
			return false
		}
	}
	return true
}

// notificationAddressValidator accepts one bare email address.
func notificationAddressValidator() validator.String {
	return stringCheck{
		description: `must be one bare email address such as "kopia@example.com", without a display name, ` +
			`angle brackets, spaces or commas, because Kopia passes it to the SMTP server as it is`,
		check: isNotificationAddress,
	}
}

// notificationAddressListValidator accepts bare email addresses separated by
// commas, the form Kopia splits into recipients with strings.Split(to, ",")
// (notification/sender/email/email_sender.go:57).
func notificationAddressListValidator() validator.String {
	return stringCheck{
		description: `must be bare email addresses separated by commas without spaces, such as ` +
			`"ops@example.com,oncall@example.com", because Kopia splits the list at the commas and passes each ` +
			`part to the SMTP server as it is`,
		check: func(s string) bool {
			for _, address := range strings.Split(s, ",") {
				if !isNotificationAddress(address) {
					return false
				}
			}
			return true
		},
	}
}

// notificationSMTPServerValidator accepts a host name or IP address without a
// port. Kopia appends ":<smtp_port>" with fmt.Sprintf("%v:%d") rather than
// net.JoinHostPort (notification/sender/email/email_sender.go:54), so a
// server with a port of its own, or an IPv6 address without brackets, gives
// an address nothing can dial.
func notificationSMTPServerValidator() validator.String {
	return stringCheck{
		description: "must be a host name or IP address without a port (set smtp_port instead) and without " +
			"spaces; an IPv6 address must be written in brackets",
		check: func(s string) bool {
			for _, r := range s {
				if unicode.IsSpace(r) || unicode.IsControl(r) {
					return false
				}
			}
			if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
				return len(s) > 2 && !strings.ContainsAny(s[1:len(s)-1], "[]")
			}
			return s != "" && !strings.ContainsAny(s, ":[]")
		},
	}
}

// isNotificationHTTPURL reports whether s is a URL Kopia's webhook and
// Pushover senders can post to. Kopia itself checks only that
// url.ParseRequestURI succeeds and the scheme is http or https
// (notification/sender/webhook/webhook_sender_options.go:30-37), which lets
// "http://" and "http:x" through to fail at send time with "no Host in
// request URL"; a host is required here as well.
func isNotificationHTTPURL(s string) bool {
	u, err := url.ParseRequestURI(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

const notificationHTTPURLDescription = "must be an absolute http:// or https:// URL with a host"

// notificationURLValidator accepts an http:// or https:// URL with a host.
func notificationURLValidator() validator.String {
	return stringCheck{description: notificationHTTPURLDescription, check: isNotificationHTTPURL}
}

// notificationSensitiveURLValidator is notificationURLValidator for a URL
// that may carry a token.
func notificationSensitiveURLValidator() validator.String {
	return notificationSensitiveCheck{description: notificationHTTPURLDescription, check: isNotificationHTTPURL}
}

// isNotificationHeaderName reports whether s is an HTTP token (RFC 9110
// §5.6.2), the only form Go sends as a header name: Kopia does not trim the
// name, so "X-A " fails at send time with "invalid header field name".
func isNotificationHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isNotificationTokenChar(r) {
			return false
		}
	}
	return true
}

// isNotificationTokenChar reports whether r is a tchar of RFC 9110 §5.6.2: an
// ASCII letter or digit, or one of the listed symbols. Every case is ASCII, so
// anything beyond ASCII is refused too.
func isNotificationTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
	}
}

// isNotificationHeaderValue reports whether a header value survives Kopia's
// headers string unchanged: one line per header, each trimmed on both sides
// (notification/sender/webhook/webhook_sender.go:37-42), and sent by Go,
// which refuses control characters.
func isNotificationHeaderValue(s string) bool {
	if strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// notificationWebhookHeadersValidator checks the webhook's headers map:
// names that are HTTP tokens and unique regardless of letter case — Go
// canonicalises them, so of two names differing only in case Kopia would
// send one — and values that read back from Kopia's headers string exactly as
// written. The map is sensitive, so values are never quoted, and neither are
// names that are not valid, in case a value ended up in one; a valid name
// only ever names a header.
type notificationWebhookHeadersValidator struct{}

func (v notificationWebhookHeadersValidator) Description(_ context.Context) string {
	return "header names must be HTTP tokens and unique regardless of letter case, and values must not " +
		"contain line breaks or other control characters, or begin or end with whitespace"
}

func (v notificationWebhookHeadersValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v notificationWebhookHeadersValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	elements := req.ConfigValue.Elements()
	names := make([]string, 0, len(elements))
	for name := range elements {
		names = append(names, name)
	}
	sort.Strings(names)

	invalidNames := 0
	byFoldedName := map[string]string{}
	for _, name := range names {
		if !isNotificationHeaderName(name) {
			invalidNames++
			continue
		}
		folded := strings.ToLower(name)
		if other, ok := byFoldedName[folded]; ok {
			resp.Diagnostics.AddAttributeError(
				req.Path,
				"Duplicate webhook header",
				fmt.Sprintf("Attribute %s sets both %q and %q. HTTP header names do not depend on letter case, so "+
					"Kopia would send only one of them; keep one.", req.Path, other, name),
			)
			continue
		}
		byFoldedName[folded] = name

		value, ok := elements[name].(types.String)
		if !ok || value.IsNull() || value.IsUnknown() {
			continue
		}
		if !isNotificationHeaderValue(value.ValueString()) {
			resp.Diagnostics.AddAttributeError(
				req.Path,
				"Invalid webhook header value",
				fmt.Sprintf("Attribute %s: the value of header %q must not contain line breaks or other control "+
					"characters, or begin or end with whitespace, which Kopia would strip. The value is not shown "+
					"because the attribute is sensitive.", req.Path, name),
			)
		}
	}
	if invalidNames > 0 {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid webhook header name",
			fmt.Sprintf("Attribute %s has %d header name(s) that are not HTTP tokens. A header name may contain "+
				"only letters, digits and the characters !#$%%&'*+-.^_`|~ — no spaces or colons. The names are "+
				"not shown because the attribute is sensitive.", req.Path, invalidNames),
		)
	}
}
