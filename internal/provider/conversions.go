package provider

import (
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// These helpers convert between Terraform values and the pointer fields of the
// client's wire types. Kopia treats an absent field as "not set here", which
// for a policy means "inherit", so a null attribute maps to a nil pointer and
// is left out of the request body (every such field is tagged omitempty), and
// a field missing from a response maps back to null.

// stringPointerOrNil converts a Terraform string into a pointer for a request
// body: null and unknown become nil.
func stringPointerOrNil(v types.String) *string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	s := v.ValueString()
	return &s
}

// stringValueOrNull converts a pointer from an API response back into a
// Terraform string.
func stringValueOrNull(s *string) types.String {
	if s == nil {
		return types.StringNull()
	}
	return types.StringValue(*s)
}

// stringValueOrNullIfEmpty maps a plain string field to Terraform, treating ""
// as absent: with omitempty, Kopia never sends an empty one.
func stringValueOrNullIfEmpty(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// boolPointerOrNil converts a Terraform bool into a pointer for a request
// body: null and unknown become nil.
func boolPointerOrNil(v types.Bool) *bool {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	b := v.ValueBool()
	return &b
}

// boolValueOrNull converts a pointer from an API response back into a
// Terraform bool.
func boolValueOrNull(b *bool) types.Bool {
	if b == nil {
		return types.BoolNull()
	}
	return types.BoolValue(*b)
}

// int64PointerOrNil converts a Terraform integer into a pointer for a request
// body: null and unknown become nil.
func int64PointerOrNil(v types.Int64) *int64 {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	i := v.ValueInt64()
	return &i
}

// int64ValueOrNull converts a pointer from an API response back into a
// Terraform integer.
func int64ValueOrNull(i *int64) types.Int64 {
	if i == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*i)
}

// int64ValueOrNullIfZero maps a plain integer field to Terraform, treating 0
// as absent: with omitempty, Kopia cannot tell a 0 from a field left out.
func int64ValueOrNullIfZero(i int64) types.Int64 {
	if i == 0 {
		return types.Int64Null()
	}
	return types.Int64Value(i)
}

// float64PointerOrNil converts a Terraform number into a pointer for a
// request body: null and unknown become nil.
func float64PointerOrNil(v types.Float64) *float64 {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	f := v.ValueFloat64()
	return &f
}

// float64ValueOrNull converts a pointer from an API response back into a
// Terraform number.
func float64ValueOrNull(f *float64) types.Float64 {
	if f == nil {
		return types.Float64Null()
	}
	return types.Float64Value(*f)
}

// float64ValueOrNullIfZero maps a plain number field to Terraform, treating 0
// as absent.
func float64ValueOrNullIfZero(f float64) types.Float64 {
	if f == 0 {
		return types.Float64Null()
	}
	return types.Float64Value(f)
}

// stringElements converts strings into Terraform string values.
func stringElements(values []string) []attr.Value {
	elements := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elements = append(elements, types.StringValue(v))
	}
	return elements
}

// stringListValue converts strings into a known list, empty for nil.
func stringListValue(values []string) types.List {
	return types.ListValueMust(types.StringType, stringElements(values))
}

// stringListOrNull converts strings into a list, or null when there are none.
// Kopia omits empty lists (omitempty), so a response cannot tell an empty list
// from an absent one, and both read back as null.
func stringListOrNull(values []string) types.List {
	if len(values) == 0 {
		return types.ListNull(types.StringType)
	}
	return stringListValue(values)
}

// stringSetValue converts strings into a known set, empty for nil.
func stringSetValue(values []string) types.Set {
	return types.SetValueMust(types.StringType, stringElements(values))
}

// stringSetOrNull converts strings into a set, or null when there are none.
func stringSetOrNull(values []string) types.Set {
	if len(values) == 0 {
		return types.SetNull(types.StringType)
	}
	return stringSetValue(values)
}

// stringMapOrNull converts a map of strings into a Terraform map, or null when
// it is empty.
func stringMapOrNull(values map[string]string) types.Map {
	if len(values) == 0 {
		return types.MapNull(types.StringType)
	}
	elements := make(map[string]attr.Value, len(values))
	for k, v := range values {
		elements[k] = types.StringValue(v)
	}
	return types.MapValueMust(types.StringType, elements)
}

// stringsOf extracts the strings of a collection's elements. known is false
// when any element is unknown, or is not a string at all, which only a schema
// bug could cause; nothing should then be sent.
func stringsOf(elements []attr.Value) (values []string, known bool) {
	values = make([]string, 0, len(elements))
	for _, element := range elements {
		s, ok := element.(types.String)
		if !ok || s.IsUnknown() {
			return nil, false
		}
		if s.IsNull() {
			continue
		}
		values = append(values, s.ValueString())
	}
	return values, true
}

// stringsFromList returns the elements of a list of strings in order: nil for
// a null list, and known = false while the list or any element is unknown,
// as in a plan, so that callers send nothing then.
func stringsFromList(v types.List) (values []string, known bool) {
	if v.IsUnknown() {
		return nil, false
	}
	if v.IsNull() {
		return nil, true
	}
	return stringsOf(v.Elements())
}

// stringsFromSet is stringsFromList for a set. The strings are sorted, so
// the request bodies built from them do not depend on element order.
func stringsFromSet(v types.Set) (values []string, known bool) {
	if v.IsUnknown() {
		return nil, false
	}
	if v.IsNull() {
		return nil, true
	}
	values, known = stringsOf(v.Elements())
	sort.Strings(values)
	return values, known
}

// stringMapFromMap returns the elements of a map of strings: nil for a null
// map, and known = false while the map or any element is unknown.
func stringMapFromMap(v types.Map) (values map[string]string, known bool) {
	if v.IsUnknown() {
		return nil, false
	}
	if v.IsNull() {
		return nil, true
	}
	values = make(map[string]string, len(v.Elements()))
	for k, element := range v.Elements() {
		s, ok := element.(types.String)
		if !ok || s.IsUnknown() {
			return nil, false
		}
		if !s.IsNull() {
			values[k] = s.ValueString()
		}
	}
	return values, true
}
