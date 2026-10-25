package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ action.Action              = &snapshotNowAction{}
	_ action.ActionWithConfigure = &snapshotNowAction{}
)

// NewSnapshotNowAction returns the kopia_snapshot_now action.
//
// Snapshots are data, not configuration, so no resource manages them; asking
// for one is a side effect, which is what an action models. The server only
// queues the snapshot and reports no task to follow, so the action does not
// wait for it.
func NewSnapshotNowAction() action.Action { return &snapshotNowAction{} }

type snapshotNowAction struct {
	client *kopiaclient.Client
}

type snapshotNowActionModel struct {
	Target types.String `tfsdk:"target"`
}

func (a *snapshotNowAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_snapshot_now"
}

func (a *snapshotNowAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Asks the Kopia server to take a snapshot of one source now, as the **Snapshot now** " +
			"button of the Kopia UI does, through `POST /api/v1/sources/upload`.\n\n" +
			"This is a Terraform *action*, not a resource, and needs **Terraform 1.14 or newer**. It runs only " +
			"when invoked — from a resource's `lifecycle { action_trigger { ... } }` or with " +
			"`terraform apply -invoke=action.kopia_snapshot_now.<name>` — and remembers nothing afterwards.\n\n" +
			"~> **The action does not wait for the snapshot.** Kopia only queues it: the server takes it in the " +
			"background once a slot is free (`upload.max_parallel_snapshots`), and reports neither " +
			"a task nor the outcome to the request. The invocation succeeds as soon as Kopia has queued the " +
			"snapshot, and names the source it queued; the `kopia_sources` data source shows the source's " +
			"`status` and, once the snapshot is done, its `last_snapshot`.\n\n" +
			"~> **Not idempotent.** Every invocation asks for another snapshot. While a snapshot of the " +
			"source is `PENDING` (waiting for a slot) or `UPLOADING`, Kopia keeps one more request waiting " +
			"behind it: invoking the action again then queues a second snapshot, taken once the first is done, " +
			"and any further invocation before that is merged with the second. A source whose policy sets " +
			"`scheduling.manual` is snapshotted all the same; one paused through the server's control API " +
			"(`status` `PAUSED`) drops the request, although Kopia reports it queued.\n\n" +
			"When the provider's `request_timeout` expires, or Terraform is interrupted, before Kopia answers, " +
			"the outcome is unknown: Kopia carries on with a request it has received, and may queue the " +
			"snapshot all the same. Check the source in `kopia_sources` before invoking the action again.\n\n" +
			"The server snapshots only the sources it knows and takes snapshots of itself, and refuses the " +
			"others with an error that says which of these applies:\n\n" +
			"* **Kopia must know the source.** It knows the sources it has a snapshot of, those with a policy " +
			"for exactly that `user@host:path` — a `kopia_policy`, even one that sets nothing, is enough — and " +
			"those added in the Kopia UI. To snapshot a new path, create its `kopia_policy` and trigger the " +
			"action from it on `after_create`.\n" +
			"* **The source must be local.** The server snapshots only the sources of its own host, the " +
			"`local_host` of `kopia_sources`, and none at all when its connection to the repository is " +
			"read-only. Kopia lists the others as `REMOTE`: the Kopia client on their own host snapshots them.\n" +
			"* **The source must match exactly**: user, host and path as Kopia writes them, letter case included.",
		Attributes: map[string]schema.Attribute{
			"target": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The source to snapshot, written as Kopia writes it: `user@host:path` with an " +
					"absolute path, such as `root@kopia1:/data/app`, `Administrator@WIN1:C:\\Data` or " +
					"`Administrator@WIN1:\\\\server\\share` — the `target` of a path's `kopia_policy`, or of an " +
					"entry of the `kopia_sources` data source. `(global)`, `@host` and `user@host` are refused: " +
					"Kopia reads a missing host, user or path in this request as \"any\", and would start a " +
					"snapshot of every source that matches.",
				Validators: []validator.String{snapshotSourceValidator{}},
			},
		},
	}
}

func (a *snapshotNowAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// snapshotSourceValidator accepts the targets of kopia_snapshot_now: a target
// kopiaclient.ParseTarget accepts that names a path, and so a user and a host
// too.
type snapshotSourceValidator struct{}

func (snapshotSourceValidator) Description(_ context.Context) string {
	return "must be USER@HOST:PATH with an absolute PATH"
}

func (v snapshotSourceValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (snapshotSourceValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, problem := parseSnapshotSource(req.ConfigValue.ValueString()); problem != "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid snapshot source", problem)
	}
}

// parseSnapshotSource parses the target of kopia_snapshot_now, or explains
// why it is not a single snapshot source. Kopia's upload handler matches
// every source when a parameter is empty (internal/server/api_snapshots.go:
// 235-249), so the global, host and user targets kopia_policy takes would
// each start a snapshot of many sources at once.
func parseSnapshotSource(text string) (kopiaclient.SourceInfo, string) {
	source, err := kopiaclient.ParseTarget(text)
	if err != nil {
		return kopiaclient.SourceInfo{}, "A snapshot source is `user@host:path` with an absolute path, such as " +
			"`root@kopia1:/data/app`, `Administrator@WIN1:C:\\Data` or `Administrator@WIN1:\\\\server\\share`, " +
			"written exactly as Kopia writes it. " + err.Error() + "."
	}
	if source.Path != "" {
		return source, ""
	}
	var scope, example string
	switch {
	case source.IsGlobal():
		scope, example = "every source the server snapshots", "root@kopia1:/data/app"
	case source.UserName == "":
		scope, example = "every source of host "+source.Host, "root@"+source.Host+":/data/app"
	default:
		scope, example = "every source of user "+source.UserName+" on host "+source.Host, source.String()+":/data/app"
	}
	return kopiaclient.SourceInfo{}, fmt.Sprintf("%q is not a single snapshot source: Kopia reads a missing host, "+
		"user or path in this request as \"any\", so it would start a snapshot of %s. kopia_snapshot_now takes "+
		"exactly one source, user@host:path, such as %q; use one action for each source.", text, scope, example)
}

// Invoke queues the snapshot and returns at once, as Kopia does: the
// snapshot runs in the background on the server, without a task id the
// action could wait on.
func (a *snapshotNowAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var config snapshotNowActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if a.client == nil {
		addUnconfiguredActionError(&resp.Diagnostics)
		return
	}

	// The validator has checked a target known at validation time; one that
	// only became known since is checked here, before anything is sent.
	source, problem := parseSnapshotSource(config.Target.ValueString())
	if problem != "" {
		resp.Diagnostics.AddAttributeError(path.Root("target"), "Invalid snapshot source", problem)
		return
	}

	queued, err := a.client.StartSnapshot(ctx, source)
	if err != nil {
		a.addStartError(ctx, source, err, &resp.Diagnostics)
		return
	}

	names := make([]string, 0, len(queued))
	for name := range queued {
		names = append(names, name)
	}
	slices.Sort(names)
	var refused []string
	for _, name := range names {
		if !queued[name] {
			refused = append(refused, strconv.Quote(name))
			continue
		}
		sendActionProgress(resp, "Kopia queued a snapshot of "+name+"; the server takes it in the background, "+
			"and the kopia_sources data source shows its progress")
	}
	if len(refused) > 0 {
		resp.Diagnostics.AddError(
			"Kopia did not queue the snapshot",
			"Kopia answered that it could not queue a snapshot of "+strings.Join(refused, ", ")+", without "+
				"saying why.",
		)
		return
	}
	tflog.Info(ctx, "Kopia queued a snapshot", map[string]any{"target": source.String(), "sources": names})
}

// addStartError explains a failed POST /api/v1/sources/upload. Kopia answers
// 404 NOT_FOUND when no source it snapshots itself matched, for any of three
// reasons; the server's list of sources usually tells which, so it is read
// to say so. A request the provider gave up on may have been served all the
// same, so that error says the outcome is unknown rather than that nothing
// was started.
func (a *snapshotNowAction) addStartError(ctx context.Context, source kopiaclient.SourceInfo, err error, diags *diag.Diagnostics) {
	switch {
	case kopiaclient.IsNotFound(err):
		detail := "Kopia has no source " + source.String() + " that this server takes snapshots of, so no snapshot " +
			"was started. "
		if finding := a.diagnoseMissingSource(ctx, source); finding != "" {
			detail += finding + "\n\n"
		}
		detail += "The server snapshots only the sources it knows and takes snapshots of itself, so one of these " +
			"applies:\n\n" +
			"- Kopia does not know the source: it has no snapshot of it and no policy for exactly this target. " +
			"Create a kopia_policy for " + strconv.Quote(source.String()) + " — one that sets nothing is enough — " +
			"and trigger the action from it on after_create.\n" +
			"- The source belongs to a host other than the server's own (local_host of the kopia_sources data " +
			"source). Kopia lists it as REMOTE, and only the Kopia client on that host snapshots it.\n" +
			"- The server's connection to the repository is read-only, which makes every source remote.\n\n" +
			"The user, host and path must match exactly, letter case included. " + err.Error()
		diags.AddAttributeError(path.Root("target"), "Kopia snapshot source not found", detail)
	case kopiaclient.IsNotConnected(err):
		diags.AddError("Unable to start Kopia snapshot",
			"The Kopia server has no repository connected, so it cannot take snapshots. "+err.Error())
	case requestTimedOut(err) || errors.Is(err, context.Canceled):
		// Kopia serves a request on a context the client cannot cancel
		// (internal/server/server.go:412), and its upload handler never looks
		// at it, so a request that was waiting for the server, behind a
		// refresh holding its lock for instance, is still served once the
		// provider has stopped waiting for the answer.
		diags.AddError("Kopia did not answer the snapshot request",
			"Asking Kopia for a snapshot of "+source.String()+" got no answer: "+err.Error()+"\n\n"+
				"The provider stopped waiting, at its request_timeout or because Terraform was interrupted, but "+
				"Kopia carries on with a request it has received, so the outcome is unknown: Kopia may have "+
				"queued the snapshot, or may still queue it. Check the source's status and last_snapshot in the "+
				"kopia_sources data source before invoking the action again, as another invocation asks for "+
				"another snapshot.")
	default:
		diags.AddError("Unable to start Kopia snapshot",
			"Asking Kopia for a snapshot of "+source.String()+" failed. "+err.Error())
	}
}

// diagnoseMissingSource looks source up in GET /api/v1/sources after Kopia
// refused to snapshot it, and says why that was, or returns "" when the
// lookup fails.
func (a *snapshotNowAction) diagnoseMissingSource(ctx context.Context, source kopiaclient.SourceInfo) string {
	sources, err := a.client.ListSources(ctx, kopiaclient.SourcesFilter(source))
	if err != nil {
		tflog.Debug(ctx, "Could not look the snapshot source up", map[string]any{"target": source.String(), "error": err.Error()})
		return ""
	}
	otherHost := source.Host != sources.LocalHost
	for _, s := range sources.Sources {
		if s.Source != source {
			continue
		}
		switch {
		case s.Status == "REMOTE" && otherHost:
			return "Kopia knows the source, but lists it as REMOTE: its host, " + source.Host + ", is not the " +
				"server's own, " + sources.LocalHost + "."
		case s.Status == "REMOTE":
			return "Kopia knows the source, and its host is the server's own, but lists it as REMOTE, which " +
				"means the server's connection to the repository is read-only."
		default:
			return "Kopia now lists the source as " + s.Status + ": it may have learnt of it just after the " +
				"request. Invoking the action again should succeed."
		}
	}
	if otherHost {
		return "Kopia does not know the source, and its host, " + source.Host + ", is not the server's own, " +
			sources.LocalHost + ", so the server would not snapshot it even once it knows it."
	}
	return "Kopia does not know the source: it has no snapshot of it and no policy for exactly this target."
}
