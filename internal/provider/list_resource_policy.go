package provider

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/list"
	listschema "github.com/hashicorp/terraform-plugin-framework/list/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ list.ListResource              = &policyListResource{}
	_ list.ListResourceWithConfigure = &policyListResource{}
)

// NewPolicyListResource returns the kopia_policy list resource.
func NewPolicyListResource() list.ListResource { return &policyListResource{} }

type policyListResource struct {
	client *kopiaclient.Client
}

type policyListConfigModel struct {
	Host     types.String `tfsdk:"host"`
	UserName types.String `tfsdk:"user_name"`
	Path     types.String `tfsdk:"path"`
}

func (l *policyListResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_policy"
}

func (l *policyListResource) ListResourceConfigSchema(_ context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	filter := func(what string) listschema.StringAttribute {
		return listschema.StringAttribute{
			Optional: true,
			MarkdownDescription: "List only the policies whose target has exactly this " + what + ", compared " +
				"as Kopia compares it: as a plain string, letter case included. Leave it out to list them all.",
			Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
		}
	}
	resp.Schema = listschema.Schema{
		MarkdownDescription: "Lists the targets that have a policy of their own on the Kopia server, for " +
			"`terraform query` (Terraform 1.14 and later), sorted by target. Each result is named after the " +
			"target, written as Kopia writes it, and carries it as the identity of `kopia_policy`, ready for an " +
			"`import` block. With `include_resource = true` it also carries every attribute of `kopia_policy`, " +
			"with the values a refresh right after importing the policy records; the listing already holds " +
			"every policy, so that costs no further request.\n\n" +
			"The global policy is listed like any other, and so are the path policies kopia-docker-watcher " +
			"keeps; leave those to the watcher, or narrow the list with the filters. The filters match " +
			"exactly, and one left out matches everything, so the global policy cannot be asked for on its " +
			"own.\n\n" +
			"Kopia may hold two stored copies of one target's policy after concurrent writes; the target is " +
			"listed once, with the newer copy, the one Kopia itself reads. Kopia can also hold policies for " +
			"targets `kopia_policy` refuses, such as `docker@` (no host) or `@host1:/data` (no user), which " +
			"only other clients write. They are left out with a warning that names them; the `kopia_policies` " +
			"data source lists them.",
		Attributes: map[string]listschema.Attribute{
			"host":      filter("host"),
			"user_name": filter("user name"),
			"path":      filter("path"),
		},
	}
}

func (l *policyListResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	l.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// List reports the policies of one GET /api/v1/policies, which holds every
// stored policy in full, so including the resource costs no further request:
// each is converted by kopia_policy's own conversion, from a prior state that
// holds nothing but the target, as a refresh after an import converts it.
func (l *policyListResource) List(ctx context.Context, req list.ListRequest, stream *list.ListResultsStream) {
	if l.client == nil {
		stream.Results = unconfiguredListError()
		return
	}

	// A list block without a config block reaches the provider as a null
	// configuration, not as an empty one: Terraform nests the arguments in an
	// optional config block, and sends that block's value. It sets no filter,
	// so it lists every policy, as an empty config block does; decoding it
	// into the model would fail instead.
	var config policyListConfigModel
	if !req.Config.Raw.IsNull() {
		if diags := req.Config.Get(ctx, &config); diags.HasError() {
			stream.Results = list.ListResultsStreamDiagnostics(diags)
			return
		}
	}
	entries, err := l.client.ListPolicies(ctx, kopiaclient.PolicyFilter{
		Host:     config.Host.ValueString(),
		UserName: config.UserName.ValueString(),
		Path:     config.Path.ValueString(),
	})
	if err != nil {
		stream.Results = listError("Unable to list Kopia policies", policyReadErrorDetail(err))
		return
	}
	listed, refused := policyListEntries(entries)

	stream.Results = func(push func(list.ListResult) bool) {
		e := newListEmitter(req, push)
		if len(refused) > 0 {
			e.warn(
				"Kopia policies left out of the list",
				"Kopia stores policies for targets kopia_policy refuses, so they cannot be imported and are not "+
					"listed: "+strings.Join(refused, ", ")+". Such a target lacks a host or a user, or has a "+
					"path Kopia never matches to a snapshot source, and only another client can have written "+
					"it. The kopia_policies data source lists them as Kopia writes them.",
			)
		}
		for _, entry := range listed {
			if e.full() {
				return
			}
			target := entry.Target.String()
			result := req.NewListResult(ctx)
			result.DisplayName = target
			setIdentity(ctx, result.Identity, "target", target, &result.Diagnostics)
			if req.IncludeResource {
				var policy policyResourceModel
				policy.fromWire(entry.Target, entry.Policy, policyBodyModel{})
				result.Diagnostics.Append(result.Resource.Set(ctx, &policy)...)
			}
			if !e.emit(result) {
				return
			}
		}
		e.flush()
	}
}

// policyListEntries picks what the kopia_policy list reports from the entries
// of GET /api/v1/policies: one entry per target, sorted by target. Kopia lists
// the oldest change first, so of two entries for one target the later is the
// newer, and the one GET /api/v1/policy returns (manifest.PickLatestID). The
// targets ParseTarget refuses, or does not give back unchanged, are returned
// apart, quoted and sorted, since kopia_policy could not import them.
func policyListEntries(entries []kopiaclient.PolicyEntry) (listed []kopiaclient.PolicyEntry, refused []string) {
	newest := make(map[kopiaclient.SourceInfo]kopiaclient.PolicyEntry, len(entries))
	for _, entry := range entries {
		newest[entry.Target] = entry
	}
	for target, entry := range newest {
		if parsed, err := kopiaclient.ParseTarget(target.String()); err != nil || parsed != target {
			refused = append(refused, strconv.Quote(target.String()))
			continue
		}
		listed = append(listed, entry)
	}
	slices.SortFunc(listed, func(a, b kopiaclient.PolicyEntry) int {
		return strings.Compare(a.Target.String(), b.Target.String())
	})
	slices.Sort(refused)
	return listed, slices.Compact(refused)
}
