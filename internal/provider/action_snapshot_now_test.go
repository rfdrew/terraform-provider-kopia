package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// These tests invoke kopia_snapshot_now through the protocol server against a
// fake POST /api/v1/sources/upload that behaves as Kopia's does: it queues a
// snapshot of every source that matches the filters and that the server
// snapshots itself, and answers 404 NOT_FOUND when there is none
// (internal/server/api_snapshots.go:172-249).

const snapshotNowActionType = "kopia_snapshot_now"

// snapshotNowFakeSources serves POST /api/v1/sources/upload and GET
// /api/v1/sources for a server whose own host is "kopia", knowing sources
// with the given statuses.
func snapshotNowFakeSources(fake *fakeKopia, sources map[kopiaclient.SourceInfo]string) {
	matching := func(r *http.Request) []kopiaclient.SourceInfo {
		q := r.URL.Query()
		var matched []kopiaclient.SourceInfo
		for s := range sources {
			if (q.Get("host") == "" || q.Get("host") == s.Host) && (q.Get("userName") == "" || q.Get("userName") == s.UserName) &&
				(q.Get("path") == "" || q.Get("path") == s.Path) {
				matched = append(matched, s)
			}
		}
		return matched
	}
	fake.handle("POST /api/v1/sources/upload", func(w http.ResponseWriter, r *http.Request) {
		queued := map[string]any{}
		for _, s := range matching(r) {
			// Kopia skips the managers that run read-only: remote sources.
			if sources[s] != "REMOTE" {
				queued[s.String()] = map[string]any{"success": true}
			}
		}
		if len(queued) == 0 {
			writeFakeResponse(w, http.StatusNotFound, fakeErrorBody(kopiaclient.CodeNotFound, "no source matching the provided filters"))
			return
		}
		writeFakeJSON(w, map[string]any{"sources": queued})
	})
	fake.handle("GET /api/v1/sources", func(w http.ResponseWriter, r *http.Request) {
		listed := []any{}
		for _, s := range matching(r) {
			listed = append(listed, map[string]any{
				"source":   map[string]any{"host": s.Host, "userName": s.UserName, "path": s.Path},
				"status":   sources[s],
				"schedule": map[string]any{},
			})
		}
		writeFakeJSON(w, map[string]any{"localUsername": "root", "localHost": "kopia", "multiUser": true, "sources": listed})
	})
}

// invokeSnapshotNow invokes kopia_snapshot_now for target.
func invokeSnapshotNow(t *testing.T, h *protocolHarness, target string) ([]string, []*tfprotov6.Diagnostic) {
	t.Helper()
	return h.invokeAction(t, snapshotNowActionType, h.actionValue(t, snapshotNowActionType, map[string]any{"target": target}))
}

// The source travels as host, userName and path, a Windows path with its
// backslashes, and the progress message names the source Kopia queued.
func TestSnapshotNowActionQueuesTheSource(t *testing.T) {
	for _, target := range []string{"root@kopia:/data/app", `Administrator@kopia:C:\Data`} {
		fake := newFakeKopia()
		source, _ := kopiaclient.ParseTarget(target)
		snapshotNowFakeSources(fake, map[kopiaclient.SourceInfo]string{
			source: "IDLE",
			{Host: "kopia", UserName: "root", Path: "/data"}: "IDLE",
		})
		h := newProtocolHarness(t, fake)

		progress, diags := invokeSnapshotNow(t, h, target)
		requireNoProtocolErrors(t, diags)
		q := fake.query(t, "POST /api/v1/sources/upload")
		if q.Get("host") != source.Host || q.Get("userName") != source.UserName || q.Get("path") != source.Path || len(q) != 3 {
			t.Errorf("%s: query = %s", target, q.Encode())
		}
		if len(progress) != 1 || !strings.Contains(progress[0], "Kopia queued a snapshot of "+target) {
			t.Errorf("%s: progress = %q", target, progress)
		}
		if fake.called("GET /api/v1/sources") {
			t.Errorf("%s: looked the source up although Kopia queued it", target)
		}
	}
}

// Kopia reads a missing user or path as "any", so the targets of kopia_policy
// that are not a single source are refused at validation, and again at
// invocation for a value that was unknown then, before anything is sent.
func TestSnapshotNowActionRefusesMoreThanOneSource(t *testing.T) {
	tests := map[string]string{
		"(global)":          "every source the server snapshots",
		"@kopia":            "every source of host kopia",
		"root@kopia":        "every source of user root on host kopia",
		"root@kopia:data":   "must be absolute",
		"@kopia:/data/app":  "needs a user name",
		"root@kopia:/data/": "ends with a slash",
	}
	h := newProtocolHarness(t, newFakeKopia())
	for target, want := range tests {
		diags := h.validateAction(t, snapshotNowActionType, h.actionValue(t, snapshotNowActionType, map[string]any{"target": target}))
		requireProtocolErrorContaining(t, diags, "Invalid snapshot source")
		requireProtocolErrorContaining(t, diags, want)

		fake := newFakeKopia()
		resp := invokeActionDirectly(t, &snapshotNowAction{client: fake.client(t)}, map[string]any{"target": target})
		requireErrorContaining(t, resp.Diagnostics, want)
		requireAttributeError(t, resp.Diagnostics, "target")
		if len(fake.apiRoutes()) != 0 {
			t.Errorf("%s: calls = %v, want none", target, fake.apiRoutes())
		}
	}
	requireNoProtocolErrors(t, h.validateAction(t, snapshotNowActionType,
		h.actionValue(t, snapshotNowActionType, map[string]any{"target": tfUnknown})))
}

// Kopia's 404 has three causes; the server's list of sources tells which one
// applies, and the error says so before listing all three.
func TestSnapshotNowActionExplainsAMissingSource(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		sources map[kopiaclient.SourceInfo]string
		want    string
	}{
		{
			name:   "unknown local source",
			target: "root@kopia:/data/app",
			want:   "Kopia does not know the source: it has no snapshot of it and no policy",
		},
		{
			name:   "unknown source of another host",
			target: "root@laptop:/home",
			want:   "its host, laptop, is not the server's own, kopia, so the server would not snapshot it",
		},
		{
			name:    "remote source",
			target:  "root@laptop:/home",
			sources: map[kopiaclient.SourceInfo]string{{Host: "laptop", UserName: "root", Path: "/home"}: "REMOTE"},
			want:    "lists it as REMOTE: its host, laptop, is not the server's own, kopia",
		},
		{
			name:    "read-only connection",
			target:  "root@kopia:/data/app",
			sources: map[kopiaclient.SourceInfo]string{{Host: "kopia", UserName: "root", Path: "/data/app"}: "REMOTE"},
			want:    "connection to the repository is read-only",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			snapshotNowFakeSources(fake, tc.sources)
			h := newProtocolHarness(t, fake)

			_, diags := invokeSnapshotNow(t, h, tc.target)
			requireProtocolErrorContaining(t, diags, "Kopia snapshot source not found")
			requireProtocolErrorContaining(t, diags, tc.want)
			requireProtocolErrorContaining(t, diags, `Create a kopia_policy for "`+tc.target+`"`)
			requireProtocolErrorContaining(t, diags, "no source matching the provided filters")
			requireNotificationAttributeError(t, diags, "target")
		})
	}
}

// A source Kopia lists by the time it is looked up was most likely added just
// after the request, so trying again is the advice.
func TestSnapshotNowActionExplainsASourceAddedMeanwhile(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("POST /api/v1/sources/upload", http.StatusNotFound, kopiaclient.CodeNotFound, "no source matching the provided filters")
	fake.reply("GET /api/v1/sources", http.StatusOK, `{"localUsername":"root","localHost":"kopia","multiUser":true,"sources":[`+
		`{"source":{"host":"kopia","userName":"root","path":"/data/app"},"status":"IDLE","schedule":{}}]}`)
	h := newProtocolHarness(t, fake)

	_, diags := invokeSnapshotNow(t, h, "root@kopia:/data/app")
	requireProtocolErrorContaining(t, diags, "Kopia now lists the source as IDLE")
}

// When the lookup fails too, the error still lists the possible causes.
func TestSnapshotNowActionExplainsAMissingSourceWithoutTheLookup(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("POST /api/v1/sources/upload", http.StatusNotFound, kopiaclient.CodeNotFound, "no source matching the provided filters")
	fake.replyError("GET /api/v1/sources", http.StatusInternalServerError, kopiaclient.CodeInternal, "internal server error")
	h := newProtocolHarness(t, fake)

	_, diags := invokeSnapshotNow(t, h, "root@kopia:/data/app")
	// No finding comes between the opening sentence and the causes.
	requireProtocolErrorContaining(t, diags, "no snapshot was started. The server snapshots only")
	requireProtocolErrorContaining(t, diags, "one of these applies")
	requireProtocolErrorContaining(t, diags, "read-only")
}

func TestSnapshotNowActionReportsOtherFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		want   string
	}{
		{"no repository", http.StatusBadRequest, kopiaclient.CodeNotConnected, "no repository connected"},
		{"server error", http.StatusInternalServerError, kopiaclient.CodeInternal, "Asking Kopia for a snapshot of root@kopia:/data/app failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.replyError("POST /api/v1/sources/upload", tc.status, tc.code, "boom")
			h := newProtocolHarness(t, fake)

			_, diags := invokeSnapshotNow(t, h, "root@kopia:/data/app")
			requireProtocolErrorContaining(t, diags, "Unable to start Kopia snapshot")
			requireProtocolErrorContaining(t, diags, tc.want)
		})
	}
}

// Kopia serves a request the provider has stopped waiting for, so a request
// that timed out, or that an interrupted Terraform cancelled, may have queued
// the snapshot all the same: the error says the outcome is unknown, rather
// than that nothing was started, and the source is not looked up.
func TestSnapshotNowActionReportsAnUnansweredRequest(t *testing.T) {
	tests := map[string]struct {
		timeout time.Duration
		fail    func(*http.Request) error
		want    string
	}{
		"timed out":   {200 * time.Millisecond, awaitRequestTimeout, "deadline exceeded"},
		"interrupted": {0, func(*http.Request) error { return context.Canceled }, "context canceled"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeKopia()
			source := kopiaclient.SourceInfo{Host: "kopia", UserName: "root", Path: "/data/app"}
			snapshotNowFakeSources(fake, map[kopiaclient.SourceInfo]string{source: "IDLE"})
			client := fake.clientLosingAnswer(t, "POST /api/v1/sources/upload", tc.timeout, tc.fail)
			h := newProtocolHarness(t, fake, withClient(client))

			progress, diags := invokeSnapshotNow(t, h, source.String())
			requireProtocolErrorContaining(t, diags, "Kopia did not answer the snapshot request")
			requireProtocolErrorContaining(t, diags, tc.want)
			requireProtocolErrorContaining(t, diags, "the outcome is unknown")
			requireProtocolErrorContaining(t, diags, "kopia_sources data source before invoking the action again")
			if len(progress) != 0 {
				t.Errorf("progress = %q, want none", progress)
			}
			if !fake.called("POST /api/v1/sources/upload") {
				t.Error("the request never reached Kopia")
			}
			if fake.called("GET /api/v1/sources") {
				t.Error("looked the source up, although Kopia did not refuse it")
			}
		})
	}
}

// Kopia's upload always reports success, but a failure it might report is
// not taken for a queued snapshot.
func TestSnapshotNowActionReportsARefusedSource(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("POST /api/v1/sources/upload", http.StatusOK, `{"sources":{"root@kopia:/data/app":{"success":false}}}`)
	h := newProtocolHarness(t, fake)

	progress, diags := invokeSnapshotNow(t, h, "root@kopia:/data/app")
	requireProtocolErrorContaining(t, diags, "Kopia did not queue the snapshot")
	if len(progress) != 0 {
		t.Errorf("progress = %q, want none", progress)
	}
}

func TestSnapshotNowActionWithoutAConfiguredProvider(t *testing.T) {
	resp := invokeActionDirectly(t, NewSnapshotNowAction(), map[string]any{"target": "root@kopia:/data/app"})
	requireErrorContaining(t, resp.Diagnostics, "Unconfigured Kopia provider")
}

// --- acceptance ---------------------------------------------------------------

// testAccCheckSourceSnapshotted waits until the server reports a finished
// snapshot of target. The action returns as soon as Kopia has queued the
// snapshot, so the check polls GET /api/v1/sources, which shows a source's
// last snapshot once it is done.
func testAccCheckSourceSnapshotted(target string) acc.TestCheckFunc {
	return func(*terraform.State) error {
		c, err := testAccClient()
		if err != nil {
			return err
		}
		source, err := kopiaclient.ParseTarget(target)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(2 * time.Minute)
		for {
			resp, err := c.ListSources(context.Background(), kopiaclient.SourcesFilter{
				Host: source.Host, UserName: source.UserName, Path: source.Path,
			})
			if err != nil {
				return fmt.Errorf("listing the sources of %s: %w", target, err)
			}
			for _, s := range resp.Sources {
				if s.Source == source && s.LastSnapshot != nil {
					return nil
				}
			}
			if time.Now().After(deadline) {
				encoded, _ := json.Marshal(resp.Sources)
				return fmt.Errorf("no snapshot of %s was taken within two minutes; sources: %s", target, encoded)
			}
			time.Sleep(time.Second)
		}
	}
}

// TestAccSnapshotNowAction snapshots a directory that exists in the test
// server's container, /app/config, which holds its repository.config. Its
// path policy makes the server know the source, and with manual scheduling
// the server takes no snapshot of it on its own, so the one that appears is
// the action's.
func TestAccSnapshotNowAction(t *testing.T) {
	host := os.Getenv(envAccSourceHost)
	if host == "" {
		host = "kopia-acc"
	}
	user := os.Getenv(envAccSourceUser)
	if user == "" {
		user = "root"
	}
	target := user + "@" + host + ":/app/config"
	config := providerConfig + fmt.Sprintf(`
resource "kopia_policy" "source" {
  target = %[1]q

  scheduling = {
    manual = true
  }

  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.kopia_snapshot_now.source]
    }
  }
}

action "kopia_snapshot_now" "source" {
  config {
    target = kopia_policy.source.target
  }
}
`, target)

	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			// Actions arrived in Terraform 1.14.
			tfversion.SkipBelow(tfversion.Version1_14_0),
		},
		CheckDestroy: testAccCheckPolicyStored(target, ""),
		Steps: []acc.TestStep{
			{
				// Creating the policy makes the server know the source; the
				// action, triggered right after, has it snapshot the path.
				Config: config,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("kopia_policy.source", "scheduling.manual", "true"),
					testAccCheckSourceSnapshotted(target),
				),
			},
			{
				// A user@host target would snapshot every path of the user, so
				// it is refused, whether Terraform validates the action while
				// planning or only when it invokes it before the update.
				Config: strings.Replace(config, `
  lifecycle {`, `
  retention = {
    keep_latest = 3
  }

  lifecycle {
    action_trigger {
      events  = [before_update]
      actions = [action.kopia_snapshot_now.whole_user]
    }
`, 1) + fmt.Sprintf(`
action "kopia_snapshot_now" "whole_user" {
  config {
    target = %q
  }
}
`, user+"@"+host),
				ExpectError: regexp.MustCompile(`is\s+not\s+a\s+single\s+snapshot\s+source`),
			},
			{
				// Back to a configuration Terraform accepts, which the destroy at
				// the end of the test validates, and which changes nothing: the
				// refused action left the policy as it was.
				Config: config,
				ConfigPlanChecks: acc.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}
