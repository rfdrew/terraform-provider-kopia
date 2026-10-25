package provider

import (
	"errors"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// testAccProtoV6ProviderFactories wires the in-process provider into the
// acceptance test framework.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"kopia": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck fails fast when the acceptance environment is incomplete.
//
// Acceptance tests talk to a real kopia server and create, change and delete
// real policies, notification profiles and settings in it, so they only run
// when TF_ACC is set. Point KOPIA_SERVER_URL and KOPIA_SERVER_PASSWORD (and
// KOPIA_SERVER_USERNAME, unless the UI user is called `kopia`) at a throwaway
// server, never a production one: `make kopia-up` starts one and prints the
// exports.
func testAccPreCheck(t *testing.T) {
	t.Helper()
	for _, name := range []string{envURL, envPassword} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s must be set for acceptance tests", name)
		}
	}
}

// providerConfig relies on the KOPIA_SERVER_* variables being present in the
// environment, so no credentials appear in the test configurations.
const providerConfig = `provider "kopia" {}` + "\n"

// scripts/bootstrap-test-kopia.sh also exports the identity the test server
// takes snapshots as, so that acceptance tests can check what depends on it.
// Both are optional: a test reading them falls back or skips the check.
const (
	envAccSourceHost = "KOPIA_ACC_SOURCE_HOST"
	envAccSourceUser = "KOPIA_ACC_SOURCE_USER"
)

// testAccClient connects to the acceptance server the way the provider does,
// from the same KOPIA_SERVER_* environment variables and through the same
// settings code, for checks the provider itself does not make: that a
// destroyed object is gone, or what the server really stored.
func testAccClient() (*kopiaclient.Client, error) {
	var diags diag.Diagnostics
	s := providerModel{}.settings(&diags)
	if diags.HasError() {
		var errs []error
		for _, d := range diags.Errors() {
			errs = append(errs, errors.New(d.Summary()+": "+d.Detail()))
		}
		return nil, errors.Join(errs...)
	}
	return kopiaclient.New(kopiaclient.Options{
		URL:                s.url,
		Username:           s.username,
		Password:           s.password,
		CertFingerprint:    s.certFingerprint,
		CAFile:             s.caFile,
		InsecureSkipVerify: s.insecureSkipVerify,
		Timeout:            s.timeout,
	})
}
