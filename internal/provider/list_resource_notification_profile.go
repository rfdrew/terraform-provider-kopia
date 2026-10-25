package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/list"
	listschema "github.com/hashicorp/terraform-plugin-framework/list/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

var (
	_ list.ListResource              = &notificationProfileListResource{}
	_ list.ListResourceWithConfigure = &notificationProfileListResource{}
)

// NewNotificationProfileListResource returns the kopia_notification_profile
// list resource.
func NewNotificationProfileListResource() list.ListResource {
	return &notificationProfileListResource{}
}

type notificationProfileListResource struct {
	client *kopiaclient.Client
}

func (l *notificationProfileListResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_profile"
}

// The list takes no arguments. Kopia's listing has no filter, a profile is
// imported by its name alone, and a server holds a handful of profiles, so a
// filter by name or method would only repeat what the results show.
func (l *notificationProfileListResource) ListResourceConfigSchema(_ context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	resp.Schema = listschema.Schema{
		MarkdownDescription: "Lists the notification profiles of the Kopia server that " +
			"`kopia_notification_profile` can manage — email, Pushover and webhook profiles — for `terraform " +
			"query` (Terraform 1.14 and later), sorted by name. Each result is named after the profile and " +
			"carries its name as the identity of `kopia_notification_profile`, ready for an `import` block.\n\n" +
			"Listing reads no setting of any profile. With `include_resource = true` every profile is also read " +
			"the way Terraform reads one right after importing it, which takes one request per profile and puts " +
			"its settings in the results — SMTP password, Pushover tokens, webhook URL and headers included, in " +
			"the attributes `kopia_notification_profile` marks sensitive — and in configuration generated from " +
			"them.\n\n" +
			"Profiles the resource cannot manage are left out with a warning that names them: those of another " +
			"method, such as `testsender`, those without a method, and those whose name Kopia's API cannot " +
			"address. With `include_resource = true`, so are those whose settings Kopia stores in a form the " +
			"provider cannot read, as reading them is what shows it; without it they are listed, and importing " +
			"one fails. The `kopia_notification_profiles` data source lists every profile. The list takes no " +
			"arguments.",
	}
}

func (l *notificationProfileListResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	l.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

// List reports the profiles of GET /api/v1/notificationProfiles, which holds
// each profile's name, method and severity but none of its settings, so
// including the resource reads every profile on its own.
func (l *notificationProfileListResource) List(ctx context.Context, req list.ListRequest, stream *list.ListResultsStream) {
	if l.client == nil {
		stream.Results = unconfiguredListError()
		return
	}

	summaries, err := l.client.ListNotificationProfiles(ctx)
	if err != nil {
		stream.Results = listError("Unable to list Kopia notification profiles", err.Error())
		return
	}
	names, refused := notificationProfileListNames(summaries)

	stream.Results = func(push func(list.ListResult) bool) {
		e := newListEmitter(req, push)
		if len(refused) > 0 {
			e.warn(
				"Kopia notification profiles left out of the list",
				"Kopia has notification profiles kopia_notification_profile cannot manage, so they are not "+
					"listed: "+strings.Join(refused, ", ")+". Only email, Pushover and webhook profiles whose "+
					"name Kopia's API can address can be imported. The kopia_notification_profiles data source "+
					"lists every profile.",
			)
		}
		for _, name := range names {
			if e.full() {
				return
			}
			result := req.NewListResult(ctx)
			result.DisplayName = name
			setIdentity(ctx, result.Identity, "name", name, &result.Diagnostics)
			if req.IncludeResource && !l.readListedProfile(ctx, name, &result, e) {
				continue
			}
			if !e.emit(result) {
				return
			}
		}
		e.flush()
	}
}

// readListedProfile fills result.Resource with what kopia_notification_profile
// records when Terraform imports the profile and refreshes it: the state its
// ImportState writes, send_test_on_apply false, converted by the resource's
// own fromProfile, with the same warnings its Read reports. It reports false
// for a profile to leave out: one deleted since it was listed, or one stored
// in a form the provider cannot read, which it warns about.
func (l *notificationProfileListResource) readListedProfile(ctx context.Context, name string, result *list.ListResult, e *listEmitter) bool {
	profile, err := l.client.GetNotificationProfile(ctx, name)
	switch {
	case kopiaclient.IsNotFound(err):
		return false
	case errors.Is(err, kopiaclient.ErrUnsupportedNotificationProfile):
		// The client names the problem without quoting any setting.
		e.warn(
			"Kopia notification profile left out of the list",
			fmt.Sprintf("Kopia stores notification profile %q in a form kopia_notification_profile cannot read, "+
				"so it is not listed: only email, Pushover and webhook profiles stored in the form Kopia reads "+
				"can be imported. %s", name, err),
		)
		return false
	case err != nil:
		result.Diagnostics.AddError("Unable to read Kopia notification profile",
			fmt.Sprintf("Reading notification profile %q, to include it in the list, failed: %s", name, err))
		return true
	}

	model := notificationProfileModel{SendTestOnApply: types.BoolValue(false)}
	model.fromProfile(name, profile)
	result.Diagnostics.Append(result.Resource.Set(ctx, &model)...)
	addNotificationProfileStoredWarnings(&result.Diagnostics, name, profile.Warnings)
	return true
}

// notificationProfileListNames picks the profiles the kopia_notification_profile
// list reports: each name once — Kopia lists a name twice should two clients
// have saved it at the same moment — sorted. Profiles the resource cannot
// manage are returned apart, quoted with the reason and sorted: a method other
// than email, pushover and webhook, no method at all, or a name Kopia's API
// cannot address. Of two entries for one name the later counts, as it is the
// one GET returns.
func notificationProfileListNames(summaries []kopiaclient.NotificationProfileSummary) (names, refused []string) {
	byName := make(map[string]kopiaclient.NotificationProfileSummary, len(summaries))
	for _, s := range summaries {
		byName[s.Name] = s
	}
	for name, s := range byName {
		switch {
		case kopiaclient.ValidateNotificationProfileName(name) != nil:
			refused = append(refused, strconv.Quote(name)+" (a name Kopia's API cannot address)")
		case s.MethodType == "":
			refused = append(refused, strconv.Quote(name)+" (no method)")
		case s.MethodType != kopiaclient.NotificationMethodEmail && s.MethodType != kopiaclient.NotificationMethodPushover &&
			s.MethodType != kopiaclient.NotificationMethodWebhook:
			refused = append(refused, strconv.Quote(name)+" (method "+strconv.Quote(s.MethodType)+")")
		default:
			names = append(names, name)
		}
	}
	slices.Sort(names)
	slices.Sort(refused)
	return names, refused
}
