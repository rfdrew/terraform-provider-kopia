package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// repositoryDescriptionFakeServer answers POST /api/v1/repo/description and
// GET /api/v1/repo/status the way Kopia does, keeping the description, and
// the throttle endpoints with fixed limits. It records each of those calls in
// order, with whether the config-file lock of client was held at the time.
// With throttleSaveError set, a throttle PUT fails with it, as Kopia does when
// it cannot write its config file.
type repositoryDescriptionFakeServer struct {
	mu                sync.Mutex
	description       string
	limits            kopiaclient.ThrottleLimits
	events            []string
	throttleSaveError string
}

func serveRepositoryDescriptionForTest(t *testing.T, fake *fakeKopia, client *kopiaclient.Client, limits kopiaclient.ThrottleLimits) *repositoryDescriptionFakeServer {
	t.Helper()
	s := &repositoryDescriptionFakeServer{description: "Repository in Filesystem: /app/repo", limits: limits}
	record := func(event string) {
		if client != nil && repositoryConfigFileLockHeld(client) {
			event += " (locked)"
		}
		s.mu.Lock()
		s.events = append(s.events, event)
		s.mu.Unlock()
	}
	status := func(w http.ResponseWriter) {
		var body map[string]any
		if err := json.Unmarshal([]byte(fakeRepoStatusJSON), &body); err != nil {
			t.Error(err)
		}
		s.mu.Lock()
		body["description"] = s.description
		if s.description == "" {
			delete(body, "description")
		}
		s.mu.Unlock()
		writeFakeJSON(w, body)
	}

	fake.handle("GET /api/v1/repo/status", func(w http.ResponseWriter, _ *http.Request) { status(w) })
	fake.handle("POST /api/v1/repo/description", func(w http.ResponseWriter, r *http.Request) {
		record("POST description")
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "unable to decode request: "+err.Error()))
			return
		}
		s.mu.Lock()
		s.description = req.Description
		s.mu.Unlock()
		status(w)
	})
	fake.handle("GET /api/v1/repo/throttle", func(w http.ResponseWriter, _ *http.Request) {
		record("GET throttle")
		s.mu.Lock()
		defer s.mu.Unlock()
		writeFakeJSON(w, s.limits)
	})
	fake.handle("PUT /api/v1/repo/throttle", func(w http.ResponseWriter, r *http.Request) {
		record("PUT throttle")
		raw, _ := io.ReadAll(r.Body)
		var limits kopiaclient.ThrottleLimits
		if err := json.Unmarshal(raw, &limits); err != nil {
			writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "unable to decode request: "+err.Error()))
			return
		}
		s.mu.Lock()
		s.limits = limits
		saveError := s.throttleSaveError
		s.mu.Unlock()
		if saveError != "" {
			writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "unable to set limits: "+saveError))
			return
		}
		writeFakeJSON(w, map[string]any{})
	})
	return s
}

// failSavingThrottle makes every throttle PUT fail to save its limits, with
// message as the reason; "" ends that.
func (s *repositoryDescriptionFakeServer) failSavingThrottle(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttleSaveError = message
}

func (s *repositoryDescriptionFakeServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func TestRepositoryDescriptionSchemaIsValid(t *testing.T) {
	requireValidResourceSchema(t, &repositoryDescriptionResource{})
}

// Saving the description makes Kopia rewrite its config file with the
// throttle limits it started with, so the limits in effect are read and put
// back straight after, all under the lock kopia_throttle takes as well.
func TestRepositoryDescriptionSavesTheThrottleLimitsAgain(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			fake := newFakeKopia()
			c := fake.client(t)
			limits := kopiaclient.ThrottleLimits{UploadBytesPerSecond: 1048576, ConcurrentReads: 8}
			server := serveRepositoryDescriptionForTest(t, fake, c, limits)
			r := &repositoryDescriptionResource{client: c}

			config := resourceValue(t, r, map[string]any{"description": "Production backups"})
			if operation == "create" {
				resp := createResource(t, r, config, resourceValue(t, r, map[string]any{"id": tfUnknown, "description": "Production backups"}))
				requireNoErrors(t, resp.Diagnostics)
				if !resp.State.Raw.Equal(resourceValue(t, r, map[string]any{"id": repositoryDescriptionID, "description": "Production backups"})) {
					t.Errorf("state = %s", resp.State.Raw)
				}
			} else {
				prior := resourceValue(t, r, map[string]any{"id": repositoryDescriptionID, "description": "Old"})
				resp := updateResource(t, r, config, resourceValue(t, r, map[string]any{"id": repositoryDescriptionID, "description": "Production backups"}), prior)
				requireNoErrors(t, resp.Diagnostics)
				if got := stringAt(t, resp.State.Raw, "description"); got != "Production backups" {
					t.Errorf("description = %q", got)
				}
			}

			want := []string{"POST description (locked)", "GET throttle (locked)", "PUT throttle (locked)"}
			if got := server.recorded(); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("calls = %v, want %v", got, want)
			}
			if got := string(fake.lastCall(t, "POST /api/v1/repo/description").body); got != `{"description":"Production backups"}` {
				t.Errorf("POST body = %s", got)
			}
			if got := string(fake.lastCall(t, "PUT /api/v1/repo/throttle").body); got != `{"maxUploadSpeedBytesPerSecond":1048576,"concurrentReads":8}` {
				t.Errorf("PUT body = %s, want the limits the GET returned", got)
			}
		})
	}
}

// Even with no limits at all the PUT is made: the config file may hold
// limits from start-up that the server no longer uses.
func TestRepositoryDescriptionSavesUnlimitedThrottleLimitsToo(t *testing.T) {
	fake := newFakeKopia()
	c := fake.client(t)
	serveRepositoryDescriptionForTest(t, fake, c, kopiaclient.ThrottleLimits{})
	r := &repositoryDescriptionResource{client: c}

	config := resourceValue(t, r, map[string]any{"description": "x"})
	resp := createResource(t, r, config, resourceValue(t, r, map[string]any{"id": tfUnknown, "description": "x"}))
	requireNoErrors(t, resp.Diagnostics)
	if got := string(fake.lastCall(t, "PUT /api/v1/repo/throttle").body); got != "{}" {
		t.Errorf("PUT body = %s, want {}", got)
	}
}

// A server that reaches its repository through a repository server has a
// description but no throttle limits, so there are none to save again.
func TestRepositoryDescriptionThroughARepositoryServer(t *testing.T) {
	fake := newFakeKopia()
	c := fake.client(t)
	serveRepositoryDescriptionForTest(t, fake, c, kopiaclient.ThrottleLimits{})
	fake.replyError("GET /api/v1/repo/throttle", http.StatusBadRequest, kopiaclient.CodeStorageConnection, "no direct storage connection")
	r := &repositoryDescriptionResource{client: c}

	config := resourceValue(t, r, map[string]any{"description": "Remote"})
	resp := createResource(t, r, config, resourceValue(t, r, map[string]any{"id": tfUnknown, "description": "Remote"}))
	requireNoErrors(t, resp.Diagnostics)
	requireNoWarnings(t, resp.Diagnostics)
	if fake.called("PUT /api/v1/repo/throttle") {
		t.Error("the throttle limits were written on a server that has none")
	}
	if got := stringAt(t, resp.State.Raw, "description"); got != "Remote" {
		t.Errorf("description = %q", got)
	}
}

// When saving the limits again fails, the description is set all the same:
// it goes into state, and the error says what is left to do.
func TestRepositoryDescriptionReportsThrottleLimitsNotSavedAgain(t *testing.T) {
	fake := newFakeKopia()
	c := fake.client(t)
	server := serveRepositoryDescriptionForTest(t, fake, c, kopiaclient.ThrottleLimits{ConcurrentWrites: 2})
	server.failSavingThrottle("error writing file: no space left on device")
	r := &repositoryDescriptionResource{client: c}

	config := resourceValue(t, r, map[string]any{"description": "Production backups"})
	resp := createResource(t, r, config, resourceValue(t, r, map[string]any{"id": tfUnknown, "description": "Production backups"}))
	requireErrorContaining(t, resp.Diagnostics, "Kopia throttle limits were not saved again")
	requireErrorContaining(t, resp.Diagnostics, "no space left on device")
	if got := stringAt(t, resp.State.Raw, "description"); got != "Production backups" {
		t.Errorf("state = %s, want the description that was set", resp.State.Raw)
	}
}

// An update that set the description but failed to save the throttle
// limits again records the description and plans an update on the next run
// even though the configuration has not changed, which repeats the whole
// write once the config file is writable again.
func TestRepositoryDescriptionUpdateSavesTheThrottleLimitsLater(t *testing.T) {
	fake := newFakeKopia()
	server := serveRepositoryDescriptionForTest(t, fake, nil, kopiaclient.ThrottleLimits{ConcurrentWrites: 2})
	h := newProtocolHarness(t, fake, withResources(NewRepositoryDescriptionResource))
	const typeName = "kopia_repository_description"

	inst := h.create(t, typeName, h.resourceValue(t, typeName, map[string]any{"description": "Production backups"}))

	server.failSavingThrottle("error writing file: no space left on device")
	changed := h.resourceValue(t, typeName, map[string]any{"description": "Offsite"})
	planned := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, planned.Diagnostics)
	applied := h.apply(t, typeName, inst, changed, planned)
	requireProtocolErrorContaining(t, applied.Diagnostics, "Kopia throttle limits were not saved again")
	inst = &resourceInstance{
		typeName: typeName,
		state:    h.decodeResource(t, typeName, applied.NewState),
		private:  applied.Private,
	}
	if got := stringAt(t, inst.state, "description"); got != "Offsite" {
		t.Errorf("description = %q, want the one Kopia set", got)
	}

	inst = h.refresh(t, inst)
	replan := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, replan.Diagnostics)
	requireProtocolWarningContaining(t, replan.Diagnostics, "Kopia throttle limits will be saved again")
	if !isUnknownAt(t, h.decodeResource(t, typeName, replan.PlannedState), "id") {
		t.Error("planned id is known, want unknown so that Terraform updates")
	}

	server.failSavingThrottle("")
	posts := fake.count("POST /api/v1/repo/description")
	inst = h.update(t, inst, changed)
	if fake.count("POST /api/v1/repo/description") != posts+1 {
		t.Errorf("the update made %d POSTs, want 1", fake.count("POST /api/v1/repo/description")-posts)
	}
	final := h.plan(t, typeName, inst, changed)
	requireNoProtocolErrors(t, final.Diagnostics)
	if planned := h.decodeResource(t, typeName, final.PlannedState); !planned.Equal(inst.state) || len(final.Diagnostics) != 0 {
		t.Errorf("plan after saving = %s with %v, want the state unchanged and no warning", planned, final.Diagnostics)
	}
}

func TestRepositoryDescriptionWithoutARepository(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("POST /api/v1/repo/description", http.StatusBadRequest, kopiaclient.CodeNotConnected, "not connected")
	fake.reply("GET /api/v1/repo/status", http.StatusOK, "{\n  \"connected\": false,\n  \"supportsContentCompression\": false,\n"+
		"  \"hostname\": \"\",\n  \"username\": \"\",\n  \"enableActions\": false\n}\n")
	r := &repositoryDescriptionResource{client: fake.client(t)}

	config := resourceValue(t, r, map[string]any{"description": "x"})
	created := createResource(t, r, config, resourceValue(t, r, map[string]any{"id": tfUnknown, "description": "x"}))
	requireErrorContaining(t, created.Diagnostics, "Unable to set Kopia repository description")
	requireErrorContaining(t, created.Diagnostics, "has no repository connected")
	if !created.State.Raw.IsNull() {
		t.Errorf("state = %s, want none", created.State.Raw)
	}
	if fake.called("GET /api/v1/repo/throttle") || fake.called("PUT /api/v1/repo/throttle") {
		t.Errorf("calls = %v, want the throttle left alone after a failed POST", fake.apiRoutes())
	}

	// A status without a repository has no description, which must not read
	// as "none" and plan to set one.
	read := readResource(t, r, resourceValue(t, r, map[string]any{"id": repositoryDescriptionID, "description": "x"}))
	requireErrorContaining(t, read.Diagnostics, "has no repository connected")
}

func TestRepositoryDescriptionReadsTheStatus(t *testing.T) {
	fake := newFakeKopia()
	r := &repositoryDescriptionResource{client: fake.client(t)}

	resp := readResource(t, r, resourceValue(t, r, map[string]any{"id": repositoryDescriptionID, "description": "Old"}))
	requireNoErrors(t, resp.Diagnostics)
	if got := stringAt(t, resp.State.Raw, "description"); got != "Repository in Filesystem: /app/repo" {
		t.Errorf("description = %q, want the one GET /api/v1/repo/status reports", got)
	}

	// master's compact encoding reads the same.
	fake.reply("GET /api/v1/repo/status", http.StatusOK, `{"connected":true,"configFile":"/app/config/repository.config",`+
		`"supportsContentCompression":true,"hostname":"kopia-acc","username":"root","description":"Compact",`+
		`"enableActions":false,"formatBlobCacheDuration":900000000000}`+"\n")
	resp = readResource(t, r, resp.State.Raw)
	requireNoErrors(t, resp.Diagnostics)
	if got := stringAt(t, resp.State.Raw, "description"); got != "Compact" {
		t.Errorf("description = %q", got)
	}
}

// Kopia's API cannot remove a description, so destroy makes no request at
// all, and says so.
func TestRepositoryDescriptionDeleteMakesNoCall(t *testing.T) {
	fake := newFakeKopia()
	r := &repositoryDescriptionResource{client: fake.client(t)}

	resp := deleteResource(t, r, resourceValue(t, r, map[string]any{"id": repositoryDescriptionID, "description": "x"}))
	requireNoErrors(t, resp.Diagnostics)
	requireWarningContaining(t, resp.Diagnostics, "Kopia repository description was left unchanged")
	if got := fake.routes(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

func TestRepositoryDescriptionImportIgnoresTheID(t *testing.T) {
	r := &repositoryDescriptionResource{}
	resp := importResource(t, r, "whatever")
	requireNoErrors(t, resp.Diagnostics)
	if got := stringAt(t, resp.State.Raw, "id"); got != repositoryDescriptionID {
		t.Errorf("imported id = %q, want %q", got, repositoryDescriptionID)
	}
}

func TestRepositoryDescriptionProtocolLifecycle(t *testing.T) {
	fake := newFakeKopia()
	server := serveRepositoryDescriptionForTest(t, fake, nil, kopiaclient.ThrottleLimits{ReadsPerSecond: 10})
	h := newProtocolHarness(t, fake, withResources(NewRepositoryDescriptionResource))
	const typeName = "kopia_repository_description"

	config := h.resourceValue(t, typeName, map[string]any{"description": "Production backups"})
	inst := h.create(t, typeName, config)
	if got := stringAt(t, inst.state, "id"); got != repositoryDescriptionID {
		t.Errorf("id = %q", got)
	}

	replan := h.plan(t, typeName, inst, config)
	requireNoProtocolErrors(t, replan.Diagnostics)
	if planned := h.decodeResource(t, typeName, replan.PlannedState); !planned.Equal(inst.state) {
		t.Errorf("plan after apply = %s, want the state unchanged", planned)
	}

	inst = h.update(t, inst, h.resourceValue(t, typeName, map[string]any{"description": "Offsite"}))
	if got := stringAt(t, inst.state, "description"); got != "Offsite" {
		t.Errorf("description = %q", got)
	}
	imported := h.importByID(t, typeName, "description")
	if !imported.state.Equal(inst.state) {
		t.Errorf("import = %s, want %s", imported.state, inst.state)
	}

	h.destroy(t, inst)
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.description != "Offsite" || server.limits != (kopiaclient.ThrottleLimits{ReadsPerSecond: 10}) {
		t.Errorf("after destroy the server has %q and %+v, want both left as they were", server.description, server.limits)
	}
}

// Kopia saves "" as no description and puts its default back at the next
// restart, so the value would drift.
func TestRepositoryDescriptionRejectsAnEmptyDescription(t *testing.T) {
	h := newProtocolHarness(t, newFakeKopia(), withResources(NewRepositoryDescriptionResource))
	diags := h.validateResource(t, "kopia_repository_description",
		h.resourceValue(t, "kopia_repository_description", map[string]any{"description": ""}))
	requireProtocolErrorContaining(t, diags, "length must be at least 1")
}

// --- acceptance --------------------------------------------------------------

// TestAccRepositoryDescriptionResource changes the test server's description
// and leaves the last one in place, as destroying the resource does. The
// throttle limits are managed in the same configuration, so the two
// resources' writes to the config file run in the same apply.
func TestAccRepositoryDescriptionResource(t *testing.T) {
	config := func(description string) string {
		return providerConfig + fmt.Sprintf(`
resource "kopia_repository_description" "this" {
  description = %q
}

resource "kopia_throttle" "this" {
  concurrent_writes = 4
}

data "kopia_repository_status" "this" {
  depends_on = [kopia_repository_description.this]
}
`, description)
	}

	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []acc.TestStep{
			{
				Config: config("tfacc repository"),
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("kopia_repository_description.this", "id", "description"),
					acc.TestCheckResourceAttr("kopia_repository_description.this", "description", "tfacc repository"),
					acc.TestCheckResourceAttr("data.kopia_repository_status.this", "description", "tfacc repository"),
				),
			},
			{
				// Import must round-trip without producing a diff.
				ResourceName:      "kopia_repository_description.this",
				ImportState:       true,
				ImportStateId:     "description",
				ImportStateVerify: true,
			},
			{
				// Changing the description keeps the throttle limits in effect.
				Config: config("tfacc repository, renamed"),
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("kopia_repository_description.this", "description", "tfacc repository, renamed"),
					acc.TestCheckResourceAttr("data.kopia_repository_status.this", "description", "tfacc repository, renamed"),
					acc.TestCheckResourceAttr("kopia_throttle.this", "concurrent_writes", "4"),
				),
			},
			{
				// A refresh of the unchanged configuration plans nothing.
				RefreshState: true,
			},
		},
	})
}
