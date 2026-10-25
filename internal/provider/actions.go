package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// Actions (Terraform 1.14 and later) are one-off operations that keep no
// state: Terraform runs one when a resource's lifecycle action_trigger fires
// or when `terraform apply -invoke=action.<type>.<name>` asks for it, and
// remembers nothing of it afterwards. The provider's actions ask the Kopia
// server to do something it otherwise does on a schedule or on a click in
// the Kopia UI: send a test notification, or take a snapshot now.

// sendActionProgress reports a step of an invocation to Terraform's UI.
func sendActionProgress(resp *action.InvokeResponse, message string) {
	if resp.SendProgress != nil {
		resp.SendProgress(action.InvokeProgressEvent{Message: message})
	}
}

// addUnconfiguredActionError is the error for an action invoked before the
// provider was configured, which Terraform never does.
func addUnconfiguredActionError(diags *diag.Diagnostics) {
	diags.AddError(
		"Unconfigured Kopia provider",
		"The action ran without a configured provider. This is a bug in the provider.",
	)
}
