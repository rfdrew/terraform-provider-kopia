package provider

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ resource.Resource                   = &policyResource{}
	_ resource.ResourceWithConfigure      = &policyResource{}
	_ resource.ResourceWithIdentity       = &policyResource{}
	_ resource.ResourceWithImportState    = &policyResource{}
	_ resource.ResourceWithModifyPlan     = &policyResource{}
	_ resource.ResourceWithValidateConfig = &policyResource{}
)

// NewPolicyResource returns the kopia_policy resource.
func NewPolicyResource() resource.Resource { return &policyResource{} }

type policyResource struct {
	client *kopiaclient.Client
}

type policyResourceModel struct {
	ID     types.String `tfsdk:"id"`
	Target types.String `tfsdk:"target"`
	policyBodyModel
}

func (r *policyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_policy"
}

// The policy's attributes are optional and not computed: the resource owns
// the whole policy of its target, as Kopia's PUT replaces it as a whole, and
// an attribute left out is not set by this policy, so it is inherited.
func (r *policyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attributes := policyResourceAttributes(policyBodyAttributes())
	attributes["id"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Same value as `target`, present because Terraform requires an `id`.",
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.UseStateForUnknown(),
		},
	}
	attributes["target"] = schema.StringAttribute{
		Required: true,
		MarkdownDescription: "What the policy applies to, written exactly as Kopia writes it: `(global)`; `@host` " +
			"for every user of a host; `user@host`; or `user@host:path` for a directory and everything below " +
			"it, such as `docker@host1:/data/app`, `Administrator@WIN1:C:\\Data` or " +
			"`Administrator@WIN1:\\\\server\\share`. Host and user are the ones Kopia records snapshot sources " +
			"under, as the `kopia_sources` data source lists them. Kopia matches policies to sources by exact " +
			"text, so a path must be absolute, without a trailing separator or `.` and `..` components, and a " +
			"Windows path must use backslashes. Changing it replaces the resource.",
		Validators: []validator.String{policyTargetValidator{}},
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.RequiresReplace(),
		},
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the snapshot policy of one target on a Kopia server: the global policy, " +
			"the policy of a host, of a user on a host, or of a directory of a user on a host.\n\n" +
			"The resource owns the whole policy of its target. Kopia replaces a policy as a whole on every " +
			"write, so every apply stores exactly what the configuration says, and whatever is set outside " +
			"Terraform — in the Kopia UI, with `kopia policy set` or by another tool — shows up as a difference " +
			"on the next plan and is overwritten by the next apply. An attribute that is left out, or `null`, " +
			"is not set by this policy and is inherited: from the policies of the parent directories, then " +
			"`user@host`, then `@host`, then `(global)`, and finally from Kopia's built-in defaults. The " +
			"`kopia_effective_policy` data source shows the outcome.\n\n" +
			"At plan time the provider asks Kopia whether it accepts the `scheduling` section, which is how " +
			"an invalid cron entry is found before the apply, and checks compressor and splitter names " +
			"against those the server supports.\n\n" +
			"~> **`false`, `0` and empty values.** Most of Kopia's settings can be left unset apart from " +
			"`false` or `0`, and for those an explicit `false` or `0` overrides what is inherited. Some cannot, " +
			"and the attribute descriptions say which: the `no_parent*` flags and `scheduling.manual` cannot " +
			"store `false`, which is accepted but means the same as leaving them out, while `0`, `\"\"` and " +
			"empty lists are refused where Kopia cannot store them. Where Kopia has another way to override " +
			"an inherited value, the description names it, such as `-1` for no size limit or `none` for no " +
			"compression. After an import, a configured `false` of that kind shows once as an update that " +
			"changes nothing on the server.\n\n" +
			"~> **Creating refuses an existing policy.** Except for `(global)`, which always exists and is " +
			"adopted, creating the resource fails when the target already has a policy, so that one set up in " +
			"the Kopia UI or by another tool is not overwritten unseen. Import it to manage it here.\n\n" +
			"~> **Destroying `(global)` resets it.** Destroying the resource deletes the policy of its target, " +
			"which then inherits everything. The global policy is not deleted, which would remove it for good, " +
			"but reset to the one `kopia repository create` stores, whose values are Kopia's built-in " +
			"defaults.\n\n" +
			"**Working alongside kopia-docker-watcher.** The watcher keeps the path policies of the containers " +
			"it backs up, such as `docker@host1:/…`, in line with their labels. Manage `(global)` and the " +
			"host-level policies (`@host1`, `docker@host1`) here, and leave the watcher's path policies to it: " +
			"a path policy described in both places would be overwritten by each in turn. What the path " +
			"policies inherit from here works as follows:\n\n" +
			"* `docker@host1` is a user policy, not a host policy, and every path policy of `docker@host1` " +
			"inherits from it before `@host1` and `(global)`.\n" +
			"* Its `scheduling.time_of_day` adds to the times of every path policy below it, unless that sets " +
			"`no_parent_time_of_day`; its `scheduling.cron` is replaced by a path policy's own `cron`; and a " +
			"path policy's `interval_seconds` does not stop an inherited `cron`: both apply.\n" +
			"* Its `scheduling.manual = true` pauses every path policy below it, and no path policy can undo " +
			"that.\n" +
			"* `upload.max_parallel_snapshots` is read only from the effective policy of the Kopia server's own " +
			"`user@host`, so it matters only there, on its `@host` and on `(global)`.",
		Attributes: attributes,
	}
}

func (r *policyResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = stringIdentitySchema("target", "Target of the policy, such as `(global)` or `docker@host1`.")
}

func (r *policyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// ValidateConfig checks what depends on the target: Kopia refuses
// max_parallel_snapshots on a path policy (snapshot/policy/upload_policy.go:31-37).
// The scheduling rules are the scheduling attribute's own validator.
func (r *policyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var target types.String
	var upload types.Object
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("target"), &target)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("upload"), &upload)...)
	if resp.Diagnostics.HasError() || target.IsNull() || target.IsUnknown() {
		return
	}
	maxParallel, ok := policyValuesOf(upload)["max_parallel_snapshots"]
	if !ok || maxParallel.IsNull() || maxParallel.IsUnknown() {
		return
	}
	si, err := kopiaclient.ParseTarget(target.ValueString())
	if err != nil || si.Path == "" {
		return
	}
	resp.Diagnostics.AddAttributeError(
		path.Root("upload").AtName("max_parallel_snapshots"),
		"Setting not allowed on a path policy",
		"Kopia refuses upload.max_parallel_snapshots on a path policy such as "+si.String()+": it reads the "+
			"number of parallel snapshots only from the policies of the server's own user@host and above. "+
			"Set it on (global), @host or user@host instead.",
	)
}

// ModifyPlan plans `id`, which is the target, and asks Kopia what only Kopia
// can answer: whether the compressor and splitter names exist, and whether it
// accepts the scheduling section, whose cron entries it parses with a library
// the provider does not have.
//
// The checks need the client, which early validation walks do not have, and
// a fully known policy. A plan that changes nothing is not checked again,
// except for lz4 on Kopia 0.23, which is only a warning then: the policy was
// accepted when it was applied, and failing every later plan would block
// unrelated changes.
func (r *policyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}

	var target types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("target"), &target)...)
	if resp.Diagnostics.HasError() || target.IsNull() || target.IsUnknown() {
		return
	}
	si, err := kopiaclient.ParseTarget(target.ValueString())
	if err != nil {
		// The attribute's validator has already reported it.
		return
	}
	// The id is the target, so it is known before the apply, even on create.
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), si.String())...)

	if r.client == nil || !resp.Plan.Raw.IsFullyKnown() {
		return
	}
	var plan policyResourceModel
	resp.Diagnostics.Append(resp.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	unchanged := !req.State.Raw.IsNull() && resp.Plan.Raw.Equal(req.State.Raw)
	r.checkAlgorithms(ctx, plan.policyBodyModel, unchanged, &resp.Diagnostics)
	if unchanged {
		return
	}
	r.checkScheduling(ctx, si, plan.policyBodyModel, &resp.Diagnostics)
}

// policyAlgorithmUse is a compressor or splitter name a policy sets.
type policyAlgorithmUse struct {
	path       path.Path
	name       string
	compressor bool
}

// policyAlgorithmUses lists the algorithm names m sets.
func policyAlgorithmUses(m policyBodyModel) []policyAlgorithmUse {
	var uses []policyAlgorithmUse
	if name := policyValuesOf(m.Compression).string("compressor_name"); name != "" {
		uses = append(uses, policyAlgorithmUse{path.Root("compression").AtName("compressor_name"), name, true})
	}
	if name := policyValuesOf(m.MetadataCompression).string("compressor_name"); name != "" {
		uses = append(uses, policyAlgorithmUse{path.Root("metadata_compression").AtName("compressor_name"), name, true})
	}
	if name := policyValuesOf(m.Splitter).string("algorithm"); name != "" {
		uses = append(uses, policyAlgorithmUse{path.Root("splitter").AtName("algorithm"), name, false})
	}
	return uses
}

// policyLZ4 is the compressor Kopia 0.23 still lists, as deprecated, but can
// no longer use: compressing with it fails (repo/compression/compressor_lz4.go
// in 0.23.1), and with it every snapshot.
const policyLZ4 = "lz4"

// checkAlgorithms checks the compressor and splitter names m sets against GET
// /api/v1/repo/algorithms. Kopia accepts any name and silently compresses
// nothing, or uses the repository's splitter, for one it does not know. The
// compressors listed there do not include "none", which a policy may still
// name, and the list cannot say that lz4, listed as deprecated everywhere, no
// longer works from Kopia 0.23 on.
func (r *policyResource) checkAlgorithms(ctx context.Context, m policyBodyModel, unchanged bool, diags *diag.Diagnostics) {
	uses := policyAlgorithmUses(m)
	if len(uses) == 0 {
		return
	}

	// The session already holds the version; this makes no request.
	version, _ := r.client.ServerVersion(ctx)
	server := "This Kopia server"
	if version != "" {
		server = "Kopia " + version
	}
	lz4Reported := false
	for _, use := range uses {
		if !use.compressor || use.name != policyLZ4 {
			continue
		}
		atLeast, known := kopiaclient.VersionAtLeast(version, 0, 23)
		summary := "Compressor no longer supported"
		detail := server + " cannot compress with lz4: Kopia 0.23 dropped support for it, although it still " +
			"lists it, and every snapshot that compresses with it fails. Choose another compressor, such as " +
			"zstd or s2-default."
		switch {
		case known && atLeast && !unchanged:
			diags.AddAttributeError(use.path, summary, detail)
		case known && atLeast:
			diags.AddAttributeWarning(use.path, summary, detail)
		case !known && !unchanged:
			diags.AddAttributeWarning(use.path, "Compressor deprecated",
				"Kopia deprecated lz4, and dropped support for it in 0.23, after which every snapshot that "+
					"compresses with it fails. The server does not report its version, so the provider "+
					"cannot tell whether it is affected. Prefer another compressor, such as zstd or s2-default.")
		default:
			continue
		}
		lz4Reported = true
	}
	if unchanged {
		return
	}

	algorithms, err := r.client.Algorithms(ctx)
	if err != nil {
		diags.AddWarning(
			"Unable to check algorithm names",
			"Reading the algorithms the Kopia server supports failed, so the compressor and splitter names "+
				"of this policy were not checked. Kopia itself accepts any name and compresses nothing, or "+
				"uses the repository's own splitter, for one it does not know. "+err.Error(),
		)
		return
	}
	compressors := map[string]bool{}
	var compressorNames []string
	for _, a := range algorithms.Compression {
		compressors[a.ID] = a.Deprecated
		if !a.Deprecated {
			compressorNames = append(compressorNames, a.ID)
		}
	}
	splitters := map[string]bool{}
	var splitterNames []string
	for _, a := range algorithms.Splitter {
		splitters[a.ID] = true
		splitterNames = append(splitterNames, a.ID)
	}
	sort.Strings(compressorNames)
	sort.Strings(splitterNames)

	for _, use := range uses {
		if !use.compressor {
			if !splitters[use.name] {
				diags.AddAttributeError(use.path, "Unknown splitter",
					server+" does not know the splitter "+strconv.Quote(use.name)+", and would silently use the "+
						"repository's own splitter instead. Use one of: "+strings.Join(splitterNames, ", ")+".")
			}
			continue
		}
		if use.name == "none" || (use.name == policyLZ4 && lz4Reported) {
			continue
		}
		deprecated, known := compressors[use.name]
		switch {
		case !known:
			diags.AddAttributeError(use.path, "Unknown compressor",
				server+" does not know the compressor "+strconv.Quote(use.name)+", and would silently "+
					"compress nothing with it. Use none or one of: "+strings.Join(compressorNames, ", ")+".")
		case deprecated:
			diags.AddAttributeWarning(use.path, "Compressor deprecated",
				server+" lists the compressor "+strconv.Quote(use.name)+" as deprecated. It still works, but "+
					"may not in a later release; prefer one of: "+strings.Join(compressorNames, ", ")+".")
		}
	}
}

// checkScheduling asks Kopia whether it accepts the planned scheduling
// section, through POST /api/v1/policy/resolve with the planned policy as its
// updates. Kopia checks there exactly what a PUT checks of the section:
// manual combined with anything else, and that every cron entry parses. The
// rest of the response is not used; for (global) and @host targets it is even
// wrong (spec: policy.md §3.6), and upcoming snapshot times change with the
// clock, so they are not planned.
func (r *policyResource) checkScheduling(ctx context.Context, target kopiaclient.SourceInfo, m policyBodyModel, diags *diag.Diagnostics) {
	if m.Scheduling.IsNull() {
		return
	}
	resolved, err := r.client.ResolvePolicy(ctx, target, m.toWire(), 0)
	if err != nil {
		diags.AddWarning(
			"Unable to check the scheduling with Kopia",
			"Asking Kopia whether it accepts the scheduling of "+target.String()+" failed, so an invalid cron "+
				"entry is only found when the policy is applied. "+err.Error(),
		)
		return
	}
	if resolved.SchedulingError != "" {
		diags.AddAttributeError(
			path.Root("scheduling"),
			"Invalid scheduling",
			"Kopia refuses this scheduling section: "+resolved.SchedulingError+".",
		)
	}
}

// policyTargetLocks serialises writes to one target. Every PUT writes a new
// manifest in a write session of its own, so two concurrent writes to the
// same target can both delete the old manifest and leave two new ones behind
// (spec: policy.md §3.3). Within one run that only happens when a
// configuration declares the same target twice, and then the lock also makes
// the existence check of the second create see the first one's policy.
var policyTargetLocks = struct {
	sync.Mutex
	byKey map[string]*sync.Mutex
}{byKey: map[string]*sync.Mutex{}}

// policyLockTarget locks target on the server c talks to, and returns the
// function that unlocks it.
func policyLockTarget(c *kopiaclient.Client, target kopiaclient.SourceInfo) func() {
	key := c.BaseURL() + "\x00" + target.String()
	policyTargetLocks.Lock()
	lock, ok := policyTargetLocks.byKey[key]
	if !ok {
		lock = &sync.Mutex{}
		policyTargetLocks.byKey[key] = lock
	}
	policyTargetLocks.Unlock()
	lock.Lock()
	return lock.Unlock
}

// targetOf parses the target of a plan or state, which the validator has
// already checked, or state that an import wrote from a parsed target.
func (m policyResourceModel) targetOf(diags *diag.Diagnostics) (kopiaclient.SourceInfo, bool) {
	target, err := kopiaclient.ParseTarget(m.Target.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("target"), "Invalid policy target", policyTargetHelp(err))
		return kopiaclient.SourceInfo{}, false
	}
	return target, true
}

// fromWire writes what Kopia reported for target into m. prior is the plan or
// the prior state; see policy_conversions.go.
func (m *policyResourceModel) fromWire(target kopiaclient.SourceInfo, policy *kopiaclient.Policy, prior policyBodyModel) {
	m.ID = types.StringValue(target.String())
	m.Target = types.StringValue(target.String())
	m.policyBodyModel = policyBodyFromWire(policy, prior)
}

func (r *policyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan policyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := plan.targetOf(&resp.Diagnostics)
	if !ok {
		return
	}

	unlock := policyLockTarget(r.client, target)
	defer unlock()

	// The global policy always exists — `kopia repository create` writes it —
	// so it is adopted. Any other existing policy belongs to someone else: the
	// Kopia UI, kopia-docker-watcher, another configuration. A PUT would
	// replace it without a trace, so it has to be imported deliberately.
	if !target.IsGlobal() {
		_, err := r.client.GetPolicy(ctx, target)
		switch {
		case err == nil:
			resp.Diagnostics.AddAttributeError(
				path.Root("target"),
				"Kopia policy already exists",
				"The Kopia server already has a policy for "+target.String()+". kopia_policy owns the whole "+
					"policy of its target and replaces it on every apply, so it does not take over an "+
					"existing one unasked: it may have been set up in the Kopia UI, by kopia-docker-watcher "+
					"or by another configuration. To manage it with Terraform, import it with an import "+
					"block or `terraform import`, using "+strconv.Quote(target.String())+" as the id; or "+
					"delete it first.",
			)
			return
		case kopiaclient.IsNotFound(err):
		default:
			resp.Diagnostics.AddError(
				"Unable to create Kopia policy",
				"Checking whether "+target.String()+" already has a policy failed, so none was written. "+
					policyReadErrorDetail(err),
			)
			return
		}
	}

	if err := r.client.SetPolicy(ctx, target, plan.toWire()); err != nil {
		addPolicyWriteError(&resp.Diagnostics, "Unable to create Kopia policy", target, err)
		return
	}
	r.readBack(ctx, target, plan, &resp.State, &resp.Diagnostics)
	setIdentity(ctx, resp.Identity, "target", target.String(), &resp.Diagnostics)
}

// readBack records what Kopia stored for target after a write. Kopia stores a
// policy exactly as sent, but state is written from what it reports all the
// same. If reading it back fails, the write has still happened, so state is
// written from the plan instead of marking the policy for replacement, and
// the next refresh reads it.
func (r *policyResource) readBack(ctx context.Context, target kopiaclient.SourceInfo, plan policyResourceModel, state *tfsdk.State, diags *diag.Diagnostics) {
	stored, err := r.client.GetPolicy(ctx, target)
	if err != nil {
		diags.AddWarning(
			"Unable to read Kopia policy back",
			"Kopia accepted the policy of "+target.String()+", but reading it back failed, so state was "+
				"written from the configuration; the next refresh reads it from Kopia. "+policyReadErrorDetail(err),
		)
		plan.ID = types.StringValue(target.String())
		diags.Append(state.Set(ctx, &plan)...)
		return
	}
	result := plan
	result.fromWire(target, stored, plan.policyBodyModel)
	diags.Append(state.Set(ctx, &result)...)
}

func (r *policyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state policyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Before the lookup, so that a refresh finding the policy gone still
	// reports one; see setIdentity.
	setIdentity(ctx, resp.Identity, "target", state.Target.ValueString(), &resp.Diagnostics)

	target, ok := state.targetOf(&resp.Diagnostics)
	if !ok {
		return
	}
	stored, err := r.client.GetPolicy(ctx, target)
	if err != nil {
		if kopiaclient.IsNotFound(err) {
			tflog.Info(ctx, "Kopia policy no longer exists; removing it from state", map[string]any{"target": target.String()})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read Kopia policy", "Reading the policy of "+target.String()+
			" failed, and state was left unchanged. "+policyReadErrorDetail(err))
		return
	}

	state.fromWire(target, stored, state.policyBodyModel)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update writes the whole planned policy. Kopia's PUT creates a policy that
// was deleted outside of Terraform since the last refresh, which is what the
// plan asks for anyway.
func (r *policyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan policyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := plan.targetOf(&resp.Diagnostics)
	if !ok {
		return
	}

	unlock := policyLockTarget(r.client, target)
	defer unlock()

	if err := r.client.SetPolicy(ctx, target, plan.toWire()); err != nil {
		addPolicyWriteError(&resp.Diagnostics, "Unable to update Kopia policy", target, err)
		return
	}
	r.readBack(ctx, target, plan, &resp.State, &resp.Diagnostics)
	setIdentity(ctx, resp.Identity, "target", target.String(), &resp.Diagnostics)
}

// Delete deletes the policy, except the global one, which is reset to the
// policy a new repository starts with: Kopia would delete it for good, and
// nothing would ever recreate it (spec: policy.md §3.4).
func (r *policyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state policyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := state.targetOf(&resp.Diagnostics)
	if !ok {
		return
	}

	unlock := policyLockTarget(r.client, target)
	defer unlock()

	if target.IsGlobal() {
		if err := r.client.SetPolicy(ctx, target, kopiaclient.DefaultGlobalPolicy()); err != nil {
			resp.Diagnostics.AddError(
				"Unable to reset the Kopia global policy",
				"Destroying the (global) policy resets it to Kopia's defaults rather than deleting it, and "+
					"writing those failed. "+policyWriteErrorDetail(err),
			)
		}
		return
	}
	// Kopia answers 200 for a target without a policy; a NOT_FOUND would mean
	// the same.
	if err := r.client.DeletePolicy(ctx, target); err != nil && !kopiaclient.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to delete Kopia policy", "Deleting the policy of "+target.String()+
			" failed. "+policyWriteErrorDetail(err))
	}
}

// ImportState imports a policy by its target, given either as the import id
// or, from Terraform 1.12, as the identity of an import block.
func (r *policyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, _ := importedID(ctx, req, "target", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	target, err := kopiaclient.ParseTarget(id)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unexpected import identifier",
			"Import a Kopia policy by its target, such as \"(global)\", \"@host1\", \"docker@host1\" or "+
				"\"docker@host1:/data/app\". "+policyTargetHelp(err),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("target"), target.String())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), target.String())...)
	setIdentity(ctx, resp.Identity, "target", target.String(), &resp.Diagnostics)
}

// policyReadErrorDetail explains a failed GET of a policy.
func policyReadErrorDetail(err error) string {
	switch {
	case errors.Is(err, kopiaclient.ErrPolicyUnavailable):
		return "Kopia answered with null instead of the policy, which it does when loading the policy fails " +
			"on the server, without saying why. This is usually transient: run the command again. " + err.Error()
	case kopiaclient.IsNotConnected(err):
		return "The Kopia server has no repository connected. " + err.Error()
	}
	return err.Error()
}

// policyWriteErrorDetail explains a failed PUT or DELETE of a policy.
func policyWriteErrorDetail(err error) string {
	if kopiaclient.IsNotConnected(err) {
		return "The Kopia server has no repository connected. " + err.Error()
	}
	return err.Error()
}

// addPolicyWriteError reports a failed PUT. Kopia reports a policy it refuses
// as 500 INTERNAL, like a storage failure, so the message is what tells them
// apart (spec: policy.md §2.2), and a refusal is pinned to the attribute it is
// about.
func addPolicyWriteError(diags *diag.Diagnostics, summary string, target kopiaclient.SourceInfo, err error) {
	detail := "Kopia did not store the policy of " + target.String() + ". " + policyWriteErrorDetail(err)
	var apiErr *kopiaclient.Error
	if !errors.As(err, &apiErr) || apiErr.Code != kopiaclient.CodeInternal {
		diags.AddError(summary, detail)
		return
	}
	switch {
	case strings.Contains(apiErr.Message, "invalid scheduling policy"):
		diags.AddAttributeError(path.Root("scheduling"), summary, detail)
	case strings.Contains(apiErr.Message, "invalid upload policy"):
		diags.AddAttributeError(path.Root("upload").AtName("max_parallel_snapshots"), summary, detail)
	case strings.Contains(apiErr.Message, "invalid policy path"):
		diags.AddAttributeError(path.Root("target"), summary, detail)
	default:
		diags.AddError(summary, detail)
	}
}
