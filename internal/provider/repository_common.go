package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// repositoryCallErrorDetail explains a failed API call of the repository
// resources and data sources: kopia_throttle, kopia_repository_description,
// kopia_sources, kopia_snapshots and the rest. Two of Kopia's answers are
// about the server's repository connection rather than the request, and say
// little on their own, so they get advice; anything else is reported as Kopia
// put it.
func repositoryCallErrorDetail(err error) string {
	switch {
	case kopiaclient.IsNotConnected(err):
		return "The Kopia server has no repository connected (NOT_CONNECTED). Connect the server to its " +
			"repository — `kopia repository connect` before `kopia server start`, or the Kopia UI — and run " +
			"Terraform again. A server started with --async-repo-connect answers this for a while after it " +
			"starts. " + err.Error()
	case kopiaclient.IsStorageConnection(err):
		return "The Kopia server reaches its repository through a Kopia repository server rather than " +
			"directly on its storage (STORAGE_CONNECTION), and only a server connected directly to storage has " +
			"throttle limits. Manage them on the repository server instead. " + err.Error()
	}
	return err.Error()
}

// repositoryTimeValue renders a time from Kopia as an RFC 3339 timestamp in
// UTC with as many fractional digits as it has, such as
// 2026-10-18T03:00:00.481516234Z: the form Kopia itself writes snapshot
// times in. Times Kopia writes in the server's local time zone, such as a
// source's next snapshot time, are converted, so every time the provider
// reports compares directly. The zero time, which only a field Kopia left
// out decodes to, is null.
func repositoryTimeValue(t time.Time) types.String {
	if t.IsZero() {
		return types.StringNull()
	}
	return types.StringValue(t.UTC().Format(time.RFC3339Nano))
}

// repositorySaveAgainKey names the private-state flag kopia_throttle and
// kopia_repository_description set when an update took effect on the server
// but left its config file behind: Kopia applied throttle limits it could not
// save, or the description was set but saving the throttle limits again
// failed. The next refresh reads the values in effect, which match the
// configuration, so without the flag the next plan would be empty and the
// file would stay behind until the server restarts and goes back to it.
const repositorySaveAgainKey = "save_again"

// repositoryPrivateState is the part of the framework's private state that
// repositorySetSaveAgain uses. The framework's own type is internal, so it
// cannot be named here.
type repositoryPrivateState interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// repositorySetSaveAgain sets the flag of repositorySaveAgainKey, or removes
// it after an update that saved everything. private must not be a nil
// pointer: the framework always provides private state, but methods called
// directly, as tests do, get none, so callers check first.
func repositorySetSaveAgain(ctx context.Context, private repositoryPrivateState, saveAgain bool, diags *diag.Diagnostics) {
	var value []byte
	if saveAgain {
		value = []byte("true")
	}
	diags.Append(private.SetKey(ctx, repositorySaveAgainKey, value)...)
}

// repositoryPlanSaveAgain plans an update of a resource whose last update set
// the flag of repositorySaveAgainKey, with a warning saying why. The values
// are unchanged, so the only change to plan is the id, which becomes unknown:
// the smallest change that makes Terraform call Update, which then writes the
// same values, and saves them, again.
func repositoryPlanSaveAgain(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse, summary, detail string) {
	// A create has no flag yet, and a destroy has nothing left to save.
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	flag, diags := req.Private.GetKey(ctx, repositorySaveAgainKey)
	resp.Diagnostics.Append(diags...)
	if len(flag) == 0 {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), types.StringUnknown())...)
	resp.Diagnostics.AddWarning(summary, detail)
}
