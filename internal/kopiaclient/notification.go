package kopiaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// A notification profile tells a Kopia server where to send notifications —
// snapshot reports, maintenance errors — and from which severity on. Kopia
// stores each one as a repository manifest labelled with the profile's name
// (notification/notifyprofile/notification_profile.go), so profiles live in
// the repository, not in the server's configuration, and a change applies to
// the next notification without a restart.
//
// The REST handlers validate nothing: POST stores whatever JSON it is given,
// and an invalid profile only shows up when Kopia fails to build a sender
// for it at send time, which it merely logs (spec: notification.md §5.1). This
// package therefore always writes a complete, explicit configuration, and
// reads stored ones tolerantly, the way Kopia itself decodes them when it
// sends.

// The notification method types this package can represent: the three that
// Kopia's CLI configures (cli/command_notification_profile_configure.go:12-14).
// Every kopia binary also registers "testsender", which only works inside
// Kopia's own tests.
const (
	NotificationMethodEmail    = "email"
	NotificationMethodPushover = "pushover"
	NotificationMethodWebhook  = "webhook"
)

// The two message formats every method accepts
// (notification/sender/notification_message.go:98-117). Kopia's code comments
// mention "md", but the senders refuse it, and the check is case-sensitive.
const (
	NotificationFormatText = "txt"
	NotificationFormatHTML = "html"
)

// The defaults Kopia applies to an option left empty or zero when it builds a
// sender. They are applied in memory only and never written back to the
// stored profile.
const (
	// DefaultEmailFormat is email's default format
	// (notification/sender/email/email_sender_options.go:59).
	DefaultEmailFormat = NotificationFormatHTML
	// DefaultSMTPPort replaces an SMTP port of 0
	// (notification/sender/email/email_sender.go:18).
	DefaultSMTPPort int64 = 587
	// DefaultPushoverFormat is Pushover's default format
	// (notification/sender/pushover/pushover_sender_options.go:30).
	DefaultPushoverFormat = NotificationFormatText
	// DefaultWebhookFormat is the webhook's default format
	// (notification/sender/webhook/webhook_sender_options.go:26).
	DefaultWebhookFormat = NotificationFormatText
	// DefaultWebhookMethod is the webhook's default HTTP method
	// (notification/sender/webhook/webhook_sender_options.go:22-24).
	DefaultWebhookMethod = "POST"
)

// Notification severities (notification/notification_send.go:45-74). On the
// wire a severity is a plain int32 and Kopia accepts any value; these are the
// ones with a name, which is what `kopia notification profile configure
// --min-severity` takes. A profile receives a message when the message's
// severity is at least the profile's MinSeverity.
const (
	NotificationSeverityVerbose int32 = -100
	NotificationSeveritySuccess int32 = -10
	NotificationSeverityReport  int32 = 0
	NotificationSeverityWarning int32 = 10
	NotificationSeverityError   int32 = 20
)

// notificationSeverities lists the named severities from the lowest to the
// highest.
var notificationSeverities = []struct {
	name  string
	value int32
}{
	{"verbose", NotificationSeverityVerbose},
	{"success", NotificationSeveritySuccess},
	{"report", NotificationSeverityReport},
	{"warning", NotificationSeverityWarning},
	{"error", NotificationSeverityError},
}

// NotificationSeverityNames returns the names of Kopia's severities, from the
// lowest to the highest.
func NotificationSeverityNames() []string {
	names := make([]string, 0, len(notificationSeverities))
	for _, s := range notificationSeverities {
		names = append(names, s.name)
	}
	return names
}

// NotificationSeverityName returns the name of a severity, and false for a
// number Kopia has no name for. Kopia's own SeverityDefault is also 0, but
// has no name of its own: 0 is "report".
func NotificationSeverityName(severity int32) (string, bool) {
	for _, s := range notificationSeverities {
		if s.value == severity {
			return s.name, true
		}
	}
	return "", false
}

// ParseNotificationSeverity returns the severity a name stands for, matching
// it exactly, as the CLI does.
func ParseNotificationSeverity(name string) (int32, bool) {
	for _, s := range notificationSeverities {
		if s.name == name {
			return s.value, true
		}
	}
	return 0, false
}

// ErrUnsupportedNotificationProfile is returned for a stored profile this
// package cannot represent, so it exists but cannot be read: one whose method
// is "testsender", a type a newer Kopia added, or none at all, or whose
// options are not an object or have an option of the wrong type. Kopia cannot
// send through most of these either.
var ErrUnsupportedNotificationProfile = errors.New("unsupported notification profile")

// EmailNotificationOptions are the options of an email profile:
// email.Options (notification/sender/email/email_sender_options.go:12-24).
//
// Kopia sends with Go's net/smtp.SendMail: a plain connection that switches to
// STARTTLS when the server offers it, never implicit TLS, and with PLAIN
// authentication only when SMTPUsername is set. CC is stored but never used.
type EmailNotificationOptions struct {
	SMTPServer   string `json:"smtpServer"`
	SMTPPort     int64  `json:"smtpPort"`
	SMTPIdentity string `json:"smtpIdentity"`
	SMTPUsername string `json:"smtpUsername"`
	SMTPPassword string `json:"smtpPassword"`
	From         string `json:"from"`
	To           string `json:"to"`
	CC           string `json:"cc"`
	Format       string `json:"format"`
}

// withDefaults fills in what Kopia would substitute for empty options.
func (o EmailNotificationOptions) withDefaults() EmailNotificationOptions {
	if o.SMTPPort == 0 {
		o.SMTPPort = DefaultSMTPPort
	}
	if o.Format == "" {
		o.Format = DefaultEmailFormat
	}
	return o
}

// PushoverNotificationOptions are the options of a Pushover profile:
// pushover.Options (notification/sender/pushover/pushover_sender_options.go:12-18).
// Endpoint is an undocumented override of Pushover's API URL that the CLI
// does not expose; empty means https://api.pushover.net/1/messages.json.
type PushoverNotificationOptions struct {
	AppToken string `json:"appToken"`
	UserKey  string `json:"userKey"`
	Format   string `json:"format"`
	// Kopia's own struct omits an empty endpoint too, so a profile written
	// here looks like one the CLI wrote.
	Endpoint string `json:"endpoint,omitempty"`
}

// withDefaults fills in what Kopia would substitute for empty options.
func (o PushoverNotificationOptions) withDefaults() PushoverNotificationOptions {
	if o.Format == "" {
		o.Format = DefaultPushoverFormat
	}
	return o
}

// WebhookNotificationOptions are the options of a webhook profile:
// webhook.Options (notification/sender/webhook/webhook_sender_options.go:13-18).
// Headers holds extra request headers as "Key: Value" lines separated by
// "\n"; see ParseWebhookHeaders.
type WebhookNotificationOptions struct {
	Endpoint string `json:"endpoint"`
	Method   string `json:"method"`
	Format   string `json:"format"`
	Headers  string `json:"headers"`
}

// withDefaults fills in what Kopia would substitute for empty options.
func (o WebhookNotificationOptions) withDefaults() WebhookNotificationOptions {
	if o.Method == "" {
		o.Method = DefaultWebhookMethod
	}
	if o.Format == "" {
		o.Format = DefaultWebhookFormat
	}
	return o
}

// NotificationProfile is one notification profile: notifyprofile.Config
// (notification/notifyprofile/notification_profile.go:22-26), with the
// method's options decoded into the struct of its type. Exactly one of Email,
// Pushover and Webhook is set.
//
// Options read from Kopia are reported the way Kopia's sender sees them: a
// missing option is its zero value, and an empty format, method or SMTP port
// is replaced with Kopia's default. Options written by this package always
// carry every key with the defaults spelt out, so that what is stored is
// exactly what is read back.
type NotificationProfile struct {
	// Name is the profile's name, Kopia's "profile". It is also the label
	// Kopia stores, finds and deletes the profile by, and is case-sensitive.
	Name string
	// MinSeverity is the lowest severity the profile receives.
	MinSeverity int32

	Email    *EmailNotificationOptions
	Pushover *PushoverNotificationOptions
	Webhook  *WebhookNotificationOptions

	// Warnings describes stored options that could be decoded here, but that
	// Kopia itself cannot use as they are stored, so the profile sends
	// nothing until it is written again. It is only ever set on a profile
	// read from Kopia.
	Warnings []string
}

// MethodType returns the profile's method type, or "" when no options are
// set.
func (p *NotificationProfile) MethodType() string {
	switch {
	case p.Email != nil:
		return NotificationMethodEmail
	case p.Pushover != nil:
		return NotificationMethodPushover
	case p.Webhook != nil:
		return NotificationMethodWebhook
	}
	return ""
}

// Secrets returns the profile's secret values, for Redact: the SMTP password,
// the Pushover tokens, and the webhook's endpoint and headers. Webhook URLs of
// services such as Slack, Discord or ntfy carry their token in the path or
// the query, and Kopia quotes the whole URL in delivery errors.
func (p *NotificationProfile) Secrets() []string {
	var secrets []string
	if p.Email != nil {
		secrets = append(secrets, p.Email.SMTPPassword)
	}
	if p.Pushover != nil {
		secrets = append(secrets, p.Pushover.AppToken, p.Pushover.UserKey)
	}
	if p.Webhook != nil {
		secrets = append(secrets, p.Webhook.Endpoint)
		// Go quotes a request URL in its errors in its re-encoded form,
		// which need not be the one configured, and keeps the query as is.
		if u, err := url.Parse(p.Webhook.Endpoint); err == nil {
			secrets = append(secrets, u.String(), u.RawQuery)
			if password, ok := u.User.Password(); ok {
				// net/http masks the password as "***" in the URLs it
				// quotes (stripPassword in net/http/client.go), which
				// leaves the rest of the URL to redact.
				masked := strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)
				secrets = append(secrets, password, masked)
			}
		}
		secrets = append(secrets, p.Webhook.Headers)
		for _, line := range strings.Split(p.Webhook.Headers, "\n") {
			secrets = append(secrets, strings.TrimSpace(line))
		}
	}
	return secrets
}

// notificationProfileWire is notifyprofile.Config as JSON. MethodConfig has
// its own JSON encoding (notification/sender/sender_config.go:19-67), an
// object with "type" and "config"; config is kept raw here and decoded by
// type.
type notificationProfileWire struct {
	Profile     string                  `json:"profile"`
	Method      *notificationMethodWire `json:"method"`
	MinSeverity int32                   `json:"minSeverity"`
}

type notificationMethodWire struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

// requestBody encodes the profile for POST /api/v1/notificationProfiles and
// POST /api/v1/testNotificationProfile, with every option written out.
//
// Kopia accepts a body without a method, but stores it as a profile that
// breaks GET, LIST and every notification the server sends (spec:
// notification.md §2.4), so a profile without exactly one method is refused
// here instead.
func (p *NotificationProfile) requestBody() (*notificationProfileWire, error) {
	var options any
	set := 0
	if p.Email != nil {
		options = p.Email.withDefaults()
		set++
	}
	if p.Pushover != nil {
		options = p.Pushover.withDefaults()
		set++
	}
	if p.Webhook != nil {
		options = p.Webhook.withDefaults()
		set++
	}
	if set != 1 {
		return nil, fmt.Errorf("notification profile %q must have exactly one method, email, pushover or webhook, but has %d", p.Name, set)
	}
	config, err := json.Marshal(options)
	if err != nil {
		return nil, fmt.Errorf("encoding the options of notification profile %q: %w", p.Name, err)
	}
	return &notificationProfileWire{
		Profile:     p.Name,
		Method:      &notificationMethodWire{Type: p.MethodType(), Config: config},
		MinSeverity: p.MinSeverity,
	}, nil
}

// NotificationProfileSummary is what ListNotificationProfiles reports of a
// profile: neither its options nor, therefore, any of its secrets.
type NotificationProfileSummary struct {
	Name string
	// MethodType is the method's type as stored, which may be one this
	// package cannot represent, such as "testsender".
	MethodType  string
	MinSeverity int32
}

// ValidateNotificationProfileName reports why Kopia could store a profile
// under name but never read or delete it, or nil when the name is usable.
//
// The profile routes take the name as one path segment
// (internal/server/server.go:169-174), and the router matches the decoded,
// cleaned path: an empty name leaves no segment, a "/" splits it in two, and
// "." and ".." are cleaned away (spec: notification.md §5.1). Every other
// name works once escaped.
func ValidateNotificationProfileName(name string) error {
	switch {
	case name == "":
		return errors.New("a notification profile name must not be empty")
	case strings.Contains(name, "/"):
		return fmt.Errorf("notification profile name %q contains a slash, which Kopia's API cannot address", name)
	case name == "." || name == "..":
		return fmt.Errorf("notification profile name %q cannot be addressed through Kopia's API", name)
	}
	return nil
}

// notificationProfilePath is the API path of the named profile.
func notificationProfilePath(name string) (string, error) {
	if err := ValidateNotificationProfileName(name); err != nil {
		return "", err
	}
	return "/api/v1/notificationProfiles/" + url.PathEscape(name), nil
}

// notificationProfileNotFoundMessage is Kopia's answer to GET of a profile
// that does not exist: notifyprofile.ErrNotFound wrapped by
// internalServerError, with HTTP 500 and code INTERNAL rather than a 404 or
// NOT_FOUND, in every supported release
// (internal/server/api_notification_profile.go:52-59,
// notification/notifyprofile/notification_profile.go:61).
const notificationProfileNotFoundMessage = "internal server error: profile not found"

// GetNotificationProfile fetches the named profile. A profile that does not
// exist yields an error IsNotFound recognises. A stored profile this package
// cannot represent — a method other than email, pushover or webhook, options
// that are not an object, an option of the wrong type — yields one wrapping
// ErrUnsupportedNotificationProfile, which names the problem without quoting
// any value, since options hold secrets.
func (c *Client) GetNotificationProfile(ctx context.Context, name string) (*NotificationProfile, error) {
	path, err := notificationProfilePath(name)
	if err != nil {
		return nil, err
	}

	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &raw); err != nil {
		if apiErr, ok := asError(err); ok && apiErr.Code == CodeInternal && strings.TrimSpace(apiErr.Message) == notificationProfileNotFoundMessage {
			return nil, &Error{
				Method:     apiErr.Method,
				Path:       apiErr.Path,
				StatusCode: apiErr.StatusCode,
				Code:       CodeNotFound,
				Message:    fmt.Sprintf("notification profile %q does not exist (%s)", name, apiErr.Message),
				Body:       apiErr.Body,
			}
		}
		return nil, notificationResponseError(http.MethodGet, path, err)
	}

	profile, err := decodeNotificationProfile(raw)
	if err != nil {
		return nil, fmt.Errorf("GET %s: notification profile %q: %w", path, name, err)
	}
	return profile, nil
}

// ListNotificationProfiles lists every stored profile without its options.
// Kopia answers null when there are none, which is returned as an empty
// slice.
//
// The list is in Kopia's order, by when each profile was last written, and
// Kopia does not de-duplicate it: should two writers have stored the same
// name at once, the name appears twice, and GetNotificationProfile returns
// the later entry (spec: notification.md §2.5).
func (c *Client) ListNotificationProfiles(ctx context.Context) ([]NotificationProfileSummary, error) {
	const path = "/api/v1/notificationProfiles"

	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &raw); err != nil {
		return nil, notificationResponseError(http.MethodGet, path, err)
	}

	// Only the name, the method type and the severity are decoded; the
	// options, and with them every secret, are skipped.
	var listed []struct {
		Profile string `json:"profile"`
		Method  *struct {
			Type string `json:"type"`
		} `json:"method"`
		MinSeverity int32 `json:"minSeverity"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, fmt.Errorf("GET %s: decoding the notification profiles: %w", path, err)
	}

	summaries := make([]NotificationProfileSummary, 0, len(listed))
	for _, p := range listed {
		summary := NotificationProfileSummary{Name: p.Profile, MinSeverity: p.MinSeverity}
		if p.Method != nil {
			summary.MethodType = p.Method.Type
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// SaveNotificationProfile creates the profile, or replaces the profile of the
// same name entirely: Kopia's POST is an upsert keyed by the name, with no
// "already exists" error and no merging (spec: notification.md §5.1). Every
// option is written out, defaults included.
//
// Kopia echoes the whole request body in some error messages, so the
// profile's secrets are redacted from the error returned.
func (c *Client) SaveNotificationProfile(ctx context.Context, profile NotificationProfile) error {
	if err := ValidateNotificationProfileName(profile.Name); err != nil {
		return err
	}
	body, err := profile.requestBody()
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/notificationProfiles", nil, body, nil)
	return redactNotificationError(err, profile.Secrets())
}

// DeleteNotificationProfile deletes the named profile. Kopia answers 200 also
// when there is no such profile, so this is idempotent. Template overrides
// made for the profile with the CLI are not deleted, and apply again to a
// profile created later under the same name.
func (c *Client) DeleteNotificationProfile(ctx context.Context, name string) error {
	path, err := notificationProfilePath(name)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// TestNotificationProfile has Kopia send its test notification through
// profile, which need not be saved: the server builds a sender from the
// request and uses the name only to look up the profile's template overrides
// (spec: notification.md §5.5). It returns once the message was delivered;
// an invalid profile and a failed delivery both come back as 400
// MALFORMED_REQUEST.
//
// Kopia runs the delivery on its own context: when the request times out on
// this side, Kopia carries on, and the message may still arrive. SMTP
// delivery has no time limit on Kopia's side at all.
//
// Delivery errors quote the full webhook URL and invalid requests the whole
// body, so the profile's secrets are redacted from the error returned.
func (c *Client) TestNotificationProfile(ctx context.Context, profile NotificationProfile) error {
	body, err := profile.requestBody()
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/testNotificationProfile", nil, body, nil)
	return redactNotificationError(err, profile.Secrets())
}

// notificationResponseError keeps the response body out of a decoding error:
// do quotes the body when a successful response is not valid JSON, and a
// profile's body holds its secrets.
func notificationResponseError(method, path string, err error) error {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Errorf("%s %s: the response is not valid JSON: %s at offset %d", method, path, syntaxErr.Error(), syntaxErr.Offset)
	}
	return err
}

// notificationRedactedError carries an error whose text had secrets removed.
// It still unwraps to the original, so the *Error predicates keep working;
// the original's own fields have been redacted too.
type notificationRedactedError struct {
	text string
	err  error
}

func (e *notificationRedactedError) Error() string { return e.text }

func (e *notificationRedactedError) Unwrap() error { return e.err }

// redactNotificationError removes secrets from err: from the message and body
// of the *Error inside it, and from its text as a whole, which wrappers made
// before the redaction computed already.
//
// When Kopia echoes a request body, the secrets in it were JSON-escaped once
// in the request and are escaped again in the error envelope: a webhook's
// "&" arrives as \u0026 in the raw body and its headers' line break as \\n.
// Redact covers one level of escaping, so the once-escaped forms are added as
// secrets of their own.
func redactNotificationError(err error, secrets []string) error {
	if err == nil {
		return nil
	}
	forms := append([]string(nil), secrets...)
	for _, secret := range secrets {
		if encoded, err := json.Marshal(secret); err == nil && secret != "" {
			forms = append(forms, string(encoded[1:len(encoded)-1]))
		}
	}
	if apiErr, ok := asError(err); ok {
		apiErr.Message = Redact(apiErr.Message, forms...)
		apiErr.Body = Redact(apiErr.Body, forms...)
	}
	return &notificationRedactedError{text: Redact(err.Error(), forms...), err: err}
}

// decodeNotificationProfile decodes a profile as GET returns it. Its error
// messages name options and JSON kinds, never values.
func decodeNotificationProfile(raw []byte) (*NotificationProfile, error) {
	var wire notificationProfileWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedNotificationProfile, err)
	}
	if wire.Method == nil {
		return nil, fmt.Errorf("%w: it has no method", ErrUnsupportedNotificationProfile)
	}

	profile := &NotificationProfile{Name: wire.Profile, MinSeverity: wire.MinSeverity}
	switch wire.Method.Type {
	case NotificationMethodEmail, NotificationMethodPushover, NotificationMethodWebhook:
	default:
		return nil, fmt.Errorf("%w: its method %q is none of %s, %s and %s", ErrUnsupportedNotificationProfile,
			wire.Method.Type, NotificationMethodEmail, NotificationMethodPushover, NotificationMethodWebhook)
	}

	config, err := newNotificationConfig(wire.Method.Config)
	if err != nil {
		return nil, fmt.Errorf("%w: %s options: %w", ErrUnsupportedNotificationProfile, wire.Method.Type, err)
	}

	switch wire.Method.Type {
	case NotificationMethodEmail:
		var o EmailNotificationOptions
		err = errors.Join(
			config.stringOption("smtpServer", &o.SMTPServer),
			config.portOption("smtpPort", &o.SMTPPort, &profile.Warnings),
			config.stringOption("smtpIdentity", &o.SMTPIdentity),
			config.stringOption("smtpUsername", &o.SMTPUsername),
			config.stringOption("smtpPassword", &o.SMTPPassword),
			config.stringOption("from", &o.From),
			config.stringOption("to", &o.To),
			config.stringOption("cc", &o.CC),
			config.stringOption("format", &o.Format),
		)
		o = o.withDefaults()
		profile.Email = &o
	case NotificationMethodPushover:
		var o PushoverNotificationOptions
		err = errors.Join(
			config.stringOption("appToken", &o.AppToken),
			config.stringOption("userKey", &o.UserKey),
			config.stringOption("format", &o.Format),
			config.stringOption("endpoint", &o.Endpoint),
		)
		o = o.withDefaults()
		profile.Pushover = &o
	case NotificationMethodWebhook:
		var o WebhookNotificationOptions
		err = errors.Join(
			config.stringOption("endpoint", &o.Endpoint),
			config.stringOption("method", &o.Method),
			config.stringOption("format", &o.Format),
			config.stringOption("headers", &o.Headers),
		)
		o = o.withDefaults()
		profile.Webhook = &o
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s options: %w", ErrUnsupportedNotificationProfile, wire.Method.Type, err)
	}
	return profile, nil
}

// notificationConfig is a stored method config, looked up the way Kopia reads
// it when it builds a sender (notification/sender/sender.go:73-83). Kopia
// keeps the config as a generic map, re-encodes it — which sorts the keys —
// and decodes the result into the method's options struct, where
// encoding/json matches keys to fields regardless of letter case and a later
// key overwrites an earlier one. So among keys that differ only in case, the
// last in sorted order wins, and unknown keys are ignored.
type notificationConfig struct {
	keys   []string
	values map[string]json.RawMessage
}

func newNotificationConfig(raw json.RawMessage) (notificationConfig, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return notificationConfig{}, fmt.Errorf("the stored options are %s, not an object", notificationJSONKind(trimmed))
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return notificationConfig{}, fmt.Errorf("decoding the stored options: %w", err)
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return notificationConfig{keys: keys, values: values}, nil
}

// lookup returns the value Kopia would decode into the option named name, and
// false when the config has none. A JSON null counts as none: encoding/json
// leaves a field untouched when it decodes a null into it, so a null neither
// sets the option nor overrides a value an earlier key gave it.
func (c notificationConfig) lookup(name string) (json.RawMessage, bool) {
	var found json.RawMessage
	for _, k := range c.keys {
		if !strings.EqualFold(k, name) {
			continue
		}
		if trimmed := bytes.TrimSpace(c.values[k]); len(trimmed) > 0 && string(trimmed) != "null" {
			found = trimmed
		}
	}
	return found, found != nil
}

// stringOption decodes a string option into dst, leaving it empty when the
// option is absent.
func (c notificationConfig) stringOption(name string, dst *string) error {
	raw, ok := c.lookup(name)
	if !ok {
		return nil
	}
	if raw[0] != '"' || json.Unmarshal(raw, dst) != nil {
		return fmt.Errorf("option %s is %s, not a string", name, notificationJSONKind(raw))
	}
	return nil
}

// portOption decodes the SMTP port, leaving it 0 when it is absent. Kopia
// needs a JSON integer here; a whole number sent as a string is accepted as
// well, for profiles another client wrote that way, and reported in warnings,
// because Kopia itself cannot build a sender from it.
func (c notificationConfig) portOption(name string, dst *int64, warnings *[]string) error {
	raw, ok := c.lookup(name)
	if !ok {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return fmt.Errorf("option %s is not a valid JSON string", name)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("option %s is a string that is not a whole number", name)
		}
		*dst = n
		*warnings = append(*warnings, "Option "+name+" is stored as a string rather than as a number, which "+
			"Kopia cannot read, so Kopia sends no email through this profile until it is saved again.")
		return nil
	}
	// Kopia decodes the port into an int, which takes a JSON integer
	// literal and nothing else: not 587.5, and not 5.87e2 either.
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return fmt.Errorf("option %s is %s, not a whole number", name, notificationJSONKind(raw))
	}
	*dst = n
	return nil
}

// notificationJSONKind names the kind of a JSON value for an error message,
// without quoting the value itself.
func notificationJSONKind(raw []byte) string {
	if len(raw) == 0 {
		return "missing"
	}
	switch raw[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	return "a number"
}

// ParseWebhookHeaders reads a webhook's headers option the way Kopia's webhook
// sender applies it (notification/sender/webhook/webhook_sender.go:37-42):
// each "\n"-separated line is trimmed, split at its first colon, and the
// value trimmed again; a line without a colon is ignored. The key is not
// trimmed — "X-A : v" yields the key "X-A ", which Go refuses to send — and
// keeps its letter case, which Go canonicalises only when it sends. A later
// line overrides an earlier one with the same key. nil means no headers.
func ParseWebhookHeaders(headers string) map[string]string {
	var parsed map[string]string
	for _, line := range strings.Split(headers, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		if parsed == nil {
			parsed = map[string]string{}
		}
		parsed[key] = strings.TrimSpace(value)
	}
	return parsed
}

// FormatWebhookHeaders writes headers in the form of the webhook's headers
// option: one "Key: Value" line per header, sorted by key, joined by "\n" —
// what the CLI writes for repeated --http-header flags. ParseWebhookHeaders
// reads the result back unchanged for any key that is an HTTP token and any
// value without line breaks or surrounding whitespace.
func FormatWebhookHeaders(headers map[string]string) string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+": "+headers[k])
	}
	return strings.Join(lines, "\n")
}
