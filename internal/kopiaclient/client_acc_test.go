package kopiaclient

import (
	"context"
	"os"
	"regexp"
	"testing"
)

// exactReleaseTag matches a Docker tag that names one Kopia release.
var exactReleaseTag = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// --- acceptance --------------------------------------------------------------

// TestAccClientSession runs the session bootstrap against a real kopia server.
// The unit tests reproduce Kopia from its source; this pins what the source
// could not show: the title of the UI page each release embeds, which carries
// the version, and the CSRF handshake through Docker's port proxy.
//
// It runs only with TF_ACC set, against the server `make kopia-up` starts.
func TestAccClientSession(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance tests run only with TF_ACC set")
	}
	for _, name := range []string{"KOPIA_SERVER_URL", "KOPIA_SERVER_PASSWORD"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s must be set for acceptance tests", name)
		}
	}
	username := os.Getenv("KOPIA_SERVER_USERNAME")
	if username == "" {
		username = DefaultUsername
	}

	c, err := New(Options{
		URL:             os.Getenv("KOPIA_SERVER_URL"),
		Username:        username,
		Password:        os.Getenv("KOPIA_SERVER_PASSWORD"),
		CertFingerprint: os.Getenv("KOPIA_SERVER_CERT_FINGERPRINT"),
	})
	if err != nil {
		t.Fatal(err)
	}

	version, err := c.ServerVersion(context.Background())
	if err != nil {
		t.Fatalf("ServerVersion() error = %v", err)
	}
	if _, _, ok := MajorMinor(version); !ok {
		t.Errorf("ServerVersion() = %q, want a release version from the page title", version)
	}
	// KOPIA_ACC_VERSION is the image tag. Only an exact release tag such as
	// 0.23.1 names the version; "latest" or "0.23" move.
	if want := os.Getenv("KOPIA_ACC_VERSION"); want != version && exactReleaseTag.MatchString(want) {
		t.Errorf("ServerVersion() = %q, want %q, the image under test", version, want)
	}

	connected, err := c.RepositoryConnected(context.Background())
	if err != nil {
		t.Fatalf("RepositoryConnected() error = %v", err)
	}
	if !connected {
		t.Error("RepositoryConnected() = false, want the test server's repository open")
	}
}
