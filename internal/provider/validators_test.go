package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// validateString runs a string validator on value and reports whether it
// passed, failing the test when a failure is not pinned to the attribute.
func validateString(t *testing.T, v validator.String, value types.String) bool {
	t.Helper()
	req := validator.StringRequest{Path: path.Root("attr"), ConfigValue: value}
	var resp validator.StringResponse
	v.ValidateString(context.Background(), req, &resp)
	if resp.Diagnostics.HasError() {
		requireAttributeError(t, resp.Diagnostics, "attr")
		return false
	}
	return true
}

func TestStringValidators(t *testing.T) {
	tests := []struct {
		name     string
		v        validator.String
		accepted []string
		rejected []string
	}{
		{
			name:     "httpURLValidator",
			v:        httpURLValidator(),
			accepted: []string{"http://127.0.0.1:51515", "https://kopia.example.com", "https://proxy.example.com/kopia/"},
			rejected: []string{"", "kopia.example.com", "ftp://kopia.example.com", "https://", "https://u:p@kopia.example.com", "https://kopia.example.com/?q=1", "https://kopia.example.com/#top"},
		},
		{
			name:     "certFingerprintValidator",
			v:        certFingerprintValidator(),
			accepted: []string{strings.Repeat("ab", 32), strings.Repeat("AB", 32), strings.TrimSuffix(strings.Repeat("AB:", 32), ":")},
			rejected: []string{"", "abc", strings.Repeat("g", 64), "SHA256:" + strings.Repeat("ab", 32)},
		},
		{
			name:     "positiveDurationValidator",
			v:        positiveDurationValidator(),
			accepted: []string{"30s", "2m", "1m30s", "500ms"},
			rejected: []string{"", "0s", "-1s", "30", "soon"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, value := range tc.accepted {
				if !validateString(t, tc.v, types.StringValue(value)) {
					t.Errorf("rejected %q", value)
				}
			}
			for _, value := range tc.rejected {
				if validateString(t, tc.v, types.StringValue(value)) {
					t.Errorf("accepted %q", value)
				}
			}
			// Null and unknown values are not validated.
			if !validateString(t, tc.v, types.StringNull()) || !validateString(t, tc.v, types.StringUnknown()) {
				t.Error("rejected a null or unknown value")
			}
			if tc.v.Description(context.Background()) == "" || tc.v.MarkdownDescription(context.Background()) == "" {
				t.Error("no description")
			}
		})
	}
}

func TestStringCheckErrorWording(t *testing.T) {
	req := validator.StringRequest{Path: path.Root("request_timeout"), ConfigValue: types.StringValue("soon")}
	var resp validator.StringResponse
	positiveDurationValidator().ValidateString(context.Background(), req, &resp)
	want := `Attribute request_timeout must be a positive duration such as "30s" or "2m", got: "soon"`
	requireErrorContaining(t, resp.Diagnostics, want)
}

func TestParsePositiveDuration(t *testing.T) {
	if d, err := parsePositiveDuration("1m30s"); err != nil || d != 90*time.Second {
		t.Fatalf("parsePositiveDuration(1m30s) = %v, %v", d, err)
	}
	if _, err := parsePositiveDuration("0s"); err == nil || !strings.Contains(err.Error(), `got "0s"`) {
		t.Fatalf("parsePositiveDuration(0s) error = %v", err)
	}
}
