package kopiaclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// policyJSONShape0223 is GET /api/v1/policy for docker@host1 after the PUT in
// spec policy.md §3.3, as Kopia 0.22.3 and later answer it: every section is
// present, empty or not, and the releases indent the body.
const policyJSONShape0223 = `{
  "retention": {
    "keepLatest": 10,
    "keepDaily": 14
  },
  "files": {},
  "errorHandling": {},
  "scheduling": {
    "cron": [
      "0 3 * * * # nightly"
    ],
    "runMissed": true
  },
  "compression": {
    "compressorName": "zstd",
    "neverCompress": [
      ".gz",
      ".jpg",
      ".zip"
    ]
  },
  "metadataCompression": {},
  "splitter": {},
  "actions": {},
  "osSnapshots": {
    "volumeShadowCopy": {}
  },
  "logging": {
    "directories": {},
    "entries": {}
  },
  "upload": {
    "maxParallelSnapshots": 2
  }
}
`

// policyJSONShape0220 is the same policy as Kopia 0.22.0 answers it: empty
// sections are left out (omitzero).
const policyJSONShape0220 = `{"retention":{"keepLatest":10,"keepDaily":14},"scheduling":{"cron":["0 3 * * * # nightly"],"runMissed":true},"compression":{"compressorName":"zstd","neverCompress":[".gz",".jpg",".zip"]},"upload":{"maxParallelSnapshots":2}}`

// policyDefaultGlobalJSON is GET /api/v1/policy for the global policy of a
// new repository on Kopia 0.22.0 (spec: policy.md §1.4); 0.22.3 and later
// add "splitter":{} and "actions":{}.
const policyDefaultGlobalJSON = `{"retention":{"keepLatest":10,"keepHourly":48,"keepDaily":7,"keepWeekly":4,"keepMonthly":24,"keepAnnual":3,"ignoreIdenticalSnapshots":false},"files":{"ignoreDotFiles":[".kopiaignore"]},"errorHandling":{"ignoreFileErrors":false,"ignoreDirectoryErrors":false,"ignoreUnknownTypes":true},"scheduling":{"runMissed":true},"compression":{"compressorName":"none"},"metadataCompression":{"compressorName":"zstd-fastest"},"osSnapshots":{"volumeShadowCopy":{"enable":0}},"logging":{"directories":{"snapshotted":5,"ignored":5},"entries":{"snapshotted":0,"ignored":5,"cacheHit":0,"cacheMiss":0}},"upload":{"maxParallelSnapshots":1,"parallelUploadAboveSize":2147483648}}`

func policyMarshal(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(encoded)
}

func policyDecode(t *testing.T, text string) Policy {
	t.Helper()
	var p Policy
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	return p
}

func TestParseTargetAccepts(t *testing.T) {
	tests := []struct {
		text string
		want SourceInfo
	}{
		{"(global)", SourceInfo{}},
		{"@host1", SourceInfo{Host: "host1"}},
		{"docker@host1", SourceInfo{Host: "host1", UserName: "docker"}},
		{"docker@host1.example.com", SourceInfo{Host: "host1.example.com", UserName: "docker"}},
		{"John Doe@WIN-PC", SourceInfo{Host: "WIN-PC", UserName: "John Doe"}},
		{"docker@host1:/", SourceInfo{Host: "host1", UserName: "docker", Path: "/"}},
		{"docker@host1:/data/app", SourceInfo{Host: "host1", UserName: "docker", Path: "/data/app"}},
		{`docker@host1:/data/odd\name`, SourceInfo{Host: "host1", UserName: "docker", Path: `/data/odd\name`}},
		{"docker@host1:/data/with: colon", SourceInfo{Host: "host1", UserName: "docker", Path: "/data/with: colon"}},
		{`Administrator@WIN1:C:\`, SourceInfo{Host: "WIN1", UserName: "Administrator", Path: `C:\`}},
		{`Administrator@WIN1:C:\Data`, SourceInfo{Host: "WIN1", UserName: "Administrator", Path: `C:\Data`}},
		{`Administrator@WIN1:d:\Users\Public`, SourceInfo{Host: "WIN1", UserName: "Administrator", Path: `d:\Users\Public`}},
		{`Administrator@WIN1:\\srv\share`, SourceInfo{Host: "WIN1", UserName: "Administrator", Path: `\\srv\share`}},
		{`Administrator@WIN1:\\srv\share\dir`, SourceInfo{Host: "WIN1", UserName: "Administrator", Path: `\\srv\share\dir`}},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			got, err := ParseTarget(tc.text)
			if err != nil {
				t.Fatalf("ParseTarget() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseTarget() = %#v, want %#v", got, tc.want)
			}
			// The text form is canonical: String gives back exactly the input.
			if got.String() != tc.text {
				t.Fatalf("String() = %q, want %q", got.String(), tc.text)
			}
		})
	}
}

func TestParseTargetRejects(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"", "write (global)"},
		{"global", "write (global)"},
		{"(GLOBAL)", "write (global)"},
		{"host1", "write (global)"},
		{"/data/app", "write (global)"},
		{" (global)", "whitespace"},
		{"docker@host1 ", "whitespace"},
		{"docker@host1\n", "whitespace"},
		{"docker @host1", "whitespace"},
		{"docker@host1 :/data", "whitespace"},
		{"@", "host is empty"},
		{"docker@", "host is empty"},
		{"docker@:/data", "host is empty"},
		{"a@b@c", `host contains "@"`},
		{"DOMAIN:docker@host1", `user name contains ":"`},
		{"@host1:/data", "needs a user name"},
		{"docker@host1:", "path is empty"},
		{"docker@host1:data/app", "must be absolute"},
		{"docker@host1:./data", "must be absolute"},
		{`docker@host1:\data`, "must be absolute"},
		{"docker@host1:C:", "must be absolute"},
		{"docker@host1:C:Data", "must be absolute"},
		{"docker@host1:/data/", "ends with a slash"},
		{`docker@host1:/data\`, "ends with a slash"},
		{"docker@host1://", "ends with a slash"},
		{`Administrator@WIN1:C:\Users\`, "ends with a slash"},
		{`Administrator@WIN1:C:\\`, "ends with a slash"},
		{`Administrator@WIN1:\\srv\share\`, "ends with a slash"},
		{"Administrator@WIN1:C:/", "backslashes"},
		{"Administrator@WIN1:c:/users", "backslashes"},
		{`Administrator@WIN1:D:\foo/bar\baz`, "backslashes"},
		{`Administrator@WIN1:\\srv/share`, "backslashes"},
		{`Administrator@WIN1:\\`, "server and a share"},
		{`Administrator@WIN1:\\srv`, "server and a share"},
		{`Administrator@WIN1:\\\share`, "server and a share"},
		{"docker@host1://x", "empty"},
		{"docker@host1:/data//app", "empty"},
		{"docker@host1:/data/./app", `"."`},
		{"docker@host1:/data/../app", `".."`},
		{`Administrator@WIN1:C:\Data\..\Other`, `".."`},
		{`Administrator@WIN1:\\srv\\share`, "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			got, err := ParseTarget(tc.text)
			if err == nil {
				t.Fatalf("ParseTarget() = %#v, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTarget() error = %q, want one containing %q", err, tc.want)
			}
		})
	}
}

// Kopia can hold targets ParseTarget refuses — a request that names only
// userName stores a policy for "docker@" — and String renders them as Kopia
// does, so that a list of the server's policies shows them faithfully.
func TestSourceInfoStringRendersEveryShape(t *testing.T) {
	tests := map[string]SourceInfo{
		"(global)":               {},
		"@host1":                 {Host: "host1"},
		"docker@host1":           {Host: "host1", UserName: "docker"},
		"docker@host1:/data/app": {Host: "host1", UserName: "docker", Path: "/data/app"},
		`admin@win:C:\Users`:     {Host: "win", UserName: "admin", Path: `C:\Users`},
		"docker@":                {UserName: "docker"},
		"@host1:/p":              {Host: "host1", Path: "/p"},
		"@:/p":                   {Path: "/p"},
		"docker@:/p":             {UserName: "docker", Path: "/p"},
	}
	for want, si := range tests {
		if got := si.String(); got != want {
			t.Errorf("%#v.String() = %q, want %q", si, got, want)
		}
	}
	if !(SourceInfo{}).IsGlobal() || (SourceInfo{Host: "h"}).IsGlobal() {
		t.Error("IsGlobal() is wrong")
	}
}

// The user parameter is camelCase userName; Kopia ignores a lowercase
// username and would address a less specific target instead.
func TestSourceInfoQuery(t *testing.T) {
	tests := []struct {
		target string
		want   string
	}{
		{"(global)", ""},
		{"@host1", "host=host1"},
		{"docker@host1", "host=host1&userName=docker"},
		{"docker@host1:/data/app", "host=host1&path=%2Fdata%2Fapp&userName=docker"},
		{`Administrator@WIN1:C:\Data`, "host=WIN1&path=C%3A%5CData&userName=Administrator"},
	}
	for _, tc := range tests {
		si, err := ParseTarget(tc.target)
		if err != nil {
			t.Fatal(err)
		}
		if got := si.query().Encode(); got != tc.want {
			t.Errorf("query(%s) = %q, want %q", tc.target, got, tc.want)
		}
	}
}

// Both releases' shapes of one stored policy decode to the same value.
func TestPolicyDecodesBothReleaseShapesAlike(t *testing.T) {
	newer := policyDecode(t, policyJSONShape0223)
	older := policyDecode(t, policyJSONShape0220)
	if !reflect.DeepEqual(newer, older) {
		t.Fatalf("0.22.3 shape decodes to %s,\n0.22.0 shape to %s", policyMarshal(t, newer), policyMarshal(t, older))
	}
	if newer.Files != nil || newer.Splitter != nil || newer.Logging != nil || newer.OSSnapshots != nil || newer.Actions != nil {
		t.Errorf("empty sections decode to non-nil values: %#v", newer)
	}
	if got := *newer.Retention.KeepDaily; got != 14 {
		t.Errorf("keepDaily = %d, want 14", got)
	}
	if got := newer.Scheduling.Cron; len(got) != 1 || got[0] != "0 3 * * * # nightly" {
		t.Errorf("cron = %q", got)
	}
	if newer.Scheduling.RunMissed == nil || !*newer.Scheduling.RunMissed {
		t.Error("runMissed is not true")
	}

	emptyNewer := policyDecode(t, `{"retention":{},"files":{},"errorHandling":{},"scheduling":{},"compression":{},"metadataCompression":{},"splitter":{},"actions":{},"osSnapshots":{"volumeShadowCopy":{}},"logging":{"directories":{},"entries":{}},"upload":{}}`)
	if !reflect.DeepEqual(emptyNewer, Policy{}) {
		t.Errorf("the 0.22.3 shape of an empty policy decodes to %#v, want the zero policy", emptyNewer)
	}

	// Kopia stores no empty list, but decodes one if it is sent; it reads the
	// same as an absent one.
	withEmptyLists := policyDecode(t, `{"scheduling":{"cron":[],"timeOfDay":[]},"files":{"ignore":[]}}`)
	if !reflect.DeepEqual(withEmptyLists, Policy{}) {
		t.Errorf("empty lists decode to %#v, want the zero policy", withEmptyLists)
	}
}

// An action is a pointer in Kopia, so an empty one is a value of its own —
// one that fails every snapshot — and is kept rather than normalised away.
func TestPolicyKeepsAnEmptyAction(t *testing.T) {
	p := policyDecode(t, `{"actions":{"beforeFolder":{}}}`)
	if p.Actions == nil || p.Actions.BeforeFolder == nil || p.Actions.AfterFolder != nil {
		t.Fatalf("actions = %#v, want an empty beforeFolder only", p.Actions)
	}
	if got := policyMarshal(t, p); got != `{"actions":{"beforeFolder":{}}}` {
		t.Errorf("re-encoded = %s", got)
	}
}

// The encoder never sends an empty section, an empty list or a plain zero
// value, whatever the caller built.
func TestPolicyEncodingLeavesOutEmptyValues(t *testing.T) {
	p := Policy{
		Retention:           &RetentionPolicy{},
		Files:               &FilesPolicy{IgnoreRules: []string{}, DotIgnoreFiles: nil},
		ErrorHandling:       &ErrorHandlingPolicy{},
		Scheduling:          &SchedulingPolicy{Cron: []string{}, TimesOfDay: []TimeOfDay{}},
		Compression:         &CompressionPolicy{OnlyCompress: []string{}},
		MetadataCompression: &MetadataCompressionPolicy{},
		Splitter:            &SplitterPolicy{},
		Actions:             &ActionsPolicy{},
		OSSnapshots:         &OSSnapshotPolicy{VolumeShadowCopy: &VolumeShadowCopyPolicy{}},
		Logging:             &LoggingPolicy{Directories: &DirLoggingPolicy{}, Entries: &EntryLoggingPolicy{}},
		Upload:              &UploadPolicy{},
	}
	if got := policyMarshal(t, p); got != "{}" {
		t.Fatalf("encoded = %s, want {}", got)
	}
	if got := policyMarshal(t, &p); got != "{}" {
		t.Fatalf("encoded through a pointer = %s, want {}", got)
	}
	// Encoding does not change the value it encodes.
	if p.Retention == nil || p.Scheduling.Cron == nil {
		t.Error("encoding modified the policy")
	}

	// Only the logging sub-section that holds something is sent.
	partial := Policy{Logging: &LoggingPolicy{Directories: &DirLoggingPolicy{}, Entries: &EntryLoggingPolicy{CacheHit: policyPointer[int64](0)}}}
	if got := policyMarshal(t, partial); got != `{"logging":{"entries":{"cacheHit":0}}}` {
		t.Errorf("encoded = %s", got)
	}
}

// Pointer fields carry an explicit false or 0, which overrides a parent;
// plain fields are only sent when they hold something.
func TestPolicyEncodingSendsExplicitPointerValues(t *testing.T) {
	p := Policy{
		Retention:     &RetentionPolicy{KeepLatest: policyPointer[int64](0), IgnoreIdenticalSnapshots: policyPointer(false)},
		Files:         &FilesPolicy{NoParentIgnoreRules: false, MaxFileSize: -1, OneFileSystem: policyPointer(false)},
		Scheduling:    &SchedulingPolicy{TimesOfDay: []TimeOfDay{{Hour: 3, Minute: 15}}, RunMissed: policyPointer(false)},
		OSSnapshots:   &OSSnapshotPolicy{VolumeShadowCopy: &VolumeShadowCopyPolicy{Enable: policyPointer[int64](OSSnapshotNever)}},
		ErrorHandling: &ErrorHandlingPolicy{IgnoreUnknownTypes: policyPointer(true)},
		Upload:        &UploadPolicy{ParallelUploadAboveSize: policyPointer[int64](-1)},
		NoParent:      true,
	}
	want := `{"retention":{"keepLatest":0,"ignoreIdenticalSnapshots":false},"files":{"maxFileSize":-1,"oneFileSystem":false},"errorHandling":{"ignoreUnknownTypes":true},"scheduling":{"timeOfDay":[{"hour":3,"min":15}],"runMissed":false},"osSnapshots":{"volumeShadowCopy":{"enable":0}},"upload":{"parallelUploadAboveSize":-1},"noParent":true}`
	if got := policyMarshal(t, p); got != want {
		t.Fatalf("encoded =\n%s\nwant\n%s", got, want)
	}
}

// The request body of spec policy.md §3.3, byte for byte.
func TestPolicyEncodingMatchesKopiasExample(t *testing.T) {
	p := Policy{
		Retention:   &RetentionPolicy{KeepLatest: policyPointer[int64](10), KeepDaily: policyPointer[int64](14)},
		Scheduling:  &SchedulingPolicy{Cron: []string{"0 3 * * * # nightly"}, RunMissed: policyPointer(true)},
		Compression: &CompressionPolicy{CompressorName: "zstd", NeverCompress: []string{".gz", ".jpg", ".zip"}},
		Upload:      &UploadPolicy{MaxParallelSnapshots: policyPointer[int64](2)},
	}
	if got := policyMarshal(t, p); got != policyJSONShape0220 {
		t.Fatalf("encoded =\n%s\nwant\n%s", got, policyJSONShape0220)
	}
	action := Policy{Actions: &ActionsPolicy{
		BeforeSnapshotRoot: &ActionCommand{Script: "#!/bin/sh\necho hi", TimeoutSeconds: 300, Mode: "essential"},
		AfterSnapshotRoot:  &ActionCommand{Command: "/usr/local/bin/notify", Arguments: []string{"done"}, Mode: "async"},
	}}
	want := `{"actions":{"beforeSnapshotRoot":{"script":"#!/bin/sh\necho hi","timeout":300,"mode":"essential"},"afterSnapshotRoot":{"path":"/usr/local/bin/notify","args":["done"],"mode":"async"}}}`
	if got := policyMarshal(t, action); got != want {
		t.Fatalf("encoded =\n%s\nwant\n%s", got, want)
	}
}

// DefaultGlobalPolicy is exactly what a new repository's global policy reads
// back as.
func TestDefaultGlobalPolicyMatchesKopia(t *testing.T) {
	if got := policyMarshal(t, DefaultGlobalPolicy()); got != policyDefaultGlobalJSON {
		t.Fatalf("DefaultGlobalPolicy() encodes to\n%s\nwant\n%s", got, policyDefaultGlobalJSON)
	}
	// Each call returns a fresh value.
	a, b := DefaultGlobalPolicy(), DefaultGlobalPolicy()
	*a.Retention.KeepLatest = 99
	if *b.Retention.KeepLatest != 10 {
		t.Error("DefaultGlobalPolicy() values share memory")
	}
}

func TestGetPolicy(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/policy", http.StatusOK, policyJSONShape0223)
	c := fake.client(t)

	target := SourceInfo{Host: "host1", UserName: "docker"}
	p, err := c.GetPolicy(context.Background(), target)
	if err != nil {
		t.Fatalf("GetPolicy() error = %v", err)
	}
	if !reflect.DeepEqual(*p, policyDecode(t, policyJSONShape0220)) {
		t.Errorf("GetPolicy() = %s", policyMarshal(t, p))
	}
	if got := fake.query(t, "GET /api/v1/policy").Encode(); got != "host=host1&userName=docker" {
		t.Errorf("query = %q", got)
	}

	// The global policy is addressed without any parameter.
	if _, err := c.GetPolicy(context.Background(), SourceInfo{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.query(t, "GET /api/v1/policy"); len(got) != 0 {
		t.Errorf("global query = %v, want none", got)
	}
}

// A missing policy is 400 NOT_FOUND; IsNotFound reports it.
func TestGetPolicyNotFound(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/policy", http.StatusBadRequest, CodeNotFound, "policy not found")
	_, err := fake.client(t).GetPolicy(context.Background(), SourceInfo{Host: "h", UserName: "u"})
	if !IsNotFound(err) {
		t.Fatalf("GetPolicy() error = %v, want one IsNotFound reports", err)
	}
}

// 200 null is a failure to load the policy, not its absence.
func TestGetPolicyNullIsAnError(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/policy", http.StatusOK, "null\n")
	_, err := fake.client(t).GetPolicy(context.Background(), SourceInfo{Host: "h", UserName: "u"})
	if !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("GetPolicy() error = %v, want ErrPolicyUnavailable", err)
	}
	if IsNotFound(err) {
		t.Fatal("a null policy is reported as not found")
	}
	if !strings.Contains(err.Error(), "u@h") {
		t.Errorf("error %q does not name the target", err)
	}
}

func TestSetPolicy(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("PUT /api/v1/policy", http.StatusOK, "{}\n")
	c := fake.client(t)

	target := SourceInfo{Host: "WIN1", UserName: "Administrator", Path: `C:\Data`}
	policy := &Policy{Scheduling: &SchedulingPolicy{Manual: true}, Splitter: &SplitterPolicy{}}
	if err := c.SetPolicy(context.Background(), target, policy); err != nil {
		t.Fatalf("SetPolicy() error = %v", err)
	}
	call := fake.lastCall(t, "PUT /api/v1/policy")
	if got := string(call.body); got != `{"scheduling":{"manual":true}}` {
		t.Errorf("body = %s", got)
	}
	if got := call.query.Get("path"); got != `C:\Data` {
		t.Errorf("path = %q", got)
	}
	if got := call.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}

	// A nil policy is the empty policy, which Kopia stores as such.
	if err := c.SetPolicy(context.Background(), SourceInfo{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(fake.lastCall(t, "PUT /api/v1/policy").body); got != "{}" {
		t.Errorf("body of a nil policy = %s", got)
	}
}

// Kopia's validation failures are 500 INTERNAL; the message tells them apart.
func TestSetPolicyValidationError(t *testing.T) {
	fake := newFakeKopia()
	const message = `internal server error: unable to set policy: failed to validate policy: invalid scheduling policy: invalid cron expression "bad"`
	fake.replyError("PUT /api/v1/policy", http.StatusInternalServerError, CodeInternal, message)
	err := fake.client(t).SetPolicy(context.Background(), SourceInfo{Host: "h", UserName: "u"}, &Policy{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeInternal || apiErr.Message != message {
		t.Fatalf("SetPolicy() error = %v, want the INTERNAL error with Kopia's message", err)
	}
}

func TestDeletePolicy(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("DELETE /api/v1/policy", http.StatusOK, "{}\n")
	if err := fake.client(t).DeletePolicy(context.Background(), SourceInfo{Host: "host1"}); err != nil {
		t.Fatalf("DeletePolicy() error = %v", err)
	}
	call := fake.lastCall(t, "DELETE /api/v1/policy")
	if got := call.query.Encode(); got != "host=host1" {
		t.Errorf("query = %q", got)
	}
	if len(call.body) != 0 {
		t.Errorf("DELETE sent a body: %s", call.body)
	}
}

func TestListPolicies(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/policies", http.StatusOK, `{
  "policies": [
    {
      "id": "7f1c2b9d0e4a4c8f9b3d2a1e6c5f8a90",
      "target": {"host": "", "userName": "", "path": ""},
      "policy": {"retention": {"keepDaily": 14}, "files": {}, "errorHandling": {}, "scheduling": {}, "compression": {}, "metadataCompression": {}, "splitter": {}, "actions": {}, "osSnapshots": {"volumeShadowCopy": {}}, "logging": {"directories": {}, "entries": {}}, "upload": {}}
    },
    {
      "id": "0e4a4c8f9b3d2a1e6c5f8a907f1c2b9d",
      "target": {"host": "", "userName": "docker", "path": ""},
      "policy": {"scheduling": {"manual": true}}
    }
  ]
}
`)
	c := fake.client(t)
	entries, err := c.ListPolicies(context.Background(), PolicyFilter{UserName: "docker"})
	if err != nil {
		t.Fatalf("ListPolicies() error = %v", err)
	}
	if got := fake.query(t, "GET /api/v1/policies").Encode(); got != "userName=docker" {
		t.Errorf("query = %q, want the camelCase userName filter", got)
	}
	if len(entries) != 2 {
		t.Fatalf("ListPolicies() returned %d entries, want 2", len(entries))
	}
	if !entries[0].Target.IsGlobal() || entries[0].ID != "7f1c2b9d0e4a4c8f9b3d2a1e6c5f8a90" {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	if got := policyMarshal(t, entries[0].Policy); got != `{"retention":{"keepDaily":14}}` {
		t.Errorf("entries[0].Policy = %s", got)
	}
	if got := entries[1].Target.String(); got != "docker@" {
		t.Errorf("entries[1] target = %q, want docker@", got)
	}

	fake.reply("GET /api/v1/policies", http.StatusOK, `{"policies":[]}`)
	entries, err = c.ListPolicies(context.Background(), PolicyFilter{})
	if err != nil || entries == nil || len(entries) != 0 {
		t.Fatalf("ListPolicies() = %v, %v, want an empty list", entries, err)
	}
	if got := fake.query(t, "GET /api/v1/policies"); len(got) != 0 {
		t.Errorf("unfiltered query = %v", got)
	}
}

func TestResolvePolicy(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("POST /api/v1/policy/resolve", http.StatusOK, `{
  "effective": {"retention": {"keepLatest": 10, "keepDaily": 14}, "files": {}, "scheduling": {"cron": ["0 3 * * *"], "runMissed": true}, "splitter": {}},
  "definition": {
    "retention": {"keepLatest": {"host": "", "userName": "", "path": ""}, "keepDaily": {"host": "host1", "userName": "docker", "path": ""}},
    "scheduling": {"cron": {"host": "host1", "userName": "docker", "path": ""}},
    "osSnapshots": {"volumeShadowCopy": {"enable": {"host": "", "userName": "", "path": ""}}},
    "logging": {"entries": {"cacheMiss": {"host": "host1", "userName": "", "path": ""}}}
  },
  "defined": {"retention": {"keepDaily": 14}, "files": {}},
  "upcomingSnapshotTimes": ["2026-10-19T03:00:00Z", "2026-10-20T03:00:00+03:00"],
  "schedulingError": "invalid cron expression \"x\""
}
`)
	c := fake.client(t)
	target := SourceInfo{Host: "host1", UserName: "docker"}
	updates := &Policy{Scheduling: &SchedulingPolicy{Cron: []string{"x"}}, Splitter: &SplitterPolicy{}}
	resolved, err := c.ResolvePolicy(context.Background(), target, updates, 2)
	if err != nil {
		t.Fatalf("ResolvePolicy() error = %v", err)
	}

	call := fake.lastCall(t, "POST /api/v1/policy/resolve")
	if got := string(call.body); got != `{"updates":{"scheduling":{"cron":["x"]}},"numUpcomingSnapshotTimes":2}` {
		t.Errorf("body = %s", got)
	}
	if got := call.query.Encode(); got != "host=host1&userName=docker" {
		t.Errorf("query = %q", got)
	}

	if got := policyMarshal(t, resolved.Effective); got != `{"retention":{"keepLatest":10,"keepDaily":14},"scheduling":{"cron":["0 3 * * *"],"runMissed":true}}` {
		t.Errorf("effective = %s", got)
	}
	if got := resolved.Definition.Retention.KeepDaily; got != target {
		t.Errorf("definition of keepDaily = %+v", got)
	}
	if !resolved.Definition.Retention.KeepLatest.IsGlobal() || !resolved.Definition.Files.MaxFileSize.IsGlobal() {
		t.Error("zero and absent definition entries do not decode as the zero target")
	}
	if got := resolved.Definition.Logging.Entries.CacheMiss.String(); got != "@host1" {
		t.Errorf("definition of logging.entries.cacheMiss = %q", got)
	}
	if got := policyMarshal(t, resolved.Defined); got != `{"retention":{"keepDaily":14}}` {
		t.Errorf("defined = %s", got)
	}
	if len(resolved.UpcomingSnapshotTimes) != 2 ||
		!resolved.UpcomingSnapshotTimes[0].Equal(time.Date(2026, 10, 19, 3, 0, 0, 0, time.UTC)) ||
		resolved.UpcomingSnapshotTimes[1].Format(time.RFC3339) != "2026-10-20T03:00:00+03:00" {
		t.Errorf("upcoming times = %v", resolved.UpcomingSnapshotTimes)
	}
	if resolved.SchedulingError != `invalid cron expression "x"` {
		t.Errorf("schedulingError = %q", resolved.SchedulingError)
	}
}

// Without updates the stored policies are used: Kopia takes "updates": null
// for that, and requires a body all the same.
func TestResolvePolicyWithoutUpdates(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("POST /api/v1/policy/resolve", http.StatusOK, `{"effective":{},"definition":{},"defined":{},"upcomingSnapshotTimes":[]}`)
	resolved, err := fake.client(t).ResolvePolicy(context.Background(), SourceInfo{}, nil, -3)
	if err != nil {
		t.Fatalf("ResolvePolicy() error = %v", err)
	}
	if got := string(fake.lastCall(t, "POST /api/v1/policy/resolve").body); got != `{"updates":null,"numUpcomingSnapshotTimes":0}` {
		t.Errorf("body = %s", got)
	}
	if resolved.Effective == nil || resolved.Definition == nil || resolved.Defined == nil || resolved.SchedulingError != "" {
		t.Errorf("resolved = %+v", resolved)
	}

	// Missing objects are filled in rather than left nil.
	fake.reply("POST /api/v1/policy/resolve", http.StatusOK, `{"upcomingSnapshotTimes":[]}`)
	resolved, err = fake.client(t).ResolvePolicy(context.Background(), SourceInfo{}, nil, 0)
	if err != nil || resolved.Effective == nil || resolved.Definition == nil || resolved.Defined == nil {
		t.Fatalf("ResolvePolicy() = %+v, %v", resolved, err)
	}
}
