package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// The kopia_notification_test action lives in this file rather than in
// action_notification_test.go, which Go would take for a file of tests.

var (
	_ action.Action              = &notificationTestAction{}
	_ action.ActionWithConfigure = &notificationTestAction{}
)

// NewNotificationTestAction returns the kopia_notification_test action.
//
// Sending a test notification is a side effect with nothing to keep in state,
// which is what an action models; kopia_notification_profile's
// send_test_on_apply covers the profiles Terraform changes, and this covers
// any profile, whenever it is invoked.
func NewNotificationTestAction() action.Action { return &notificationTestAction{} }

type notificationTestAction struct {
	client *kopiaclient.Client
}

type notificationTestActionModel struct {
	Name types.String `tfsdk:"name"`
}

func (a *notificationTestAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_test"
}

func (a *notificationTestAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Has the Kopia server send its test notification through a stored notification " +
			"profile, to prove that the profile delivers: after it is created, after a change to the mail " +
			"server or the network, or on demand.\n\n" +
			"This is a Terraform *action*, not a resource, and needs **Terraform 1.14 or newer**. It runs only " +
			"when invoked — from a resource's `lifecycle { action_trigger { ... } }` or with " +
			"`terraform apply -invoke=action.kopia_notification_test.<name>` — and remembers nothing " +
			"afterwards. It reads the profile from Kopia and posts it to `POST /api/v1/testNotificationProfile`, " +
			"the call behind the test button of the Kopia UI: Kopia builds a sender from the profile and " +
			"delivers its test message through it, whatever the profile's `min_severity`, and the invocation " +
			"fails when it cannot. Any email, Pushover or webhook profile can be tested, whether " +
			"`kopia_notification_profile` manages it or it was set up in the Kopia UI or with the CLI.\n\n" +
			"~> **Every invocation delivers a real message.** Trigger it on `after_create` and `after_update` of " +
			"the profile, or invoke it by hand, rather than on every apply.\n\n" +
			"Kopia waits for the delivery without an overall time limit of its own — for email, without any — " +
			"so the provider's `request_timeout` bounds the test; when it expires, the outcome is unknown and " +
			"the message may still arrive. Kopia quotes a profile's settings in some of its errors, so the " +
			"secrets among them — SMTP password, Pushover tokens, webhook URL and headers — are replaced with " +
			"`<redacted>` in the errors the action reports.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the notification profile to test, as `kopia_notification_profile` " +
					"and the `kopia_notification_profiles` data source show it; case-sensitive. The profile must " +
					"exist when the action runs: to test one created in the same apply, trigger the action from " +
					"that `kopia_notification_profile` on `after_create`, with its `name` here.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
					notificationProfileNameValidator(),
				},
			},
		},
	}
}

func (a *notificationTestAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// Invoke tests the profile as Kopia stores it. Kopia's test endpoint takes a
// whole profile rather than a name (spec: notification.md §5.5), so the stored
// one is read first, and posted back as the provider would write it.
func (a *notificationTestAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var config notificationTestActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if a.client == nil {
		addUnconfiguredActionError(&resp.Diagnostics)
		return
	}
	name := config.Name.ValueString()

	profile, err := a.client.GetNotificationProfile(ctx, name)
	if err != nil {
		addNotificationTestReadError(name, err, &resp.Diagnostics)
		return
	}
	// A profile read from Kopia carries every option, with Kopia's defaults
	// filled in, and the test sends them written out the way the provider
	// writes them. That is also how Kopia reads them, except for the stored
	// forms Kopia cannot use, which the read reports as warnings.
	for _, warning := range profile.Warnings {
		resp.Diagnostics.AddWarning(
			"Kopia cannot use notification profile as stored",
			fmt.Sprintf("Notification profile %q was saved by another client in a form Kopia cannot use, so Kopia "+
				"sends nothing through it. %s The test sends the profile as the provider would save it, which "+
				"Kopia can use, so it may succeed all the same; save the profile again, through "+
				"kopia_notification_profile or the Kopia UI, to correct it.", name, warning),
		)
	}

	sendActionProgress(resp, fmt.Sprintf("Sending Kopia's test notification through %s profile %q",
		profile.MethodType(), name))
	if err := a.client.TestNotificationProfile(ctx, *profile); err != nil {
		resp.Diagnostics.AddError("Kopia could not send the test notification", notificationTestErrorDetail(*profile, err))
		return
	}

	tflog.Info(ctx, "Kopia delivered the test notification", map[string]any{"profile": name, "method": profile.MethodType()})
	sendActionProgress(resp, fmt.Sprintf("Kopia delivered its test notification through profile %q", name))
}

// addNotificationTestReadError explains why the profile to test could not be
// read. Kopia answers a missing profile with 500 INTERNAL "profile not
// found", which the client reports as not found.
func addNotificationTestReadError(name string, err error, diags *diag.Diagnostics) {
	switch {
	case kopiaclient.IsNotFound(err):
		diags.AddAttributeError(
			path.Root("name"),
			"Kopia notification profile not found",
			fmt.Sprintf("Kopia has no notification profile named %q, so there is nothing to test. Names are "+
				"case-sensitive, and the kopia_notification_profiles data source lists the profiles there are. To "+
				"test a profile created in the same apply, trigger the action from that kopia_notification_profile "+
				"on after_create, so that it runs once the profile exists. %s", name, err),
		)
	case errors.Is(err, kopiaclient.ErrUnsupportedNotificationProfile):
		// The client names the problem without quoting any setting.
		diags.AddAttributeError(
			path.Root("name"),
			"Unsupported Kopia notification profile",
			fmt.Sprintf("Kopia's notification profile %q cannot be tested through Terraform: only email, "+
				"Pushover and webhook profiles stored in the form Kopia reads can. %s", name, err),
		)
	case kopiaclient.IsNotConnected(err):
		diags.AddError("Unable to read Kopia notification profile",
			"The Kopia server has no repository connected, and Kopia keeps notification profiles in the "+
				"repository. "+err.Error())
	default:
		diags.AddError("Unable to read Kopia notification profile",
			fmt.Sprintf("Reading notification profile %q, to send it to Kopia's test, failed: %s", name, err))
	}
}

// notificationTestErrorDetail explains a failed test notification, with the
// profile's secrets redacted. Kopia answers every failure with 400
// MALFORMED_REQUEST, and tells a profile it cannot build a sender from apart
// from a failed delivery only by the message.
func notificationTestErrorDetail(profile kopiaclient.NotificationProfile, err error) string {
	detail := fmt.Sprintf("Kopia could not send its test notification through notification profile %q: %s",
		profile.Name, notificationProfileErrorText(err, profile))
	var apiErr *kopiaclient.Error
	if errors.As(err, &apiErr) && apiErr.Code == kopiaclient.CodeMalformedRequest {
		switch {
		case strings.HasPrefix(apiErr.Message, "unable to construct sender:"):
			detail += "\n\nKopia cannot build a sender from the profile's settings, so it sends no notification " +
				"through the profile at all until they are corrected."
		case strings.HasPrefix(apiErr.Message, "unable to send notification:"):
			detail += "\n\nKopia built a sender from the profile, but sending the message failed. The message " +
				"is sent from the Kopia server, so check that the server can reach the service and that the " +
				"service accepts the profile's credentials; a webhook counts as delivered only on HTTP 200."
		}
	}
	if requestTimedOut(err) {
		detail += "\n\n" + notificationTestTimeoutNote
	}
	return detail
}
