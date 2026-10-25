package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// policyTargetValidator accepts what kopiaclient.ParseTarget accepts, and
// reports why it refuses anything else.
type policyTargetValidator struct{}

func (policyTargetValidator) Description(_ context.Context) string {
	return "must be (global), @HOST, USER@HOST or USER@HOST:PATH with an absolute PATH"
}

func (v policyTargetValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (policyTargetValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := kopiaclient.ParseTarget(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid policy target", policyTargetHelp(err))
	}
}

// policyTargetHelp explains a target ParseTarget refused.
func policyTargetHelp(err error) string {
	return "A policy target is `(global)`, `@host` for every user of a host, `user@host`, or " +
		"`user@host:path` with an absolute path such as `/data/app`, `C:\\Data` or `\\\\server\\share`, " +
		"written exactly as Kopia writes it. " + err.Error() + "."
}

// policyInt64Check adapts a predicate into an integer validator, the way
// stringCheck does for strings.
type policyInt64Check struct {
	description string
	check       func(int64) bool
}

func (v policyInt64Check) Description(_ context.Context) string { return v.description }

func (v policyInt64Check) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v policyInt64Check) ValidateInt64(_ context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() || v.check(req.ConfigValue.ValueInt64()) {
		return
	}
	resp.Diagnostics.AddAttributeError(
		req.Path,
		"Invalid Attribute Value",
		fmt.Sprintf("Attribute %s %s, got: %d", req.Path, v.description, req.ConfigValue.ValueInt64()),
	)
}

// policySizeLimitValidator accepts a size in bytes for one of Kopia's plain
// size limits: positive, or -1 for "no limit". Kopia cannot store 0, which
// would mean "inherit", and any negative number means "no limit", so -1 is the
// one spelling of it that is accepted.
func policySizeLimitValidator() validator.Int64 {
	return policyInt64Check{
		description: "must be at least 1, or -1 for no limit",
		check:       func(v int64) bool { return v >= 1 || v == -1 },
	}
}

// policyNonZeroValidator refuses 0 for parallel_upload_above_size, which Kopia
// divides by (snapshot/upload/upload.go:166-173).
func policyNonZeroValidator() validator.Int64 {
	return policyInt64Check{
		description: "must not be 0: use a negative number never to upload in parallel",
		check:       func(v int64) bool { return v != 0 },
	}
}

// policyExtensionValidator accepts a file extension as Kopia's compression
// lists hold it.
func policyExtensionValidator() validator.String {
	return stringvalidator.RegexMatches(policyExtensionPattern, "must be a file extension with its leading dot, such as .gz")
}

// policyManualSchedulingValidator enforces Kopia's one scheduling rule that
// does not need a cron parser: with manual = true, the scheduling section must
// hold nothing else (snapshot/policy/scheduling_policy.go:276-280, which
// compares the whole section with {Manual: true}). An explicit
// run_missed = false is something else too; no_parent_time_of_day = false is
// not, because it is never sent.
type policyManualSchedulingValidator struct{}

// policyManualConflicts are the scheduling attributes manual excludes.
var policyManualConflicts = []string{"interval_seconds", "time_of_day", "no_parent_time_of_day", "cron", "run_missed"}

func (policyManualSchedulingValidator) Description(_ context.Context) string {
	return "manual = true cannot be combined with any other scheduling attribute"
}

func (policyManualSchedulingValidator) MarkdownDescription(_ context.Context) string {
	return "`manual = true` cannot be combined with any other scheduling attribute"
}

func (policyManualSchedulingValidator) ValidateObject(_ context.Context, req validator.ObjectRequest, resp *validator.ObjectResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	attrs := req.ConfigValue.Attributes()
	manual, ok := attrs["manual"].(types.Bool)
	if !ok || manual.IsNull() || manual.IsUnknown() || !manual.ValueBool() {
		return
	}
	for _, name := range policyManualConflicts {
		value, ok := attrs[name]
		// An unknown value may still turn out null; the plan-time check
		// with Kopia catches it once it is known.
		if !ok || value.IsNull() || value.IsUnknown() {
			continue
		}
		if flag, isBool := value.(types.Bool); isBool && name == "no_parent_time_of_day" && !flag.ValueBool() {
			continue
		}
		resp.Diagnostics.AddAttributeError(
			req.Path.AtName(name),
			"Conflicting scheduling settings",
			fmt.Sprintf("%s cannot be set together with manual = true: Kopia refuses a manual scheduling "+
				"policy that sets anything else, even run_missed = false. Remove %s, or remove manual.",
				req.Path.AtName(name), name),
		)
	}
}

// policyCronEntryValidator warns about a cron entry Kopia would read
// differently from what it looks like. Kopia strips everything from the first
// "#" on as a comment before parsing an entry (scheduling_policy.go:292-294),
// which is harmless after a space, but turns cron's n-th weekday syntax, such
// as "0 0 * * 5#3", into an entry that runs every week. An entry that is
// nothing but a comment is skipped altogether. Whether the rest parses is
// checked with Kopia at plan time.
type policyCronEntryValidator struct{}

func (policyCronEntryValidator) Description(_ context.Context) string {
	return "should not use # other than for a comment"
}

func (v policyCronEntryValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (policyCronEntryValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	entry := req.ConfigValue.ValueString()
	schedule, comment, hasComment := strings.Cut(entry, "#")
	switch {
	case strings.TrimSpace(schedule) == "":
		resp.Diagnostics.AddAttributeWarning(
			req.Path,
			"Cron entry without a schedule",
			fmt.Sprintf("Kopia strips everything from a # on as a comment and skips an entry that leaves "+
				"nothing, so %q never triggers a snapshot.", entry),
		)
	case hasComment && schedule[len(schedule)-1] != ' ' && schedule[len(schedule)-1] != '\t':
		resp.Diagnostics.AddAttributeWarning(
			req.Path,
			"Cron entry truncated at #",
			fmt.Sprintf("Kopia treats everything from a # on as a comment, so it reads %q as %q and ignores "+
				"%q. Cron's n-th weekday syntax, such as 5#3, cannot be used with Kopia: it would run "+
				"every week. Put a space before # if it starts a comment.",
				entry, strings.TrimSpace(schedule), "#"+comment),
		)
	}
}

// policyIgnoreRuleValidator warns about ignore rules Kopia is likely to reject
// when it takes a snapshot. Kopia compiles the rules only then
// (fs/ignorefs/ignorefs.go:372-378), so a malformed rule fails every snapshot
// of the source rather than the apply; these two mistakes are the ones that
// can be spotted reliably without Kopia's matcher.
type policyIgnoreRuleValidator struct{}

func (policyIgnoreRuleValidator) Description(_ context.Context) string {
	return "should not end in an escaping backslash or leave a [ unclosed"
}

func (policyIgnoreRuleValidator) MarkdownDescription(_ context.Context) string {
	return "should not end in an escaping `\\` or leave a `[` unclosed"
}

func (policyIgnoreRuleValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	rule := req.ConfigValue.ValueString()
	escaped, inClass := false, false
	for i := 0; i < len(rule); i++ {
		switch {
		case escaped:
			escaped = false
		case rule[i] == '\\':
			escaped = true
		case rule[i] == '[':
			inClass = true
		case rule[i] == ']':
			inClass = false
		}
	}
	switch {
	case escaped:
		resp.Diagnostics.AddAttributeWarning(
			req.Path,
			"Ignore rule ends in a backslash",
			fmt.Sprintf("In ignore rules a backslash escapes the next character, and %q ends in one that "+
				"escapes nothing. Kopia checks the rules only when it takes a snapshot, which may then fail.", rule),
		)
	case inClass:
		resp.Diagnostics.AddAttributeWarning(
			req.Path,
			"Ignore rule leaves a [ unclosed",
			fmt.Sprintf("%q opens a character class with [ that no ] closes; escape it as \\[ to match a "+
				"literal bracket. Kopia checks the rules only when it takes a snapshot, which may then fail.", rule),
		)
	}
}
