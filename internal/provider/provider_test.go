package provider

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
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

// envAccRestart, set to 1, lets acceptance tests restart the test server with
// scripts/bootstrap-test-kopia.sh --restart. Set it only when that script
// started the server, and give go test the KOPIA_TEST_* settings it was
// started with: the script restarts only a server started from the same image
// in the same mode. Run these tests on their own, as CI does, because a
// restart fails whatever reaches the server meanwhile, and `go test ./...`
// runs the packages' tests at the same time:
//
//	KOPIA_ACC_RESTART=1 TF_ACC=1 go test ./internal/provider -run '^TestAcc.*_restart$' -count=1
const envAccRestart = "KOPIA_ACC_RESTART"

// testAccPreCheckRestart skips a test that restarts the server unless
// KOPIA_ACC_RESTART is 1.
func testAccPreCheckRestart(t *testing.T) {
	t.Helper()
	if os.Getenv(envAccRestart) != "1" {
		t.Skipf("set %s=1 to run acceptance tests that restart the test server", envAccRestart)
	}
}

// testAccRestartServer restarts the test server with
// scripts/bootstrap-test-kopia.sh --restart, which keeps the repository and
// its config file, and takes over the variables the script exports. With TLS,
// Kopia generates a new certificate whenever it starts, so the fingerprint
// changes; the new values replace the old ones for the rest of the process,
// not for this test alone, because the old ones no longer match the server.
func testAccRestartServer(t *testing.T) {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("bash", "../../scripts/bootstrap-test-kopia.sh", "--restart")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	t.Logf("scripts/bootstrap-test-kopia.sh --restart:\n%s", stderr.String())
	if err != nil {
		t.Fatalf("restarting the test server: %v", err)
	}

	// The script prints `export NAME=value` and `unset NAME...` lines, quoted
	// with printf %q, which leaves the values it exports as they are.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if names, ok := strings.CutPrefix(line, "unset "); ok {
			for _, name := range strings.Fields(names) {
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		assignment, isExport := strings.CutPrefix(line, "export ")
		name, value, ok := strings.Cut(assignment, "=")
		if !isExport || !ok {
			t.Fatalf("unexpected line from scripts/bootstrap-test-kopia.sh: %q", line)
		}
		if strings.ContainsAny(value, "\\'\"$ ") {
			// The value itself is left out: it may be a password.
			t.Fatalf("scripts/bootstrap-test-kopia.sh exported %s quoted, which this test cannot read", name)
		}
		if err := os.Setenv(name, value); err != nil {
			t.Fatal(err)
		}
	}
}

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
