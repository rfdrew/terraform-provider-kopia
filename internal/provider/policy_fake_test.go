package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// policyTestStore emulates Kopia's policy endpoints on a fakeKopia: GET, PUT
// and DELETE /api/v1/policy, GET /api/v1/policies, POST /api/v1/policy/resolve
// and GET /api/v1/repo/algorithms, with what the provider relies on from each
// (spec: policy.md §2 and §3):
//
//   - a policy is stored as the JSON object that was PUT, numbers exactly as
//     sent, so a test sees what the provider sent, fields Kopia's Policy does
//     not have included, and is served back, keys sorted, in the shape of the
//     release being emulated: 0.22.3 and later send every section, empty or
//     not, while 0.22.0 leaves empty ones out;
//   - a missing policy is 400 NOT_FOUND, not 404, and DELETE answers 200
//     whether or not there was one;
//   - PUT refuses manual scheduling combined with anything else, a cron entry
//     the fake treats as unparsable (one containing "invalid", standing in for
//     cronexpr), maxParallelSnapshots on a path and a path ending in a
//     separator, each with Kopia's 500 INTERNAL and message;
//   - resolve reports the same scheduling check as schedulingError, and the
//     updates (or the stored policy) as the effective policy.
type policyTestStore struct {
	mu sync.Mutex

	// compact serves the 0.22.0 shape instead of the 0.22.3 one.
	compact bool
	// policies are the stored policies, by target, as decoded JSON objects.
	policies map[kopiaclient.SourceInfo]map[string]any
	// ids are the manifest ids; every PUT assigns a new one, as Kopia does.
	ids    map[kopiaclient.SourceInfo]string
	nextID int
	// written lists the targets in the order of their writes.
	written []kopiaclient.SourceInfo
}

// newPolicyTestStore routes the policy endpoints of fake to a new, empty
// store; seed the global policy with putRaw when a test needs it.
func newPolicyTestStore(fake *fakeKopia) *policyTestStore {
	s := &policyTestStore{
		policies: map[kopiaclient.SourceInfo]map[string]any{},
		ids:      map[kopiaclient.SourceInfo]string{},
	}
	fake.handle("GET /api/v1/policy", s.serveGet)
	fake.handle("PUT /api/v1/policy", s.servePut)
	fake.handle("DELETE /api/v1/policy", s.serveDelete)
	fake.handle("GET /api/v1/policies", s.serveList)
	fake.handle("POST /api/v1/policy/resolve", s.serveResolve)
	fake.reply("GET /api/v1/repo/algorithms", http.StatusOK, policyTestAlgorithmsJSON)
	return s
}

// policyTestAlgorithmsJSON is GET /api/v1/repo/algorithms, abridged to the
// lists a policy refers to; lz4 and zstd-best-compression are deprecated in
// every release.
const policyTestAlgorithmsJSON = `{
  "defaultHash": "BLAKE2B-256-128",
  "defaultEncryption": "AES256-GCM-HMAC-SHA256",
  "defaultEcc": "",
  "defaultSplitter": "DYNAMIC-4M-BUZHASH",
  "splitter": [
    {"id": "DYNAMIC", "deprecated": false},
    {"id": "DYNAMIC-4M-BUZHASH", "deprecated": false},
    {"id": "FIXED-1M", "deprecated": false}
  ],
  "compression": [
    {"id": "lz4", "deprecated": true},
    {"id": "s2-default", "deprecated": false},
    {"id": "zstd", "deprecated": false},
    {"id": "zstd-best-compression", "deprecated": true},
    {"id": "zstd-fastest", "deprecated": false}
  ]
}
`

// policyTestDefaultGlobalJSON is the global policy `kopia repository create`
// stores (spec: policy.md §1.4).
const policyTestDefaultGlobalJSON = `{"retention":{"keepLatest":10,"keepHourly":48,"keepDaily":7,"keepWeekly":4,"keepMonthly":24,"keepAnnual":3,"ignoreIdenticalSnapshots":false},"files":{"ignoreDotFiles":[".kopiaignore"]},"errorHandling":{"ignoreFileErrors":false,"ignoreDirectoryErrors":false,"ignoreUnknownTypes":true},"scheduling":{"runMissed":true},"compression":{"compressorName":"none"},"metadataCompression":{"compressorName":"zstd-fastest"},"osSnapshots":{"volumeShadowCopy":{"enable":0}},"logging":{"directories":{"snapshotted":5,"ignored":5},"entries":{"snapshotted":0,"ignored":5,"cacheHit":0,"cacheMiss":0}},"upload":{"maxParallelSnapshots":1,"parallelUploadAboveSize":2147483648}}`

// putRaw stores body as the policy of target, as another writer would.
func (s *policyTestStore) putRaw(t *testing.T, target, body string) {
	t.Helper()
	si, err := kopiaclient.ParseTarget(target)
	if err != nil {
		// The odd targets Kopia also holds, such as "docker@".
		si = policyTestOddTarget(t, target)
	}
	decoded, err := policyTestDecode([]byte(body))
	if err != nil {
		t.Fatalf("policy body %s: %v", body, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(si, decoded)
}

// policyTestOddTarget parses the few targets ParseTarget refuses that tests
// store: "user@" and "@host:path".
func policyTestOddTarget(t *testing.T, target string) kopiaclient.SourceInfo {
	t.Helper()
	user, rest, ok := strings.Cut(target, "@")
	if !ok {
		t.Fatalf("cannot store a policy for %q", target)
	}
	host, path, _ := strings.Cut(rest, ":")
	return kopiaclient.SourceInfo{Host: host, UserName: user, Path: path}
}

// stored returns the stored policy of target as compact JSON with sorted
// keys, or "" when there is none.
func (s *policyTestStore) stored(t *testing.T, target string) string {
	t.Helper()
	si, err := kopiaclient.ParseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[si]
	if !ok {
		return ""
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// has reports whether target has a stored policy.
func (s *policyTestStore) has(t *testing.T, target string) bool {
	t.Helper()
	return s.stored(t, target) != ""
}

// remove deletes a policy behind the provider's back.
func (s *policyTestStore) remove(t *testing.T, target string) {
	t.Helper()
	si, err := kopiaclient.ParseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.policies, si)
}

// store keeps p for si under a new manifest id. The caller holds s.mu.
func (s *policyTestStore) store(si kopiaclient.SourceInfo, p map[string]any) {
	s.nextID++
	s.policies[si] = p
	s.ids[si] = fmt.Sprintf("%032x", s.nextID)
	s.written = append(s.written, si)
}

// policyTestTarget reads the target from the query, as Kopia does
// (internal/server/api_policies.go:44-54).
func policyTestTarget(r *http.Request) kopiaclient.SourceInfo {
	q := r.URL.Query()
	return kopiaclient.SourceInfo{Host: q.Get("host"), UserName: q.Get("userName"), Path: q.Get("path")}
}

func (s *policyTestStore) serveGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	p, ok := s.policies[policyTestTarget(r)]
	shaped := s.shape(p)
	s.mu.Unlock()
	if !ok {
		writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeNotFound, "policy not found"))
		return
	}
	writeFakeJSON(w, shaped)
}

func (s *policyTestStore) servePut(w http.ResponseWriter, r *http.Request) {
	target := policyTestTarget(r)
	raw, _ := io.ReadAll(r.Body)
	p, err := policyTestDecode(raw)
	if err != nil {
		writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "malformed request body"))
		return
	}
	if p == nil {
		p = map[string]any{}
	}
	if message := policyTestValidate(target, p); message != "" {
		writeFakeResponse(w, http.StatusInternalServerError, fakeErrorBody(kopiaclient.CodeInternal, message))
		return
	}
	s.mu.Lock()
	s.store(target, p)
	s.mu.Unlock()
	writeFakeJSON(w, map[string]any{})
}

func (s *policyTestStore) serveDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delete(s.policies, policyTestTarget(r))
	s.mu.Unlock()
	writeFakeJSON(w, map[string]any{})
}

func (s *policyTestStore) serveList(w http.ResponseWriter, r *http.Request) {
	filter := policyTestTarget(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Kopia lists each manifest once, oldest change first; written repeats a
	// target on every write, and the ids grow with every write.
	entries := []map[string]any{}
	seen := map[kopiaclient.SourceInfo]bool{}
	for _, si := range s.written {
		p, ok := s.policies[si]
		if !ok || seen[si] || (filter.Host != "" && si.Host != filter.Host) ||
			(filter.UserName != "" && si.UserName != filter.UserName) || (filter.Path != "" && si.Path != filter.Path) {
			continue
		}
		seen[si] = true
		entries = append(entries, map[string]any{
			"id":     s.ids[si],
			"target": map[string]any{"host": si.Host, "userName": si.UserName, "path": si.Path},
			"policy": s.shape(p),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i]["id"].(string) < entries[j]["id"].(string) })
	writeFakeJSON(w, map[string]any{"policies": entries})
}

func (s *policyTestStore) serveResolve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Updates                  map[string]any `json:"updates"`
		NumUpcomingSnapshotTimes int            `json:"numUpcomingSnapshotTimes"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &req); err != nil {
		writeFakeResponse(w, http.StatusBadRequest, fakeErrorBody(kopiaclient.CodeMalformedRequest, "unable to decode request: "+err.Error()))
		return
	}
	target := policyTestTarget(r)
	s.mu.Lock()
	stored, ok := s.policies[target]
	s.mu.Unlock()
	if !ok {
		stored = map[string]any{}
	}
	effective := req.Updates
	if effective == nil {
		effective = stored
	}

	times := []string{}
	for i := 0; i < req.NumUpcomingSnapshotTimes; i++ {
		times = append(times, fmt.Sprintf("2026-10-%02dT03:00:00Z", 19+i))
	}
	resp := map[string]any{
		"effective":             s.shape(effective),
		"definition":            map[string]any{},
		"defined":               s.shape(stored),
		"upcomingSnapshotTimes": times,
	}
	if message := policyTestSchedulingError(effective); message != "" {
		resp["schedulingError"] = message
	}
	writeFakeJSON(w, resp)
}

// policyTestValidate is the check of Kopia's PUT (policy_manager.go:161-178):
// scheduling, then upload, then the path, with the first failure's message.
func policyTestValidate(target kopiaclient.SourceInfo, p map[string]any) string {
	const prefix = "internal server error: unable to set policy: "
	if message := policyTestSchedulingError(p); message != "" {
		return prefix + "failed to validate policy: invalid scheduling policy: " + message
	}
	if upload, _ := p["upload"].(map[string]any); upload != nil && upload["maxParallelSnapshots"] != nil && target.Path != "" {
		return prefix + "failed to validate policy: invalid upload policy: max parallel snapshots cannot be " +
			"specified for paths, only global, username@hostname or @hostname"
	}
	if path := target.Path; len(path) > 1 && (strings.HasSuffix(path, "/") || strings.HasSuffix(path, `\`)) {
		return prefix + "invalid policy path: path cannot end with a slash or a backslash"
	}
	return ""
}

// policyTestSchedulingError is ValidateSchedulingPolicy
// (scheduling_policy.go:276-290), with "invalid" standing in for whatever
// cronexpr cannot parse. Kopia refuses manual together with any other field
// of the section that decodes to something, which a null never does.
func policyTestSchedulingError(p map[string]any) string {
	scheduling, _ := p["scheduling"].(map[string]any)
	if scheduling == nil {
		return ""
	}
	if manual, _ := scheduling["manual"].(bool); manual {
		for name, value := range scheduling {
			if name != "manual" && value != nil {
				return "invalid scheduling policy: manual cannot be combined with other scheduling policies"
			}
		}
	}
	cron, _ := scheduling["cron"].([]any)
	for _, entry := range cron {
		text, _ := entry.(string)
		schedule, _, _ := strings.Cut(text, "#")
		if strings.Contains(schedule, "invalid") {
			return fmt.Sprintf("invalid cron expression %q", text)
		}
	}
	return ""
}

// shape returns p as the emulated release sends it. 0.22.3 and later send
// every section, and the structs nested in logging and osSnapshots, even when
// they are empty (omitempty never applies to a struct); 0.22.0 sends them as
// stored. The caller holds s.mu, or owns p.
func (s *policyTestStore) shape(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	shaped := policyTestCopy(p)
	if s.compact {
		return shaped
	}
	policyTestEnsure(shaped, "retention", "files", "errorHandling", "scheduling", "compression",
		"metadataCompression", "splitter", "actions", "osSnapshots", "logging", "upload")
	if osSnapshots, ok := shaped["osSnapshots"].(map[string]any); ok {
		policyTestEnsure(osSnapshots, "volumeShadowCopy")
	}
	if logging, ok := shaped["logging"].(map[string]any); ok {
		policyTestEnsure(logging, "directories", "entries")
	}
	return shaped
}

// policyTestEnsure adds each of names that section lacks, or holds null for,
// as {}: Kopia decodes null into a struct as nothing, and sends the struct.
func policyTestEnsure(section map[string]any, names ...string) {
	for _, name := range names {
		if section[name] == nil {
			section[name] = map[string]any{}
		}
	}
}

// policyTestDecode decodes a JSON object with its numbers as written.
func policyTestDecode(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded map[string]any
	err := decoder.Decode(&decoded)
	return decoded, err
}

// policyTestCopy deep-copies a decoded JSON object.
func policyTestCopy(p map[string]any) map[string]any {
	encoded, _ := json.Marshal(p)
	copied, _ := policyTestDecode(encoded)
	return copied
}
