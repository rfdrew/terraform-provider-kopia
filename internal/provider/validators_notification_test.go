package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestIsNotificationAddress(t *testing.T) {
	for s, want := range map[string]bool{
		"kopia@example.com":           true,
		"postmaster":                  true,
		"o'brien+kopia@example.com":   true,
		"":                            false,
		"Kopia <kopia@example.com>":   false,
		"<kopia@example.com>":         false,
		"a@example.com,b@example.com": false,
		" kopia@example.com":          false,
		"kopia@example.com\r\nBcc: x": false,
		"kopia @example.com":          false,
	} {
		if got := isNotificationAddress(s); got != want {
			t.Errorf("isNotificationAddress(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestNotificationAddressListValidator(t *testing.T) {
	v := notificationAddressListValidator()
	for s, valid := range map[string]bool{
		"ops@example.com":                     true,
		"ops@example.com,oncall@example.com":  true,
		"ops@example.com, oncall@example.com": false,
		"ops@example.com,":                    false,
		",ops@example.com":                    false,
		"":                                    false,
	} {
		resp := &validator.StringResponse{}
		v.ValidateString(context.Background(), validator.StringRequest{Path: path.Root("to"), ConfigValue: types.StringValue(s)}, resp)
		if resp.Diagnostics.HasError() == valid {
			t.Errorf("%q: errors = %v, want valid = %v", s, resp.Diagnostics, valid)
		}
	}
}

func TestNotificationSMTPServerValidator(t *testing.T) {
	v := notificationSMTPServerValidator()
	for s, valid := range map[string]bool{
		"smtp.example.com":     true,
		"192.0.2.25":           true,
		"[2001:db8::25]":       true,
		"localhost":            true,
		"smtp.example.com:587": false,
		"2001:db8::25":         false,
		"[2001:db8::25]:587":   false,
		"[]":                   false,
		"smtp example.com":     false,
		"":                     false,
		"[[2001:db8::25]]":     false,
	} {
		resp := &validator.StringResponse{}
		v.ValidateString(context.Background(), validator.StringRequest{Path: path.Root("smtp_server"), ConfigValue: types.StringValue(s)}, resp)
		if resp.Diagnostics.HasError() == valid {
			t.Errorf("%q: errors = %v, want valid = %v", s, resp.Diagnostics, valid)
		}
	}
}

// Kopia checks only that the endpoint parses and uses http or https, which
// lets "http://" and "http:x" through to fail at send time; a host is
// required here as well.
func TestIsNotificationHTTPURL(t *testing.T) {
	for s, want := range map[string]bool{
		"https://ntfy.example.org/kopia":           true,
		"http://hooks.internal:8080/kopia?x=1&y=2": true,
		"http://[2001:db8::1]/hook":                true,
		"https://user:pass@example.com/hook":       true,
		"http://":                                  false,
		"http:x":                                   false,
		"ftp://example.com/":                       false,
		"/relative":                                false,
		"example.com/hook":                         false,
		"http://x y/":                              false,
		"":                                         false,
	} {
		if got := isNotificationHTTPURL(s); got != want {
			t.Errorf("isNotificationHTTPURL(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestIsNotificationHeaderNameAndValue(t *testing.T) {
	for s, want := range map[string]bool{
		"Authorization":   true,
		"X-Custom_1":      true,
		"x-lower":         true,
		"!#$%&'*+-.^_`|~": true,
		"":                false,
		"X A":             false,
		"X-A ":            false,
		"X-A:":            false,
		"Ünicode":         false,
		"X\tA":            false,
	} {
		if got := isNotificationHeaderName(s); got != want {
			t.Errorf("isNotificationHeaderName(%q) = %v, want %v", s, got, want)
		}
	}
	for s, want := range map[string]bool{
		"Bearer tk":     true,
		"a:b, c":        true,
		"":              true,
		"ünïcode":       true,
		" leading":      false,
		"trailing ":     false,
		"line\nbreak":   false,
		"carriage\rret": false,
		"tab\there":     false,
		"nul\x00":       false,
	} {
		if got := isNotificationHeaderValue(s); got != want {
			t.Errorf("isNotificationHeaderValue(%q) = %v, want %v", s, got, want)
		}
	}
}

// A sensitive attribute's validator says what is wrong without quoting the
// value, which Terraform hides everywhere else.
func TestNotificationSensitiveURLValidatorHidesTheValue(t *testing.T) {
	const secret = "ftp://hooks.example.com/" + notificationTestHookPath
	resp := &validator.StringResponse{}
	notificationSensitiveURLValidator().ValidateString(context.Background(), validator.StringRequest{
		Path:        path.Root("webhook").AtName("endpoint"),
		ConfigValue: types.StringValue(secret),
	}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("no error for an ftp:// endpoint")
	}
	for _, d := range resp.Diagnostics {
		if strings.Contains(d.Detail(), notificationTestHookPath) {
			t.Errorf("the error quotes the value: %s", d.Detail())
		}
		if !strings.Contains(d.Detail(), "not shown because the attribute is sensitive") {
			t.Errorf("the error does not say why the value is not shown: %s", d.Detail())
		}
	}
}
