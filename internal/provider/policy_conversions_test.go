package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// policyTestFromJSON converts a policy as Kopia reports it, without a prior
// state, as an import or a data source does.
func policyTestFromJSON(t *testing.T, body string) policyBodyModel {
	t.Helper()
	var p kopiaclient.Policy
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	return policyBodyFromWire(&p, policyBodyModel{})
}

// policyTestToJSON is the body toWire produces for m.
func policyTestToJSON(t *testing.T, m policyBodyModel) string {
	t.Helper()
	encoded, err := json.Marshal(m.toWire())
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// policyTestNullBody is a policy body that sets nothing.
func policyTestNullBody() policyBodyModel {
	return policyBodyFromWire(nil, policyBodyModel{})
}

// Kopia's OSSnapshotMode is a number; the names are the CLI's. A number
// without a name, which another writer can store, is shown as it is, so that
// it shows up as a difference, and converts back to itself.
func TestPolicyVolumeShadowCopyConversion(t *testing.T) {
	for number, name := range map[int64]string{0: "never", 1: "always", 2: "when-available", 7: "7"} {
		n := number
		got := policyVolumeShadowCopyValue(&n)
		if got.ValueString() != name {
			t.Errorf("%d reads as %s, want %q", number, got, name)
		}
		back := policyVolumeShadowCopyToWire(got)
		if back == nil || *back != number {
			t.Errorf("%q converts back to %v, want %d", name, back, number)
		}
	}
	if got := policyVolumeShadowCopyValue(nil); !got.IsNull() {
		t.Errorf("nil reads as %s, want null", got)
	}
	for _, v := range []attr.Value{types.StringNull(), types.StringUnknown()} {
		if got := policyVolumeShadowCopyToWire(v); got != nil {
			t.Errorf("%s converts to %d, want nothing", v, *got)
		}
	}

	m := policyTestFromJSON(t, `{"osSnapshots":{"volumeShadowCopy":{"enable":7}}}`)
	if got := policyTestToJSON(t, m); got != `{"osSnapshots":{"volumeShadowCopy":{"enable":7}}}` {
		t.Errorf("round trip = %s", got)
	}
}

// Kopia stores lists as they were sent, so another writer may have stored
// duplicates; the sets that hold them keep each value once.
func TestPolicyConversionCollapsesDuplicatesInSets(t *testing.T) {
	m := policyTestFromJSON(t, `{"scheduling":{"timeOfDay":[{"hour":3,"min":0},{"hour":3,"min":0},{"hour":1,"min":30}]},"compression":{"neverCompress":[".gz",".gz",".zip"]}}`)
	times := policyValuesOf(m.Scheduling)["time_of_day"].(types.Set)
	if n := len(times.Elements()); n != 2 {
		t.Errorf("time_of_day has %d elements, want 2: %s", n, times)
	}
	never := policyValuesOf(m.Compression).set("never_compress")
	if len(never) != 2 || never[0] != ".gz" || never[1] != ".zip" {
		t.Errorf("never_compress = %v", never)
	}
	// The times go back sorted, whatever order the set holds them in.
	if got := policyTestToJSON(t, m); got != `{"scheduling":{"timeOfDay":[{"hour":1,"min":30},{"hour":3,"min":0}]},"compression":{"neverCompress":[".gz",".zip"]}}` {
		t.Errorf("round trip = %s", got)
	}
}

// An empty action is a value of its own in Kopia, one that fails every
// snapshot; it reads as an object of nulls rather than as no action, so that
// a configuration without it shows a difference.
func TestPolicyConversionKeepsAnEmptyAction(t *testing.T) {
	m := policyTestFromJSON(t, `{"actions":{"afterFolder":{}}}`)
	actions := policyValuesOf(m.Actions)
	if actions == nil {
		t.Fatal("actions is null")
	}
	if after := actions["after_folder"]; after.IsNull() {
		t.Error("the empty after_folder action reads as null")
	}
	if before := actions["before_folder"]; !before.IsNull() {
		t.Errorf("before_folder = %s, want null", before)
	}
}

// A body that sets nothing, or holds unknown values, sends nothing.
func TestPolicyConversionOfNothing(t *testing.T) {
	if got := policyTestToJSON(t, policyTestNullBody()); got != "{}" {
		t.Errorf("a null body converts to %s", got)
	}
	m := policyTestNullBody()
	m.Retention = types.ObjectUnknown(policyObjectTypes("retention"))
	m.NoParent = types.BoolUnknown()
	if got := policyTestToJSON(t, m); got != "{}" {
		t.Errorf("an unknown body converts to %s", got)
	}

	// policyBodyObject wraps a body as the `effective` object of
	// kopia_effective_policy.
	object := policyBodyObject(policyTestNullBody())
	if object.IsNull() || !object.Attributes()["retention"].IsNull() {
		t.Errorf("effective object = %s", object)
	}
}

// Plain fields are read as null when Kopia leaves them out, unless the prior
// value was a false that Kopia cannot store.
func TestPolicyPlainBool(t *testing.T) {
	tests := []struct {
		wire  bool
		prior attr.Value
		want  types.Bool
	}{
		{true, nil, types.BoolValue(true)},
		{true, types.BoolValue(false), types.BoolValue(true)},
		{false, nil, types.BoolNull()},
		{false, types.BoolNull(), types.BoolNull()},
		{false, types.BoolUnknown(), types.BoolNull()},
		{false, types.BoolValue(true), types.BoolNull()},
		{false, types.BoolValue(false), types.BoolValue(false)},
	}
	for _, tc := range tests {
		if got := policyPlainBool(tc.wire, tc.prior); !got.Equal(tc.want) {
			t.Errorf("policyPlainBool(%v, %v) = %s, want %s", tc.wire, tc.prior, got, tc.want)
		}
	}
}
