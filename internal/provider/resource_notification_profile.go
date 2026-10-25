package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ resource.Resource                     = &notificationProfileResource{}
	_ resource.ResourceWithConfigure        = &notificationProfileResource{}
	_ resource.ResourceWithConfigValidators = &notificationProfileResource{}
	_ resource.ResourceWithImportState      = &notificationProfileResource{}
	_ resource.ResourceWithIdentity         = &notificationProfileResource{}
)

// NewNotificationProfileResource returns the kopia_notification_profile
// resource.
func NewNotificationProfileResource() resource.Resource { return &notificationProfileResource{} }

type notificationProfileResource struct {
	client *kopiaclient.Client
}

type notificationProfileModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	MinSeverity     types.String `tfsdk:"min_severity"`
	Email           types.Object `tfsdk:"email"`
	Pushover        types.Object `tfsdk:"pushover"`
	Webhook         types.Object `tfsdk:"webhook"`
	SendTestOnApply types.Bool   `tfsdk:"send_test_on_apply"`
}

type notificationEmailModel struct {
	SMTPServer   types.String `tfsdk:"smtp_server"`
	SMTPPort     types.Int64  `tfsdk:"smtp_port"`
	SMTPIdentity types.String `tfsdk:"smtp_identity"`
	SMTPUsername types.String `tfsdk:"smtp_username"`
	SMTPPassword types.String `tfsdk:"smtp_password"`
	From         types.String `tfsdk:"from"`
	To           types.String `tfsdk:"to"`
	CC           types.String `tfsdk:"cc"`
	Format       types.String `tfsdk:"format"`
}

type notificationPushoverModel struct {
	AppToken types.String `tfsdk:"app_token"`
	UserKey  types.String `tfsdk:"user_key"`
	Format   types.String `tfsdk:"format"`
	Endpoint types.String `tfsdk:"endpoint"`
}

type notificationWebhookModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Method   types.String `tfsdk:"method"`
	Format   types.String `tfsdk:"format"`
	Headers  types.Map    `tfsdk:"headers"`
}

// The object types of the three method attributes, for building their values
// outside a plan or state.
var (
	notificationEmailAttrTypes = map[string]attr.Type{
		"smtp_server":   types.StringType,
		"smtp_port":     types.Int64Type,
		"smtp_identity": types.StringType,
		"smtp_username": types.StringType,
		"smtp_password": types.StringType,
		"from":          types.StringType,
		"to":            types.StringType,
		"cc":            types.StringType,
		"format":        types.StringType,
	}
	notificationPushoverAttrTypes = map[string]attr.Type{
		"app_token": types.StringType,
		"user_key":  types.StringType,
		"format":    types.StringType,
		"endpoint":  types.StringType,
	}
	notificationWebhookAttrTypes = map[string]attr.Type{
		"endpoint": types.StringType,
		"method":   types.StringType,
		"format":   types.StringType,
		"headers":  types.MapType{ElemType: types.StringType},
	}
)

// notificationMethodPattern accepts an HTTP method in upper case. Kopia sends
// whatever the profile holds; HTTP methods are case-sensitive, so "post"
// reaches the service as an unknown method.
var notificationMethodPattern = regexp.MustCompile(`^[A-Z]+$`)

func (r *notificationProfileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_profile"
}

// notificationFormatAttribute is the `format` of a method. The default is
// written out rather than left to Kopia, so that what the provider stores is
// what it reads back.
func notificationFormatAttribute(defaultFormat string) schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		Computed: true,
		Default:  stringdefault.StaticString(defaultFormat),
		MarkdownDescription: "Message format: `txt` or `html`. Defaults to `" + defaultFormat + "`. Kopia accepts " +
			"no other value — its code comments mention `md`, but its senders refuse it — and picks the matching " +
			"template, plain text or HTML.",
		Validators: []validator.String{
			stringvalidator.OneOf(kopiaclient.NotificationFormatText, kopiaclient.NotificationFormatHTML),
		},
	}
}

// notificationSeverityDescription explains min_severity, for the resource and
// the data source.
const notificationSeverityDescription = "Kopia's severities, from the lowest, are `verbose`, `success`, " +
	"`report`, `warning` and `error`. The server's scheduler sends a snapshot report as `report`, or as " +
	"`warning` or `error` when the snapshot had problems; nothing Kopia sends today is below `report` except " +
	"the test notification, which ignores this setting, so `verbose`, `success` and `report` currently behave " +
	"the same. Kopia's API accepts any number here: one that has no name is shown as the number, such as `\"3\"`."

func (r *notificationProfileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Kopia notification profile: a destination — email, " +
			"[Pushover](https://pushover.net) or a webhook — to which the Kopia server sends its notifications, " +
			"such as snapshot reports, from a minimum severity on. Exactly one of `email`, `pushover` and " +
			"`webhook` must be set; moving from one to another changes the profile in place.\n\n" +
			"Kopia keeps profiles in the repository, so the server needs one connected, and applies a change to " +
			"the next notification without a restart. Kopia checks nothing when it saves a profile — an invalid " +
			"one is stored, and then silently skipped whenever a notification goes out — so the provider " +
			"validates every setting at plan time, and `send_test_on_apply` proves that delivery works.\n\n" +
			"Creating a profile under a name that already exists fails, because Kopia would overwrite it " +
			"without a word: import the existing profile instead. Destroying the resource deletes the profile. " +
			"Template overrides made for it with `kopia notification template set` are not deleted with it, " +
			"and apply again to a profile created later under the same name.\n\n" +
			"~> **Secrets are stored in state.** Kopia returns every setting whenever a profile is read — SMTP " +
			"password, Pushover tokens, webhook URL and headers included — so they are kept in Terraform state, " +
			"and changes made outside Terraform show up as drift. They are marked sensitive. Kopia itself also " +
			"writes a saved profile's settings, secrets included, to its log files at debug level, the level " +
			"those files record by default.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Same value as `name`, present because Terraform requires an `id`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the profile, unique on the server and case-sensitive. It must not " +
					"be empty, contain `/`, or be `.` or `..`: Kopia would store such a profile, but could then " +
					"neither read nor delete it. Changing it replaces the profile.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
					notificationProfileNameValidator(),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"min_severity": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("report"),
				MarkdownDescription: "Lowest severity of the notifications the profile receives. Defaults to " +
					"`report`, as for a profile made with `kopia notification profile configure`. " +
					notificationSeverityDescription + " Such a number is replaced on the next apply.",
				Validators: []validator.String{
					stringvalidator.OneOf(kopiaclient.NotificationSeverityNames()...),
				},
			},
			"email": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Send notifications by email. Kopia uses Go's `net/smtp`: it connects " +
					"without encryption and switches to TLS with STARTTLS when the server offers it, verifying the " +
					"server's certificate against the system's trusted roots. There is no implicit TLS, so SMTPS " +
					"on port 465 does not work, and no setting to skip certificate verification. Kopia sends the " +
					"password only over TLS, or to a server at `localhost`, `127.0.0.1` or `::1`.",
				Attributes: map[string]schema.Attribute{
					"smtp_server": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Host name or IP address of the SMTP server, without a port.",
						Validators:          []validator.String{notificationSMTPServerValidator()},
					},
					"smtp_port": schema.Int64Attribute{
						Optional:            true,
						Computed:            true,
						Default:             int64default.StaticInt64(kopiaclient.DefaultSMTPPort),
						MarkdownDescription: "Port of the SMTP server. Defaults to `587`, the submission port.",
						Validators:          []validator.Int64{int64validator.Between(1, 65535)},
					},
					"smtp_identity": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "Authorisation identity for SMTP PLAIN authentication, which almost " +
							"every server leaves empty. Leave it out for none. Requires `smtp_username`.",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
							notificationSMTPAuthValidator(),
						},
					},
					"smtp_username": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "User name to authenticate with. Leave it out to send without " +
							"authentication; when it is set, the server must offer SMTP authentication.",
						Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
					},
					"smtp_password": schema.StringAttribute{
						Optional:  true,
						Sensitive: true,
						MarkdownDescription: "Password to authenticate with. Leave it out for none. Requires " +
							"`smtp_username`.",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
							notificationSMTPAuthValidator(),
						},
					},
					"from": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "Sender address, used for the envelope and the `From:` header: a bare " +
							"address such as `kopia@example.com`. Kopia hands it to the SMTP server as it is, so a " +
							"display name such as `Kopia <kopia@example.com>` does not work.",
						Validators: []validator.String{notificationAddressValidator()},
					},
					"to": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "Recipients: bare addresses separated by commas without spaces, such " +
							"as `ops@example.com,oncall@example.com`. Kopia splits the list at the commas and hands " +
							"each part to the SMTP server as it is.",
						Validators: []validator.String{notificationAddressListValidator()},
					},
					"cc": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "Copy recipients, in the same form as `to`.\n\n~> **Has no effect.** " +
							"Kopia stores this setting, but its email sender neither sends to these addresses nor " +
							"adds a `Cc:` header. It is offered only so that a profile set up elsewhere can be " +
							"managed as it is.",
						Validators: []validator.String{notificationAddressListValidator()},
					},
					"format": notificationFormatAttribute(kopiaclient.DefaultEmailFormat),
				},
			},
			"pushover": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Send notifications through [Pushover](https://pushover.net).",
				Attributes: map[string]schema.Attribute{
					"app_token": schema.StringAttribute{
						Required:            true,
						Sensitive:           true,
						MarkdownDescription: "API token of the Pushover application to send as.",
						Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
					},
					"user_key": schema.StringAttribute{
						Required:            true,
						Sensitive:           true,
						MarkdownDescription: "Key of the Pushover user or group to send to.",
						Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
					},
					"format": notificationFormatAttribute(kopiaclient.DefaultPushoverFormat),
					"endpoint": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "URL of the Pushover message API. Leave it out for Pushover's own, " +
							"`https://api.pushover.net/1/messages.json`. Kopia keeps this setting for its tests and " +
							"its CLI does not offer it; set it only for a service that implements the same API.",
						Validators: []validator.String{notificationURLValidator()},
					},
				},
			},
			"webhook": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Send notifications as HTTP requests. The body is the rendered message, " +
					"without a `Content-Type` header unless `headers` sets one, and the subject goes in a " +
					"`Subject` header. Kopia sends from the server, honouring the proxy variables in its " +
					"environment and following redirects.\n\n" +
					"~> **Only HTTP 200 counts as delivered.** A service that answers 201, 202 or 204 makes every " +
					"notification fail.",
				Attributes: map[string]schema.Attribute{
					"endpoint": schema.StringAttribute{
						Required:  true,
						Sensitive: true,
						MarkdownDescription: "URL to send to, `http://` or `https://`. It is sensitive because " +
							"services such as Slack, Discord or ntfy put their token in the URL.",
						Validators: []validator.String{notificationSensitiveURLValidator()},
					},
					"method": schema.StringAttribute{
						Optional: true,
						Computed: true,
						Default:  stringdefault.StaticString(kopiaclient.DefaultWebhookMethod),
						MarkdownDescription: "HTTP method, in upper case. Defaults to `POST`; Kopia's CLI offers " +
							"`POST` and `PUT`.",
						Validators: []validator.String{
							stringvalidator.RegexMatches(notificationMethodPattern, "must be an HTTP method in upper case, such as POST or PUT"),
						},
					},
					"format": notificationFormatAttribute(kopiaclient.DefaultWebhookFormat),
					"headers": schema.MapAttribute{
						Optional:    true,
						Sensitive:   true,
						ElementType: types.StringType,
						MarkdownDescription: "Extra request headers, by name, such as `{ Authorization = \"Bearer " +
							"…\" }`. Names must be HTTP tokens and unique regardless of letter case; values must not " +
							"contain line breaks or other control characters, or begin or end with whitespace. A " +
							"`Subject` header replaces the subject Kopia sets. Kopia stores the headers as one " +
							"`Name: value` line each, which the provider writes sorted by name. Leave it out for " +
							"none.",
						Validators: []validator.Map{
							mapvalidator.SizeAtLeast(1),
							mapvalidator.NoNullValues(),
							notificationWebhookHeadersValidator{},
						},
					},
				},
			},
			"send_test_on_apply": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Have Kopia send its test notification through the profile after every " +
					"apply that creates or changes it, so that a wrong setting fails the apply rather than the " +
					"next real notification. Defaults to `false`. Turning it on changes nothing in Kopia and " +
					"sends nothing by itself, and every test delivers a real message.\n\n" +
					"Kopia waits for delivery without an overall time limit of its own — for email, without any — " +
					"so the provider's `request_timeout` bounds the test; when it expires, the outcome is unknown " +
					"and the message may still arrive. A failed test fails the apply, but the profile has been saved: after a " +
					"create, Terraform marks it tainted and replaces it, testing again, on the next apply; after " +
					"an update, the next apply tests again only if the profile changes again.",
			},
		},
	}
}

// ConfigValidators requires exactly one method.
func (r *notificationProfileResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("email"), path.MatchRoot("pushover"), path.MatchRoot("webhook")),
	}
}

func (r *notificationProfileResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = stringIdentitySchema("name", "Name of the notification profile.")
}

func (r *notificationProfileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// notificationSeverityString shows a severity by its name, or as the decimal
// number when Kopia has no name for it. No name can be configured as a
// number, so such a value always plans a change back to a named one.
func notificationSeverityString(severity int32) string {
	if name, ok := kopiaclient.NotificationSeverityName(severity); ok {
		return name
	}
	return strconv.FormatInt(int64(severity), 10)
}

// notificationSeverityValue is the inverse of notificationSeverityString.
func notificationSeverityValue(s string) (int32, bool) {
	if severity, ok := kopiaclient.ParseNotificationSeverity(s); ok {
		return severity, true
	}
	n, err := strconv.ParseInt(s, 10, 32)
	return int32(n), err == nil
}

// toProfile builds the profile the model describes. Every option is set, the
// optional ones left out as "", so that the profile Kopia stores is complete.
// It is only called on a plan at apply time and on state, where every value
// is known.
func (m *notificationProfileModel) toProfile(ctx context.Context, diags *diag.Diagnostics) kopiaclient.NotificationProfile {
	profile := kopiaclient.NotificationProfile{Name: m.Name.ValueString()}

	severity, ok := notificationSeverityValue(m.MinSeverity.ValueString())
	if !ok {
		diags.AddAttributeError(path.Root("min_severity"), "Invalid notification severity",
			fmt.Sprintf("%q is not one of Kopia's severities. This is a bug in the provider.", m.MinSeverity.ValueString()))
	}
	profile.MinSeverity = severity

	options := basetypes.ObjectAsOptions{}
	if !m.Email.IsNull() && !m.Email.IsUnknown() {
		var email notificationEmailModel
		diags.Append(m.Email.As(ctx, &email, options)...)
		profile.Email = &kopiaclient.EmailNotificationOptions{
			SMTPServer:   email.SMTPServer.ValueString(),
			SMTPPort:     email.SMTPPort.ValueInt64(),
			SMTPIdentity: email.SMTPIdentity.ValueString(),
			SMTPUsername: email.SMTPUsername.ValueString(),
			SMTPPassword: email.SMTPPassword.ValueString(),
			From:         email.From.ValueString(),
			To:           email.To.ValueString(),
			CC:           email.CC.ValueString(),
			Format:       email.Format.ValueString(),
		}
	}
	if !m.Pushover.IsNull() && !m.Pushover.IsUnknown() {
		var pushover notificationPushoverModel
		diags.Append(m.Pushover.As(ctx, &pushover, options)...)
		profile.Pushover = &kopiaclient.PushoverNotificationOptions{
			AppToken: pushover.AppToken.ValueString(),
			UserKey:  pushover.UserKey.ValueString(),
			Format:   pushover.Format.ValueString(),
			Endpoint: pushover.Endpoint.ValueString(),
		}
	}
	if !m.Webhook.IsNull() && !m.Webhook.IsUnknown() {
		var webhook notificationWebhookModel
		diags.Append(m.Webhook.As(ctx, &webhook, options)...)
		headers, _ := stringMapFromMap(webhook.Headers)
		profile.Webhook = &kopiaclient.WebhookNotificationOptions{
			Endpoint: webhook.Endpoint.ValueString(),
			Method:   webhook.Method.ValueString(),
			Format:   webhook.Format.ValueString(),
			Headers:  kopiaclient.FormatWebhookHeaders(headers),
		}
	}
	return profile
}

// fromProfile records a profile as Kopia returned it. The name is the one it
// was looked up by, which is the label Kopia found it under.
//
// Required options are kept as Kopia stores them, even when empty, so that a
// profile broken elsewhere plans a change; optional ones that are empty map
// to null, the only way to configure "none", which is why their validators
// refuse "". send_test_on_apply exists only in Terraform and keeps its value.
func (m *notificationProfileModel) fromProfile(name string, p *kopiaclient.NotificationProfile) {
	m.ID = types.StringValue(name)
	m.Name = types.StringValue(name)
	m.MinSeverity = types.StringValue(notificationSeverityString(p.MinSeverity))
	if m.SendTestOnApply.IsNull() || m.SendTestOnApply.IsUnknown() {
		m.SendTestOnApply = types.BoolValue(false)
	}

	m.Email = types.ObjectNull(notificationEmailAttrTypes)
	if o := p.Email; o != nil {
		m.Email = types.ObjectValueMust(notificationEmailAttrTypes, map[string]attr.Value{
			"smtp_server":   types.StringValue(o.SMTPServer),
			"smtp_port":     types.Int64Value(o.SMTPPort),
			"smtp_identity": stringValueOrNullIfEmpty(o.SMTPIdentity),
			"smtp_username": stringValueOrNullIfEmpty(o.SMTPUsername),
			"smtp_password": stringValueOrNullIfEmpty(o.SMTPPassword),
			"from":          types.StringValue(o.From),
			"to":            types.StringValue(o.To),
			"cc":            stringValueOrNullIfEmpty(o.CC),
			"format":        types.StringValue(o.Format),
		})
	}

	m.Pushover = types.ObjectNull(notificationPushoverAttrTypes)
	if o := p.Pushover; o != nil {
		m.Pushover = types.ObjectValueMust(notificationPushoverAttrTypes, map[string]attr.Value{
			"app_token": types.StringValue(o.AppToken),
			"user_key":  types.StringValue(o.UserKey),
			"format":    types.StringValue(o.Format),
			"endpoint":  stringValueOrNullIfEmpty(o.Endpoint),
		})
	}

	m.Webhook = types.ObjectNull(notificationWebhookAttrTypes)
	if o := p.Webhook; o != nil {
		m.Webhook = types.ObjectValueMust(notificationWebhookAttrTypes, map[string]attr.Value{
			"endpoint": types.StringValue(o.Endpoint),
			"method":   types.StringValue(o.Method),
			"format":   types.StringValue(o.Format),
			"headers":  stringMapOrNull(kopiaclient.ParseWebhookHeaders(o.Headers)),
		})
	}
}

// notificationProfileErrorText is err's text with the profile's secrets
// removed. The client already redacts what Kopia echoes from its writes; this
// covers every other error that reaches a diagnostic.
func notificationProfileErrorText(err error, profile kopiaclient.NotificationProfile) string {
	return kopiaclient.Redact(err.Error(), profile.Secrets()...)
}

func (r *notificationProfileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan notificationProfileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	profile := plan.toProfile(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Kopia's POST replaces a profile of the same name without a word, which
	// would silently take over one set up in the UI, by the CLI or by another
	// Terraform configuration. A profile this provider cannot represent
	// exists just the same.
	_, err := r.client.GetNotificationProfile(ctx, name)
	switch {
	case err == nil || errors.Is(err, kopiaclient.ErrUnsupportedNotificationProfile):
		resp.Diagnostics.AddAttributeError(
			path.Root("name"),
			"Kopia notification profile already exists",
			fmt.Sprintf("The Kopia server already has a notification profile named %q, and creating one under "+
				"the same name would overwrite it. To manage it with Terraform, import it, with an import block "+
				"whose id is %q or with `terraform import`; otherwise choose another name.", name, name),
		)
		return
	case !kopiaclient.IsNotFound(err):
		resp.Diagnostics.AddError(
			"Unable to create Kopia notification profile",
			fmt.Sprintf("Checking whether a notification profile named %q already exists failed: %s", name,
				notificationProfileErrorText(err, profile)),
		)
		return
	}

	if err := r.client.SaveNotificationProfile(ctx, profile); err != nil {
		resp.Diagnostics.AddError("Unable to create Kopia notification profile", notificationProfileErrorText(err, profile))
		return
	}

	// From here on the profile exists, so it is recorded in state even when
	// something below fails: Terraform then marks the resource tainted
	// instead of losing track of the profile.
	stored, err := r.client.GetNotificationProfile(ctx, name)
	if err != nil {
		plan.ID = types.StringValue(name)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)
		resp.Diagnostics.AddError(
			"Unable to read Kopia notification profile back after creation",
			fmt.Sprintf("Kopia saved notification profile %q, and it was recorded in state, but reading it back "+
				"failed, so it is marked tainted: the next apply replaces it. %s", name,
				notificationProfileErrorText(err, profile)),
		)
		return
	}

	plan.fromProfile(name, stored)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.SendTestOnApply.ValueBool() {
		r.sendTest(ctx, profile, true, &resp.Diagnostics)
	}
}

func (r *notificationProfileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state notificationProfileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()

	// Before the lookup, so that a refresh finding the profile gone still
	// reports one; see setIdentity.
	setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)

	profile, err := r.client.GetNotificationProfile(ctx, name)
	if err != nil {
		// The client keeps option values out of its read errors already; the
		// secrets last recorded are removed as well, for good measure. After
		// an import the state has none yet.
		var ignored diag.Diagnostics
		detail := notificationProfileErrorText(err, state.toProfile(ctx, &ignored))
		switch {
		case kopiaclient.IsNotFound(err):
			resp.State.RemoveResource(ctx)
		case errors.Is(err, kopiaclient.ErrUnsupportedNotificationProfile):
			resp.Diagnostics.AddError(
				"Unsupported Kopia notification profile",
				fmt.Sprintf("Kopia's notification profile %q cannot be managed with Terraform: only email, "+
					"Pushover and webhook profiles stored in the form Kopia reads can. Correct it in Kopia, or "+
					"stop managing it with `terraform state rm`. %s", name, detail),
			)
		default:
			resp.Diagnostics.AddError("Unable to read Kopia notification profile", detail)
		}
		return
	}

	state.fromProfile(name, profile)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	addNotificationProfileStoredWarnings(&resp.Diagnostics, name, profile.Warnings)
}

// addNotificationProfileStoredWarnings reports the stored options of a profile
// that Kopia itself cannot use, as a read finds them: the warnings of a
// NotificationProfile read from Kopia.
func addNotificationProfileStoredWarnings(diags *diag.Diagnostics, name string, warnings []string) {
	for _, warning := range warnings {
		diags.AddWarning(
			"Kopia cannot use notification profile as stored",
			fmt.Sprintf("Notification profile %q was saved by another client in a form Kopia cannot use. %s "+
				"Any change made through Terraform saves the profile again, and so does `terraform apply "+
				"-replace` on this resource.", name, warning),
		)
	}
}

func (r *notificationProfileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state notificationProfileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	profile := plan.toProfile(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	// The state may hold what another client stored, such as a severity
	// with no name; it only decides below whether the profile changes.
	var ignored diag.Diagnostics
	prior := state.toProfile(ctx, &ignored)

	// POST replaces the whole profile, and recreates one deleted since the
	// last refresh.
	if err := r.client.SaveNotificationProfile(ctx, profile); err != nil {
		resp.Diagnostics.AddError("Unable to update Kopia notification profile", notificationProfileErrorText(err, profile))
		return
	}

	stored, err := r.client.GetNotificationProfile(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to read Kopia notification profile back after update",
			fmt.Sprintf("Kopia saved notification profile %q, but reading it back failed. %s", name,
				notificationProfileErrorText(err, profile)),
		)
		return
	}

	plan.fromProfile(name, stored)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Turning send_test_on_apply on, or changing nothing else, does not
	// change the profile, and a test then would only send a message nobody
	// asked for.
	if plan.SendTestOnApply.ValueBool() && !reflect.DeepEqual(profile, prior) {
		r.sendTest(ctx, profile, false, &resp.Diagnostics)
	}
}

func (r *notificationProfileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state notificationProfileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Kopia answers 200 for a profile that is already gone.
	if err := r.client.DeleteNotificationProfile(ctx, state.Name.ValueString()); err != nil && !kopiaclient.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to delete Kopia notification profile", err.Error())
	}
}

// ImportState imports a profile by name, given either as the import id or,
// from Terraform 1.12, as the identity of an import block. Read fills in the
// rest, or drops a profile that does not exist, which Terraform reports.
func (r *notificationProfileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	name, _ := importedID(ctx, req, "name", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := kopiaclient.ValidateNotificationProfileName(name); err != nil {
		resp.Diagnostics.AddError(
			"Unexpected import identifier",
			"Import a notification profile by its name, as `kopia notification profile list` shows it: "+
				err.Error()+".",
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
	// The default, so that importing plans no change for a configuration
	// that leaves it out.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("send_test_on_apply"), false)...)
	setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)
}

// sendTest has Kopia send its test notification through profile, after an
// apply has saved it. A failure is an error on the method's attribute, after
// the state has been set.
func (r *notificationProfileResource) sendTest(ctx context.Context, profile kopiaclient.NotificationProfile, created bool, diags *diag.Diagnostics) {
	err := r.client.TestNotificationProfile(ctx, profile)
	if err == nil {
		tflog.Info(ctx, "Kopia delivered the test notification", map[string]any{"profile": profile.Name, "method": profile.MethodType()})
		return
	}

	var detail strings.Builder
	fmt.Fprintf(&detail, "Kopia saved notification profile %q, but could not deliver its test notification: %s",
		profile.Name, notificationProfileErrorText(err, profile))
	if requestTimedOut(err) {
		detail.WriteString("\n\n" + notificationTestTimeoutNote)
	}
	if created {
		detail.WriteString("\n\nThe profile is recorded in state but marked tainted, so the next apply replaces " +
			"it and tests it again.")
	} else {
		detail.WriteString("\n\nThe change is recorded in state, so the next apply tests the profile again only " +
			"if it changes again.")
	}
	diags.AddAttributeError(path.Root(profile.MethodType()), "Kopia could not send the test notification", detail.String())
}

// notificationTestTimeoutNote explains a test notification that outlasted the
// provider's request_timeout.
const notificationTestTimeoutNote = "The provider stopped waiting at its request_timeout, but Kopia carries on " +
	"delivering, so the outcome is unknown and the message may still arrive."

// requestTimedOut reports whether a request failed because the provider
// stopped waiting for it at its request_timeout. Kopia serves a request on a
// context the client cannot cancel, so it may have been served all the same:
// a test notification delivered (spec: notification.md §5.5), a snapshot
// queued.
func requestTimedOut(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}
