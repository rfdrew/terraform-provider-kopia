package provider

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// These tests exercise the harness in testhelpers_test.go against
// harness_widget, a resource that exists only here and keeps its objects in
// memory, and harness_status, a data source that reads the fake kopia through
// the real client. They double as an example of how to use the harness.

// harnessWidgetStore is the in-memory backend of harness_widget.
type harnessWidgetStore struct {
	mu      sync.Mutex
	widgets map[string]harnessWidget
}

type harnessWidget struct {
	size     int64
	labels   []string
	mode     string
	level    int64
	revision int64
}

func newHarnessWidgetStore() *harnessWidgetStore {
	return &harnessWidgetStore{widgets: map[string]harnessWidget{}}
}

type harnessWidgetResource struct {
	client *kopiaclient.Client
	store  *harnessWidgetStore
}

var (
	_ resource.ResourceWithConfigure      = &harnessWidgetResource{}
	_ resource.ResourceWithIdentity       = &harnessWidgetResource{}
	_ resource.ResourceWithImportState    = &harnessWidgetResource{}
	_ resource.ResourceWithModifyPlan     = &harnessWidgetResource{}
	_ resource.ResourceWithValidateConfig = &harnessWidgetResource{}
)

type harnessWidgetModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Size     types.Int64  `tfsdk:"size"`
	Labels   types.List   `tfsdk:"labels"`
	Settings types.Object `tfsdk:"settings"`
	Revision types.Int64  `tfsdk:"revision"`
}

var harnessWidgetSettingsTypes = map[string]attr.Type{"mode": types.StringType, "level": types.Int64Type}

func (r *harnessWidgetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_harness_widget"
}

func (r *harnessWidgetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A test resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Same as `name`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Name.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"size": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Size.",
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
			"labels": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Labels.",
			},
			"settings": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Settings.",
				Attributes: map[string]schema.Attribute{
					"mode": schema.StringAttribute{Optional: true, MarkdownDescription: "Mode."},
					"level": schema.Int64Attribute{
						Optional:            true,
						Computed:            true,
						Default:             int64default.StaticInt64(3),
						MarkdownDescription: "Level.",
					},
				},
			},
			"revision": schema.Int64Attribute{Computed: true, MarkdownDescription: "Bumped by every write."},
		},
	}
}

func (r *harnessWidgetResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = stringIdentitySchema("name", "Name of the widget.")
}

func (r *harnessWidgetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *harnessWidgetResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var labels types.List
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("labels"), &labels)...)
	values, known := stringsFromList(labels)
	if !known {
		return
	}
	for _, label := range values {
		if label == "" {
			resp.Diagnostics.AddAttributeError(path.Root("labels"), "Empty label", "Labels must not be empty.")
		}
	}
}

func (r *harnessWidgetResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var name types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &name)...)
	if name.ValueString() == "forbidden" {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Forbidden name", "The name forbidden is taken.")
	}
}

func (m *harnessWidgetModel) fromStore(name string, w harnessWidget) {
	m.ID = types.StringValue(name)
	m.Name = types.StringValue(name)
	m.Size = int64ValueOrNullIfZero(w.size)
	m.Labels = stringListOrNull(w.labels)
	m.Revision = types.Int64Value(w.revision)
	if w.mode == "" && w.level == 0 {
		m.Settings = types.ObjectNull(harnessWidgetSettingsTypes)
		return
	}
	m.Settings = types.ObjectValueMust(harnessWidgetSettingsTypes, map[string]attr.Value{
		"mode":  stringValueOrNullIfEmpty(w.mode),
		"level": types.Int64Value(w.level),
	})
}

func (m *harnessWidgetModel) toStore() harnessWidget {
	w := harnessWidget{size: m.Size.ValueInt64()}
	w.labels, _ = stringsFromList(m.Labels)
	if !m.Settings.IsNull() {
		attrs := m.Settings.Attributes()
		w.mode = attrs["mode"].(types.String).ValueString()
		w.level = attrs["level"].(types.Int64).ValueInt64()
	}
	return w
}

func (r *harnessWidgetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if r.client == nil {
		resp.Diagnostics.AddError("Unconfigured", "The widget resource has no client.")
		return
	}
	var plan harnessWidgetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()

	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if _, exists := r.store.widgets[name]; exists {
		resp.Diagnostics.AddError("Widget already exists", "Import widget "+name+" instead.")
		return
	}
	w := plan.toStore()
	w.revision = 1
	r.store.widgets[name] = w

	plan.fromStore(name, w)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)
}

func (r *harnessWidgetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state harnessWidgetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)

	r.store.mu.Lock()
	w, exists := r.store.widgets[name]
	r.store.mu.Unlock()
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}
	state.fromStore(name, w)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *harnessWidgetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan harnessWidgetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()

	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	prior, exists := r.store.widgets[name]
	if !exists {
		resp.Diagnostics.AddError("Widget no longer exists", "Widget "+name+" was deleted outside of Terraform.")
		return
	}
	w := plan.toStore()
	w.revision = prior.revision + 1
	r.store.widgets[name] = w

	plan.fromStore(name, w)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)
}

func (r *harnessWidgetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state harnessWidgetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	delete(r.store.widgets, state.Name.ValueString())
}

func (r *harnessWidgetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	name, _ := importedID(ctx, req, "name", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
	setIdentity(ctx, resp.Identity, "name", name, &resp.Diagnostics)
}

// harnessStatusDataSource reads the fake through the client: whether a
// repository is connected, and the server version.
type harnessStatusDataSource struct {
	client *kopiaclient.Client
}

type harnessStatusModel struct {
	Connected types.Bool   `tfsdk:"connected"`
	Version   types.String `tfsdk:"version"`
}

func (d *harnessStatusDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_harness_status"
}

func (d *harnessStatusDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = datasourceschema.Schema{
		MarkdownDescription: "A test data source.",
		Attributes: map[string]datasourceschema.Attribute{
			"connected": datasourceschema.BoolAttribute{Computed: true, MarkdownDescription: "Connected."},
			"version":   datasourceschema.StringAttribute{Computed: true, MarkdownDescription: "Version."},
		},
	}
}

func (d *harnessStatusDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *harnessStatusDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	connected, err := d.client.RepositoryConnected(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Kopia repository status", err.Error())
		return
	}
	version, err := d.client.ServerVersion(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Kopia version", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &harnessStatusModel{
		Connected: types.BoolValue(connected),
		Version:   types.StringValue(version),
	})...)
}

// harnessWidgetHarness registers only the test types.
func harnessWidgetHarness(t *testing.T, fake *fakeKopia, store *harnessWidgetStore) *protocolHarness {
	t.Helper()
	return newProtocolHarness(t, fake,
		withResources(func() resource.Resource { return &harnessWidgetResource{store: store} }),
		withDataSources(func() datasource.DataSource { return &harnessStatusDataSource{} }),
	)
}

func TestHarnessSchemasAreValid(t *testing.T) {
	requireValidResourceSchema(t, &harnessWidgetResource{})
	requireValidDataSourceSchema(t, &harnessStatusDataSource{})
}

func TestHarnessTFValue(t *testing.T) {
	typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"s":    tftypes.String,
		"n":    tftypes.Number,
		"b":    tftypes.Bool,
		"list": tftypes.List{ElementType: tftypes.String},
		"set":  tftypes.Set{ElementType: tftypes.Number},
		"map":  tftypes.Map{ElementType: tftypes.String},
		"obj":  tftypes.Object{AttributeTypes: map[string]tftypes.Type{"x": tftypes.Number, "y": tftypes.String}},
		"u":    tftypes.String,
		"raw":  tftypes.Bool,
	}}
	v := tfValue(t, typ, map[string]any{
		"s":    "text",
		"n":    int64(42),
		"b":    true,
		"list": []string{"a", "b"},
		"set":  []any{1, 2.5},
		"map":  map[string]string{"k": "v"},
		"obj":  map[string]any{"x": 7},
		"u":    tfUnknown,
		"raw":  tftypes.NewValue(tftypes.Bool, false),
	})

	if got := stringAt(t, v, "s"); got != "text" {
		t.Errorf("s = %q", got)
	}
	if got := int64At(t, v, "n"); got != 42 {
		t.Errorf("n = %d", got)
	}
	if !boolAt(t, v, "b") || boolAt(t, v, "raw") {
		t.Error("b or raw decoded wrongly")
	}
	if got := strings.Join(stringsAt(t, v, "list"), ","); got != "a,b" {
		t.Errorf("list = %s", got)
	}
	if got := float64At(t, v, "set", 1) + float64At(t, v, "set", 0); got != 3.5 {
		t.Errorf("set elements sum to %v, want 3.5", got)
	}
	if got := stringAt(t, v, "map", "k"); got != "v" {
		t.Errorf("map[k] = %q", got)
	}
	if got := int64At(t, v, "obj", "x"); got != 7 || !isNullAt(t, v, "obj", "y") {
		t.Errorf("obj = %s", valueAt(t, v, "obj"))
	}
	if !isUnknownAt(t, v, "u") {
		t.Error("u is not unknown")
	}

	for name, bad := range map[string]any{
		"unknown attribute": map[string]any{"nope": 1},
		"wrong type":        map[string]any{"s": 1},
		"not an object":     "text",
	} {
		if _, err := toTFValue(typ, bad); err == nil {
			t.Errorf("%s: toTFValue accepted %v", name, bad)
		}
	}
}

func TestHarnessDirectLifecycle(t *testing.T) {
	store := newHarnessWidgetStore()
	r := &harnessWidgetResource{client: newFakeKopia().client(t), store: store}

	config := resourceValue(t, r, map[string]any{
		"name":     "w1",
		"size":     5,
		"labels":   []string{"a"},
		"settings": map[string]any{"mode": "fast", "level": 2},
	})
	plan := resourceValue(t, r, map[string]any{
		"id":       tfUnknown,
		"name":     "w1",
		"size":     5,
		"labels":   []string{"a"},
		"settings": map[string]any{"mode": "fast", "level": 2},
		"revision": tfUnknown,
	})
	created := createResource(t, r, config, plan)
	requireNoErrors(t, created.Diagnostics)
	if !created.State.Raw.IsFullyKnown() {
		t.Fatalf("state after create holds unknown values: %s", created.State.Raw)
	}
	if got := stringAt(t, created.State.Raw, "id"); got != "w1" {
		t.Errorf("id = %q", got)
	}
	if got := identityAttr(t, created.Identity, "name"); got != "w1" {
		t.Errorf("identity name = %q", got)
	}

	read := readResource(t, r, created.State.Raw)
	requireNoErrors(t, read.Diagnostics)
	if !read.State.Raw.Equal(created.State.Raw) {
		t.Errorf("read changed the state: %s", read.State.Raw)
	}
	if got := identityAttr(t, read.Identity, "name"); got != "w1" {
		t.Errorf("identity after read = %q", got)
	}

	updateConfig := resourceValue(t, r, map[string]any{"name": "w1", "size": 6})
	updatePlan := resourceValue(t, r, map[string]any{"id": "w1", "name": "w1", "size": 6, "revision": tfUnknown})
	updated := updateResource(t, r, updateConfig, updatePlan, created.State.Raw)
	requireNoErrors(t, updated.Diagnostics)
	if got := int64At(t, updated.State.Raw, "revision"); got != 2 {
		t.Errorf("revision after update = %d, want 2", got)
	}
	if !isNullAt(t, updated.State.Raw, "settings") || !isNullAt(t, updated.State.Raw, "labels") {
		t.Errorf("cleared attributes are not null: %s", updated.State.Raw)
	}

	deleted := deleteResource(t, r, updated.State.Raw)
	requireNoErrors(t, deleted.Diagnostics)
	gone := readResource(t, r, updated.State.Raw)
	requireNoErrors(t, gone.Diagnostics)
	if !gone.State.Raw.IsNull() {
		t.Fatalf("read of a deleted widget kept it in state: %s", gone.State.Raw)
	}
}

func TestHarnessDirectModelValue(t *testing.T) {
	r := &harnessWidgetResource{}
	got := modelValue(t, r, &harnessWidgetModel{
		ID:       types.StringValue("w1"),
		Name:     types.StringValue("w1"),
		Size:     types.Int64Null(),
		Labels:   types.ListNull(types.StringType),
		Settings: types.ObjectNull(harnessWidgetSettingsTypes),
		Revision: types.Int64Value(1),
	})
	want := resourceValue(t, r, map[string]any{"id": "w1", "name": "w1", "revision": 1})
	if !got.Equal(want) {
		t.Fatalf("modelValue() = %s, want %s", got, want)
	}
}

func TestHarnessDirectImportAndValidation(t *testing.T) {
	r := &harnessWidgetResource{store: newHarnessWidgetStore()}

	byID := importResource(t, r, "w1")
	requireNoErrors(t, byID.Diagnostics)
	if got := stringAt(t, byID.State.Raw, "name"); got != "w1" {
		t.Errorf("imported name = %q", got)
	}
	if got := identityAttr(t, byID.Identity, "name"); got != "w1" {
		t.Errorf("imported identity = %q", got)
	}

	byIdentity := importResourceByIdentity(t, r, map[string]any{"name": "w2"})
	requireNoErrors(t, byIdentity.Diagnostics)
	if got := stringAt(t, byIdentity.State.Raw, "id"); got != "w2" {
		t.Errorf("imported id = %q", got)
	}

	empty := importResourceByIdentity(t, r, map[string]any{"name": ""})
	requireErrorContaining(t, empty.Diagnostics, "Missing import identity")

	diags := validateResourceConfig(t, r, resourceValue(t, r, map[string]any{"name": "w1", "labels": []string{"ok", ""}}))
	requireAttributeError(t, diags, "labels")
	// Unknown labels cannot be checked yet and are let through.
	requireNoErrors(t, validateResourceConfig(t, r, resourceValue(t, r, map[string]any{"name": "w1", "labels": tfUnknown})))

	config := resourceValue(t, r, map[string]any{"name": "forbidden"})
	plan := resourceValue(t, r, map[string]any{"id": tfUnknown, "name": "forbidden", "revision": tfUnknown})
	modified := modifyResourcePlan(t, r, config, plan, nullResourceValue(t, r))
	requireAttributeError(t, modified.Diagnostics, "name")
	destroy := modifyResourcePlan(t, r, nullResourceValue(t, r), nullResourceValue(t, r), plan)
	requireNoErrors(t, destroy.Diagnostics)
}

func TestHarnessDirectDataSource(t *testing.T) {
	d := &harnessStatusDataSource{client: newFakeKopia().client(t)}
	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	if !boolAt(t, resp.State.Raw, "connected") || stringAt(t, resp.State.Raw, "version") != testedKopiaVersion {
		t.Fatalf("state = %s", resp.State.Raw)
	}
}

// The protocol harness runs what the framework runs between the calls:
// validators, defaults, plan modifiers, identity bookkeeping.
func TestHarnessProtocolLifecycle(t *testing.T) {
	store := newHarnessWidgetStore()
	fake := newFakeKopia()
	h := harnessWidgetHarness(t, fake, store)
	const typeName = "kopia_harness_widget"

	config := h.resourceValue(t, typeName, map[string]any{"name": "w1", "size": 5, "settings": map[string]any{"mode": "fast"}})
	inst := h.create(t, typeName, config)
	if got := int64At(t, inst.state, "settings", "level"); got != 3 {
		t.Errorf("settings.level = %d, want the default 3", got)
	}
	if got := stringAt(t, h.identityValue(t, typeName, inst.identity), "name"); got != "w1" {
		t.Errorf("identity name = %q", got)
	}

	// A plan against an unchanged configuration is empty.
	replan := h.plan(t, typeName, inst, config)
	requireNoProtocolErrors(t, replan.Diagnostics)
	if planned := h.decodeResource(t, typeName, replan.PlannedState); !planned.Equal(inst.state) {
		diffs, _ := planned.Diff(inst.state)
		t.Fatalf("plan after apply changes %v", diffs)
	}

	// Changing size updates in place; UseStateForUnknown keeps the id known.
	changed := h.resourceValue(t, typeName, map[string]any{"name": "w1", "size": 6, "settings": map[string]any{"mode": "fast"}})
	planned := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, planned.Diagnostics)
	plannedState := h.decodeResource(t, typeName, planned.PlannedState)
	if isUnknownAt(t, plannedState, "id") || !isUnknownAt(t, plannedState, "revision") {
		t.Errorf("planned update = %s, want a known id and an unknown revision", plannedState)
	}
	if len(planned.RequiresReplace) != 0 {
		t.Errorf("changing size requires replacement: %v", planned.RequiresReplace)
	}
	inst = h.update(t, inst, changed)
	if got := int64At(t, inst.state, "revision"); got != 2 {
		t.Errorf("revision = %d, want 2", got)
	}

	// Renaming forces replacement.
	renamed := h.plan(t, typeName, inst, h.resourceValue(t, typeName, map[string]any{"name": "w2"}))
	requireNoProtocolErrors(t, renamed.Diagnostics)
	if len(renamed.RequiresReplace) == 0 {
		t.Error("renaming does not require replacement")
	}

	// ModifyPlan's errors reach the protocol response.
	forbidden := h.plan(t, typeName, nil, h.resourceValue(t, typeName, map[string]any{"name": "forbidden"}))
	requireProtocolErrorContaining(t, forbidden.Diagnostics, "Forbidden name")

	// Import by id and by identity read the widget back in full.
	byID := h.importByID(t, typeName, "w1")
	if !byID.state.Equal(inst.state) {
		t.Errorf("import by id = %s, want %s", byID.state, inst.state)
	}
	byIdentity := h.importByIdentity(t, typeName, map[string]any{"name": "w1"})
	if !byIdentity.state.Equal(inst.state) {
		t.Errorf("import by identity = %s, want %s", byIdentity.state, inst.state)
	}

	h.destroy(t, inst)
	if len(store.widgets) != 0 {
		t.Fatalf("destroy left %v", store.widgets)
	}
	refreshed := h.refresh(t, inst)
	if !refreshed.state.IsNull() {
		t.Fatalf("refresh after destroy = %s, want the resource removed", refreshed.state)
	}
}

func TestHarnessProtocolValidation(t *testing.T) {
	h := harnessWidgetHarness(t, newFakeKopia(), newHarnessWidgetStore())
	const typeName = "kopia_harness_widget"

	diags := h.validateResource(t, typeName, h.resourceValue(t, typeName, map[string]any{"name": "w1", "size": 0}))
	requireProtocolErrorContaining(t, diags, "must be at least 1")

	diags = h.validateResource(t, typeName, h.resourceValue(t, typeName, map[string]any{"name": "w1", "labels": []string{""}}))
	requireProtocolErrorContaining(t, diags, "Labels must not be empty")
}

func TestHarnessProtocolDataSource(t *testing.T) {
	fake := newFakeKopia()
	fake.title = "KopiaUI v0.22.3"
	h := harnessWidgetHarness(t, fake, newHarnessWidgetStore())

	state := h.readDataSource(t, "kopia_harness_status", h.dataSourceValue(t, "kopia_harness_status", map[string]any{}))
	if !boolAt(t, state, "connected") || stringAt(t, state, "version") != "0.22.3" {
		t.Fatalf("state = %s", state)
	}
	if !fake.called("GET /api/v1/repo/status") {
		t.Error("the data source did not reach the fake")
	}

	fake.replyError("GET /api/v1/repo/status", 400, kopiaclient.CodeNotConnected, "not connected")
	resp := h.readDataSourceResponse(t, "kopia_harness_status", h.dataSourceValue(t, "kopia_harness_status", map[string]any{}))
	requireProtocolErrorContaining(t, resp.Diagnostics, "NOT_CONNECTED")
}

// A server restart between two operations invalidates the session's CSRF
// token; the client renews it on its own, through the provider too.
func TestHarnessSurvivesAServerRestart(t *testing.T) {
	fake := newFakeKopia()
	h := harnessWidgetHarness(t, fake, newHarnessWidgetStore())
	config := h.dataSourceValue(t, "kopia_harness_status", map[string]any{})

	h.readDataSource(t, "kopia_harness_status", config)
	fake.restart()
	fake.handle("GET /api/v1/repo/status", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]any{"connected": false})
	})
	state := h.readDataSource(t, "kopia_harness_status", config)
	if boolAt(t, state, "connected") {
		t.Error("connected = true, want the answer of the new handler")
	}

	if got := fake.count("GET /"); got != 2 {
		t.Errorf("GET / was requested %d times, want 2: the session and its renewal", got)
	}
	want := []string{"GET /api/v1/repo/status", "GET /api/v1/repo/status", "GET /api/v1/repo/status"}
	if got := fake.apiRoutes(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("API calls = %v, want %v: one before the restart, one rejected, one retried", got, want)
	}
	if got := fake.query(t, "GET /api/v1/repo/status"); len(got) != 0 {
		t.Errorf("query = %v, want none", got)
	}
}

// Terraform proposes the prior value for a computed attribute the
// configuration leaves null, descends into nested attributes, and proposes
// null for a nested attribute the configuration leaves out.
func TestHarnessProposedNewState(t *testing.T) {
	h := harnessWidgetHarness(t, newFakeKopia(), newHarnessWidgetStore())
	const typeName = "kopia_harness_widget"
	block := h.resourceSchema(t, typeName).Block

	prior := h.resourceValue(t, typeName, map[string]any{
		"id": "w1", "name": "w1", "size": 5, "revision": 4,
		"settings": map[string]any{"mode": "fast", "level": 2},
	})

	proposed := proposedNewState(t, block, prior, h.resourceValue(t, typeName, map[string]any{
		"name": "w1", "settings": map[string]any{"mode": "slow"},
	}))
	if got := stringAt(t, proposed, "id"); got != "w1" {
		t.Errorf("proposed id = %q, want the prior w1", got)
	}
	if got := int64At(t, proposed, "revision"); got != 4 {
		t.Errorf("proposed revision = %d, want the prior 4", got)
	}
	if !isNullAt(t, proposed, "size") {
		t.Error("proposed size is not null: it is not computed, so the configuration's null wins")
	}
	if got := stringAt(t, proposed, "settings", "mode"); got != "slow" {
		t.Errorf("proposed settings.mode = %q, want the configured slow", got)
	}
	if got := int64At(t, proposed, "settings", "level"); got != 2 {
		t.Errorf("proposed settings.level = %d, want the prior 2", got)
	}

	withoutSettings := proposedNewState(t, block, prior, h.resourceValue(t, typeName, map[string]any{"name": "w1"}))
	if !isNullAt(t, withoutSettings, "settings") {
		t.Error("proposed settings is not null although the configuration leaves it out")
	}
}

func TestCaptureAttr(t *testing.T) {
	state := &terraform.State{Modules: []*terraform.ModuleState{{
		Path: []string{"root"},
		Resources: map[string]*terraform.ResourceState{
			"kopia_policy.test": {Type: "kopia_policy", Primary: &terraform.InstanceState{ID: "(global)", Attributes: map[string]string{"id": "(global)"}}},
		},
	}}}
	var got string
	if err := captureAttr("kopia_policy.test", "id", &got)(state); err != nil || got != "(global)" {
		t.Fatalf("captureAttr() = %q, %v, want (global)", got, err)
	}
	if err := captureAttr("kopia_policy.missing", "id", &got)(state); err == nil {
		t.Fatal("captureAttr() found a resource that is not in state")
	}
}

// requireAttributeError and friends match on the path's string form.
func TestHarnessAttributeDiagnostics(t *testing.T) {
	var diags diag.Diagnostics
	diags.AddAttributeError(path.Root("scheduling").AtName("time_of_day"), "Bad", "Bad time.")
	diags.AddAttributeWarning(path.Root("retention"), "Odd", "Odd retention.")
	requireAttributeError(t, diags, "scheduling.time_of_day")
	requireAttributeWarning(t, diags, "retention")
	requireErrorContaining(t, diags, "Bad time")
	requireWarningContaining(t, diags, "Odd retention")
	requireNoErrorContaining(t, diags, "Odd")
}

// A harness with resources of its own serves no list resources unless told
// which, because the framework refuses a list resource whose resource is not
// registered; it serves provider.go's actions unless told otherwise.
func TestHarnessListResourceAndActionRegistrations(t *testing.T) {
	fake := newFakeKopia()

	h := newProtocolHarness(t, fake, withResources(NewPolicyResource))
	if got := harnessSortedKeys(h.schemas.ListResourceSchemas); len(got) != 0 {
		t.Errorf("list resources = %v, want none", got)
	}
	if got := strings.Join(harnessSortedKeys(h.schemas.ActionSchemas), ","); got != "kopia_notification_test,kopia_snapshot_now" {
		t.Errorf("actions = %s, want provider.go's", got)
	}

	h = newProtocolHarness(t, fake, withResources(NewPolicyResource), withListResources(NewPolicyListResource),
		withActions(NewSnapshotNowAction))
	if got := strings.Join(harnessSortedKeys(h.schemas.ListResourceSchemas), ","); got != "kopia_policy" {
		t.Errorf("list resources = %s, want kopia_policy", got)
	}
	if got := strings.Join(harnessSortedKeys(h.schemas.ActionSchemas), ","); got != "kopia_snapshot_now" {
		t.Errorf("actions = %s, want kopia_snapshot_now", got)
	}
}
