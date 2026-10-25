// Package provider implements the Terraform provider for Kopia.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// Ensure the implementation satisfies the framework interfaces.
var _ provider.Provider = &kopiaProvider{}

// Address is the provider's source address in the Terraform Registry.
const Address = "registry.terraform.io/rfdrew/kopia"

// testedKopiaVersion names the Kopia release this provider is built and
// verified against, and supportedKopiaLines the release lines it supports.
// Configure warns when the server reports a release outside them.
const testedKopiaVersion = "0.23.1"

var supportedKopiaLines = []struct{ major, minor int }{{0, 22}, {0, 23}}

// The environment variables the provider reads. They are the names Kopia's
// own CLI uses for the same settings, so a shell set up for `kopia server`
// commands needs nothing new — with one catch: those commands read
// KOPIA_SERVER_USERNAME and KOPIA_SERVER_PASSWORD as the server-control
// credentials, while the provider needs the UI user's.
const (
	envURL                = "KOPIA_SERVER_URL"
	envUsername           = "KOPIA_SERVER_USERNAME"
	envPassword           = "KOPIA_SERVER_PASSWORD"
	envCertFingerprint    = "KOPIA_SERVER_CERT_FINGERPRINT"
	envCAFile             = "KOPIA_SERVER_CA_FILE"
	envInsecureSkipVerify = "KOPIA_SERVER_INSECURE_SKIP_VERIFY"
	envRequestTimeout     = "KOPIA_SERVER_REQUEST_TIMEOUT"
)

type kopiaProvider struct {
	// version is set at build time and reported to Terraform.
	version string

	// transport replaces the HTTP transport of the configured client. It is
	// nil outside tests, which use it to answer requests in process.
	transport http.RoundTripper
}

// New returns a provider factory for the given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &kopiaProvider{version: version}
	}
}

type providerModel struct {
	URL                types.String `tfsdk:"url"`
	Username           types.String `tfsdk:"username"`
	Password           types.String `tfsdk:"password"`
	TLSCertFingerprint types.String `tfsdk:"tls_cert_fingerprint"`
	CACertFile         types.String `tfsdk:"ca_cert_file"`
	InsecureSkipVerify types.Bool   `tfsdk:"insecure_skip_verify"`
	RequestTimeout     types.String `tfsdk:"request_timeout"`
}

func (p *kopiaProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "kopia"
	resp.Version = p.version
}

func (p *kopiaProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the configuration of a running [Kopia](https://kopia.io) server — snapshot " +
			"policies, notification profiles, throttling and the repository description — through the HTTP API " +
			"behind its web UI. Built and verified against Kopia " + testedKopiaVersion + "; the 0.22 and 0.23 " +
			"releases are supported.\n\n" +
			"The provider signs in as the server's UI user: the one `kopia server start` names with " +
			"`--server-username` (default `kopia`) and `--server-password`. The server-control user and repository " +
			"users cannot use this API, and a server started with `--no-ui` does not serve it at all. Most " +
			"resources and data sources also need the server to have a repository connected.",
		Attributes: map[string]schema.Attribute{
			"url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Root URL of the Kopia server, e.g. `https://kopia.example.com:51515`: the " +
					"address `kopia server start --address` listens on, or the one a reverse proxy serves it " +
					"under. Required, here or in the environment. May also be set with the `" + envURL + "` " +
					"environment variable.",
				Validators: []validator.String{httpURLValidator()},
			},
			"username": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Name of the server's UI user, as given to `kopia server start " +
					"--server-username`. Defaults to `kopia`, Kopia's own default. May also be set with the `" +
					envUsername + "` environment variable.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"password": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Password of the UI user, as given to `kopia server start " +
					"--server-password`. Required, here or in the environment. May also be set with the `" +
					envPassword + "` environment variable.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"tls_cert_fingerprint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "SHA-256 fingerprint of the server's TLS certificate, to trust that one " +
					"certificate instead of verifying it against certificate authorities; the host name is then " +
					"not checked. A server started with `--tls-generate-cert` prints it as `SERVER CERT SHA256: …`, " +
					"and `openssl x509 -noout -fingerprint -sha256 -in cert.pem` prints it for any certificate: 64 " +
					"hexadecimal digits in either case, optionally separated by colons. Conflicts with " +
					"`ca_cert_file` and `insecure_skip_verify`. May also be set with the `" + envCertFingerprint +
					"` environment variable.",
				Validators: []validator.String{certFingerprintValidator()},
			},
			"ca_cert_file": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Path to a PEM file of CA certificates to verify the server's certificate " +
					"with, instead of the system's trusted roots. A self-signed certificate can serve as its own " +
					"CA; the host name in `url` must then be one of the names in the certificate. Conflicts with " +
					"`tls_cert_fingerprint` and `insecure_skip_verify`. May also be set with the `" + envCAFile +
					"` environment variable.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"insecure_skip_verify": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Skip verification of the server's TLS certificate. Defaults to `false`. " +
					"Prefer `tls_cert_fingerprint`, which trusts exactly one certificate. Conflicts with " +
					"`tls_cert_fingerprint` and `ca_cert_file`. May also be set with the `" +
					envInsecureSkipVerify + "` environment variable.",
			},
			"request_timeout": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Time limit for each HTTP request to the server, as a Go duration such as " +
					"`30s` or `2m`. Defaults to `30s`. Kopia finishes a request even when the provider has " +
					"stopped waiting for it, so a write that timed out may still have taken effect. May also be " +
					"set with the `" + envRequestTimeout + "` environment variable.",
				Validators: []validator.String{positiveDurationValidator()},
			},
		},
	}
}

// providerSettings is the configuration after the environment has filled in
// what the provider block leaves out.
type providerSettings struct {
	url                string
	username           string
	password           string
	certFingerprint    string
	caFile             string
	insecureSkipVerify bool
	timeout            time.Duration
}

// settingSource names where a setting came from, for error messages: the
// attribute, or the environment variable that stood in for it.
func settingSource(configured attr.Value, attribute, env string) string {
	if configured.IsNull() {
		return env
	}
	return "`" + attribute + "`"
}

// addSettingError reports an invalid setting, pinned to its attribute when the
// value came from the configuration rather than the environment.
func addSettingError(diags *diag.Diagnostics, configured attr.Value, attribute, summary, detail string) {
	if configured.IsNull() {
		diags.AddError(summary, detail)
		return
	}
	diags.AddAttributeError(path.Root(attribute), summary, detail)
}

// stringSetting is the configured value of a string attribute or, when the
// configuration leaves it out, the environment variable's. An empty variable
// counts as unset.
func stringSetting(configured types.String, env string) string {
	if !configured.IsNull() {
		return configured.ValueString()
	}
	return os.Getenv(env)
}

// settings merges the configuration with the environment and validates the
// result. Explicit configuration wins over the environment.
func (m providerModel) settings(diags *diag.Diagnostics) providerSettings {
	s := providerSettings{
		url:             stringSetting(m.URL, envURL),
		username:        stringSetting(m.Username, envUsername),
		password:        stringSetting(m.Password, envPassword),
		certFingerprint: stringSetting(m.TLSCertFingerprint, envCertFingerprint),
		caFile:          stringSetting(m.CACertFile, envCAFile),
		timeout:         kopiaclient.DefaultTimeout,
	}
	if s.username == "" {
		s.username = kopiaclient.DefaultUsername
	}

	if !m.InsecureSkipVerify.IsNull() {
		s.insecureSkipVerify = m.InsecureSkipVerify.ValueBool()
	} else if env := os.Getenv(envInsecureSkipVerify); env != "" {
		parsed, err := strconv.ParseBool(env)
		if err != nil {
			diags.AddError(
				"Invalid "+envInsecureSkipVerify,
				"Expected a boolean such as \"true\" or \"false\", got "+strconv.Quote(env)+".",
			)
		}
		s.insecureSkipVerify = parsed
	}

	if timeout := stringSetting(m.RequestTimeout, envRequestTimeout); timeout != "" {
		parsed, err := parsePositiveDuration(timeout)
		if err != nil {
			addSettingError(diags, m.RequestTimeout, "request_timeout", "Invalid request timeout",
				settingSource(m.RequestTimeout, "request_timeout", envRequestTimeout)+" "+err.Error()+".")
		}
		s.timeout = parsed
	}

	if s.url == "" {
		diags.AddAttributeError(
			path.Root("url"),
			"Missing Kopia server URL",
			"Set the provider's `url` attribute or the "+envURL+" environment variable to the root URL of the "+
				"Kopia server, e.g. https://kopia.example.com:51515.",
		)
	}
	if s.password == "" {
		diags.AddAttributeError(
			path.Root("password"),
			"Missing Kopia server password",
			"Set the provider's `password` attribute or the "+envPassword+" environment variable to the "+
				"password of the server's UI user, the one `kopia server start` takes as --server-password.",
		)
	}

	if s.certFingerprint != "" {
		normalised, err := kopiaclient.NormaliseFingerprint(s.certFingerprint)
		if err != nil {
			addSettingError(diags, m.TLSCertFingerprint, "tls_cert_fingerprint", "Invalid TLS certificate fingerprint",
				settingSource(m.TLSCertFingerprint, "tls_cert_fingerprint", envCertFingerprint)+" must be the "+
					"SHA-256 fingerprint of a certificate: 64 hexadecimal digits, optionally separated by colons.")
		}
		s.certFingerprint = normalised
	}

	var tlsSettings []string
	if s.certFingerprint != "" {
		tlsSettings = append(tlsSettings, settingSource(m.TLSCertFingerprint, "tls_cert_fingerprint", envCertFingerprint))
	}
	if s.caFile != "" {
		tlsSettings = append(tlsSettings, settingSource(m.CACertFile, "ca_cert_file", envCAFile))
	}
	if s.insecureSkipVerify {
		tlsSettings = append(tlsSettings, settingSource(m.InsecureSkipVerify, "insecure_skip_verify", envInsecureSkipVerify))
	}
	if len(tlsSettings) > 1 {
		diags.AddError(
			"Conflicting TLS settings",
			"tls_cert_fingerprint, ca_cert_file and insecure_skip_verify each decide on their own how the "+
				"server's certificate is trusted, so only one of them may be set, counting the "+
				envCertFingerprint+", "+envCAFile+" and "+envInsecureSkipVerify+" environment variables. "+
				"Set: "+strings.Join(tlsSettings, ", ")+".",
		)
	}

	return s
}

func (p *kopiaProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// A value that is only known after apply cannot be used to connect, and
	// silently falling back to the environment would connect somewhere else.
	for _, setting := range []struct {
		value     attr.Value
		attribute string
		env       string
	}{
		{config.URL, "url", envURL},
		{config.Username, "username", envUsername},
		{config.Password, "password", envPassword},
		{config.TLSCertFingerprint, "tls_cert_fingerprint", envCertFingerprint},
		{config.CACertFile, "ca_cert_file", envCAFile},
		{config.InsecureSkipVerify, "insecure_skip_verify", envInsecureSkipVerify},
		{config.RequestTimeout, "request_timeout", envRequestTimeout},
	} {
		if setting.value.IsUnknown() {
			resp.Diagnostics.AddAttributeError(
				path.Root(setting.attribute),
				"Unknown Kopia provider setting",
				"The provider cannot connect to Kopia while `"+setting.attribute+"` is unknown, as it is when "+
					"it refers to something that is only known after apply. Set it to a value known at plan "+
					"time, use the "+setting.env+" environment variable instead, or apply what it depends on "+
					"first with -target.",
			)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	settings := config.settings(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	kopia, err := kopiaclient.New(kopiaclient.Options{
		URL:                settings.url,
		Username:           settings.username,
		Password:           settings.password,
		CertFingerprint:    settings.certFingerprint,
		CAFile:             settings.caFile,
		InsecureSkipVerify: settings.insecureSkipVerify,
		Timeout:            settings.timeout,
		UserAgent:          "terraform-provider-kopia/" + p.version,
		Transport:          p.transport,
	})
	if err != nil {
		resp.Diagnostics.AddError("Invalid Kopia provider configuration", err.Error())
		return
	}

	// Starting the session is a GET / that every later request needs anyway,
	// so it doubles as the connectivity and credentials check: failing here
	// produces one clear error instead of the same failure repeated across
	// every resource.
	version, err := kopia.ServerVersion(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to connect to Kopia", connectErrorDetail(kopia, err))
		return
	}
	tflog.Info(ctx, "connected to Kopia", map[string]any{"url": kopia.BaseURL(), "version": version})

	// Kopia's web UI API is internal and unversioned, so a release outside
	// the supported lines may have changed it. An unknown version is left
	// alone: builds without version information report none.
	if major, minor, ok := kopiaclient.MajorMinor(version); ok && !isSupportedKopia(major, minor) {
		resp.Diagnostics.AddWarning(
			"Untested Kopia version",
			"The Kopia server at "+kopia.BaseURL()+" reports version "+version+". This provider is built and "+
				"verified against Kopia "+testedKopiaVersion+" and supports the 0.22 and 0.23 releases; Kopia's "+
				"web UI API is internal and unversioned, so with another release some resources may fail or "+
				"behave differently.",
		)
	}

	// GET /api/v1/repo/status is the first call that needs the CSRF token, so
	// it also proves the session works end to end.
	connected, err := kopia.RepositoryConnected(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Kopia repository status", repositoryStatusErrorDetail(kopia, err))
		return
	}
	if !connected {
		resp.Diagnostics.AddWarning(
			"Kopia server has no repository connected",
			"The Kopia server at "+kopia.BaseURL()+" answers, but has no repository connected, so every "+
				"resource and data source that needs one fails with NOT_CONNECTED. Connect the server to its "+
				"repository — `kopia repository connect` before `kopia server start`, or the Kopia UI — and "+
				"run Terraform again.",
		)
	}

	resp.DataSourceData = kopia
	resp.ResourceData = kopia
	resp.EphemeralResourceData = kopia
	resp.ListResourceData = kopia
	resp.ActionData = kopia
}

// isSupportedKopia reports whether major.minor is a supported release line.
func isSupportedKopia(major, minor int) bool {
	for _, line := range supportedKopiaLines {
		if line.major == major && line.minor == minor {
			return true
		}
	}
	return false
}

// connectErrorDetail explains why the session could not be started. Each
// failure of GET / points at a different mistake (spec: auth.md §6).
func connectErrorDetail(kopia *kopiaclient.Client, err error) string {
	switch {
	case errors.Is(err, kopiaclient.ErrBadCredentials):
		return "Kopia rejected the username or password (HTTP 401). Check `username` and `password`: they must " +
			"be the UI user's, as given to `kopia server start --server-username` (default `kopia`) and " +
			"`--server-password`. " + err.Error()
	case errors.Is(err, kopiaclient.ErrNotUIUser):
		return "Kopia accepted the password, but " + strconv.Quote(kopia.Username()) + " is not the server's UI " +
			"user (HTTP 403). Only the user `kopia server start` names with --server-username (default " +
			"`kopia`) may use the web UI's API; the server-control user and repository users cannot. " + err.Error()
	case errors.Is(err, kopiaclient.ErrUINotServed):
		return "GET " + kopia.BaseURL() + "/ answered 404. Either the server was started with --no-ui, which " +
			"also removes the API this provider uses, or `url` is not the root address of a Kopia server. " +
			err.Error()
	case errors.Is(err, kopiaclient.ErrNotKopiaServer):
		return "The server at " + kopia.BaseURL() + " answered GET / without the CSRF token every Kopia server " +
			"puts in its web UI page, so it is not a Kopia server, or it serves a custom --html directory whose " +
			"index.html has no </head>. " + err.Error()
	}
	return fmt.Sprintf("Could not start a session with the Kopia server at %s: %s", kopia.BaseURL(), err)
}

// repositoryStatusErrorDetail explains a failure of the first API call, after
// GET / succeeded.
func repositoryStatusErrorDetail(kopia *kopiaclient.Client, err error) string {
	switch {
	case kopiaclient.IsCSRFRejected(err):
		return "Kopia issued a CSRF token but rejected it on GET /api/v1/repo/status, even after issuing a " +
			"fresh one. The token is bound to the Kopia-Session-Cookie cookie and travels in the " +
			"X-Kopia-Csrf-Token header; a reverse proxy in front of the server that drops either causes this. " +
			err.Error()
	case kopiaclient.IsAccessDenied(err):
		return strconv.Quote(kopia.Username()) + " is not the server's UI user (HTTP 403). Only the user " +
			"`kopia server start` names with --server-username (default `kopia`) may use the web UI's API. " +
			err.Error()
	}
	return fmt.Sprintf("GET %s/api/v1/repo/status failed: %s", kopia.BaseURL(), err)
}

func (p *kopiaProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewPolicyResource,
		NewNotificationProfileResource,
		NewThrottleResource,
		NewRepositoryDescriptionResource,
	}
}

func (p *kopiaProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewPoliciesDataSource,
		NewEffectivePolicyDataSource,
		NewNotificationProfilesDataSource,
		NewRepositoryStatusDataSource,
		NewCurrentUserDataSource,
		NewAlgorithmsDataSource,
		NewSourcesDataSource,
		NewSnapshotsDataSource,
	}
}

// clientFromProviderData pulls the configured client out of the framework's
// provider data, guarding against the nil that appears during early validation
// walks.
func clientFromProviderData(providerData any, diags interface {
	AddError(summary, detail string)
}) *kopiaclient.Client {
	if providerData == nil {
		return nil
	}
	kopia, ok := providerData.(*kopiaclient.Client)
	if !ok {
		diags.AddError(
			"Unexpected provider data",
			"The provider was configured with an unexpected internal type. This is a bug in the provider.",
		)
		return nil
	}
	return kopia
}
