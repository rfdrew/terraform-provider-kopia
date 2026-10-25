package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Resource identity is the small object Terraform 1.12 and later store next to
// a resource's state to say which remote object it is, and accept in an import
// block's `identity` argument instead of an import id. Every resource here that
// has one is identified by a single string that never changes while the object
// exists and that Read always knows, because it is what Read looks up:
//
//   - kopia_policy by its target, such as `(global)` or `docker@host1`;
//   - kopia_notification_profile by its name.
//
// The singletons kopia_throttle and kopia_repository_description deliberately
// have none: their only key is a constant, so every Kopia server would share
// one identity.
//
// The framework requires an identity after every successful create, read and
// update of a resource that declares one, whether or not Terraform supports
// identities. Older Terraform neither sends nor stores it and is otherwise
// unaffected.

// stringIdentitySchema is the identity of a resource identified by one string,
// held in the identity attribute of the given name.
func stringIdentitySchema(attribute, description string) identityschema.Schema {
	return identityschema.Schema{
		Attributes: map[string]identityschema.Attribute{
			attribute: identityschema.StringAttribute{
				RequiredForImport: true,
				Description:       description,
			},
		},
	}
}

// setIdentity records value as the identity attribute of the given name.
// identity is nil only when a method is called directly rather than through
// the framework, and an empty value has nothing to record.
//
// Read calls it with the value from state before looking the object up. A
// refresh that finds the object gone drops it from state without an error,
// and the framework still requires an identity then whenever state had none
// yet — state written by Terraform before 1.12 — failing the refresh with
// "Missing Resource Identity After Read" otherwise.
func setIdentity(ctx context.Context, identity *tfsdk.ResourceIdentity, attribute, value string, diags *diag.Diagnostics) {
	if identity == nil || value == "" {
		return
	}
	diags.Append(identity.SetAttribute(ctx, path.Root(attribute), value)...)
}

// importedID returns what an import names the resource by: the import id, in
// whatever form the resource accepts, or — for an import block that used
// `identity` — the identity attribute of the given name. byIdentity tells the
// two apart.
func importedID(ctx context.Context, req resource.ImportStateRequest, attribute string, diags *diag.Diagnostics) (id string, byIdentity bool) {
	if req.ID != "" || req.Identity == nil {
		return req.ID, false
	}
	var value types.String
	diags.Append(req.Identity.GetAttribute(ctx, path.Root(attribute), &value)...)
	if diags.HasError() {
		return "", true
	}
	if value.ValueString() == "" {
		diags.AddError(
			"Missing import identity",
			"The import block's identity must set `"+attribute+"` to name the Kopia object to import.",
		)
		return "", true
	}
	return value.ValueString(), true
}
