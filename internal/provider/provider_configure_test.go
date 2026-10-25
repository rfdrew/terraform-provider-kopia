package provider

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// clearKopiaEnv unsets every variable the provider reads, so that a developer
// shell exported for the acceptance tests cannot leak into these tests. An
// empty variable counts as unset.
func clearKopiaEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{envURL, envUsername, envPassword, envCertFingerprint, envCAFile, envInsecureSkipVerify, envRequestTimeout} {
		t.Setenv(name, "")
	}
}

// providerConfigValue builds a provider configuration; attributes attrs
// leaves out are null.
func providerConfigValue(t *testing.T, attrs map[string]any) tfsdk.Config {
	t.Helper()
	var schemaResp provider.SchemaResponse
	(&kopiaProvider{}).Schema(context.Background(), provider.SchemaRequest{}, &schemaResp)
	requireNoErrors(t, schemaResp.Diagnostics)
	return tfsdk.Config{
		Schema: schemaResp.Schema,
		Raw:    tfValue(t, schemaResp.Schema.Type().TerraformType(context.Background()), attrs),
	}
}

// configure runs the real Configure against fake.
func configure(t *testing.T, fake *fakeKopia, attrs map[string]any) *provider.ConfigureResponse {
	t.Helper()
	p := &kopiaProvider{version: "test", transport: fake}
	resp := &provider.ConfigureResponse{}
	p.Configure(context.Background(), provider.ConfigureRequest{Config: providerConfigValue(t, attrs)}, resp)
	return resp
}

// fakeCredentials is a configuration that signs in to the fake.
func fakeCredentials() map[string]any {
	return map[string]any{"url": fakeURL, "username": fakeUsername, "password": fakePassword}
}

func TestProviderSchemaIsValid(t *testing.T) {
	var resp provider.SchemaResponse
	(&kopiaProvider{}).Schema(context.Background(), provider.SchemaRequest{}, &resp)
	requireNoErrors(t, resp.Diagnostics)
	requireNoErrors(t, resp.Schema.ValidateImplementation(context.Background()))
	if !resp.Schema.Attributes["password"].IsSensitive() {
		t.Error("password is not sensitive")
	}
}

// The acceptance factories serve the same provider the binary does.
func TestAcceptanceFactoryServesTheProvider(t *testing.T) {
	server, err := testAccProtoV6ProviderFactories["kopia"]()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireNoProtocolErrors(t, resp.Diagnostics)
	if resp.Provider == nil || len(resp.Provider.Block.Attributes) != 7 {
		t.Fatalf("provider schema = %+v, want the seven connection settings", resp.Provider)
	}
}

// Every kind of provider-level object gets the client: one that is left out
// fails on every use, and nothing short of a real Terraform run would notice.
func TestConfigureHandsTheClientToEveryObjectKind(t *testing.T) {
	clearKopiaEnv(t)
	resp := configure(t, newFakeKopia(), fakeCredentials())
	requireNoErrors(t, resp.Diagnostics)
	requireNoWarnings(t, resp.Diagnostics)
	for name, data := range map[string]any{
		"DataSourceData":        resp.DataSourceData,
		"ResourceData":          resp.ResourceData,
		"EphemeralResourceData": resp.EphemeralResourceData,
		"ListResourceData":      resp.ListResourceData,
		"ActionData":            resp.ActionData,
	} {
		if _, ok := data.(*kopiaclient.Client); !ok {
			t.Errorf("Configure set %s to %T, want the client", name, data)
		}
	}
}

// Configure starts the session and proves it works with one API call, so a
// broken setup fails once, here, rather than in every resource.
func TestConfigureStartsTheSession(t *testing.T) {
	clearKopiaEnv(t)
	fake := newFakeKopia()
	requireNoErrors(t, configure(t, fake, fakeCredentials()).Diagnostics)

	got := strings.Join(fake.routes(), ", ")
	if want := "GET /, GET /api/v1/repo/status"; got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
	if ua := fake.lastCall(t, "GET /api/v1/repo/status").header.Get("User-Agent"); ua != "terraform-provider-kopia/test" {
		t.Errorf("User-Agent = %q, want terraform-provider-kopia/test", ua)
	}
}

func TestConfigureReadsTheEnvironment(t *testing.T) {
	clearKopiaEnv(t)
	t.Setenv(envURL, fakeURL)
	t.Setenv(envUsername, fakeUsername)
	t.Setenv(envPassword, fakePassword)
	t.Setenv(envRequestTimeout, "45s")

	resp := configure(t, newFakeKopia(), map[string]any{})
	requireNoErrors(t, resp.Diagnostics)
	if got := resp.ResourceData.(*kopiaclient.Client).BaseURL(); got != fakeURL {
		t.Fatalf("connected to %s, want %s from %s", got, fakeURL, envURL)
	}
}

// Explicit configuration wins over the environment.
func TestConfigureConfigurationWinsOverTheEnvironment(t *testing.T) {
	clearKopiaEnv(t)
	t.Setenv(envURL, "https://elsewhere.example.com")
	t.Setenv(envUsername, "someone-else")
	t.Setenv(envPassword, "wrong")

	resp := configure(t, newFakeKopia(), fakeCredentials())
	requireNoErrors(t, resp.Diagnostics)
	if got := resp.ResourceData.(*kopiaclient.Client).BaseURL(); got != fakeURL {
		t.Fatalf("connected to %s, want %s from the configuration", got, fakeURL)
	}
}

// Kopia's own default UI user is "kopia".
func TestConfigureDefaultsTheUsernameToKopia(t *testing.T) {
	clearKopiaEnv(t)
	fake := newFakeKopia()
	fake.username = "kopia"

	resp := configure(t, fake, map[string]any{"url": fakeURL, "password": fakePassword})
	requireNoErrors(t, resp.Diagnostics)
	req := &http.Request{Header: fake.lastCall(t, "GET /").header}
	if user, _, _ := req.BasicAuth(); user != "kopia" {
		t.Fatalf("signed in as %q, want kopia", user)
	}
}

func TestConfigureRequiresTheURLAndPassword(t *testing.T) {
	clearKopiaEnv(t)
	fake := newFakeKopia()
	resp := configure(t, fake, map[string]any{})
	requireAttributeError(t, resp.Diagnostics, "url")
	requireAttributeError(t, resp.Diagnostics, "password")
	requireErrorContaining(t, resp.Diagnostics, envURL)
	requireErrorContaining(t, resp.Diagnostics, envPassword)
	if len(fake.routes()) != 0 {
		t.Fatalf("Configure contacted the server without a configuration: %v", fake.routes())
	}
}

// A value only known after apply cannot be used to connect, and falling back
// to the environment instead would connect somewhere else.
func TestConfigureRejectsUnknownValues(t *testing.T) {
	clearKopiaEnv(t)
	t.Setenv(envURL, fakeURL)
	attrs := fakeCredentials()
	attrs["url"] = tfUnknown

	resp := configure(t, newFakeKopia(), attrs)
	requireAttributeError(t, resp.Diagnostics, "url")
	requireErrorContaining(t, resp.Diagnostics, "Unknown Kopia provider setting")
}

func TestConfigureValidatesTheTimeout(t *testing.T) {
	for _, value := range []string{"0s", "-5s", "30", "soon"} {
		t.Run("attribute "+value, func(t *testing.T) {
			clearKopiaEnv(t)
			attrs := fakeCredentials()
			attrs["request_timeout"] = value
			resp := configure(t, newFakeKopia(), attrs)
			requireAttributeError(t, resp.Diagnostics, "request_timeout")
			requireErrorContaining(t, resp.Diagnostics, "`request_timeout` must be a positive duration")
		})
		t.Run("environment "+value, func(t *testing.T) {
			clearKopiaEnv(t)
			t.Setenv(envRequestTimeout, value)
			resp := configure(t, newFakeKopia(), fakeCredentials())
			requireErrorContaining(t, resp.Diagnostics, envRequestTimeout+" must be a positive duration")
		})
	}
}

func TestConfigureValidatesInsecureSkipVerifyFromTheEnvironment(t *testing.T) {
	clearKopiaEnv(t)
	t.Setenv(envInsecureSkipVerify, "maybe")
	resp := configure(t, newFakeKopia(), fakeCredentials())
	requireErrorContaining(t, resp.Diagnostics, "Invalid "+envInsecureSkipVerify)
}

func TestConfigureValidatesTheFingerprint(t *testing.T) {
	clearKopiaEnv(t)
	attrs := fakeCredentials()
	attrs["url"] = "https://kopia.test"
	attrs["tls_cert_fingerprint"] = "abc"
	resp := configure(t, newFakeKopia(), attrs)
	requireAttributeError(t, resp.Diagnostics, "tls_cert_fingerprint")

	clearKopiaEnv(t)
	t.Setenv(envCertFingerprint, strings.Repeat("ab", 31))
	attrs = fakeCredentials()
	attrs["url"] = "https://kopia.test"
	resp = configure(t, newFakeKopia(), attrs)
	requireErrorContaining(t, resp.Diagnostics, envCertFingerprint+" must be the SHA-256 fingerprint")
}

// OpenSSL's colon-separated upper-case form is accepted and normalised.
func TestConfigureAcceptsAnOpenSSLFingerprint(t *testing.T) {
	clearKopiaEnv(t)
	attrs := fakeCredentials()
	attrs["url"] = "https://kopia.test"
	attrs["tls_cert_fingerprint"] = strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	requireNoErrors(t, configure(t, newFakeKopia(), attrs).Diagnostics)
}

// The three ways of trusting the certificate exclude each other, whether they
// come from the configuration or from the environment.
func TestConfigureRejectsConflictingTLSSettings(t *testing.T) {
	fingerprint := strings.Repeat("ab", 32)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		attrs map[string]any
		env   map[string]string
		want  string
	}{
		{
			name:  "fingerprint and insecure",
			attrs: map[string]any{"tls_cert_fingerprint": fingerprint, "insecure_skip_verify": true},
			want:  "Set: `tls_cert_fingerprint`, `insecure_skip_verify`.",
		},
		{
			name:  "fingerprint and CA file",
			attrs: map[string]any{"tls_cert_fingerprint": fingerprint, "ca_cert_file": caFile},
			want:  "Set: `tls_cert_fingerprint`, `ca_cert_file`.",
		},
		{
			name:  "configuration and environment",
			attrs: map[string]any{"ca_cert_file": caFile},
			env:   map[string]string{envInsecureSkipVerify: "true"},
			want:  "Set: `ca_cert_file`, " + envInsecureSkipVerify + ".",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearKopiaEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			attrs := fakeCredentials()
			attrs["url"] = "https://kopia.test"
			for k, v := range tc.attrs {
				attrs[k] = v
			}
			fake := newFakeKopia()
			resp := configure(t, fake, attrs)
			requireErrorContaining(t, resp.Diagnostics, "Conflicting TLS settings")
			requireErrorContaining(t, resp.Diagnostics, tc.want)
			if len(fake.routes()) != 0 {
				t.Fatalf("Configure contacted the server: %v", fake.routes())
			}
		})
	}

	// insecure_skip_verify = false trusts nothing extra, so it conflicts with
	// nothing.
	clearKopiaEnv(t)
	attrs := fakeCredentials()
	attrs["url"] = "https://kopia.test"
	attrs["tls_cert_fingerprint"] = fingerprint
	attrs["insecure_skip_verify"] = false
	requireNoErrors(t, configure(t, newFakeKopia(), attrs).Diagnostics)
}

// A fingerprint would do nothing over plain HTTP.
func TestConfigureRejectsAFingerprintForAnHTTPURL(t *testing.T) {
	clearKopiaEnv(t)
	attrs := fakeCredentials()
	attrs["tls_cert_fingerprint"] = strings.Repeat("ab", 32)
	resp := configure(t, newFakeKopia(), attrs)
	requireErrorContaining(t, resp.Diagnostics, "Invalid Kopia provider configuration")
	requireErrorContaining(t, resp.Diagnostics, "only applies to an https:// URL")
}

func TestConfigureReportsAnUnreadableCAFile(t *testing.T) {
	clearKopiaEnv(t)
	attrs := fakeCredentials()
	attrs["url"] = "https://kopia.test"
	attrs["ca_cert_file"] = filepath.Join(t.TempDir(), "missing.pem")
	resp := configure(t, newFakeKopia(), attrs)
	requireErrorContaining(t, resp.Diagnostics, "reading the CA certificate file")
}

// Each way the session bootstrap fails points at a different mistake, and the
// error says which.
func TestConfigureExplainsConnectionFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeKopia)
		want  string
	}{
		{"wrong password", func(f *fakeKopia) { f.password = "rotated" }, "Kopia rejected the username or password (HTTP 401)"},
		{"not the UI user", func(f *fakeKopia) { f.uiUser = "kopia" }, `"tfacc" is not the server's UI user (HTTP 403)`},
		{"--no-ui", func(f *fakeKopia) { f.noUI = true }, "Either the server was started with --no-ui"},
		{"not kopia", func(f *fakeKopia) { f.omitToken = true }, "so it is not a Kopia server"},
		{"CSRF stripped", func(f *fakeKopia) { f.rejectCSRF = true }, "a reverse proxy in front of the server that drops either causes this"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearKopiaEnv(t)
			fake := newFakeKopia()
			tc.setup(fake)
			resp := configure(t, fake, fakeCredentials())
			requireErrorContaining(t, resp.Diagnostics, tc.want)
			requireNoErrorContaining(t, resp.Diagnostics, fakePassword)
			if resp.ResourceData != nil || resp.DataSourceData != nil {
				t.Error("Configure handed out a client despite the error")
			}
		})
	}
}

// Kopia's web UI API is unversioned, so releases outside the supported lines
// get a warning; a version the page does not reveal gets none.
func TestConfigureWarnsAboutUntestedVersions(t *testing.T) {
	tests := map[string]bool{
		"KopiaUI v0.23.1":       false,
		"KopiaUI v0.23.0":       false,
		"KopiaUI v0.22.3":       false,
		"KopiaUI vv0.22.0":      false,
		"KopiaUI v0.21.1":       true,
		"KopiaUI v0.24.0-rc1":   true,
		"KopiaUI v1.0.0":        true,
		"KopiaUI v0-unofficial": false,
		"Kopia UI":              false,
	}
	for title, wantWarning := range tests {
		t.Run(title, func(t *testing.T) {
			clearKopiaEnv(t)
			fake := newFakeKopia()
			fake.title = title
			resp := configure(t, fake, fakeCredentials())
			requireNoErrors(t, resp.Diagnostics)
			warned := false
			for _, d := range resp.Diagnostics.Warnings() {
				if strings.Contains(d.Summary(), "Untested Kopia version") {
					warned = true
				}
			}
			if warned != wantWarning {
				t.Errorf("title %q: warning = %v, want %v (%v)", title, warned, wantWarning, resp.Diagnostics)
			}
		})
	}
}

func TestConfigureWarnsWhenNoRepositoryIsConnected(t *testing.T) {
	clearKopiaEnv(t)
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, "{\n  \"connected\": false,\n  \"supportsContentCompression\": false,\n  \"hostname\": \"\",\n  \"username\": \"\",\n  \"enableActions\": false\n}\n")
	resp := configure(t, fake, fakeCredentials())
	requireNoErrors(t, resp.Diagnostics)
	requireWarningContaining(t, resp.Diagnostics, "Kopia server has no repository connected")
	if resp.ResourceData == nil {
		t.Error("Configure withheld the client; data sources that need no repository should still work")
	}
}

func TestClientFromProviderData(t *testing.T) {
	var diags testDiagnostics
	if c := clientFromProviderData(nil, &diags); c != nil || diags.errors != 0 {
		t.Fatalf("nil provider data gave %v with %d errors, want nil and none", c, diags.errors)
	}
	client := newFakeKopia().client(t)
	if c := clientFromProviderData(client, &diags); c != client || diags.errors != 0 {
		t.Fatalf("client provider data gave %v with %d errors, want the client", c, diags.errors)
	}
	if c := clientFromProviderData("something else", &diags); c != nil || diags.errors != 1 {
		t.Fatalf("unexpected provider data gave %v with %d errors, want nil and one error", c, diags.errors)
	}
}

// testDiagnostics counts errors, for clientFromProviderData.
type testDiagnostics struct{ errors int }

func (d *testDiagnostics) AddError(_, _ string) { d.errors++ }
