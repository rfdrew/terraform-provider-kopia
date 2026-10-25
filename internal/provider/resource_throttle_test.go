package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// repositoryConfigFileLockHeld reports whether someone holds c's config-file
// lock, by trying to take it from another goroutine for a moment. Called from
// a fake handler, the holder can only be the resource waiting for that very
// handler, so it cannot let go in between. When the lock is held, the
// goroutine takes and releases it once the holder is done.
func repositoryConfigFileLockHeld(c *kopiaclient.Client) bool {
	acquired := make(chan struct{})
	go func() {
		c.LockConfigFile()
		c.UnlockConfigFile()
		close(acquired)
	}()
	select {
	case <-acquired:
		return false
	case <-time.After(50 * time.Millisecond):
		return true
	}
}

// throttleFakeServer answers the throttle endpoints the way Kopia does: PUT
// replaces every limit with the body, so a limit the body leaves out becomes
// unlimited, and GET returns the limits in effect with the unlimited ones
// left out. It records whether the config-file lock of client was held
// during each PUT. With saveError set, a PUT applies the limits and then
// fails with it, as Kopia does when it cannot write its config file.
type throttleFakeServer struct {
	mu         sync.Mutex
	limits     kopiaclient.ThrottleLimits
	lockedPuts []bool
	saveError  string
}

func serveThrottleForTest(fake *fakeKopia, client *kopiaclient.Client, initial kopiaclient.ThrottleLimits) *throttleFakeServer {
	s := &throttleFakeServer{limits: initial}
	fake.handle("GET /api/v1/repo/throttle", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeFakeJSON(w, s.limits)
	})
	fake.handle("PUT /api/v1/repo/throttle", func(w http.ResponseWriter, r *http.Request) {
		locked := client != nil && repositoryConfigFileLockHeld(client)
		raw, _ := io.ReadAll(r.Body)
		var limits kopiaclient.ThrottleLimits
		if err := json.Unmarshal(raw, &limits); err != nil {
			writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "unable to decode request: "+err.Error()))
			return
		}
		s.mu.Lock()
		s.limits = limits
		s.lockedPuts = append(s.lockedPuts, locked)
		saveError := s.saveError
		s.mu.Unlock()
		if saveError != "" {
			writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "unable to set limits: "+saveError))
			return
		}
		writeFakeJSON(w, map[string]any{})
	})
	return s
}

// failSaving makes every PUT fail to save its limits after applying them,
// with message as the reason; "" ends that.
func (s *throttleFakeServer) failSaving(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveError = message
}

func (s *throttleFakeServer) current() kopiaclient.ThrottleLimits {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits
}

func (s *throttleFakeServer) everyPutLocked(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lockedPuts) == 0 {
		t.Fatal("no PUT /api/v1/repo/throttle was made")
	}
	for i, locked := range s.lockedPuts {
		if !locked {
			t.Errorf("PUT %d was made without the config-file lock", i+1)
		}
	}
}

// throttleAllLimits sets every limit, as configuration and plan alike: the
// resource has nothing computed but its id.
func throttleAllLimits(id any) map[string]any {
	return map[string]any{
		"id":                                  id,
		"reads_per_second":                    10,
		"writes_per_second":                   5.5,
		"lists_per_second":                    0.25,
		"max_upload_speed_bytes_per_second":   10485760,
		"max_download_speed_bytes_per_second": 2e21,
		"concurrent_reads":                    8,
		"concurrent_writes":                   4,
	}
}

func TestThrottleSchemaIsValid(t *testing.T) {
	requireValidResourceSchema(t, &throttleResource{})
}

// Create sends every limit in one PUT, without reading the server's limits
// first: the resource owns all seven rather than merging into them.
func TestThrottleCreateSendsEveryLimit(t *testing.T) {
	fake := newFakeKopia()
	c := fake.client(t)
	server := serveThrottleForTest(fake, c, kopiaclient.ThrottleLimits{})
	r := &throttleResource{client: c}

	config := throttleAllLimits(nil)
	resp := createResource(t, r, resourceValue(t, r, config), resourceValue(t, r, throttleAllLimits(tfUnknown)))
	requireNoErrors(t, resp.Diagnostics)

	if got := fake.apiRoutes(); strings.Join(got, ",") != "PUT /api/v1/repo/throttle,GET /api/v1/repo/throttle" {
		t.Errorf("API calls = %v, want the PUT, then a GET to read the limits back", got)
	}
	body := fake.body(t, "PUT /api/v1/repo/throttle")
	want := map[string]any{
		"readsPerSecond": 10.0, "writesPerSecond": 5.5, "listsPerSecond": 0.25,
		"maxUploadSpeedBytesPerSecond": 10485760.0, "maxDownloadSpeedBytesPerSecond": 2e21,
		"concurrentReads": 8.0, "concurrentWrites": 4.0,
	}
	if len(body) != len(want) {
		t.Errorf("body = %v, want all seven limits", body)
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("body[%s] = %v, want %v", key, body[key], value)
		}
	}
	server.everyPutLocked(t)

	if got := stringAt(t, resp.State.Raw, "id"); got != throttleID {
		t.Errorf("id = %q, want %q", got, throttleID)
	}
	if !resp.State.Raw.Equal(resourceValue(t, r, throttleAllLimits(throttleID))) {
		t.Errorf("state = %s, want every limit as configured", resp.State.Raw)
	}
}

// A null limit is unlimited: it is left out of the PUT, which Kopia reads as
// 0, so limits set outside Terraform do not survive, and it reads back as
// null.
func TestThrottleNullLimitsAreSentAsUnlimited(t *testing.T) {
	fake := newFakeKopia()
	c := fake.client(t)
	server := serveThrottleForTest(fake, c, kopiaclient.ThrottleLimits{UploadBytesPerSecond: 1048576, ConcurrentReads: 2})
	r := &throttleResource{client: c}

	config := resourceValue(t, r, map[string]any{"concurrent_writes": 4})
	plan := resourceValue(t, r, map[string]any{"id": tfUnknown, "concurrent_writes": 4})
	resp := createResource(t, r, config, plan)
	requireNoErrors(t, resp.Diagnostics)

	if got := string(fake.lastCall(t, "PUT /api/v1/repo/throttle").body); got != `{"concurrentWrites":4}` {
		t.Errorf("body = %s, want only the configured limit", got)
	}
	if got := server.current(); got != (kopiaclient.ThrottleLimits{ConcurrentWrites: 4}) {
		t.Errorf("server limits = %+v, want the others unlimited", got)
	}
	if !resp.State.Raw.Equal(resourceValue(t, r, map[string]any{"id": throttleID, "concurrent_writes": 4})) {
		t.Errorf("state = %s, want every other limit null", resp.State.Raw)
	}
}

// Kopia leaves unlimited limits out, which decode as 0; 0 is null, whether
// the field is absent or, should a release ever send it, explicit.
func TestThrottleReadMapsUnlimitedToNull(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/throttle", http.StatusOK,
		"{\n  \"readsPerSecond\": 0,\n  \"maxUploadSpeedBytesPerSecond\": 1048576,\n  \"concurrentReads\": 0\n}\n")
	r := &throttleResource{client: fake.client(t)}

	resp := readResource(t, r, resourceValue(t, r, throttleAllLimits(throttleID)))
	requireNoErrors(t, resp.Diagnostics)
	want := resourceValue(t, r, map[string]any{"id": throttleID, "max_upload_speed_bytes_per_second": 1048576})
	if !resp.State.Raw.Equal(want) {
		t.Errorf("state = %s, want %s", resp.State.Raw, want)
	}

	// Removing every limit outside Terraform reads as a change, not as the
	// resource disappearing: the limits always exist.
	fake.reply("GET /api/v1/repo/throttle", http.StatusOK, "{}\n")
	resp = readResource(t, r, want)
	requireNoErrors(t, resp.Diagnostics)
	if resp.State.Raw.IsNull() || !isNullAt(t, resp.State.Raw, "max_upload_speed_bytes_per_second") {
		t.Errorf("state = %s, want the resource kept with no limits", resp.State.Raw)
	}
}

// Destroying resets every limit with PUT {}, the only reset the API has.
func TestThrottleDeleteSendsAnEmptyObject(t *testing.T) {
	fake := newFakeKopia()
	c := fake.client(t)
	server := serveThrottleForTest(fake, c, kopiaclient.ThrottleLimits{ReadsPerSecond: 10, ConcurrentWrites: 4})
	r := &throttleResource{client: c}

	resp := deleteResource(t, r, resourceValue(t, r, throttleAllLimits(throttleID)))
	requireNoErrors(t, resp.Diagnostics)
	requireNoWarnings(t, resp.Diagnostics)

	if got := string(fake.lastCall(t, "PUT /api/v1/repo/throttle").body); got != "{}" {
		t.Errorf("body = %s, want {}", got)
	}
	if got := fake.apiRoutes(); len(got) != 1 {
		t.Errorf("API calls = %v, want the PUT alone", got)
	}
	if got := server.current(); got != (kopiaclient.ThrottleLimits{}) {
		t.Errorf("server limits = %+v, want none", got)
	}
	server.everyPutLocked(t)
}

// Kopia answers 400 MALFORMED_REQUEST both for limits it refused and for
// limits it applied but could not save, and the diagnostic says which. Either
// way a create leaves the resource out of state, so the next apply creates it
// again, and a destroy leaves it in state, so the next one tries again.
func TestThrottleWriteErrors(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		want     string
		readBack bool
	}{
		{
			name:     "config file not writable",
			message:  "unable to set limits: error writing file: open /app/config/repository.config.tmp: read-only file system",
			want:     "Kopia applied the throttle limits, which are in effect now, but could not write them",
			readBack: true,
		},
		{
			name:    "limit refused",
			message: "unable to set limits: ConcurrentReads: invalid limit",
			want:    "Kopia refused the throttle limits and kept the ones it had",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.replyError("PUT /api/v1/repo/throttle", http.StatusBadRequest, kopiaclient.CodeMalformedRequest, tc.message)
			fake.reply("GET /api/v1/repo/throttle", http.StatusOK, "{\n  \"concurrentReads\": 8\n}\n")
			r := &throttleResource{client: fake.client(t)}

			config := resourceValue(t, r, map[string]any{"concurrent_reads": 8})
			resp := createResource(t, r, config, resourceValue(t, r, map[string]any{"id": tfUnknown, "concurrent_reads": 8}))
			requireErrorContaining(t, resp.Diagnostics, tc.want)
			requireErrorContaining(t, resp.Diagnostics, tc.message)
			if !resp.State.Raw.IsNull() {
				t.Errorf("state = %s, want none after a failed create", resp.State.Raw)
			}
			// Limits Kopia refused were never in effect, so there is nothing to
			// read back.
			if fake.called("GET /api/v1/repo/throttle") != tc.readBack {
				t.Errorf("GET after the failed PUT = %v, want %v", fake.called("GET /api/v1/repo/throttle"), tc.readBack)
			}

			deleted := deleteResource(t, r, resourceValue(t, r, map[string]any{"id": throttleID, "concurrent_reads": 8}))
			requireErrorContaining(t, deleted.Diagnostics, tc.message)
			if tc.readBack {
				requireErrorContaining(t, deleted.Diagnostics, "Kopia removed every throttle limit, so the server is unlimited now")
			} else {
				requireErrorContaining(t, deleted.Diagnostics, "Unable to remove Kopia throttle limits")
			}
		})
	}
}

// NOT_CONNECTED is about the server's repository connection, and the
// diagnostics say what to do about it.
func TestThrottleWithoutARepository(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/repo/throttle", http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected")
	fake.replyError("PUT /api/v1/repo/throttle", http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected")
	r := &throttleResource{client: fake.client(t)}
	state := resourceValue(t, r, map[string]any{"id": throttleID, "concurrent_reads": 8})

	read := readResource(t, r, state)
	requireErrorContaining(t, read.Diagnostics, "Unable to read Kopia throttle limits")
	requireErrorContaining(t, read.Diagnostics, "has no repository connected")
	requireErrorContaining(t, read.Diagnostics, kopiaclient.CodeNotConnected)

	deleted := deleteResource(t, r, state)
	requireErrorContaining(t, deleted.Diagnostics, "has no repository connected")

	// Through the protocol server, as the framework sets up an update: a
	// failed one keeps the prior state.
	h := newProtocolHarness(t, fake, withResources(NewThrottleResource))
	const typeName = "kopia_throttle"
	inst := &resourceInstance{typeName: typeName, state: h.resourceValue(t, typeName, map[string]any{"id": throttleID, "concurrent_reads": 8})}
	changed := h.resourceValue(t, typeName, map[string]any{"concurrent_reads": 4})
	planned := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, planned.Diagnostics)
	applied := h.apply(t, typeName, inst, changed, planned)
	requireProtocolErrorContaining(t, applied.Diagnostics, "Unable to update Kopia throttle limits")
	requireProtocolErrorContaining(t, applied.Diagnostics, "has no repository connected")
	if got := h.decodeResource(t, typeName, applied.NewState); !got.Equal(inst.state) {
		t.Errorf("state after a failed update = %s, want the prior state", got)
	}
}

// A server that now reaches its repository through a repository server has
// no limits of its own: a refresh removes the resource with a warning rather
// than failing, because Terraform refreshes before it destroys, and creating
// it fails with the same explanation.
func TestThrottleOnARepositoryServer(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/repo/throttle", http.StatusBadRequest, kopiaclient.CodeStorageConnection, "no direct storage connection")
	fake.replyError("PUT /api/v1/repo/throttle", http.StatusBadRequest, kopiaclient.CodeStorageConnection, "no direct storage connection")
	r := &throttleResource{client: fake.client(t)}
	state := resourceValue(t, r, map[string]any{"id": throttleID, "concurrent_reads": 8})

	read := readResource(t, r, state)
	requireNoErrors(t, read.Diagnostics)
	requireWarningContaining(t, read.Diagnostics, "Kopia throttle limits no longer exist")
	requireWarningContaining(t, read.Diagnostics, "Manage them on the repository server")
	if !read.State.Raw.IsNull() {
		t.Errorf("state = %s, want the resource removed", read.State.Raw)
	}

	created := createResource(t, r, resourceValue(t, r, map[string]any{"concurrent_reads": 8}),
		resourceValue(t, r, map[string]any{"id": tfUnknown, "concurrent_reads": 8}))
	requireErrorContaining(t, created.Diagnostics, "Unable to set Kopia throttle limits")
	requireErrorContaining(t, created.Diagnostics, "Manage them on the repository server")

	// Destroying without a refresh (-refresh=false) only warns as well.
	deleted := deleteResource(t, r, state)
	requireNoErrors(t, deleted.Diagnostics)
	requireWarningContaining(t, deleted.Diagnostics, "Kopia throttle limits were left unchanged")
}

func TestThrottleImportIgnoresTheID(t *testing.T) {
	r := &throttleResource{}
	resp := importResource(t, r, "anything")
	requireNoErrors(t, resp.Diagnostics)
	if got := stringAt(t, resp.State.Raw, "id"); got != throttleID {
		t.Errorf("imported id = %q, want %q", got, throttleID)
	}
}

// The whole lifecycle through the protocol server: plans stay empty after an
// apply, import reads what was applied, and destroy removes every limit.
func TestThrottleProtocolLifecycle(t *testing.T) {
	fake := newFakeKopia()
	server := serveThrottleForTest(fake, nil, kopiaclient.ThrottleLimits{DownloadBytesPerSecond: 5e6})
	h := newProtocolHarness(t, fake, withResources(NewThrottleResource))
	const typeName = "kopia_throttle"

	config := h.resourceValue(t, typeName, map[string]any{"max_upload_speed_bytes_per_second": 1048576, "concurrent_reads": 8})
	inst := h.create(t, typeName, config)
	if got := server.current(); got != (kopiaclient.ThrottleLimits{UploadBytesPerSecond: 1048576, ConcurrentReads: 8}) {
		t.Errorf("server limits = %+v, want the configured ones alone", got)
	}
	if !isNullAt(t, inst.state, "max_download_speed_bytes_per_second") {
		t.Errorf("state = %s, want the download limit the server had replaced", inst.state)
	}

	replan := h.plan(t, typeName, inst, config)
	requireNoProtocolErrors(t, replan.Diagnostics)
	if planned := h.decodeResource(t, typeName, replan.PlannedState); !planned.Equal(inst.state) {
		t.Errorf("plan after apply = %s, want the state unchanged", planned)
	}

	changed := h.resourceValue(t, typeName, map[string]any{"reads_per_second": 0.5})
	planned := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, planned.Diagnostics)
	if len(planned.RequiresReplace) != 0 {
		t.Errorf("changing the limits requires replacement: %v", planned.RequiresReplace)
	}
	inst = h.update(t, inst, changed)
	if got := server.current(); got != (kopiaclient.ThrottleLimits{ReadsPerSecond: 0.5}) {
		t.Errorf("server limits = %+v, want the reads limit alone", got)
	}
	if got := float64At(t, inst.state, "reads_per_second"); got != 0.5 {
		t.Errorf("reads_per_second = %v, want 0.5", got)
	}

	imported := h.importByID(t, typeName, "throttle")
	if !imported.state.Equal(inst.state) {
		t.Errorf("import = %s, want %s", imported.state, inst.state)
	}

	h.destroy(t, inst)
	if got := server.current(); got != (kopiaclient.ThrottleLimits{}) {
		t.Errorf("server limits after destroy = %+v, want none", got)
	}
}

// An update whose limits Kopia applied but could not save records them, as
// they are in effect, and plans an update on the next run even though the
// configuration has not changed, so that the limits are saved once the config
// file is writable again.
func TestThrottleUpdateSavesUnsavedLimitsAgain(t *testing.T) {
	fake := newFakeKopia()
	server := serveThrottleForTest(fake, nil, kopiaclient.ThrottleLimits{})
	h := newProtocolHarness(t, fake, withResources(NewThrottleResource))
	const typeName = "kopia_throttle"

	inst := h.create(t, typeName, h.resourceValue(t, typeName, map[string]any{"concurrent_reads": 8}))

	// The config file turns read-only: Kopia applies the limits, then fails to
	// save them.
	server.failSaving("error writing file: open /app/config/repository.config: read-only file system")
	changed := h.resourceValue(t, typeName, map[string]any{"concurrent_reads": 4})
	planned := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, planned.Diagnostics)
	applied := h.apply(t, typeName, inst, changed, planned)
	requireProtocolErrorContaining(t, applied.Diagnostics, "Kopia throttle limits were not saved")
	inst = &resourceInstance{
		typeName: typeName,
		state:    h.decodeResource(t, typeName, applied.NewState),
		private:  applied.Private,
	}
	if got := int64At(t, inst.state, "concurrent_reads"); got != 4 {
		t.Errorf("concurrent_reads = %d, want 4, the limit in effect", got)
	}
	if got := server.current(); got != (kopiaclient.ThrottleLimits{ConcurrentReads: 4}) {
		t.Errorf("server limits = %+v, want the new ones in effect", got)
	}

	// A refresh keeps the mark, and the unchanged configuration plans an
	// update with a warning.
	inst = h.refresh(t, inst)
	replan := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, replan.Diagnostics)
	requireProtocolWarningContaining(t, replan.Diagnostics, "Kopia throttle limits will be saved again")
	if !isUnknownAt(t, h.decodeResource(t, typeName, replan.PlannedState), "id") {
		t.Errorf("planned id = %s, want unknown so that Terraform updates", valueAt(t, h.decodeResource(t, typeName, replan.PlannedState), "id"))
	}

	// Once the file is writable, that update saves the limits and clears the
	// mark: the plan after it is empty.
	server.failSaving("")
	puts := fake.count("PUT /api/v1/repo/throttle")
	inst = h.update(t, inst, changed)
	if fake.count("PUT /api/v1/repo/throttle") != puts+1 || stringAt(t, inst.state, "id") != throttleID {
		t.Errorf("update made %d PUTs and left id %s", fake.count("PUT /api/v1/repo/throttle")-puts, valueAt(t, inst.state, "id"))
	}
	final := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, final.Diagnostics)
	if planned := h.decodeResource(t, typeName, final.PlannedState); !planned.Equal(inst.state) || len(final.Diagnostics) != 0 {
		t.Errorf("plan after saving = %s with %v, want the state unchanged and no warning", planned, final.Diagnostics)
	}
}

// Kopia reads 0 as unlimited, which the resource spells null, and refuses
// negative limits.
func TestThrottleValidatesLimits(t *testing.T) {
	h := newProtocolHarness(t, newFakeKopia(), withResources(NewThrottleResource))
	const typeName = "kopia_throttle"

	for name, attrs := range map[string]map[string]any{
		"zero rate":           {"reads_per_second": 0},
		"negative rate":       {"max_upload_speed_bytes_per_second": -1},
		"zero concurrency":    {"concurrent_reads": 0},
		"too much concurrent": {"concurrent_writes": int64(math.MaxInt32) + 1},
	} {
		t.Run(name, func(t *testing.T) {
			diags := h.validateResource(t, typeName, h.resourceValue(t, typeName, attrs))
			requireProtocolErrorContaining(t, diags, "Attribute")
		})
	}

	diags := h.validateResource(t, typeName, h.resourceValue(t, typeName, map[string]any{"reads_per_second": 0}))
	requireProtocolErrorContaining(t, diags, "value must be greater than 0, got: 0")

	requireNoProtocolErrors(t, h.validateResource(t, typeName, h.resourceValue(t, typeName, map[string]any{
		"reads_per_second": 0.001, "concurrent_writes": math.MaxInt32,
	})))
	requireNoProtocolErrors(t, h.validateResource(t, typeName, h.resourceValue(t, typeName, map[string]any{
		"reads_per_second": tfUnknown,
	})))
}

// --- acceptance --------------------------------------------------------------

// testAccThrottleLimits reads the limits in effect on the test server.
func testAccThrottleLimits() (*kopiaclient.ThrottleLimits, error) {
	c, err := testAccClient()
	if err != nil {
		return nil, err
	}
	return c.GetThrottle(context.Background())
}

// testAccCheckThrottleRemoved checks that destroying kopia_throttle left the
// server without limits.
func testAccCheckThrottleRemoved(*terraform.State) error {
	limits, err := testAccThrottleLimits()
	if err != nil {
		return err
	}
	if *limits != (kopiaclient.ThrottleLimits{}) {
		return fmt.Errorf("the server still has throttle limits after destroy: %+v", *limits)
	}
	return nil
}

func TestAccThrottleResource(t *testing.T) {
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckThrottleRemoved,
		Steps: []acc.TestStep{
			{
				// Every limit, fractional rates included.
				Config: providerConfig + `
resource "kopia_throttle" "this" {
  reads_per_second                    = 100
  writes_per_second                   = 50.5
  lists_per_second                    = 0.5
  max_upload_speed_bytes_per_second   = 10485760
  max_download_speed_bytes_per_second = 20971520
  concurrent_reads                    = 16
  concurrent_writes                   = 8
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("kopia_throttle.this", "id", "throttle"),
					acc.TestCheckResourceAttr("kopia_throttle.this", "writes_per_second", "50.5"),
					acc.TestCheckResourceAttr("kopia_throttle.this", "lists_per_second", "0.5"),
					acc.TestCheckResourceAttr("kopia_throttle.this", "max_upload_speed_bytes_per_second", "10485760"),
					acc.TestCheckResourceAttr("kopia_throttle.this", "concurrent_writes", "8"),
				),
			},
			{
				// Import must round-trip without producing a diff; the import id
				// is ignored.
				ResourceName:      "kopia_throttle.this",
				ImportState:       true,
				ImportStateId:     "throttle",
				ImportStateVerify: true,
			},
			{
				// Limits left out become unlimited on the server and null in
				// state, rather than keeping their previous values.
				Config: providerConfig + `
resource "kopia_throttle" "this" {
  max_upload_speed_bytes_per_second = 1048576
}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("kopia_throttle.this", "max_upload_speed_bytes_per_second", "1048576"),
					acc.TestCheckNoResourceAttr("kopia_throttle.this", "reads_per_second"),
					acc.TestCheckNoResourceAttr("kopia_throttle.this", "concurrent_reads"),
					func(*terraform.State) error {
						limits, err := testAccThrottleLimits()
						if err != nil {
							return err
						}
						if *limits != (kopiaclient.ThrottleLimits{UploadBytesPerSecond: 1048576}) {
							return fmt.Errorf("server limits = %+v, want the upload limit alone", *limits)
						}
						return nil
					},
				),
			},
			{
				// A refresh of an unchanged configuration plans nothing.
				RefreshState: true,
			},
		},
	})
}

// TestAccThrottleResource_restart restarts the server between two plans.
// Kopia saves the limits in its config file and applies them again when it
// opens the repository at start, so afterwards the server reports the same
// limits and the next plan is empty. It runs only with KOPIA_ACC_RESTART set;
// see envAccRestart.
func TestAccThrottleResource_restart(t *testing.T) {
	// The rates are generous because they already throttle the server while
	// it opens the repository again.
	want := kopiaclient.ThrottleLimits{
		ReadsPerSecond:         100,
		WritesPerSecond:        50.5,
		ListsPerSecond:         20.5,
		UploadBytesPerSecond:   10485760,
		DownloadBytesPerSecond: 20971520,
		ConcurrentReads:        16,
		ConcurrentWrites:       8,
	}
	config := providerConfig + `
resource "kopia_throttle" "this" {
  reads_per_second                    = 100
  writes_per_second                   = 50.5
  lists_per_second                    = 20.5
  max_upload_speed_bytes_per_second   = 10485760
  max_download_speed_bytes_per_second = 20971520
  concurrent_reads                    = 16
  concurrent_writes                   = 8
}
`
	checkServerLimits := func() error {
		limits, err := testAccThrottleLimits()
		if err != nil {
			return err
		}
		if *limits != want {
			return fmt.Errorf("server limits = %+v, want %+v", *limits, want)
		}
		return nil
	}

	acc.Test(t, acc.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testAccPreCheckRestart(t)
		},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckThrottleRemoved,
		Steps: []acc.TestStep{
			{
				Config: config,
				Check:  func(*terraform.State) error { return checkServerLimits() },
			},
			{
				// The server is asked for its limits before Terraform reads
				// them, and the plan, refresh included, must not change them.
				// The repository status holds the limits the config file had
				// when the repository was opened, so it shows that they came
				// back from there.
				PreConfig: func() {
					testAccRestartServer(t)
					if err := checkServerLimits(); err != nil {
						t.Errorf("after the restart: %v", err)
					}
					c, err := testAccClient()
					if err != nil {
						t.Fatal(err)
					}
					status, err := c.RepoStatus(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if status.ThrottlingLimits == nil || *status.ThrottlingLimits != want {
						t.Errorf("after the restart the repository was opened with limits %+v, want %+v",
							status.ThrottlingLimits, want)
					}
				},
				Config: config,
				ConfigPlanChecks: acc.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}
