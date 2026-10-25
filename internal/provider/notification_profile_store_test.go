package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// notificationTestStore emulates Kopia's notification profile endpoints on
// top of the fake kopia (internal/server/api_notification_profile.go):
//
//   - POST /api/v1/notificationProfiles stores the body under its "profile",
//     replacing any profile of that name, and moves it to the end of the list;
//   - GET of one profile returns it, or 500 INTERNAL "profile not found";
//   - GET of the list returns every profile in the order they were written,
//     or null when there are none;
//   - DELETE removes the profile and answers 200 either way;
//   - POST /api/v1/testNotificationProfile records the body, and fails with
//     testError when that is set, as a failed delivery does.
//
// Kopia keeps a profile's options as a generic map, so the store keeps the
// posted JSON decoded into generic values: a GET then returns the keys sorted
// and the numbers re-encoded, through writeFakeJSON, the way a 0.22 or 0.23
// server does. The fake routes by exact path, so the profile names a test
// reads or deletes are registered up front.
type notificationTestStore struct {
	mu        sync.Mutex
	order     []string
	profiles  map[string]any
	tests     []map[string]any
	testError string
}

func newNotificationTestStore(fake *fakeKopia, names ...string) *notificationTestStore {
	s := &notificationTestStore{profiles: map[string]any{}}
	fake.handle("POST /api/v1/notificationProfiles", s.save)
	fake.handle("GET /api/v1/notificationProfiles", s.list)
	fake.handle("POST /api/v1/testNotificationProfile", s.test)
	for _, name := range names {
		route := "/api/v1/notificationProfiles/" + name
		fake.handle("GET "+route, func(w http.ResponseWriter, _ *http.Request) { s.get(w, name) })
		fake.handle("DELETE "+route, func(w http.ResponseWriter, _ *http.Request) { s.delete(w, name) })
	}
	return s
}

// put stores document as if another client had saved it under name.
func (s *notificationTestStore) put(t *testing.T, name, document string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatalf("stored profile %q is not JSON: %v", name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write(name, decoded)
}

// write stores a profile and moves it to the end of the list; s.mu is held.
func (s *notificationTestStore) write(name string, profile any) {
	s.forget(name)
	s.order = append(s.order, name)
	s.profiles[name] = profile
}

// forget removes a profile; s.mu is held.
func (s *notificationTestStore) forget(name string) {
	delete(s.profiles, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// drop deletes a profile behind Terraform's back, as the UI or the CLI would.
func (s *notificationTestStore) drop(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(name)
}

// failTests makes every later test send fail with message, as a failed
// delivery does; "" makes them succeed again.
func (s *notificationTestStore) failTests(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.testError = message
}

// stored returns the profile stored under name, decoded, or nil.
func (s *notificationTestStore) stored(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	profile, _ := s.profiles[name].(map[string]any)
	return profile
}

// testsSent returns the bodies posted to the test endpoint.
func (s *notificationTestStore) testsSent() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.tests...)
}

// decodeNotificationTestBody decodes a request body the way Kopia does, or
// answers its malformed-request error.
func decodeNotificationTestBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "malformed request body: "+string(raw)))
		return nil, false
	}
	return body, true
}

func (s *notificationTestStore) save(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeNotificationTestBody(w, r)
	if !ok {
		return
	}
	name, _ := body["profile"].(string)
	s.mu.Lock()
	s.write(name, body)
	s.mu.Unlock()
	writeFakeResponse(w, http.StatusOK, "{}\n")
}

func (s *notificationTestStore) get(w http.ResponseWriter, name string) {
	s.mu.Lock()
	profile, ok := s.profiles[name]
	s.mu.Unlock()
	if !ok {
		writeFakeResponse(w, http.StatusInternalServerError, fakeErrorBody(kopiaclient.CodeInternal, "internal server error: profile not found"))
		return
	}
	writeFakeJSON(w, profile)
}

func (s *notificationTestStore) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	profiles := make([]any, 0, len(s.order))
	for _, name := range s.order {
		profiles = append(profiles, s.profiles[name])
	}
	s.mu.Unlock()
	if len(profiles) == 0 {
		writeFakeResponse(w, http.StatusOK, "null\n")
		return
	}
	writeFakeJSON(w, profiles)
}

func (s *notificationTestStore) delete(w http.ResponseWriter, name string) {
	s.mu.Lock()
	s.forget(name)
	s.mu.Unlock()
	writeFakeResponse(w, http.StatusOK, "{}\n")
}

func (s *notificationTestStore) test(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeNotificationTestBody(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	s.tests = append(s.tests, body)
	failure := s.testError
	s.mu.Unlock()
	if failure != "" {
		writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, failure))
		return
	}
	writeFakeResponse(w, http.StatusOK, "{}\n")
}
