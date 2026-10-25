package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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

// kopia_repository_description deliberately has no resource identity: a
// singleton keyed by the constant "description" would share one with every
// other Kopia server. See resource_identity.go.
var (
	_ resource.Resource                = &repositoryDescriptionResource{}
	_ resource.ResourceWithConfigure   = &repositoryDescriptionResource{}
	_ resource.ResourceWithImportState = &repositoryDescriptionResource{}
	_ resource.ResourceWithModifyPlan  = &repositoryDescriptionResource{}
)

// repositoryDescriptionID is the id of the one description a server has.
const repositoryDescriptionID = "description"

// NewRepositoryDescriptionResource returns the kopia_repository_description
// resource.
func NewRepositoryDescriptionResource() resource.Resource { return &repositoryDescriptionResource{} }

type repositoryDescriptionResource struct {
	client *kopiaclient.Client
}

type repositoryDescriptionModel struct {
	ID          types.String `tfsdk:"id"`
	Description types.String `tfsdk:"description"`
}

func (r *repositoryDescriptionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_repository_description"
}

func (r *repositoryDescriptionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the description of the Kopia server's repository connection: the name the " +
			"Kopia UI shows for the repository, which Kopia sets to `Repository in <storage>` — such as " +
			"`Repository in Filesystem: /repository` — until it is changed.\n\n" +
			"This is a singleton: a Kopia server has one description, so declare at most one of these " +
			"resources. Kopia saves the description in the server's config file, `repository.config`, not in " +
			"the repository, so other clients of the same repository keep their own; the `kopia_repository_status` " +
			"data source reports it as `description`. Disconnecting the repository deletes the config file, " +
			"and with it the description.\n\n" +
			"~> **Destroying this resource leaves the description in place.** Kopia's API can set a " +
			"description but not remove one, so destroying only removes the resource from Terraform state.\n\n" +
			"~> **Kopia rewrites its whole config file with the settings the server started with whenever " +
			"the description changes.** That would put the saved throttle limits back to those the server " +
			"started with, so after each change the provider saves the limits in effect again, without " +
			"changing them, under the same lock `kopia_throttle` takes; when that fails, the apply fails, and " +
			"the next apply sets the description and saves the limits again. Other changes made to the file " +
			"since the server started, with `kopia repository set-client` or `kopia cache set`, are reverted " +
			"all the same.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Always `description`: a Kopia server has one repository description.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"description": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Description of the repository. It must not be empty: Kopia saves `\"\"` as " +
					"no description at all, and puts its default back at the next restart.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
		},
	}
}

func (r *repositoryDescriptionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// write sets the description and then saves the throttle limits again,
// holding the config-file lock throughout.
//
// To save the description, Kopia writes back the whole client configuration
// it opened the repository with, throttle limits included, so the limits in
// the config file revert to their value at start-up while the ones in effect
// stay as they are (spec: repo.md §5.3). Putting the limits in effect back
// with GET and PUT saves them again, and changes nothing else; holding the
// lock keeps kopia_throttle from writing in between, which would be undone.
//
// The status Kopia answers the POST with is returned whenever the POST
// succeeded, even when saving the limits afterwards failed, because the
// description is set either way: the caller records it, then reports the
// error, which limitsNotSaved flags.
func (r *repositoryDescriptionResource) write(ctx context.Context, description string, diags *diag.Diagnostics) (status *kopiaclient.RepoStatus, limitsNotSaved bool) {
	r.client.LockConfigFile()
	defer r.client.UnlockConfigFile()

	status, err := r.client.SetDescription(ctx, description)
	if err != nil {
		diags.AddError("Unable to set Kopia repository description", repositoryCallErrorDetail(err))
		return nil, false
	}

	limits, err := r.client.GetThrottle(ctx)
	if kopiaclient.IsStorageConnection(err) {
		// A server that reaches its repository through a repository server
		// has no throttle limits, so there are none to save.
		return status, false
	}
	if err == nil {
		err = r.client.SetThrottle(ctx, *limits)
	}
	if err != nil {
		diags.AddError(
			"Kopia throttle limits were not saved again",
			"Kopia set the repository description, and in doing so put the throttle limits saved in its config "+
				"file back to those the server started with. The provider saves the limits in effect again "+
				"after every description change, and that failed: the limits in effect are unchanged, but the "+
				"server goes back to the saved ones when it restarts. The next apply sets the description and "+
				"saves the limits again. "+repositoryCallErrorDetail(err),
		)
		return status, true
	}
	return status, false
}

func (r *repositoryDescriptionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan repositoryDescriptionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	status, _ := r.write(ctx, plan.Description.ValueString(), &resp.Diagnostics)
	if status == nil {
		return
	}
	// Saved even when saving the throttle limits again failed: the
	// description is set, and the error marks the resource tainted, so the
	// next apply replaces it, which sets the description, and saves the
	// limits, again.
	plan.fromStatus(status)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// fromStatus copies the description Kopia reports into the model. Kopia
// leaves an empty description out, which reads as null.
func (m *repositoryDescriptionModel) fromStatus(status *kopiaclient.RepoStatus) {
	m.ID = types.StringValue(repositoryDescriptionID)
	m.Description = stringValueOrNullIfEmpty(status.Description)
}

func (r *repositoryDescriptionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state repositoryDescriptionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	status, err := r.client.RepoStatus(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Kopia repository description", repositoryCallErrorDetail(err))
		return
	}
	// GET /api/v1/repo/status answers even without a repository, with no
	// description at all. Reading that as "no description" would plan to set
	// one on a server that cannot take it, and one that is only still
	// connecting (--async-repo-connect) has its description back soon.
	if !status.Connected {
		resp.Diagnostics.AddError(
			"Unable to read Kopia repository description",
			"The Kopia server has no repository connected, and the description belongs to the repository "+
				"connection. Connect the server to its repository — `kopia repository connect` before `kopia "+
				"server start`, or the Kopia UI — and run Terraform again.",
		)
		return
	}

	state.fromStatus(status)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// ModifyPlan plans an update after an update that set the description but
// failed to save the throttle limits again, so that the next apply saves
// them; see repositorySaveAgainKey.
func (r *repositoryDescriptionResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	repositoryPlanSaveAgain(ctx, req, resp,
		"Kopia throttle limits will be saved again",
		"The last apply of kopia_repository_description set the description, but then failed to save the "+
			"throttle limits again, which Kopia had put back to those the server started with. This apply sets "+
			"the same description again, and saves the limits in effect.",
	)
}

// Update records the description whenever Kopia set it. When saving the
// throttle limits again failed, the next apply is marked to repeat the whole
// write: with state matching the configuration, its plan would otherwise be
// empty, and an update, unlike a create, does not taint the resource.
func (r *repositoryDescriptionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan repositoryDescriptionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	status, limitsNotSaved := r.write(ctx, plan.Description.ValueString(), &resp.Diagnostics)
	if status == nil {
		return
	}
	plan.fromStatus(status)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Private != nil {
		repositorySetSaveAgain(ctx, resp.Private, limitsNotSaved, &resp.Diagnostics)
	}
}

// Delete removes the description from state without changing the server.
// POST /api/v1/repo/description can only set one, and an empty one is
// replaced by Kopia's default at the next restart anyway, so leaving the
// description alone is the only outcome that is the same before and after a
// restart.
func (r *repositoryDescriptionResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning(
		"Kopia repository description was left unchanged",
		"kopia_repository_description has been removed from Terraform state, but the Kopia server keeps the "+
			"description. Kopia's API can set a repository description but not remove one; change it in the "+
			"Kopia UI if it should go.",
	)
}

// ImportState adopts the server's description. The import id is ignored
// because there is only one; Read fills in the description.
func (r *repositoryDescriptionResource) ImportState(ctx context.Context, _ resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), repositoryDescriptionID)...)
}
