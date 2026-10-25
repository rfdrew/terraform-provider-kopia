package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// identityForTest is a policy-like identity holding attrs, or the null
// identity the framework starts with when attrs is nil.
func identityForTest(t *testing.T, attrs map[string]any) *tfsdk.ResourceIdentity {
	t.Helper()
	s := stringIdentitySchema("target", "Target of the policy.")
	typ := s.Type().TerraformType(context.Background())
	if attrs == nil {
		return &tfsdk.ResourceIdentity{Schema: s, Raw: tftypes.NewValue(typ, nil)}
	}
	return &tfsdk.ResourceIdentity{Schema: s, Raw: tfValue(t, typ, attrs)}
}

func TestStringIdentitySchema(t *testing.T) {
	s := stringIdentitySchema("target", "Target of the policy.")
	attribute, ok := s.Attributes["target"]
	if !ok || len(s.Attributes) != 1 {
		t.Fatalf("attributes = %v, want target only", s.Attributes)
	}
	if !attribute.IsRequiredForImport() || attribute.GetDescription() != "Target of the policy." {
		t.Errorf("target = %+v, want required for import with the description", attribute)
	}
}

func TestSetIdentity(t *testing.T) {
	var diags diag.Diagnostics
	identity := identityForTest(t, nil)
	setIdentity(context.Background(), identity, "target", "docker@host1", &diags)
	requireNoErrors(t, diags)
	if got := identityAttr(t, identity, "target"); got != "docker@host1" {
		t.Fatalf("identity target = %q", got)
	}

	// Nothing to record: no identity, or no value.
	setIdentity(context.Background(), nil, "target", "docker@host1", &diags)
	empty := identityForTest(t, nil)
	setIdentity(context.Background(), empty, "target", "", &diags)
	requireNoErrors(t, diags)
	if !empty.Raw.IsNull() {
		t.Fatalf("an empty value was recorded: %s", empty.Raw)
	}
}

func TestImportedID(t *testing.T) {
	ctx := context.Background()

	var diags diag.Diagnostics
	id, byIdentity := importedID(ctx, resource.ImportStateRequest{ID: "docker@host1"}, "target", &diags)
	if id != "docker@host1" || byIdentity || diags.HasError() {
		t.Errorf("import id: got %q, %v, %v", id, byIdentity, diags)
	}

	id, byIdentity = importedID(ctx, resource.ImportStateRequest{Identity: identityForTest(t, map[string]any{"target": "(global)"})}, "target", &diags)
	if id != "(global)" || !byIdentity || diags.HasError() {
		t.Errorf("identity: got %q, %v, %v", id, byIdentity, diags)
	}

	_, byIdentity = importedID(ctx, resource.ImportStateRequest{Identity: identityForTest(t, map[string]any{"target": tftypes.NewValue(tftypes.String, nil)})}, "target", &diags)
	if !byIdentity {
		t.Error("an empty identity was not recognised as one")
	}
	requireErrorContaining(t, diags, "Missing import identity")
}
