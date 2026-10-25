package provider

import (
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestScalarConversions(t *testing.T) {
	if got := stringPointerOrNil(types.StringValue("x")); got == nil || *got != "x" {
		t.Errorf("stringPointerOrNil(x) = %v", got)
	}
	if stringPointerOrNil(types.StringNull()) != nil || stringPointerOrNil(types.StringUnknown()) != nil {
		t.Error("stringPointerOrNil kept a null or unknown value")
	}
	if got := boolPointerOrNil(types.BoolValue(false)); got == nil || *got {
		t.Errorf("boolPointerOrNil(false) = %v, want a pointer to false", got)
	}
	if boolPointerOrNil(types.BoolNull()) != nil || boolPointerOrNil(types.BoolUnknown()) != nil {
		t.Error("boolPointerOrNil kept a null or unknown value")
	}
	if got := int64PointerOrNil(types.Int64Value(0)); got == nil || *got != 0 {
		t.Errorf("int64PointerOrNil(0) = %v, want a pointer to 0", got)
	}
	if int64PointerOrNil(types.Int64Null()) != nil || int64PointerOrNil(types.Int64Unknown()) != nil {
		t.Error("int64PointerOrNil kept a null or unknown value")
	}
	if got := float64PointerOrNil(types.Float64Value(0.5)); got == nil || *got != 0.5 {
		t.Errorf("float64PointerOrNil(0.5) = %v", got)
	}
	if float64PointerOrNil(types.Float64Null()) != nil || float64PointerOrNil(types.Float64Unknown()) != nil {
		t.Error("float64PointerOrNil kept a null or unknown value")
	}

	s, b, i, f := "x", false, int64(0), 0.0
	if got := stringValueOrNull(&s); !got.Equal(types.StringValue("x")) || !stringValueOrNull(nil).IsNull() {
		t.Errorf("stringValueOrNull = %v", got)
	}
	if got := boolValueOrNull(&b); !got.Equal(types.BoolValue(false)) || !boolValueOrNull(nil).IsNull() {
		t.Errorf("boolValueOrNull = %v", got)
	}
	if got := int64ValueOrNull(&i); !got.Equal(types.Int64Value(0)) || !int64ValueOrNull(nil).IsNull() {
		t.Errorf("int64ValueOrNull = %v", got)
	}
	if got := float64ValueOrNull(&f); !got.Equal(types.Float64Value(0)) || !float64ValueOrNull(nil).IsNull() {
		t.Errorf("float64ValueOrNull = %v", got)
	}

	if !stringValueOrNullIfEmpty("").IsNull() || !stringValueOrNullIfEmpty("x").Equal(types.StringValue("x")) {
		t.Error("stringValueOrNullIfEmpty")
	}
	if !int64ValueOrNullIfZero(0).IsNull() || !int64ValueOrNullIfZero(-1).Equal(types.Int64Value(-1)) {
		t.Error("int64ValueOrNullIfZero")
	}
	if !float64ValueOrNullIfZero(0).IsNull() || !float64ValueOrNullIfZero(1.5).Equal(types.Float64Value(1.5)) {
		t.Error("float64ValueOrNullIfZero")
	}
}

func TestCollectionConversions(t *testing.T) {
	if got := stringListValue(nil); got.IsNull() || len(got.Elements()) != 0 {
		t.Errorf("stringListValue(nil) = %v, want an empty list", got)
	}
	if !stringListOrNull(nil).IsNull() || !stringListOrNull([]string{}).IsNull() {
		t.Error("stringListOrNull of nothing is not null")
	}
	list := stringListOrNull([]string{"b", "a"})
	if values, known := stringsFromList(list); !known || !reflect.DeepEqual(values, []string{"b", "a"}) {
		t.Errorf("stringsFromList round trip = %v, %v, want [b a] in order", values, known)
	}

	if got := stringSetValue(nil); got.IsNull() || len(got.Elements()) != 0 {
		t.Errorf("stringSetValue(nil) = %v, want an empty set", got)
	}
	if !stringSetOrNull(nil).IsNull() {
		t.Error("stringSetOrNull of nothing is not null")
	}
	if values, known := stringsFromSet(stringSetOrNull([]string{"b", "a"})); !known || !reflect.DeepEqual(values, []string{"a", "b"}) {
		t.Errorf("stringsFromSet = %v, %v, want [a b] sorted", values, known)
	}

	if !stringMapOrNull(nil).IsNull() {
		t.Error("stringMapOrNull of nothing is not null")
	}
	m := stringMapOrNull(map[string]string{"Authorization": "Bearer x"})
	if values, known := stringMapFromMap(m); !known || !reflect.DeepEqual(values, map[string]string{"Authorization": "Bearer x"}) {
		t.Errorf("stringMapFromMap round trip = %v, %v", values, known)
	}

	// Null collections convert to nil and count as known; unknown ones, or
	// ones with an unknown element, as a plan may hold, do not.
	if values, known := stringsFromList(types.ListNull(types.StringType)); values != nil || !known {
		t.Errorf("stringsFromList(null) = %v, %v", values, known)
	}
	if _, known := stringsFromList(types.ListUnknown(types.StringType)); known {
		t.Error("stringsFromList(unknown) is known")
	}
	partly := types.ListValueMust(types.StringType, []attr.Value{types.StringValue("a"), types.StringUnknown()})
	if _, known := stringsFromList(partly); known {
		t.Error("stringsFromList with an unknown element is known")
	}
	if _, known := stringsFromSet(types.SetUnknown(types.StringType)); known {
		t.Error("stringsFromSet(unknown) is known")
	}
	if values, known := stringsFromSet(types.SetNull(types.StringType)); values != nil || !known {
		t.Errorf("stringsFromSet(null) = %v, %v", values, known)
	}
	if _, known := stringMapFromMap(types.MapUnknown(types.StringType)); known {
		t.Error("stringMapFromMap(unknown) is known")
	}
	if values, known := stringMapFromMap(types.MapNull(types.StringType)); values != nil || !known {
		t.Errorf("stringMapFromMap(null) = %v, %v", values, known)
	}
	partlyMap := types.MapValueMust(types.StringType, map[string]attr.Value{"a": types.StringUnknown()})
	if _, known := stringMapFromMap(partlyMap); known {
		t.Error("stringMapFromMap with an unknown element is known")
	}
}
