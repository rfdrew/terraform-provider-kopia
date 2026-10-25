package provider

import (
	"context"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// Test harness shared by every resource and data source test in this package.
// Nothing here opens a socket: the fake kopia in fake_kopia_test.go answers
// through the client's Transport, and resources are driven either by calling
// their methods directly or through the framework's protocol server, the one
// Terraform talks to. harness_test.go exercises every helper below against a
// small test resource, and doubles as a worked example.
//
// # The fake server
//
//	fake := newFakeKopia()                         // GET /, CSRF and repo/status built in
//	fake.reply("GET /api/v1/policy", 200, json)    // fixed answer; replies() for a sequence
//	fake.replyError(route, 400, kopiaclient.CodeNotFound, "policy not found")
//	fake.handle(route, func(w, r) { ... })         // full control; writeFakeJSON(w, v) helps
//	c := fake.client(t)                            // *kopiaclient.Client wired to the fake
//	fake.called(route), fake.count(route), fake.routes(), fake.apiRoutes()
//	fake.query(t, route), fake.body(t, route), fake.lastCall(t, route).body (raw bytes)
//	fake.restart()                                 // invalidates CSRF tokens, as a real restart does
//
// Routes are "METHOD /path" without the query string; handlers read
// r.URL.Query(), and tests read the recorded query with fake.query.
//
// # Values
//
// Plans, states and configurations are built as tftypes.Value from plain Go
// values, against the object type of a schema:
//
//	resourceValue(t, r, map[string]any{"target": "(global)", "retention": map[string]any{"keep_latest": 10}})
//	dataSourceValue(t, d, attrs)       // the same for a data source
//	nullResourceValue(t, r)            // the null object: no prior state, or a destroy plan
//	modelValue(t, r, &model)           // from a model struct with tfsdk tags instead
//	tfValue(t, typ, v)                 // any type, e.g. h.resourceType(...) or a nested attribute's
//
// tfValue (and every builder above) accepts: nil for null, tfUnknown for an
// unknown value, a tftypes.Value as it is, string, bool, any Go integer or
// float (or *big.Float) for a number, a slice for a list or set, and a map
// with string keys for a map or an object. Attributes an object leaves out are
// null, so a nil or empty map is an object of nulls, not the null object
// (nullResourceValue); an attribute the type does not have fails the test.
//
// Reading values back: valueAt(t, v, "retention", "keep_latest") walks
// attributes, map keys (strings) and list elements (ints); stringAt, int64At,
// float64At, boolAt and stringsAt convert, and isNullAt and isUnknownAt test.
//
// # Schemas and framework values
//
//	resourceSchema(t, r), dataSourceSchema(t, d), resourceIdentitySchema(t, r)
//	requireValidResourceSchema(t, r), requireValidDataSourceSchema(t, d) // ValidateImplementation
//	resourceType(t, r), dataSourceType(t, d)          // the schemas' tftypes object types
//	stateOf(t, r, raw), planOf(t, r, raw), configOf(t, r, raw) // tfsdk.State, Plan, Config
//	nullIdentity(t, r), identityOf(t, r, attrs)       // *tfsdk.ResourceIdentity
//
// # Calling resource and data source methods directly
//
//	resp := createResource(t, r, config, plan)            // resource.CreateResponse
//	resp := readResource(t, r, prior)                     // resource.ReadResponse
//	resp := updateResource(t, r, config, plan, prior)     // resource.UpdateResponse
//	resp := deleteResource(t, r, prior)                   // resource.DeleteResponse
//	resp := importResource(t, r, "id")                    // resource.ImportStateResponse
//	resp := importResourceByIdentity(t, r, map[string]any{"target": "(global)"})
//	resp := modifyResourcePlan(t, r, config, plan, prior) // resource.ModifyPlanResponse
//	diags := validateResourceConfig(t, r, config)         // ValidateConfig and ConfigValidators
//	resp := readDataSource(t, d, config)                  // datasource.ReadResponse
//	resp := invokeActionDirectly(t, a, attrs)             // action.InvokeResponse, without validation
//
// Responses start out the way the framework initialises them (Create: null
// state; Read, Update and Delete: the prior state; ModifyPlan: the plan;
// a data source: the configuration). A resource with an identity gets a null
// identity in the request and the response, so setIdentity works; read it
// with identityAttr(t, resp.Identity, "target"). Plans are not computed for
// direct calls: pass what Terraform would plan, with tfUnknown for computed
// values. Private state is not available to direct calls, and attribute
// validators, defaults and plan modifiers do not run: use the protocol
// harness for those.
//
// # Driving the protocol server
//
//	h := newProtocolHarness(t, fake)                   // provider.go's registrations
//	h := newProtocolHarness(t, fake, withResources(NewPolicyResource), withDataSources(...))
//	inst := h.create(t, "kopia_policy", config)        // validate, plan and apply, as `terraform apply` does
//	inst = h.update(t, inst, config)                   // plan and apply a change
//	inst = h.refresh(t, inst)                          // ReadResource; inst.state is null when it was removed
//	h.destroy(t, inst)
//	inst = h.importByID(t, "kopia_policy", "(global)") // ImportResourceState, then ReadResource
//	inst = h.importByIdentity(t, "kopia_policy", map[string]any{"target": "(global)"})
//	state := h.readDataSource(t, "kopia_policies", config)
//
// The harness provider hands the fake's client to every object instead of
// running Configure, which provider_configure_test.go covers. A
// resourceInstance carries state, identity and private data from one step to
// the next, as Terraform does.
//
// Those calls fail the test on an error diagnostic. For the diagnostics
// themselves, or to look at a plan, use the single steps, which return the
// raw protocol responses: h.validateResource, h.validateDataSource,
// h.plan(t, typeName, prior, config) (prior nil for a create, config null for
// a destroy; the proposed new state is built the way Terraform core builds
// it, see proposedNewState), h.apply, h.read, h.importResource,
// h.readDataSourceResponse. Check them with
// requireNoProtocolErrors, requireProtocolErrorContaining and
// requireProtocolWarningContaining, and decode with h.decodeResource,
// h.decodeDataSource and h.identityValue. Configurations for these are built
// with h.resourceValue and h.dataSourceValue, from the served schema.
//
// List resources and actions are only reached through the protocol server:
//
//	results := h.list(t, "kopia_policy", h.listValue(t, "kopia_policy", attrs), includeResource, limit)
//	diags := listDiagnostics(results)                  // every result's diagnostics together
//	diags = h.validateList(t, "kopia_policy", config)
//	progress, diags := h.invokeAction(t, "kopia_snapshot_now", h.actionValue(t, "kopia_snapshot_now", attrs))
//	diags = h.validateAction(t, "kopia_snapshot_now", config)
//
// withListResources and withActions register other ones than provider.go's;
// a harness built withResources lists nothing unless it says which list
// resources to serve, because the framework refuses a list resource whose
// resource is not registered.
//
// # Diagnostics
//
// requireNoErrors, requireErrorContaining, requireNoErrorContaining,
// requireWarningContaining, requireNoWarnings, requireAttributeError and
// requireAttributeWarning check framework diagnostics; the attribute
// variants take a path such as "scheduling.time_of_day".
//
// # Acceptance tests
//
// provider_test.go has testAccProtoV6ProviderFactories, testAccPreCheck and
// providerConfig. captureAttr below keeps an attribute for a later step. This
// file imports terraform-plugin-testing's helper/resource as acc, because the
// framework's resource package already has the name; acceptance tests should
// do the same.

// --- values -----------------------------------------------------------------

// unknownValue is the type of tfUnknown.
type unknownValue struct{}

// tfUnknown marks a value as unknown wherever tfValue accepts a value.
var tfUnknown = unknownValue{}

// tfValue converts a Go value into a tftypes.Value of typ; see the package
// comment above for what it accepts.
func tfValue(t *testing.T, typ tftypes.Type, v any) tftypes.Value {
	t.Helper()
	value, err := toTFValue(typ, v)
	if err != nil {
		t.Fatalf("building a %s value: %v", typ, err)
	}
	return value
}

func toTFValue(typ tftypes.Type, v any) (tftypes.Value, error) {
	switch v := v.(type) {
	case nil:
		return tftypes.NewValue(typ, nil), nil
	case unknownValue:
		return tftypes.NewValue(typ, tftypes.UnknownValue), nil
	case tftypes.Value:
		if !v.Type().Equal(typ) {
			return tftypes.Value{}, fmt.Errorf("value of type %s where %s was expected", v.Type(), typ)
		}
		return v, nil
	}

	switch typ := typ.(type) {
	case tftypes.List:
		elements, err := toTFElements(typ.ElementType, v)
		if err != nil {
			return tftypes.Value{}, err
		}
		return tftypes.NewValue(typ, elements), nil
	case tftypes.Set:
		elements, err := toTFElements(typ.ElementType, v)
		if err != nil {
			return tftypes.Value{}, err
		}
		return tftypes.NewValue(typ, elements), nil
	case tftypes.Map:
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
			return tftypes.Value{}, fmt.Errorf("want a map with string keys for %s, got %T", typ, v)
		}
		elements := make(map[string]tftypes.Value, rv.Len())
		for _, key := range rv.MapKeys() {
			element, err := toTFValue(typ.ElementType, rv.MapIndex(key).Interface())
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("[%q]: %w", key.String(), err)
			}
			elements[key.String()] = element
		}
		return tftypes.NewValue(typ, elements), nil
	case tftypes.Object:
		attrs, ok := v.(map[string]any)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("want map[string]any for an object, got %T", v)
		}
		for name := range attrs {
			if _, ok := typ.AttributeTypes[name]; !ok {
				return tftypes.Value{}, fmt.Errorf("the object has no attribute %q (it has %s)", name, strings.Join(harnessSortedKeys(typ.AttributeTypes), ", "))
			}
		}
		values := make(map[string]tftypes.Value, len(typ.AttributeTypes))
		for name, attrType := range typ.AttributeTypes {
			value, err := toTFValue(attrType, attrs[name])
			if err != nil {
				return tftypes.Value{}, fmt.Errorf(".%s: %w", name, err)
			}
			values[name] = value
		}
		return tftypes.NewValue(typ, values), nil
	}

	switch {
	case typ.Is(tftypes.String):
		s, ok := v.(string)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("want a string, got %T", v)
		}
		return tftypes.NewValue(typ, s), nil
	case typ.Is(tftypes.Bool):
		b, ok := v.(bool)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("want a bool, got %T", v)
		}
		return tftypes.NewValue(typ, b), nil
	case typ.Is(tftypes.Number):
		n, err := toBigFloat(v)
		if err != nil {
			return tftypes.Value{}, err
		}
		return tftypes.NewValue(typ, n), nil
	}
	return tftypes.Value{}, fmt.Errorf("type %s is not supported", typ)
}

func toTFElements(elementType tftypes.Type, v any) ([]tftypes.Value, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return nil, fmt.Errorf("want a slice for a list or set, got %T", v)
	}
	elements := make([]tftypes.Value, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		element, err := toTFValue(elementType, rv.Index(i).Interface())
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		elements = append(elements, element)
	}
	return elements, nil
}

func toBigFloat(v any) (*big.Float, error) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return new(big.Float).SetInt64(rv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return new(big.Float).SetUint64(rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return big.NewFloat(rv.Float()), nil
	}
	if f, ok := v.(*big.Float); ok {
		return f, nil
	}
	return nil, fmt.Errorf("want a number, got %T", v)
}

func harnessSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resourceSchema returns r's schema, failing the test on a schema error.
func resourceSchema(t *testing.T, r resource.Resource) resourceschema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	requireNoErrors(t, resp.Diagnostics)
	return resp.Schema
}

// dataSourceSchema returns d's schema, failing the test on a schema error.
func dataSourceSchema(t *testing.T, d datasource.DataSource) datasourceschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	d.Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	requireNoErrors(t, resp.Diagnostics)
	return resp.Schema
}

// resourceIdentitySchema returns r's identity schema, failing the test when r
// has none.
func resourceIdentitySchema(t *testing.T, r resource.Resource) identityschema.Schema {
	t.Helper()
	withIdentity, ok := r.(resource.ResourceWithIdentity)
	if !ok {
		t.Fatalf("%T declares no identity", r)
	}
	var resp resource.IdentitySchemaResponse
	withIdentity.IdentitySchema(context.Background(), resource.IdentitySchemaRequest{}, &resp)
	requireNoErrors(t, resp.Diagnostics)
	return resp.IdentitySchema
}

// requireValidResourceSchema fails the test when r's schema breaks one of the
// framework's rules, which the framework would otherwise only report when
// Terraform first asks for the schema.
func requireValidResourceSchema(t *testing.T, r resource.Resource) {
	t.Helper()
	requireNoErrors(t, resourceSchema(t, r).ValidateImplementation(context.Background()))
}

// requireValidDataSourceSchema is requireValidResourceSchema for a data source.
func requireValidDataSourceSchema(t *testing.T, d datasource.DataSource) {
	t.Helper()
	requireNoErrors(t, dataSourceSchema(t, d).ValidateImplementation(context.Background()))
}

// resourceType is the object type of r's schema.
func resourceType(t *testing.T, r resource.Resource) tftypes.Type {
	t.Helper()
	return resourceSchema(t, r).Type().TerraformType(context.Background())
}

// dataSourceType is the object type of d's schema.
func dataSourceType(t *testing.T, d datasource.DataSource) tftypes.Type {
	t.Helper()
	return dataSourceSchema(t, d).Type().TerraformType(context.Background())
}

// resourceValue builds an object of r's schema; attributes attrs leaves out
// are null.
func resourceValue(t *testing.T, r resource.Resource, attrs map[string]any) tftypes.Value {
	t.Helper()
	return tfValue(t, resourceType(t, r), attrs)
}

// nullResourceValue is the null object of r's schema.
func nullResourceValue(t *testing.T, r resource.Resource) tftypes.Value {
	t.Helper()
	return tftypes.NewValue(resourceType(t, r), nil)
}

// dataSourceValue builds an object of d's schema; attributes attrs leaves out
// are null.
func dataSourceValue(t *testing.T, d datasource.DataSource, attrs map[string]any) tftypes.Value {
	t.Helper()
	return tfValue(t, dataSourceType(t, d), attrs)
}

// modelValue builds an object of r's schema from a model struct with tfsdk
// tags, as resp.State.Set would store it.
func modelValue(t *testing.T, r resource.Resource, model any) tftypes.Value {
	t.Helper()
	state := tfsdk.State{Schema: resourceSchema(t, r), Raw: nullResourceValue(t, r)}
	if diags := state.Set(context.Background(), model); diags.HasError() {
		t.Fatalf("building a value from %T: %v", model, diags)
	}
	return state.Raw
}

// stateOf, planOf and configOf wrap a value of r's schema.
func stateOf(t *testing.T, r resource.Resource, raw tftypes.Value) tfsdk.State {
	t.Helper()
	return tfsdk.State{Schema: resourceSchema(t, r), Raw: raw}
}

func planOf(t *testing.T, r resource.Resource, raw tftypes.Value) tfsdk.Plan {
	t.Helper()
	return tfsdk.Plan{Schema: resourceSchema(t, r), Raw: raw}
}

func configOf(t *testing.T, r resource.Resource, raw tftypes.Value) tfsdk.Config {
	t.Helper()
	return tfsdk.Config{Schema: resourceSchema(t, r), Raw: raw}
}

// nullIdentity is the empty identity the framework hands a resource with an
// identity, or nil for a resource without one.
func nullIdentity(t *testing.T, r resource.Resource) *tfsdk.ResourceIdentity {
	t.Helper()
	if _, ok := r.(resource.ResourceWithIdentity); !ok {
		return nil
	}
	s := resourceIdentitySchema(t, r)
	return &tfsdk.ResourceIdentity{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}
}

// identityOf builds an identity of r's identity schema.
func identityOf(t *testing.T, r resource.Resource, attrs map[string]any) *tfsdk.ResourceIdentity {
	t.Helper()
	s := resourceIdentitySchema(t, r)
	return &tfsdk.ResourceIdentity{Schema: s, Raw: tfValue(t, s.Type().TerraformType(context.Background()), attrs)}
}

// identityAttr reads a string attribute of an identity; "" when it is null.
func identityAttr(t *testing.T, identity *tfsdk.ResourceIdentity, attribute string) string {
	t.Helper()
	if identity == nil {
		t.Fatal("no identity")
	}
	var value types.String
	requireNoErrors(t, identity.GetAttribute(context.Background(), path.Root(attribute), &value))
	return value.ValueString()
}

// valueAt walks v: a string step selects an object attribute or a map
// element, an int step a list element. It fails the test when a step does not
// exist or crosses a null or unknown value.
func valueAt(t *testing.T, v tftypes.Value, steps ...any) tftypes.Value {
	t.Helper()
	current := v
	for i, step := range steps {
		if !current.IsKnown() || current.IsNull() {
			t.Fatalf("%v: cannot step into a null or unknown value at step %d", steps, i)
		}
		switch step := step.(type) {
		case string:
			var attrs map[string]tftypes.Value
			if err := current.As(&attrs); err != nil {
				t.Fatalf("%v: step %d (%q): %v", steps, i, step, err)
			}
			next, ok := attrs[step]
			if !ok {
				t.Fatalf("%v: step %d: no attribute or key %q in %s", steps, i, step, current)
			}
			current = next
		case int:
			var elements []tftypes.Value
			if err := current.As(&elements); err != nil {
				t.Fatalf("%v: step %d ([%d]): %v", steps, i, step, err)
			}
			if step < 0 || step >= len(elements) {
				t.Fatalf("%v: step %d: index %d out of range, %d elements", steps, i, step, len(elements))
			}
			current = elements[step]
		default:
			t.Fatalf("%v: step %d is a %T, want a string or an int", steps, i, step)
		}
	}
	return current
}

// isNullAt reports whether the value at steps is null.
func isNullAt(t *testing.T, v tftypes.Value, steps ...any) bool {
	t.Helper()
	return valueAt(t, v, steps...).IsNull()
}

// isUnknownAt reports whether the value at steps is unknown.
func isUnknownAt(t *testing.T, v tftypes.Value, steps ...any) bool {
	t.Helper()
	return !valueAt(t, v, steps...).IsKnown()
}

// knownAt returns the value at steps, failing the test when it is null or
// unknown.
func knownAt(t *testing.T, v tftypes.Value, steps ...any) tftypes.Value {
	t.Helper()
	value := valueAt(t, v, steps...)
	if !value.IsKnown() || value.IsNull() {
		t.Fatalf("%v is %s, want a known value", steps, value)
	}
	return value
}

func stringAt(t *testing.T, v tftypes.Value, steps ...any) string {
	t.Helper()
	var s string
	if err := knownAt(t, v, steps...).As(&s); err != nil {
		t.Fatalf("%v: %v", steps, err)
	}
	return s
}

func boolAt(t *testing.T, v tftypes.Value, steps ...any) bool {
	t.Helper()
	var b bool
	if err := knownAt(t, v, steps...).As(&b); err != nil {
		t.Fatalf("%v: %v", steps, err)
	}
	return b
}

func int64At(t *testing.T, v tftypes.Value, steps ...any) int64 {
	t.Helper()
	n := new(big.Float)
	if err := knownAt(t, v, steps...).As(&n); err != nil {
		t.Fatalf("%v: %v", steps, err)
	}
	i, accuracy := n.Int64()
	if accuracy != big.Exact {
		t.Fatalf("%v = %s, want an integer", steps, n)
	}
	return i
}

func float64At(t *testing.T, v tftypes.Value, steps ...any) float64 {
	t.Helper()
	n := new(big.Float)
	if err := knownAt(t, v, steps...).As(&n); err != nil {
		t.Fatalf("%v: %v", steps, err)
	}
	f, _ := n.Float64()
	return f
}

// stringsAt returns the elements of a list or set of strings, in their order
// in the value.
func stringsAt(t *testing.T, v tftypes.Value, steps ...any) []string {
	t.Helper()
	var elements []tftypes.Value
	if err := knownAt(t, v, steps...).As(&elements); err != nil {
		t.Fatalf("%v: %v", steps, err)
	}
	values := make([]string, 0, len(elements))
	for i, element := range elements {
		var s string
		if err := element.As(&s); err != nil {
			t.Fatalf("%v[%d]: %v", steps, i, err)
		}
		values = append(values, s)
	}
	return values
}

// --- direct calls -----------------------------------------------------------

// createResource runs r.Create with config and plan.
func createResource(t *testing.T, r resource.Resource, config, plan tftypes.Value) resource.CreateResponse {
	t.Helper()
	resp := resource.CreateResponse{State: stateOf(t, r, nullResourceValue(t, r)), Identity: nullIdentity(t, r)}
	r.Create(context.Background(), resource.CreateRequest{
		Config:   configOf(t, r, config),
		Plan:     planOf(t, r, plan),
		Identity: nullIdentity(t, r),
	}, &resp)
	return resp
}

// readResource runs r.Read on prior.
func readResource(t *testing.T, r resource.Resource, prior tftypes.Value) resource.ReadResponse {
	t.Helper()
	resp := resource.ReadResponse{State: stateOf(t, r, prior.Copy()), Identity: nullIdentity(t, r)}
	r.Read(context.Background(), resource.ReadRequest{
		State:    stateOf(t, r, prior),
		Identity: nullIdentity(t, r),
	}, &resp)
	return resp
}

// updateResource runs r.Update from prior to plan. The response starts from
// the prior state, as the framework's does since v1.19.0, so an Update that
// fails before setting state leaves the prior state, not the plan.
func updateResource(t *testing.T, r resource.Resource, config, plan, prior tftypes.Value) resource.UpdateResponse {
	t.Helper()
	resp := resource.UpdateResponse{State: stateOf(t, r, prior.Copy()), Identity: nullIdentity(t, r)}
	r.Update(context.Background(), resource.UpdateRequest{
		Config:   configOf(t, r, config),
		Plan:     planOf(t, r, plan),
		State:    stateOf(t, r, prior),
		Identity: nullIdentity(t, r),
	}, &resp)
	return resp
}

// deleteResource runs r.Delete on prior.
func deleteResource(t *testing.T, r resource.Resource, prior tftypes.Value) resource.DeleteResponse {
	t.Helper()
	resp := resource.DeleteResponse{State: stateOf(t, r, prior.Copy())}
	r.Delete(context.Background(), resource.DeleteRequest{
		State:    stateOf(t, r, prior),
		Identity: nullIdentity(t, r),
	}, &resp)
	return resp
}

// importResource runs r.ImportState with an import id, as `terraform import`
// and an import block with `id` do.
func importResource(t *testing.T, r resource.Resource, id string) resource.ImportStateResponse {
	t.Helper()
	importer, ok := r.(resource.ResourceWithImportState)
	if !ok {
		t.Fatalf("%T does not implement ImportState", r)
	}
	resp := resource.ImportStateResponse{State: stateOf(t, r, nullResourceValue(t, r)), Identity: nullIdentity(t, r)}
	importer.ImportState(context.Background(), resource.ImportStateRequest{ID: id}, &resp)
	return resp
}

// importResourceByIdentity runs r.ImportState for an import block with
// `identity`.
func importResourceByIdentity(t *testing.T, r resource.Resource, identity map[string]any) resource.ImportStateResponse {
	t.Helper()
	importer, ok := r.(resource.ResourceWithImportState)
	if !ok {
		t.Fatalf("%T does not implement ImportState", r)
	}
	resp := resource.ImportStateResponse{State: stateOf(t, r, nullResourceValue(t, r)), Identity: identityOf(t, r, identity)}
	importer.ImportState(context.Background(), resource.ImportStateRequest{Identity: identityOf(t, r, identity)}, &resp)
	return resp
}

// modifyResourcePlan runs r.ModifyPlan. prior is the null object for a
// create, and plan is the null object for a destroy.
func modifyResourcePlan(t *testing.T, r resource.Resource, config, plan, prior tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	modifier, ok := r.(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatalf("%T does not implement ModifyPlan", r)
	}
	resp := resource.ModifyPlanResponse{Plan: planOf(t, r, plan.Copy())}
	modifier.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: configOf(t, r, config),
		Plan:   planOf(t, r, plan),
		State:  stateOf(t, r, prior),
	}, &resp)
	return resp
}

// validateResourceConfig runs r's ValidateConfig and ConfigValidators, as the
// framework does after the attribute validators.
func validateResourceConfig(t *testing.T, r resource.Resource, config tftypes.Value) diag.Diagnostics {
	t.Helper()
	ctx := context.Background()
	req := resource.ValidateConfigRequest{Config: configOf(t, r, config)}
	var diags diag.Diagnostics
	if withValidators, ok := r.(resource.ResourceWithConfigValidators); ok {
		for _, v := range withValidators.ConfigValidators(ctx) {
			var resp resource.ValidateConfigResponse
			v.ValidateResource(ctx, req, &resp)
			diags.Append(resp.Diagnostics...)
		}
	}
	if withValidate, ok := r.(resource.ResourceWithValidateConfig); ok {
		var resp resource.ValidateConfigResponse
		withValidate.ValidateConfig(ctx, req, &resp)
		diags.Append(resp.Diagnostics...)
	}
	return diags
}

// readDataSource runs d.Read with config.
func readDataSource(t *testing.T, d datasource.DataSource, config tftypes.Value) datasource.ReadResponse {
	t.Helper()
	s := dataSourceSchema(t, d)
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: config.Copy()}}
	d.Read(context.Background(), datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: config}}, &resp)
	return resp
}

// invokeActionDirectly runs a.Invoke with attrs as its configuration, which
// skips the validation the protocol server would run first: the way to reach
// what Invoke checks of a value that was unknown at validation time. Progress
// messages are dropped; h.invokeAction collects them.
func invokeActionDirectly(t *testing.T, a action.Action, attrs map[string]any) action.InvokeResponse {
	t.Helper()
	ctx := context.Background()
	var schemaResp action.SchemaResponse
	a.Schema(ctx, action.SchemaRequest{}, &schemaResp)
	requireNoErrors(t, schemaResp.Diagnostics)
	config := tfsdk.Config{Schema: schemaResp.Schema, Raw: tfValue(t, schemaResp.Schema.Type().TerraformType(ctx), attrs)}
	var resp action.InvokeResponse
	a.Invoke(ctx, action.InvokeRequest{Config: config}, &resp)
	return resp
}

// --- diagnostics ------------------------------------------------------------

func requireNoErrors(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if diags.HasError() {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func requireErrorContaining(t *testing.T, diags diag.Diagnostics, want string) {
	t.Helper()
	for _, d := range diags.Errors() {
		if strings.Contains(d.Detail(), want) || strings.Contains(d.Summary(), want) {
			return
		}
	}
	t.Fatalf("expected an error containing %q, got %v", want, diags)
}

func requireNoErrorContaining(t *testing.T, diags diag.Diagnostics, unwanted string) {
	t.Helper()
	for _, d := range diags.Errors() {
		if strings.Contains(d.Detail(), unwanted) || strings.Contains(d.Summary(), unwanted) {
			t.Fatalf("unexpected error containing %q: %s: %s", unwanted, d.Summary(), d.Detail())
		}
	}
}

func requireWarningContaining(t *testing.T, diags diag.Diagnostics, want string) {
	t.Helper()
	for _, d := range diags.Warnings() {
		if strings.Contains(d.Detail(), want) || strings.Contains(d.Summary(), want) {
			return
		}
	}
	t.Fatalf("expected a warning containing %q, got %v", want, diags)
}

func requireNoWarnings(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if n := diags.WarningsCount(); n != 0 {
		t.Fatalf("unexpected warnings: %v", diags.Warnings())
	}
}

// requireAttributeError fails the test unless an error is attached to the
// attribute at attributePath, written as "a.b.c".
func requireAttributeError(t *testing.T, diags diag.Diagnostics, attributePath string) {
	t.Helper()
	requireAttributeDiagnostic(t, diags.Errors(), attributePath, "error")
}

// requireAttributeWarning is requireAttributeError for a warning.
func requireAttributeWarning(t *testing.T, diags diag.Diagnostics, attributePath string) {
	t.Helper()
	requireAttributeDiagnostic(t, diags.Warnings(), attributePath, "warning")
}

func requireAttributeDiagnostic(t *testing.T, diags diag.Diagnostics, attributePath, kind string) {
	t.Helper()
	for _, d := range diags {
		if withPath, ok := d.(diag.DiagnosticWithPath); ok && withPath.Path().String() == attributePath {
			return
		}
	}
	t.Fatalf("expected an %s on %s, got %v", kind, attributePath, diags)
}

// --- protocol server ----------------------------------------------------------

// harnessProvider is the kopia provider with a fixed client instead of the
// one Configure would build, and optionally its own registrations.
type harnessProvider struct {
	kopiaProvider
	client        *kopiaclient.Client
	resources     []func() resource.Resource
	dataSources   []func() datasource.DataSource
	listResources []func() list.ListResource
	actions       []func() action.Action
}

func (p *harnessProvider) Configure(_ context.Context, _ fwprovider.ConfigureRequest, resp *fwprovider.ConfigureResponse) {
	resp.ResourceData = p.client
	resp.DataSourceData = p.client
	resp.EphemeralResourceData = p.client
	resp.ListResourceData = p.client
	resp.ActionData = p.client
}

func (p *harnessProvider) Resources(ctx context.Context) []func() resource.Resource {
	if p.resources != nil {
		return p.resources
	}
	return p.kopiaProvider.Resources(ctx)
}

func (p *harnessProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	if p.dataSources != nil {
		return p.dataSources
	}
	return p.kopiaProvider.DataSources(ctx)
}

// ListResources registers provider.go's list resources only alongside
// provider.go's resources: the framework refuses a list resource whose
// resource is not registered, so a harness with its own resources lists
// nothing unless withListResources says otherwise.
func (p *harnessProvider) ListResources(ctx context.Context) []func() list.ListResource {
	if p.listResources != nil || p.resources != nil {
		return p.listResources
	}
	return p.kopiaProvider.ListResources(ctx)
}

func (p *harnessProvider) Actions(ctx context.Context) []func() action.Action {
	if p.actions != nil {
		return p.actions
	}
	return p.kopiaProvider.Actions(ctx)
}

// harnessOption changes what newProtocolHarness registers.
type harnessOption func(*harnessProvider)

// withResources registers exactly these resources instead of provider.go's.
func withResources(constructors ...func() resource.Resource) harnessOption {
	return func(p *harnessProvider) { p.resources = constructors }
}

// withDataSources registers exactly these data sources instead of
// provider.go's.
func withDataSources(constructors ...func() datasource.DataSource) harnessOption {
	return func(p *harnessProvider) { p.dataSources = constructors }
}

// withListResources registers exactly these list resources instead of
// provider.go's. Each needs its resource registered too.
func withListResources(constructors ...func() list.ListResource) harnessOption {
	return func(p *harnessProvider) { p.listResources = constructors }
}

// withActions registers exactly these actions instead of provider.go's.
func withActions(constructors ...func() action.Action) harnessOption {
	return func(p *harnessProvider) { p.actions = constructors }
}

// withClient configures the provider with c instead of a plain client of the
// fake, such as one whose requests time out.
func withClient(c *kopiaclient.Client) harnessOption {
	return func(p *harnessProvider) { p.client = c }
}

// protocolHarness is a configured protocol server plus the schemas it serves.
type protocolHarness struct {
	server     tfprotov6.ProviderServer
	schemas    *tfprotov6.GetProviderSchemaResponse
	identities map[string]*tfprotov6.ResourceIdentitySchema
}

// resourceInstance is what Terraform keeps of a resource between operations:
// its state, identity and private data.
type resourceInstance struct {
	typeName string
	state    tftypes.Value
	identity *tfprotov6.ResourceIdentityData
	private  []byte
}

// newProtocolHarness starts the provider's protocol server with the fake's
// client and configures it, as Terraform 1.14 would.
func newProtocolHarness(t *testing.T, fake *fakeKopia, opts ...harnessOption) *protocolHarness {
	t.Helper()
	ctx := context.Background()
	p := &harnessProvider{kopiaProvider: kopiaProvider{version: "test"}, client: fake.client(t)}
	for _, opt := range opts {
		opt(p)
	}
	server, err := providerserver.NewProtocol6WithError(p)()
	if err != nil {
		t.Fatal(err)
	}

	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireNoProtocolErrors(t, schemas.Diagnostics)
	identities, err := server.GetResourceIdentitySchemas(ctx, &tfprotov6.GetResourceIdentitySchemasRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireNoProtocolErrors(t, identities.Diagnostics)

	config := dynamicValue(t, tfValue(t, schemas.Provider.ValueType(), map[string]any{}))
	resp, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{TerraformVersion: "1.14.0", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	requireNoProtocolErrors(t, resp.Diagnostics)

	return &protocolHarness{server: server, schemas: schemas, identities: identities.IdentitySchemas}
}

func (h *protocolHarness) resourceSchema(t *testing.T, typeName string) *tfprotov6.Schema {
	t.Helper()
	s, ok := h.schemas.ResourceSchemas[typeName]
	if !ok {
		t.Fatalf("no resource %s is registered; registered: %s", typeName, strings.Join(harnessSortedKeys(h.schemas.ResourceSchemas), ", "))
	}
	return s
}

func (h *protocolHarness) dataSourceSchema(t *testing.T, typeName string) *tfprotov6.Schema {
	t.Helper()
	s, ok := h.schemas.DataSourceSchemas[typeName]
	if !ok {
		t.Fatalf("no data source %s is registered; registered: %s", typeName, strings.Join(harnessSortedKeys(h.schemas.DataSourceSchemas), ", "))
	}
	return s
}

// resourceType, dataSourceType and identityType are the object types of the
// served schemas.
func (h *protocolHarness) resourceType(t *testing.T, typeName string) tftypes.Type {
	t.Helper()
	return h.resourceSchema(t, typeName).ValueType()
}

func (h *protocolHarness) dataSourceType(t *testing.T, typeName string) tftypes.Type {
	t.Helper()
	return h.dataSourceSchema(t, typeName).ValueType()
}

func (h *protocolHarness) identityType(t *testing.T, typeName string) tftypes.Type {
	t.Helper()
	s, ok := h.identities[typeName]
	if !ok {
		t.Fatalf("resource %s declares no identity", typeName)
	}
	return s.ValueType()
}

// resourceValue and dataSourceValue build objects of the served schemas.
func (h *protocolHarness) resourceValue(t *testing.T, typeName string, attrs map[string]any) tftypes.Value {
	t.Helper()
	return tfValue(t, h.resourceType(t, typeName), attrs)
}

func (h *protocolHarness) dataSourceValue(t *testing.T, typeName string, attrs map[string]any) tftypes.Value {
	t.Helper()
	return tfValue(t, h.dataSourceType(t, typeName), attrs)
}

// identityData encodes an identity of typeName.
func (h *protocolHarness) identityData(t *testing.T, typeName string, attrs map[string]any) *tfprotov6.ResourceIdentityData {
	t.Helper()
	return &tfprotov6.ResourceIdentityData{IdentityData: dynamicValue(t, tfValue(t, h.identityType(t, typeName), attrs))}
}

// identityValue decodes an identity of typeName; the null object when there
// is none.
func (h *protocolHarness) identityValue(t *testing.T, typeName string, identity *tfprotov6.ResourceIdentityData) tftypes.Value {
	t.Helper()
	typ := h.identityType(t, typeName)
	if identity == nil || identity.IdentityData == nil {
		return tftypes.NewValue(typ, nil)
	}
	return decodeDynamicValue(t, identity.IdentityData, typ)
}

// decodeResource and decodeDataSource decode a protocol value of the served
// schemas.
func (h *protocolHarness) decodeResource(t *testing.T, typeName string, dv *tfprotov6.DynamicValue) tftypes.Value {
	t.Helper()
	return decodeDynamicValue(t, dv, h.resourceType(t, typeName))
}

func (h *protocolHarness) decodeDataSource(t *testing.T, typeName string, dv *tfprotov6.DynamicValue) tftypes.Value {
	t.Helper()
	return decodeDynamicValue(t, dv, h.dataSourceType(t, typeName))
}

func dynamicValue(t *testing.T, v tftypes.Value) *tfprotov6.DynamicValue {
	t.Helper()
	dv, err := tfprotov6.NewDynamicValue(v.Type(), v)
	if err != nil {
		t.Fatal(err)
	}
	return &dv
}

func decodeDynamicValue(t *testing.T, dv *tfprotov6.DynamicValue, typ tftypes.Type) tftypes.Value {
	t.Helper()
	if dv == nil {
		t.Fatal("no value")
	}
	v, err := dv.Unmarshal(typ)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// validateResource runs ValidateResourceConfig: attribute validators, then
// ConfigValidators and ValidateConfig.
func (h *protocolHarness) validateResource(t *testing.T, typeName string, config tftypes.Value) []*tfprotov6.Diagnostic {
	t.Helper()
	resp, err := h.server.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{
		TypeName: typeName,
		Config:   dynamicValue(t, config),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Diagnostics
}

// validateDataSource runs ValidateDataResourceConfig.
func (h *protocolHarness) validateDataSource(t *testing.T, typeName string, config tftypes.Value) []*tfprotov6.Diagnostic {
	t.Helper()
	resp, err := h.server.ValidateDataResourceConfig(context.Background(), &tfprotov6.ValidateDataResourceConfigRequest{
		TypeName: typeName,
		Config:   dynamicValue(t, config),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Diagnostics
}

// plan runs PlanResourceChange from prior (nil for a create) to config (a
// null value for a destroy), with the proposed new state Terraform core
// would send.
func (h *protocolHarness) plan(t *testing.T, typeName string, prior *resourceInstance, config tftypes.Value) *tfprotov6.PlanResourceChangeResponse {
	t.Helper()
	typ := h.resourceType(t, typeName)
	priorState := tftypes.NewValue(typ, nil)
	var priorIdentity *tfprotov6.ResourceIdentityData
	var priorPrivate []byte
	if prior != nil {
		priorState, priorIdentity, priorPrivate = prior.state, prior.identity, prior.private
	}
	proposed := tftypes.NewValue(typ, nil)
	if !config.IsNull() {
		proposed = proposedNewState(t, h.resourceSchema(t, typeName).Block, priorState, config)
	}
	resp, err := h.server.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName:         typeName,
		PriorState:       dynamicValue(t, priorState),
		ProposedNewState: dynamicValue(t, proposed),
		Config:           dynamicValue(t, config),
		PriorPrivate:     priorPrivate,
		PriorIdentity:    priorIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// apply runs ApplyResourceChange for a plan h.plan made.
func (h *protocolHarness) apply(t *testing.T, typeName string, prior *resourceInstance, config tftypes.Value, planned *tfprotov6.PlanResourceChangeResponse) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	priorState := tftypes.NewValue(h.resourceType(t, typeName), nil)
	if prior != nil {
		priorState = prior.state
	}
	resp, err := h.server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:        typeName,
		PriorState:      dynamicValue(t, priorState),
		PlannedState:    planned.PlannedState,
		Config:          dynamicValue(t, config),
		PlannedPrivate:  planned.PlannedPrivate,
		PlannedIdentity: planned.PlannedIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// planAndApply validates config, plans and applies, failing the test on any
// error, and returns what Terraform would store.
func (h *protocolHarness) planAndApply(t *testing.T, typeName string, prior *resourceInstance, config tftypes.Value) *resourceInstance {
	t.Helper()
	if !config.IsNull() {
		requireNoProtocolErrors(t, h.validateResource(t, typeName, config))
	}
	planned := h.plan(t, typeName, prior, config)
	requireNoProtocolErrors(t, planned.Diagnostics)
	applied := h.apply(t, typeName, prior, config, planned)
	requireNoProtocolErrors(t, applied.Diagnostics)
	return &resourceInstance{
		typeName: typeName,
		state:    h.decodeResource(t, typeName, applied.NewState),
		identity: applied.NewIdentity,
		private:  applied.Private,
	}
}

// create applies config to a new resource, as `terraform apply` does.
func (h *protocolHarness) create(t *testing.T, typeName string, config tftypes.Value) *resourceInstance {
	t.Helper()
	return h.planAndApply(t, typeName, nil, config)
}

// update applies config to an existing resource.
func (h *protocolHarness) update(t *testing.T, inst *resourceInstance, config tftypes.Value) *resourceInstance {
	t.Helper()
	return h.planAndApply(t, inst.typeName, inst, config)
}

// destroy plans and applies the resource's destruction.
func (h *protocolHarness) destroy(t *testing.T, inst *resourceInstance) {
	t.Helper()
	h.planAndApply(t, inst.typeName, inst, tftypes.NewValue(h.resourceType(t, inst.typeName), nil))
}

// read runs ReadResource on inst.
func (h *protocolHarness) read(t *testing.T, inst *resourceInstance) *tfprotov6.ReadResourceResponse {
	t.Helper()
	resp, err := h.server.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{
		TypeName:        inst.typeName,
		CurrentState:    dynamicValue(t, inst.state),
		Private:         inst.private,
		CurrentIdentity: inst.identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// refresh reads inst, failing the test on an error. The returned state is
// null when the resource removed itself.
func (h *protocolHarness) refresh(t *testing.T, inst *resourceInstance) *resourceInstance {
	t.Helper()
	resp := h.read(t, inst)
	requireNoProtocolErrors(t, resp.Diagnostics)
	return &resourceInstance{
		typeName: inst.typeName,
		state:    h.decodeResource(t, inst.typeName, resp.NewState),
		identity: resp.NewIdentity,
		private:  resp.Private,
	}
}

// importResource runs ImportResourceState with an import id or, when id is
// "", with identity.
func (h *protocolHarness) importResource(t *testing.T, typeName, id string, identity *tfprotov6.ResourceIdentityData) *tfprotov6.ImportResourceStateResponse {
	t.Helper()
	resp, err := h.server.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{
		TypeName: typeName,
		ID:       id,
		Identity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// importByID imports a resource by import id and reads it, as Terraform does,
// failing the test on an error.
func (h *protocolHarness) importByID(t *testing.T, typeName, id string) *resourceInstance {
	t.Helper()
	return h.finishImport(t, typeName, h.importResource(t, typeName, id, nil))
}

// importByIdentity imports a resource by identity and reads it.
func (h *protocolHarness) importByIdentity(t *testing.T, typeName string, identity map[string]any) *resourceInstance {
	t.Helper()
	return h.finishImport(t, typeName, h.importResource(t, typeName, "", h.identityData(t, typeName, identity)))
}

func (h *protocolHarness) finishImport(t *testing.T, typeName string, resp *tfprotov6.ImportResourceStateResponse) *resourceInstance {
	t.Helper()
	requireNoProtocolErrors(t, resp.Diagnostics)
	if len(resp.ImportedResources) != 1 {
		t.Fatalf("import returned %d resources, want 1", len(resp.ImportedResources))
	}
	imported := resp.ImportedResources[0]
	return h.refresh(t, &resourceInstance{
		typeName: typeName,
		state:    h.decodeResource(t, typeName, imported.State),
		identity: imported.Identity,
		private:  imported.Private,
	})
}

// readDataSourceResponse runs ReadDataSource with config.
func (h *protocolHarness) readDataSourceResponse(t *testing.T, typeName string, config tftypes.Value) *tfprotov6.ReadDataSourceResponse {
	t.Helper()
	resp, err := h.server.ReadDataSource(context.Background(), &tfprotov6.ReadDataSourceRequest{
		TypeName: typeName,
		Config:   dynamicValue(t, config),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readDataSource validates config and reads the data source, failing the test
// on an error, and returns its state.
func (h *protocolHarness) readDataSource(t *testing.T, typeName string, config tftypes.Value) tftypes.Value {
	t.Helper()
	requireNoProtocolErrors(t, h.validateDataSource(t, typeName, config))
	resp := h.readDataSourceResponse(t, typeName, config)
	requireNoProtocolErrors(t, resp.Diagnostics)
	return h.decodeDataSource(t, typeName, resp.State)
}

// listResourceSchema is the served schema of a list block's configuration.
func (h *protocolHarness) listResourceSchema(t *testing.T, typeName string) *tfprotov6.Schema {
	t.Helper()
	s, ok := h.schemas.ListResourceSchemas[typeName]
	if !ok {
		t.Fatalf("no list resource %s is registered; registered: %s", typeName, strings.Join(harnessSortedKeys(h.schemas.ListResourceSchemas), ", "))
	}
	return s
}

// actionSchema is the served schema of an action.
func (h *protocolHarness) actionSchema(t *testing.T, typeName string) *tfprotov6.Schema {
	t.Helper()
	s, ok := h.schemas.ActionSchemas[typeName]
	if !ok {
		t.Fatalf("no action %s is registered; registered: %s", typeName, strings.Join(harnessSortedKeys(h.schemas.ActionSchemas), ", "))
	}
	return s.Schema
}

// listValue and actionValue build configurations of the served schemas.
func (h *protocolHarness) listValue(t *testing.T, typeName string, attrs map[string]any) tftypes.Value {
	t.Helper()
	return tfValue(t, h.listResourceSchema(t, typeName).ValueType(), attrs)
}

func (h *protocolHarness) actionValue(t *testing.T, typeName string, attrs map[string]any) tftypes.Value {
	t.Helper()
	return tfValue(t, h.actionSchema(t, typeName).ValueType(), attrs)
}

// listServer and actionServer are the protocol server with the RPCs that
// tfprotov6 still keeps out of ProviderServer.
func (h *protocolHarness) listServer(t *testing.T) tfprotov6.ProviderServerWithListResource {
	t.Helper()
	server, ok := h.server.(tfprotov6.ProviderServerWithListResource)
	if !ok {
		t.Fatal("the protocol server does not serve list resources")
	}
	return server
}

func (h *protocolHarness) actionServer(t *testing.T) tfprotov6.ProviderServerWithActions {
	t.Helper()
	server, ok := h.server.(tfprotov6.ProviderServerWithActions)
	if !ok {
		t.Fatal("the protocol server does not serve actions")
	}
	return server
}

// validateList runs ValidateListResourceConfig: the list block's attribute
// validators.
func (h *protocolHarness) validateList(t *testing.T, typeName string, config tftypes.Value) []*tfprotov6.Diagnostic {
	t.Helper()
	resp, err := h.listServer(t).ValidateListResourceConfig(context.Background(), &tfprotov6.ValidateListResourceConfigRequest{
		TypeName: typeName,
		Config:   dynamicValue(t, config),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Diagnostics
}

// list runs ListResource the way `terraform query` does and collects every
// result the stream yields. A limit of 0 sets none; Terraform always sends
// one, 100 unless the list block sets another.
func (h *protocolHarness) list(t *testing.T, typeName string, config tftypes.Value, includeResource bool, limit int64) []tfprotov6.ListResourceResult {
	t.Helper()
	stream, err := h.listServer(t).ListResource(context.Background(), &tfprotov6.ListResourceRequest{
		TypeName:        typeName,
		Config:          dynamicValue(t, config),
		IncludeResource: includeResource,
		Limit:           limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	var results []tfprotov6.ListResourceResult
	for result := range stream.Results {
		results = append(results, result)
	}
	return results
}

// listDiagnostics gathers the diagnostics of every list result.
func listDiagnostics(results []tfprotov6.ListResourceResult) []*tfprotov6.Diagnostic {
	var diags []*tfprotov6.Diagnostic
	for _, r := range results {
		diags = append(diags, r.Diagnostics...)
	}
	return diags
}

// validateAction runs ValidateActionConfig: the action's attribute validators,
// then its config validators.
func (h *protocolHarness) validateAction(t *testing.T, typeName string, config tftypes.Value) []*tfprotov6.Diagnostic {
	t.Helper()
	resp, err := h.actionServer(t).ValidateActionConfig(context.Background(), &tfprotov6.ValidateActionConfigRequest{
		ActionType: typeName,
		Config:     dynamicValue(t, config),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Diagnostics
}

// invokeAction runs InvokeAction and returns the progress messages the action
// sent, in order, and the diagnostics it completed with.
func (h *protocolHarness) invokeAction(t *testing.T, typeName string, config tftypes.Value) ([]string, []*tfprotov6.Diagnostic) {
	t.Helper()
	stream, err := h.actionServer(t).InvokeAction(context.Background(), &tfprotov6.InvokeActionRequest{
		ActionType: typeName,
		Config:     dynamicValue(t, config),
	})
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	var diags []*tfprotov6.Diagnostic
	completed := false
	for event := range stream.Events {
		switch e := event.Type.(type) {
		case tfprotov6.ProgressInvokeActionEventType:
			progress = append(progress, e.Message)
		case tfprotov6.CompletedInvokeActionEventType:
			diags = e.Diagnostics
			completed = true
		}
	}
	if !completed {
		t.Fatal("the action's event stream ended without a completed event")
	}
	return progress, diags
}

// proposedNewState builds the proposed new state Terraform core sends with
// PlanResourceChange (internal/plans/objchange in Terraform): the configured
// value of every attribute, except that a computed attribute the
// configuration leaves null keeps its prior value. Nested attributes are
// merged element by element — single objects as a whole, lists by index, maps
// by key, sets by their non-computed attributes — and a nested attribute the
// configuration leaves null is proposed as null even when it is computed,
// which is what Terraform does too.
func proposedNewState(t *testing.T, block *tfprotov6.SchemaBlock, prior, config tftypes.Value) tftypes.Value {
	t.Helper()
	if !config.IsKnown() || config.IsNull() {
		return config
	}
	attrs := proposedAttributes(t, block.Attributes, prior, config)
	// Kopia's schemas use nested attributes, not blocks; a block is proposed
	// as configured.
	configAttrs := harnessObjectAttributes(t, config)
	for _, nested := range block.BlockTypes {
		attrs[nested.TypeName] = configAttrs[nested.TypeName]
	}
	return tftypes.NewValue(config.Type(), attrs)
}

func proposedAttributes(t *testing.T, attributes []*tfprotov6.SchemaAttribute, prior, config tftypes.Value) map[string]tftypes.Value {
	t.Helper()
	priorAttrs := harnessObjectAttributes(t, prior)
	configAttrs := harnessObjectAttributes(t, config)
	proposed := make(map[string]tftypes.Value, len(attributes))
	for _, attr := range attributes {
		configV := configAttrs[attr.Name]
		priorV, ok := priorAttrs[attr.Name]
		if !ok {
			priorV = tftypes.NewValue(configV.Type(), nil)
		}

		var value tftypes.Value
		switch {
		case !attr.Computed:
			value = configV
		case configV.IsKnown() && !configV.IsNull():
			value = configV
		default:
			value = priorV
		}
		if attr.NestedType != nil {
			value = proposedNested(t, attr.NestedType, priorV, configV)
		}
		proposed[attr.Name] = value
	}
	return proposed
}

func proposedNested(t *testing.T, object *tfprotov6.SchemaObject, prior, config tftypes.Value) tftypes.Value {
	t.Helper()
	if !config.IsKnown() || config.IsNull() {
		return config
	}
	priorKnown := prior.IsKnown() && !prior.IsNull()

	switch object.Nesting {
	case tfprotov6.SchemaObjectNestingModeSingle:
		return tftypes.NewValue(config.Type(), proposedAttributes(t, object.Attributes, harnessKnownOrNull(prior), config))

	case tfprotov6.SchemaObjectNestingModeList:
		var configElems, priorElems []tftypes.Value
		harnessAs(t, config, &configElems)
		if priorKnown {
			harnessAs(t, prior, &priorElems)
		}
		elemType := config.Type().(tftypes.List).ElementType
		proposed := make([]tftypes.Value, 0, len(configElems))
		for i, elem := range configElems {
			priorElem := tftypes.NewValue(elemType, nil)
			if i < len(priorElems) {
				priorElem = priorElems[i]
			}
			proposed = append(proposed, proposedObject(t, object, priorElem, elem))
		}
		return tftypes.NewValue(config.Type(), proposed)

	case tfprotov6.SchemaObjectNestingModeMap:
		var configElems, priorElems map[string]tftypes.Value
		harnessAs(t, config, &configElems)
		if priorKnown {
			harnessAs(t, prior, &priorElems)
		}
		elemType := config.Type().(tftypes.Map).ElementType
		proposed := make(map[string]tftypes.Value, len(configElems))
		for key, elem := range configElems {
			priorElem, ok := priorElems[key]
			if !ok {
				priorElem = tftypes.NewValue(elemType, nil)
			}
			proposed[key] = proposedObject(t, object, priorElem, elem)
		}
		return tftypes.NewValue(config.Type(), proposed)

	case tfprotov6.SchemaObjectNestingModeSet:
		var configElems, priorElems []tftypes.Value
		harnessAs(t, config, &configElems)
		if priorKnown {
			harnessAs(t, prior, &priorElems)
		}
		elemType := config.Type().(tftypes.Set).ElementType
		used := make([]bool, len(priorElems))
		proposed := make([]tftypes.Value, 0, len(configElems))
		for _, elem := range configElems {
			priorElem := tftypes.NewValue(elemType, nil)
			for i, candidate := range priorElems {
				if !used[i] && sameNonComputed(t, object, candidate, elem) {
					used[i] = true
					priorElem = candidate
					break
				}
			}
			proposed = append(proposed, proposedObject(t, object, priorElem, elem))
		}
		return tftypes.NewValue(config.Type(), proposed)
	}
	t.Fatalf("unsupported nesting mode %v", object.Nesting)
	return tftypes.Value{}
}

// proposedObject is one element of a nested attribute.
func proposedObject(t *testing.T, object *tfprotov6.SchemaObject, prior, config tftypes.Value) tftypes.Value {
	t.Helper()
	if !config.IsKnown() || config.IsNull() {
		return config
	}
	return tftypes.NewValue(config.Type(), proposedAttributes(t, object.Attributes, harnessKnownOrNull(prior), config))
}

// sameNonComputed reports whether two set elements agree on every attribute
// that is not computed, which is how Terraform pairs configured set elements
// with prior ones.
func sameNonComputed(t *testing.T, object *tfprotov6.SchemaObject, a, b tftypes.Value) bool {
	t.Helper()
	aAttrs, bAttrs := harnessObjectAttributes(t, a), harnessObjectAttributes(t, b)
	for _, attr := range object.Attributes {
		if attr.Computed {
			continue
		}
		if !aAttrs[attr.Name].Equal(bAttrs[attr.Name]) {
			return false
		}
	}
	return true
}

// harnessKnownOrNull turns an unknown value into a null one of the same type.
func harnessKnownOrNull(v tftypes.Value) tftypes.Value {
	if !v.IsKnown() {
		return tftypes.NewValue(v.Type(), nil)
	}
	return v
}

// harnessObjectAttributes returns an object's attributes; none for null.
func harnessObjectAttributes(t *testing.T, v tftypes.Value) map[string]tftypes.Value {
	t.Helper()
	if v.IsNull() || !v.IsKnown() {
		return map[string]tftypes.Value{}
	}
	var attrs map[string]tftypes.Value
	harnessAs(t, v, &attrs)
	return attrs
}

func harnessAs(t *testing.T, v tftypes.Value, dst any) {
	t.Helper()
	if err := v.As(dst); err != nil {
		t.Fatal(err)
	}
}

func requireNoProtocolErrors(t *testing.T, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("unexpected error: %s: %s", d.Summary, d.Detail)
		}
	}
}

func requireProtocolErrorContaining(t *testing.T, diags []*tfprotov6.Diagnostic, want string) {
	t.Helper()
	requireProtocolDiagnostic(t, diags, tfprotov6.DiagnosticSeverityError, want)
}

func requireProtocolWarningContaining(t *testing.T, diags []*tfprotov6.Diagnostic, want string) {
	t.Helper()
	requireProtocolDiagnostic(t, diags, tfprotov6.DiagnosticSeverityWarning, want)
}

func requireProtocolDiagnostic(t *testing.T, diags []*tfprotov6.Diagnostic, severity tfprotov6.DiagnosticSeverity, want string) {
	t.Helper()
	for _, d := range diags {
		if d.Severity == severity && (strings.Contains(d.Summary, want) || strings.Contains(d.Detail, want)) {
			return
		}
	}
	var got []string
	for _, d := range diags {
		got = append(got, fmt.Sprintf("%s: %s: %s", d.Severity, d.Summary, d.Detail))
	}
	t.Fatalf("expected a %s containing %q, got %v", severity, want, got)
}

// --- acceptance -------------------------------------------------------------

// captureAttr stores an attribute's value for comparison in a later step.
func captureAttr(resourceName, attribute string, into *string) acc.TestCheckFunc {
	return func(state *terraform.State) error {
		rs, ok := state.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", resourceName)
		}
		*into = rs.Primary.Attributes[attribute]
		return nil
	}
}
