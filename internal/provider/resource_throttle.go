package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// kopia_throttle deliberately has no resource identity: a singleton keyed by
// the constant "throttle" would share one with every other Kopia server. See
// resource_identity.go.
var (
	_ resource.Resource                = &throttleResource{}
	_ resource.ResourceWithConfigure   = &throttleResource{}
	_ resource.ResourceWithImportState = &throttleResource{}
	_ resource.ResourceWithModifyPlan  = &throttleResource{}
)

// throttleID is the id of the one kopia_throttle a server has.
const throttleID = "throttle"

// NewThrottleResource returns the kopia_throttle resource.
func NewThrottleResource() resource.Resource { return &throttleResource{} }

type throttleResource struct {
	client *kopiaclient.Client
}

type throttleModel struct {
	ID                             types.String  `tfsdk:"id"`
	ReadsPerSecond                 types.Float64 `tfsdk:"reads_per_second"`
	WritesPerSecond                types.Float64 `tfsdk:"writes_per_second"`
	ListsPerSecond                 types.Float64 `tfsdk:"lists_per_second"`
	MaxUploadSpeedBytesPerSecond   types.Float64 `tfsdk:"max_upload_speed_bytes_per_second"`
	MaxDownloadSpeedBytesPerSecond types.Float64 `tfsdk:"max_download_speed_bytes_per_second"`
	ConcurrentReads                types.Int64   `tfsdk:"concurrent_reads"`
	ConcurrentWrites               types.Int64   `tfsdk:"concurrent_writes"`
}

func (r *throttleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_throttle"
}

// throttleUnlimited ends the description of every limit.
const throttleUnlimited = " Must be greater than 0. Leave it out, or set it to `null`, for no limit."

// Every limit is owned outright: PUT /api/v1/repo/throttle replaces all seven
// at once, so a limit the configuration leaves out is sent as unlimited
// rather than kept, and nothing is Computed. Kopia answers with exactly the
// values it was given, which is what state is written from.
func (r *throttleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	rate := func(description string) schema.Float64Attribute {
		return schema.Float64Attribute{
			Optional:            true,
			MarkdownDescription: description + throttleUnlimited,
			Validators:          []validator.Float64{throttleRateValidator{}},
		}
	}
	concurrency := func(description string) schema.Int64Attribute {
		return schema.Int64Attribute{
			Optional:            true,
			MarkdownDescription: description + throttleUnlimited,
			// Kopia decodes the value into a Go int, which a 32-bit build
			// cannot hold beyond this; no real limit comes near it.
			Validators: []validator.Int64{int64validator.Between(1, math.MaxInt32)},
		}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the throttle limits of the Kopia server's repository connection: how many " +
			"requests per second it sends to the repository's storage, how many bytes per second it uploads and " +
			"downloads, and how many reads and writes it runs at once — the limits `kopia server throttle set` " +
			"changes. They apply to everything the server does with the repository, snapshots and maintenance " +
			"alike.\n\n" +
			"This is a singleton: a Kopia server has one set of limits, so declare at most one of these " +
			"resources. It owns all seven limits. A limit left out, or set to `null`, is unlimited, and creating " +
			"the resource replaces whatever limits the server had, rather than adopting them.\n\n" +
			"The rates are averages: Kopia lets up to 60 seconds' worth through in a burst. Kopia saves the " +
			"limits in the server's config file, `repository.config` (`config_file` of the " +
			"`kopia_repository_status` data source), and uses them again after a restart. On a server that has " +
			"never had limits set this way, the first read reports the limits in the repository's storage " +
			"configuration, set with `--max-upload-speed` and `--max-download-speed` of `kopia repository " +
			"connect`, or none.\n\n" +
			"~> **The config file must be on persistent, writable storage**, such as a volume on `/app/config` " +
			"for the official Docker image. When Kopia cannot write it, the new limits still take effect, but " +
			"the apply fails, and the server goes back to the saved limits when it restarts; the next apply " +
			"tries to save them again.\n\n" +
			"~> **Destroying this resource sets every limit to unlimited and saves that.** Kopia's API cannot " +
			"remove saved limits, so limits from the storage configuration do not come back, not even after a " +
			"restart.\n\n" +
			"~> **Changing the repository description puts the saved limits back to those the server started " +
			"with.** Kopia rewrites its config file with the settings it was started with whenever the " +
			"description changes, although the limits in effect stay as they are. `kopia_repository_description` " +
			"saves the current limits again straight away; a description changed in the Kopia UI does not, and " +
			"the server then loses the limits at its next restart. Within one provider configuration, this " +
			"resource and `kopia_repository_description` never write the config file at the same time; the " +
			"Kopia UI and the `kopia` command line are not coordinated with them.\n\n" +
			"~> **Disconnecting or reconnecting the repository discards the limits.** Disconnecting deletes the " +
			"config file, and connecting writes a new one without limits. The next refresh shows the " +
			"difference, and the next apply sets the limits again.\n\n" +
			"~> **A server that reaches its repository through a Kopia repository server has no limits of its " +
			"own.** Kopia refuses every throttle request there with `STORAGE_CONNECTION`; manage the limits on " +
			"the repository server instead. A refresh then removes this resource from state with a warning, " +
			"and creating it fails.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Always `throttle`: a Kopia server has one set of throttle limits.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"reads_per_second": rate("Average number of blob reads per second: requests for a blob or for " +
				"its metadata."),
			"writes_per_second": rate("Average number of blob writes per second, deletions included."),
			"lists_per_second":  rate("Average number of blob listings per second."),
			"max_upload_speed_bytes_per_second": rate("Average number of bytes per second written to the " +
				"repository's storage — bytes, not bits."),
			"max_download_speed_bytes_per_second": rate("Average number of bytes per second read from the " +
				"repository's storage — bytes, not bits. Reading a blob of unknown length counts 20,000,000 " +
				"bytes up front, and settles the difference once the blob is read."),
			"concurrent_reads": concurrency("Maximum number of blob reads, including requests for a blob's " +
				"metadata, in flight at once."),
			"concurrent_writes": concurrency("Maximum number of blob writes and deletions in flight at once."),
		},
	}
}

func (r *throttleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// throttleRateValidator requires a rate greater than 0. Kopia reads 0 as
// unlimited, which this resource spells null so that a read never turns a
// configured value into another, and it refuses a negative rate.
// float64validator has no exclusive lower bound.
type throttleRateValidator struct{}

func (throttleRateValidator) Description(_ context.Context) string {
	return "value must be greater than 0"
}

func (v throttleRateValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (throttleRateValidator) ValidateFloat64(_ context.Context, req validator.Float64Request, resp *validator.Float64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() || req.ConfigValue.ValueFloat64() > 0 {
		return
	}
	resp.Diagnostics.AddAttributeError(
		req.Path,
		"Invalid Attribute Value",
		fmt.Sprintf("Attribute %s value must be greater than 0, got: %s. Leave it out, or set it to null, for no "+
			"limit.", req.Path, strconv.FormatFloat(req.ConfigValue.ValueFloat64(), 'g', -1, 64)),
	)
}

// toLimits builds the PUT body from every limit. A null limit is 0, which
// Kopia leaves out of the body and reads as unlimited.
func (m *throttleModel) toLimits() kopiaclient.ThrottleLimits {
	return kopiaclient.ThrottleLimits{
		ReadsPerSecond:         m.ReadsPerSecond.ValueFloat64(),
		WritesPerSecond:        m.WritesPerSecond.ValueFloat64(),
		ListsPerSecond:         m.ListsPerSecond.ValueFloat64(),
		UploadBytesPerSecond:   m.MaxUploadSpeedBytesPerSecond.ValueFloat64(),
		DownloadBytesPerSecond: m.MaxDownloadSpeedBytesPerSecond.ValueFloat64(),
		ConcurrentReads:        m.ConcurrentReads.ValueInt64(),
		ConcurrentWrites:       m.ConcurrentWrites.ValueInt64(),
	}
}

// fromLimits copies the limits Kopia reports into the model. Kopia leaves
// unlimited limits out, which decode as 0, and 0 maps to null: the value
// that asks for no limit.
func (m *throttleModel) fromLimits(limits *kopiaclient.ThrottleLimits) {
	m.ID = types.StringValue(throttleID)
	m.ReadsPerSecond = float64ValueOrNullIfZero(limits.ReadsPerSecond)
	m.WritesPerSecond = float64ValueOrNullIfZero(limits.WritesPerSecond)
	m.ListsPerSecond = float64ValueOrNullIfZero(limits.ListsPerSecond)
	m.MaxUploadSpeedBytesPerSecond = float64ValueOrNullIfZero(limits.UploadBytesPerSecond)
	m.MaxDownloadSpeedBytesPerSecond = float64ValueOrNullIfZero(limits.DownloadBytesPerSecond)
	m.ConcurrentReads = int64ValueOrNullIfZero(limits.ConcurrentReads)
	m.ConcurrentWrites = int64ValueOrNullIfZero(limits.ConcurrentWrites)
}

// throttleNotSaved and throttleRemovedNotSaved explain limits that Kopia
// applied, or removed, but could not write to its config file.
const (
	throttleNotSaved = "Kopia applied the throttle limits, which are in effect now, but could not write them " +
		"to its config file, repository.config, so the server goes back to the limits saved there when it " +
		"restarts."
	throttleRemovedNotSaved = "Kopia removed every throttle limit, so the server is unlimited now, but could " +
		"not write that to its config file, repository.config, so the server goes back to the limits saved " +
		"there when it restarts."
)

// addThrottleWriteError reports a failed PUT /api/v1/repo/throttle. Kopia
// answers 400 MALFORMED_REQUEST both when it refused the limits and when it
// applied them but could not save them, and the two need opposite advice.
// notSaved explains the second case, which differs between setting limits and
// removing them.
func addThrottleWriteError(diags *diag.Diagnostics, summary, notSaved string, err error) {
	if errors.Is(err, kopiaclient.ErrThrottleNotPersisted) {
		diags.AddError(
			"Kopia throttle limits were not saved",
			notSaved+" Make sure the directory holding the config file is writable and on persistent storage "+
				"— a volume on /app/config for the official Docker image — and apply again. "+err.Error(),
		)
		return
	}
	if errors.Is(err, kopiaclient.ErrThrottleRejected) {
		diags.AddError(summary, "Kopia refused the throttle limits and kept the ones it had. "+err.Error())
		return
	}
	diags.AddError(summary, repositoryCallErrorDetail(err))
}

// apply replaces the server's limits with the plan's and reads them back,
// both under the config-file lock, which kopia_repository_description takes
// too: Kopia saves the limits by rewriting the same file the description is
// saved in, without any lock of its own, so two such writes at once could
// lose one (spec: repo.md §4.5 (f)).
//
// PUT answers {}, so the limits are read back to record what Kopia holds,
// as every resource here writes state from the server's answer. ok reports
// whether plan now holds them; notSaved, whether Kopia applied them but could
// not save them, which has been reported already.
func (r *throttleResource) apply(ctx context.Context, plan *throttleModel, summary string, diags *diag.Diagnostics) (ok, notSaved bool) {
	r.client.LockConfigFile()
	defer r.client.UnlockConfigFile()

	if err := r.client.SetThrottle(ctx, plan.toLimits()); err != nil {
		addThrottleWriteError(diags, summary, throttleNotSaved, err)
		if !errors.Is(err, kopiaclient.ErrThrottleNotPersisted) {
			return false, false
		}
		notSaved = true
	}
	limits, err := r.client.GetThrottle(ctx)
	if err != nil {
		diags.AddError("Unable to read Kopia throttle limits back", "Kopia accepted the throttle limits, but "+
			"reading them back failed. "+repositoryCallErrorDetail(err))
		return false, notSaved
	}
	plan.fromLimits(limits)
	return true, notSaved
}

// Create leaves the resource out of state whenever the limits could not be
// both applied and saved, even though Kopia applied them in the second case:
// the next plan then creates it again, which is the retry the apply needs,
// whereas a tainted resource would be replaced, removing every limit for a
// moment in between.
func (r *throttleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan throttleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if ok, notSaved := r.apply(ctx, &plan, "Unable to set Kopia throttle limits", &resp.Diagnostics); !ok || notSaved {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *throttleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state throttleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The limits always exist, so a read never removes the resource because
	// of them: a server whose limits were removed reports them as unlimited,
	// which shows up as a difference from the configuration.
	limits, err := r.client.GetThrottle(ctx)
	if kopiaclient.IsStorageConnection(err) {
		// A server that now reaches its repository through a repository
		// server has no limits of its own. Failing the refresh would leave a
		// resource that cannot even be destroyed, because Terraform refreshes
		// before it destroys; removing it instead plans to create it again,
		// which fails with the same explanation.
		resp.Diagnostics.AddWarning(
			"Kopia throttle limits no longer exist",
			"kopia_throttle has been removed from Terraform state: "+repositoryCallErrorDetail(err),
		)
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Kopia throttle limits", repositoryCallErrorDetail(err))
		return
	}

	state.fromLimits(limits)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// ModifyPlan plans an update after an update whose limits Kopia applied but
// could not save, so that the next apply saves them; see
// repositorySaveAgainKey.
func (r *throttleResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	repositoryPlanSaveAgain(ctx, req, resp,
		"Kopia throttle limits will be saved again",
		"The last apply of kopia_throttle set limits that Kopia applied but could not write to its config "+
			"file, so the server would go back to the limits saved there when it restarts. This apply sets the "+
			"same limits again, to save them.",
	)
}

// Update records the limits in effect even when Kopia could not save them,
// because they are what the server now uses, and marks them to be saved again
// by the next apply: with state matching the configuration, the next plan
// would otherwise be empty.
func (r *throttleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan throttleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ok, notSaved := r.apply(ctx, &plan, "Unable to update Kopia throttle limits", &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Private != nil {
		repositorySetSaveAgain(ctx, resp.Private, notSaved, &resp.Diagnostics)
	}
}

// Delete removes every limit with PUT {}, the only reset the API has. Kopia
// saves that as an explicit "no limits", so limits from the repository's
// storage configuration do not come back either (spec: repo.md §4.5 (b)).
func (r *throttleResource) Delete(ctx context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	r.client.LockConfigFile()
	defer r.client.UnlockConfigFile()

	err := r.client.SetThrottle(ctx, kopiaclient.ThrottleLimits{})
	switch {
	case err == nil:
	case kopiaclient.IsStorageConnection(err):
		// The server reaches its repository through a repository server
		// now, and has no limits of its own to remove. Failing would leave
		// a resource nothing can destroy.
		resp.Diagnostics.AddWarning(
			"Kopia throttle limits were left unchanged",
			"kopia_throttle has been removed from Terraform state without resetting any limits: the Kopia server "+
				"now reaches its repository through a Kopia repository server rather than directly on its storage "+
				"(STORAGE_CONNECTION), and has no throttle limits of its own. "+err.Error(),
		)
	default:
		// The resource stays in state on any error, so the next destroy
		// tries again.
		addThrottleWriteError(&resp.Diagnostics, "Unable to remove Kopia throttle limits", throttleRemovedNotSaved, err)
	}
}

// ImportState adopts the server's limits. The import id is ignored because
// there is only one set of them; Read fills in the limits.
func (r *throttleResource) ImportState(ctx context.Context, _ resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), throttleID)...)
}
