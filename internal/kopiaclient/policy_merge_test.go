package kopiaclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// policyMerge runs mergePolicyJSON, failing the test on an error.
func policyMerge(t *testing.T, stored string, policy *Policy) string {
	t.Helper()
	var raw json.RawMessage
	if stored != "" {
		raw = json.RawMessage(stored)
	}
	merged, err := mergePolicyJSON(raw, policy)
	if err != nil {
		t.Fatalf("mergePolicyJSON(%s) error = %v", stored, err)
	}
	return string(merged)
}

// policyModelPaths lists the keys of o as dotted paths, an object owned as a
// whole and a list of objects marked, and the keys of a list's elements after
// "[]".
func policyModelPaths(o *policyObject, prefix string) []string {
	var paths []string
	for _, key := range o.keys {
		path := prefix + key.name
		switch {
		case key.sameWhole != nil:
			paths = append(paths, path+" (whole)")
		case key.elements != nil:
			paths = append(paths, path+" (list)")
		default:
			paths = append(paths, path)
		}
		if key.object != nil {
			paths = append(paths, policyModelPaths(key.object, path+".")...)
		}
		if key.elements != nil {
			paths = append(paths, policyModelPaths(key.elements, path+"[].")...)
		}
	}
	return paths
}

// policyActionPaths lists the keys of one action.
func policyActionPaths(name string) []string {
	prefix := "actions." + name
	return []string{prefix + " (whole)", prefix + ".path", prefix + ".args", prefix + ".script", prefix + ".timeout", prefix + ".mode"}
}

// The keys the merge takes for Policy's are exactly those Policy encodes, at
// every level, in its order; only the actions are owned whole, and only the
// times of day are a list of objects.
func TestPolicyModelIsWhatPolicyEncodes(t *testing.T) {
	want := []string{
		"retention", "retention.keepLatest", "retention.keepHourly", "retention.keepDaily", "retention.keepWeekly",
		"retention.keepMonthly", "retention.keepAnnual", "retention.ignoreIdenticalSnapshots",
		"files", "files.ignore", "files.noParentIgnore", "files.ignoreDotFiles", "files.noParentDotFiles",
		"files.ignoreCacheDirs", "files.maxFileSize", "files.oneFileSystem",
		"errorHandling", "errorHandling.ignoreFileErrors", "errorHandling.ignoreDirectoryErrors",
		"errorHandling.ignoreUnknownTypes",
		"scheduling", "scheduling.intervalSeconds", "scheduling.timeOfDay (list)", "scheduling.timeOfDay[].hour",
		"scheduling.timeOfDay[].min", "scheduling.noParentTimeOfDay",
		"scheduling.manual", "scheduling.cron", "scheduling.runMissed",
		"compression", "compression.compressorName", "compression.onlyCompress", "compression.noParentOnlyCompress",
		"compression.neverCompress", "compression.noParentNeverCompress", "compression.minSize", "compression.maxSize",
		"metadataCompression", "metadataCompression.compressorName",
		"splitter", "splitter.algorithm",
		"actions",
	}
	for _, action := range []string{"beforeFolder", "afterFolder", "beforeSnapshotRoot", "afterSnapshotRoot"} {
		want = append(want, policyActionPaths(action)...)
	}
	want = append(want,
		"osSnapshots", "osSnapshots.volumeShadowCopy", "osSnapshots.volumeShadowCopy.enable",
		"logging", "logging.directories", "logging.directories.snapshotted", "logging.directories.ignored",
		"logging.entries", "logging.entries.snapshotted", "logging.entries.ignored", "logging.entries.cacheHit",
		"logging.entries.cacheMiss",
		"upload", "upload.maxParallelSnapshots", "upload.maxParallelFileReads", "upload.parallelUploadAboveSize",
		"noParent",
	)
	if got := policyModelPaths(policyModel, ""); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("model =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A policy with every field set encodes exactly these keys.
	action := &ActionCommand{Command: "/bin/true", Arguments: []string{"a"}, Script: "x", TimeoutSeconds: 1, Mode: "async"}
	full := Policy{
		Retention: &RetentionPolicy{
			KeepLatest: policyPointer[int64](1), KeepHourly: policyPointer[int64](1), KeepDaily: policyPointer[int64](1),
			KeepWeekly: policyPointer[int64](1), KeepMonthly: policyPointer[int64](1), KeepAnnual: policyPointer[int64](1),
			IgnoreIdenticalSnapshots: policyPointer(true),
		},
		Files: &FilesPolicy{
			IgnoreRules: []string{"a"}, NoParentIgnoreRules: true, DotIgnoreFiles: []string{".a"}, NoParentDotIgnoreFiles: true,
			IgnoreCacheDirectories: policyPointer(true), MaxFileSize: 1, OneFileSystem: policyPointer(true),
		},
		ErrorHandling: &ErrorHandlingPolicy{
			IgnoreFileErrors: policyPointer(true), IgnoreDirectoryErrors: policyPointer(true), IgnoreUnknownTypes: policyPointer(true),
		},
		Scheduling: &SchedulingPolicy{
			IntervalSeconds: 60, TimesOfDay: []TimeOfDay{{Hour: 1}}, NoParentTimeOfDay: true, Manual: true,
			Cron: []string{"0 1 * * *"}, RunMissed: policyPointer(true),
		},
		Compression: &CompressionPolicy{
			CompressorName: "zstd", OnlyCompress: []string{".a"}, NoParentOnlyCompress: true, NeverCompress: []string{".b"},
			NoParentNeverCompress: true, MinSize: 1, MaxSize: 2,
		},
		MetadataCompression: &MetadataCompressionPolicy{CompressorName: "zstd"},
		Splitter:            &SplitterPolicy{Algorithm: "FIXED-1M"},
		Actions:             &ActionsPolicy{BeforeFolder: action, AfterFolder: action, BeforeSnapshotRoot: action, AfterSnapshotRoot: action},
		OSSnapshots:         &OSSnapshotPolicy{VolumeShadowCopy: &VolumeShadowCopyPolicy{Enable: policyPointer[int64](1)}},
		Logging: &LoggingPolicy{
			Directories: &DirLoggingPolicy{Snapshotted: policyPointer[int64](1), Ignored: policyPointer[int64](1)},
			Entries: &EntryLoggingPolicy{
				Snapshotted: policyPointer[int64](1), Ignored: policyPointer[int64](1),
				CacheHit: policyPointer[int64](1), CacheMiss: policyPointer[int64](1),
			},
		},
		Upload: &UploadPolicy{
			MaxParallelSnapshots: policyPointer[int64](1), MaxParallelFileReads: policyPointer[int64](1),
			ParallelUploadAboveSize: policyPointer[int64](1),
		},
		NoParent: true,
	}
	var encoded func(prefix string, data json.RawMessage) []string
	encoded = func(prefix string, data json.RawMessage) []string {
		if elements, _ := policyElements(data); len(elements) > 0 {
			return encoded(strings.TrimSuffix(prefix, ".")+"[].", elements[0])
		}
		members, _, _ := policyMembers(data)
		var paths []string
		for _, m := range members {
			paths = append(paths, prefix+m.name)
			paths = append(paths, encoded(prefix+m.name+".", m.value)...)
		}
		return paths
	}
	var wantEncoded []string
	for _, path := range want {
		path = strings.TrimSuffix(strings.TrimSuffix(path, " (whole)"), " (list)")
		wantEncoded = append(wantEncoded, path)
	}
	fullJSON := policyMarshal(t, full)
	if got := encoded("", json.RawMessage(fullJSON)); strings.Join(got, "\n") != strings.Join(wantEncoded, "\n") {
		t.Errorf("a full policy encodes\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantEncoded, "\n"))
	}

	// Written over nothing, or over itself, it is sent exactly as encoded.
	for _, stored := range []string{"", fullJSON} {
		if got := policyMerge(t, stored, &full); got != fullJSON {
			t.Errorf("the full policy over %q =\n%s\nwant\n%s", stored, got, fullJSON)
		}
	}
}

// Without a stored policy, or with one that holds only what Policy models,
// the body is exactly the encoding of the policy, in either release's shape.
func TestMergePolicyJSONWithNothingToKeepIsTheEncoding(t *testing.T) {
	policies := map[string]*Policy{
		"empty":   {},
		"example": func() *Policy { p := policyDecode(t, policyJSONShape0220); return &p }(),
		"default": DefaultGlobalPolicy(),
		"action":  {Actions: &ActionsPolicy{BeforeFolder: &ActionCommand{}}, NoParent: true},
	}
	stored := []string{"", "null", "{}", policyJSONShape0223, policyJSONShape0220, policyDefaultGlobalJSON,
		`{"retention":null,"actions":{"beforeFolder":{"path":"/bin/false"}},"noParent":true}`}
	for name, policy := range policies {
		want := policyMarshal(t, policy)
		for _, s := range stored {
			if got := policyMerge(t, s, policy); got != want {
				t.Errorf("%s over %s =\n%s\nwant\n%s", name, s, got, want)
			}
		}
	}
	if got := policyMerge(t, policyJSONShape0223, nil); got != "{}" {
		t.Errorf("nil policy = %s, want {}", got)
	}
}

// A top-level key Policy does not model is kept, whatever the policy says.
func TestMergePolicyJSONKeepsAnUnknownTopLevelKey(t *testing.T) {
	stored := `{"retention":{"keepDaily":7},"newFlag":true,"newLimit":3}`
	policy := &Policy{Retention: &RetentionPolicy{KeepDaily: policyPointer[int64](14)}}
	if got, want := policyMerge(t, stored, policy), `{"retention":{"keepDaily":14},"newFlag":true,"newLimit":3}`; got != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}
	if got, want := policyMerge(t, stored, &Policy{}), `{"newFlag":true,"newLimit":3}`; got != want {
		t.Errorf("merged with an empty policy =\n%s\nwant\n%s", got, want)
	}
}

// A key inside a section, at any depth, that Policy does not model is kept;
// the section is then sent for it even when the policy leaves the section
// out, without any of the section's modelled fields.
func TestMergePolicyJSONKeepsAnUnknownKeyInASection(t *testing.T) {
	stored := `{"retention":{"keepDaily":7,"keepQuarterly":4},` +
		`"logging":{"directories":{"snapshotted":5,"newLevel":2},"entries":{},"newCategory":{"enabled":true}},` +
		`"osSnapshots":{"volumeShadowCopy":{"enable":1,"newOption":"x"},"newMechanism":{"enable":1}}}`

	policy := &Policy{
		Retention: &RetentionPolicy{KeepDaily: policyPointer[int64](14)},
		Logging:   &LoggingPolicy{Entries: &EntryLoggingPolicy{CacheHit: policyPointer[int64](1)}},
	}
	want := `{"retention":{"keepDaily":14,"keepQuarterly":4},` +
		`"osSnapshots":{"volumeShadowCopy":{"newOption":"x"},"newMechanism":{"enable":1}},` +
		`"logging":{"directories":{"newLevel":2},"entries":{"cacheHit":1},"newCategory":{"enabled":true}}}`
	if got := policyMerge(t, stored, policy); got != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}

	want = `{"retention":{"keepQuarterly":4},` +
		`"osSnapshots":{"volumeShadowCopy":{"newOption":"x"},"newMechanism":{"enable":1}},` +
		`"logging":{"directories":{"newLevel":2},"newCategory":{"enabled":true}}}`
	if got := policyMerge(t, stored, &Policy{}); got != want {
		t.Errorf("merged with an empty policy =\n%s\nwant\n%s", got, want)
	}
}

// A section Policy does not model is kept as a whole, as Kopia sent it.
func TestMergePolicyJSONKeepsAnUnknownSection(t *testing.T) {
	stored := `{"scheduling":{"manual":true},"encryption":{"mode":"strict","keys":["a","b"],"nested":{"x":1}}}`
	policy := &Policy{Scheduling: &SchedulingPolicy{IntervalSeconds: 3600}}
	want := `{"scheduling":{"intervalSeconds":3600},"encryption":{"mode":"strict","keys":["a","b"],"nested":{"x":1}}}`
	if got := policyMerge(t, stored, policy); got != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}
}

// Every field Policy models belongs to the policy: one it leaves out is
// removed, however the stored policy set it, and a section left with nothing
// is not sent.
func TestMergePolicyJSONRemovesWhatThePolicyLeavesOut(t *testing.T) {
	policy := &Policy{Compression: &CompressionPolicy{CompressorName: "zstd"}}
	if got, want := policyMerge(t, policyDefaultGlobalJSON, policy), `{"compression":{"compressorName":"zstd"}}`; got != want {
		t.Errorf("merged over the default global policy = %s, want %s", got, want)
	}

	// A list is the policy's: what the stored one had is gone, and a time of
	// day the policy changes is a new one, without the unknown fields of the
	// stored one.
	stored := `{"scheduling":{"timeOfDay":[{"hour":3,"min":0,"newSecond":30}],"cron":["0 1 * * *"],"manual":true}}`
	policy = &Policy{Scheduling: &SchedulingPolicy{TimesOfDay: []TimeOfDay{{Hour: 4}}}}
	if got, want := policyMerge(t, stored, policy), `{"scheduling":{"timeOfDay":[{"hour":4,"min":0}]}}`; got != want {
		t.Errorf("merged = %s, want %s", got, want)
	}
}

// An action is there or not as a whole. One the policy has keeps the fields
// of the stored one Policy does not model, as long as it runs the same
// command; one it leaves out is removed with them, as on their own they would
// make an action without a command, which fails every snapshot it runs
// before. An action Policy does not model is kept.
func TestMergePolicyJSONOwnsActionsWhole(t *testing.T) {
	stored := `{"actions":{"beforeSnapshotRoot":{"path":"/bin/backup","args":["db"],"timeout":60,"newRetries":3},` +
		`"afterSnapshotRoot":{"path":"/bin/notify","newRetries":1},"beforeRestore":{"path":"/bin/prepare"}}}`
	policy := &Policy{Actions: &ActionsPolicy{
		BeforeSnapshotRoot: &ActionCommand{Command: "/bin/backup", Arguments: []string{"db"}, TimeoutSeconds: 300, Mode: "essential"},
	}}
	want := `{"actions":{"beforeSnapshotRoot":{"path":"/bin/backup","args":["db"],"timeout":300,"mode":"essential","newRetries":3},` +
		`"beforeRestore":{"path":"/bin/prepare"}}}`
	if got := policyMerge(t, stored, policy); got != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}
	if got, want := policyMerge(t, stored, &Policy{}), `{"actions":{"beforeRestore":{"path":"/bin/prepare"}}}`; got != want {
		t.Errorf("merged with an empty policy = %s, want %s", got, want)
	}

	// Another path, other arguments or a script make another command, which
	// the fields set with the old one may not suit.
	for _, other := range []*ActionCommand{
		{Command: "/bin/backup2", Arguments: []string{"db"}},
		{Command: "/bin/backup"},
		{Command: "/bin/backup", Arguments: []string{"db"}, Script: "#!/bin/sh"},
	} {
		policy := &Policy{Actions: &ActionsPolicy{BeforeSnapshotRoot: other}}
		want := `{"actions":{"beforeSnapshotRoot":` + policyMarshal(t, other) + `,"beforeRestore":{"path":"/bin/prepare"}}}`
		if got := policyMerge(t, stored, policy); got != want {
			t.Errorf("merged with %s =\n%s\nwant\n%s", policyMarshal(t, other), got, want)
		}
	}

	// An empty action the policy has is sent, as encoding sends it.
	policy = &Policy{Actions: &ActionsPolicy{AfterFolder: &ActionCommand{}}}
	if got, want := policyMerge(t, `{"actions":{"afterFolder":{"newRetries":1}}}`, policy), `{"actions":{"afterFolder":{"newRetries":1}}}`; got != want {
		t.Errorf("merged = %s, want %s", got, want)
	}
	if got, want := policyMerge(t, "", policy), `{"actions":{"afterFolder":{}}}`; got != want {
		t.Errorf("merged = %s, want %s", got, want)
	}

	// Kopia never repeats a key, but Go decodes a repeated one into the same
	// action, and a null unsets it; the stored action is the one that leaves.
	policy = &Policy{Actions: &ActionsPolicy{BeforeFolder: &ActionCommand{Command: "/a"}}}
	for stored, want := range map[string]string{
		`{"actions":{"beforeFolder":{"path":"/a","retries":1},"beforeFolder":null}}`:                   `{"actions":{"beforeFolder":{"path":"/a"}}}`,
		`{"actions":{"beforeFolder":{"path":"/b","retries":1},"BeforeFolder":{"path":"/a"}}}`:          `{"actions":{"beforeFolder":{"path":"/a","retries":1}}}`,
		`{"actions":{"beforeFolder":{"path":"/a","retries":1},"beforeFolder":{"path":"/b"}}}`:          `{"actions":{"beforeFolder":{"path":"/a"}}}`,
		`{"actions":{"beforeFolder":null,"beforeFolder":{"path":"/a","retries":2},"x":{"retries":3}}}`: `{"actions":{"beforeFolder":{"path":"/a","retries":2},"x":{"retries":3}}}`,
	} {
		if got := policyMerge(t, stored, policy); got != want {
			t.Errorf("merged over %s = %s, want %s", stored, got, want)
		}
	}
}

// Kopia decodes keys case-insensitively, with Go's folding, so a stored key
// that matches a modelled one in another case is that field: it is never
// kept beside the policy's, and the unknown fields inside it are kept under
// Policy's spelling. A key that only looks alike is a field of its own.
func TestMergePolicyJSONMatchesKeysAsKopiaDoes(t *testing.T) {
	// U+212A KELVIN SIGN folds to k, as Go's decoder agrees.
	const kelvinDaily = "\u212AeepDaily"
	var decoded Policy
	if err := json.Unmarshal([]byte(`{"RETENTION":{"`+kelvinDaily+`":5},"NoParent":true}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Retention == nil || decoded.Retention.KeepDaily == nil || *decoded.Retention.KeepDaily != 5 || !decoded.NoParent {
		t.Fatalf("Go decodes %s, want keepDaily 5 and noParent", policyMarshal(t, decoded))
	}

	stored := `{"RETENTION":{"` + kelvinDaily + `":5,"KeepQuarterly":4,"keep_hourly":2},"NoParent":true,"Retention":{"keepBiweekly":1}}`
	policy := &Policy{Retention: &RetentionPolicy{KeepDaily: policyPointer[int64](7)}}
	want := `{"retention":{"keepDaily":7,"KeepQuarterly":4,"keep_hourly":2,"keepBiweekly":1}}`
	if got := policyMerge(t, stored, policy); got != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}

	// Every modelled key, in upper case, is the policy's.
	for _, path := range policyModelPaths(policyModel, "") {
		if strings.Contains(path, "[]") {
			continue
		}
		path = strings.TrimSuffix(strings.TrimSuffix(path, " (whole)"), " (list)")
		names := strings.Split(path, ".")
		body := "null"
		for i := len(names) - 1; i >= 0; i-- {
			body = `{"` + strings.ToUpper(names[i]) + `":` + body + `}`
		}
		if got := policyMerge(t, body, &Policy{}); got != "{}" {
			t.Errorf("%s: merged %s into %s, want {}", path, body, got)
		}
	}
}

// null means "not set" to Kopia: for a modelled key it is the policy's like
// any other value, and an unknown one is kept as it is.
func TestMergePolicyJSONWithNulls(t *testing.T) {
	stored := `{"retention":null,"files":{"ignore":null,"newList":null},"newSection":null}`
	if got, want := policyMerge(t, stored, &Policy{}), `{"files":{"newList":null},"newSection":null}`; got != want {
		t.Errorf("merged = %s, want %s", got, want)
	}
	policy := &Policy{Retention: &RetentionPolicy{KeepDaily: policyPointer[int64](1)}}
	if got, want := policyMerge(t, stored, policy), `{"retention":{"keepDaily":1},"files":{"newList":null},"newSection":null}`; got != want {
		t.Errorf("merged = %s, want %s", got, want)
	}
}

// What Kopia reported for an unknown key is sent back exactly: empty values
// that would mean "not set" for a known field, and numbers beyond float64's
// precision. Only Kopia's indentation goes, and <, > and & are escaped, as in
// the rest of the body.
func TestMergePolicyJSONKeepsUnknownValuesExactly(t *testing.T) {
	stored := `{
  "newSection": {},
  "newList": [],
  "newFlag": false,
  "newCount": 0,
  "newName": "",
  "newLimit": 9007199254740993,
  "newRatio": 1.50,
  "newText": "a < b"
}
`
	want := `{"newSection":{},"newList":[],"newFlag":false,"newCount":0,"newName":"","newLimit":9007199254740993,"newRatio":1.50,"newText":"a \u003c b"}`
	if got := policyMerge(t, stored, &Policy{}); got != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}

	nested := "{\n  \"files\": {\n    \"newRules\": [\n      {\n        \"glob\": \"*.tmp\"\n      }\n    ]\n  },\n" +
		"  \"newSection\": {\n    \"a\": [\n      1,\n      2\n    ],\n    \"b\": {\n      \"c\": true\n    }\n  }\n}\n"
	want = `{"files":{"newRules":[{"glob":"*.tmp"}]},"newSection":{"a":[1,2],"b":{"c":true}}}`
	if got := policyMerge(t, nested, &Policy{}); got != want {
		t.Errorf("merged nested values =\n%s\nwant\n%s", got, want)
	}
}

// 0.22.0 leaves empty sections out and 0.22.3 sends them as {}; the same
// stored policy merges to the same body from both.
func TestMergePolicyJSONIsTheSameForBothReleaseShapes(t *testing.T) {
	newer := strings.Replace(policyJSONShape0223, `"files": {},`, `"files": {"newRule": "x"},`, 1)
	older := strings.Replace(policyJSONShape0220, `{"retention"`, `{"files":{"newRule":"x"},"retention"`, 1)
	policy := &Policy{Upload: &UploadPolicy{MaxParallelSnapshots: policyPointer[int64](4)}}
	want := `{"files":{"newRule":"x"},"upload":{"maxParallelSnapshots":4}}`
	for name, stored := range map[string]string{"0.22.3": newer, "0.22.0": older} {
		if got := policyMerge(t, stored, policy); got != want {
			t.Errorf("%s: merged =\n%s\nwant\n%s", name, got, want)
		}
	}
}

func TestMergePolicyJSONRefusesWhatIsNotAPolicy(t *testing.T) {
	for _, stored := range []string{`[]`, `"policy"`, `{"retention":`, `{} {}`} {
		if _, err := mergePolicyJSON(json.RawMessage(stored), &Policy{}); err == nil {
			t.Errorf("mergePolicyJSON(%s) succeeded", stored)
		}
	}
}

func TestGetPolicyJSON(t *testing.T) {
	fake := newFakeKopia()
	stored := strings.Replace(policyJSONShape0223, `"splitter": {},`, `"splitter": {"newChunking": "auto"},`, 1)
	fake.reply("GET /api/v1/policy", http.StatusOK, stored)
	c := fake.client(t)

	target := SourceInfo{Host: "host1", UserName: "docker"}
	raw, err := c.GetPolicyJSON(context.Background(), target)
	if err != nil {
		t.Fatalf("GetPolicyJSON() error = %v", err)
	}
	if !strings.Contains(string(raw), `"newChunking": "auto"`) {
		t.Errorf("GetPolicyJSON() = %s, want the field Policy does not model", raw)
	}
	if got := fake.query(t, "GET /api/v1/policy").Encode(); got != "host=host1&userName=docker" {
		t.Errorf("query = %q", got)
	}

	// GetPolicy decodes the same body.
	p, err := c.GetPolicy(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if got := policyMarshal(t, p); got != policyJSONShape0220 {
		t.Errorf("GetPolicy() = %s", got)
	}

	fake.reply("GET /api/v1/policy", http.StatusOK, "null\n")
	if _, err := c.GetPolicyJSON(context.Background(), target); !errors.Is(err, ErrPolicyUnavailable) {
		t.Errorf("GetPolicyJSON() of null error = %v, want ErrPolicyUnavailable", err)
	}
	fake.replyError("GET /api/v1/policy", http.StatusBadRequest, CodeNotFound, "policy not found")
	if _, err := c.GetPolicyJSON(context.Background(), target); !IsNotFound(err) {
		t.Errorf("GetPolicyJSON() of a missing policy error = %v, want one IsNotFound reports", err)
	}
	fake.reply("GET /api/v1/policy", http.StatusOK, `["not","a","policy"]`)
	for name, get := range map[string]func() error{
		"GetPolicyJSON": func() error { _, err := c.GetPolicyJSON(context.Background(), target); return err },
		"GetPolicy":     func() error { _, err := c.GetPolicy(context.Background(), target); return err },
	} {
		if err := get(); err == nil || !strings.Contains(err.Error(), "docker@host1") {
			t.Errorf("%s() of a list error = %v, want one naming the target", name, err)
		}
	}
}

func TestSetPolicyKeepingUnknown(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("PUT /api/v1/policy", http.StatusOK, "{}\n")
	c := fake.client(t)

	target := SourceInfo{Host: "host1"}
	stored := json.RawMessage(`{"upload":{"maxParallelSnapshots":1,"newBandwidth":100},"newFlag":true}`)
	policy := &Policy{Upload: &UploadPolicy{MaxParallelSnapshots: policyPointer[int64](2)}}
	if err := c.SetPolicyKeepingUnknown(context.Background(), target, policy, stored); err != nil {
		t.Fatalf("SetPolicyKeepingUnknown() error = %v", err)
	}
	call := fake.lastCall(t, "PUT /api/v1/policy")
	if got, want := string(call.body), `{"upload":{"maxParallelSnapshots":2,"newBandwidth":100},"newFlag":true}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if got := call.query.Encode(); got != "host=host1" {
		t.Errorf("query = %q", got)
	}

	// Without a stored policy it sends what SetPolicy sends.
	if err := c.SetPolicyKeepingUnknown(context.Background(), target, policy, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(fake.lastCall(t, "PUT /api/v1/policy").body); got != `{"upload":{"maxParallelSnapshots":2}}` {
		t.Errorf("body without a stored policy = %s", got)
	}

	// A stored policy that cannot be merged is not written over.
	puts := fake.count("PUT /api/v1/policy")
	if err := c.SetPolicyKeepingUnknown(context.Background(), target, policy, json.RawMessage(`[1]`)); err == nil {
		t.Error("SetPolicyKeepingUnknown() over a list succeeded")
	}
	if fake.count("PUT /api/v1/policy") != puts {
		t.Error("SetPolicyKeepingUnknown() wrote over a stored policy it could not merge")
	}
}

// Each entry keeps the policy as Kopia sent it, and CanonicalJSON gives it in
// the provider's form with the fields Policy does not model.
func TestListPoliciesKeepsTheStoredJSON(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/policies", http.StatusOK, `{"policies":[
    {"id": "a", "target": {"host": "", "userName": "", "path": ""},
     "policy": {"retention": {"keepDaily": 14, "keepQuarterly": 2}, "files": {}, "logging": {"directories": {}, "entries": {}}, "newFlag": true}},
    {"id": "b", "target": {"host": "h", "userName": "", "path": ""}, "policy": null},
    {"id": "c", "target": {"host": "h", "userName": "u", "path": ""}}
  ]}`)
	entries, err := fake.client(t).ListPolicies(context.Background(), PolicyFilter{})
	if err != nil {
		t.Fatalf("ListPolicies() error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("ListPolicies() returned %d entries, want 3", len(entries))
	}
	if !strings.Contains(string(entries[0].PolicyJSON), `"keepQuarterly": 2`) {
		t.Errorf("entries[0].PolicyJSON = %s", entries[0].PolicyJSON)
	}
	if got := policyMarshal(t, entries[0].Policy); got != `{"retention":{"keepDaily":14}}` {
		t.Errorf("entries[0].Policy = %s", got)
	}
	want := []string{`{"retention":{"keepDaily":14,"keepQuarterly":2},"newFlag":true}`, "{}", "{}"}
	for i, entry := range entries {
		if i > 0 && (entry.Policy != nil || entry.PolicyJSON != nil) {
			t.Errorf("entries[%d] = %+v, want no policy", i, entry)
		}
		got, err := entry.CanonicalJSON()
		if err != nil {
			t.Fatalf("entries[%d].CanonicalJSON() error = %v", i, err)
		}
		if string(got) != want[i] {
			t.Errorf("entries[%d].CanonicalJSON() = %s, want %s", i, got, want[i])
		}
	}
}

// Test types for the description of a struct's JSON object.
type (
	policyTestInner struct {
		Value int `json:"value"`
	}
	policyTestStamp struct{ Seconds int64 }
	policyTestText  struct{ Value string }
	policyTestShape struct {
		Inner    *policyTestInner `json:"inner,omitempty"`
		Stamp    policyTestStamp  `json:"stamp"`
		Text     *policyTestText  `json:"text"`
		Skipped  string           `json:"-"`
		Untagged string
		List     []policyTestInner `json:"list"`
		Stamps   []policyTestStamp `json:"stamps"`
		Names    []string          `json:"names"`
	}
	policyTestEmbedding struct {
		policyTestInner
	}
)

func (s policyTestStamp) MarshalJSON() ([]byte, error) { return json.Marshal(s.Seconds) }

func (t policyTestText) MarshalText() ([]byte, error) { return []byte(t.Value), nil }

// The description follows encoding/json: a struct that encodes itself is a
// value like any other, "-" leaves a field out, an untagged field goes by its
// Go name, and only a list of plain structs is a list of objects.
func TestPolicyObjectOfFollowsEncodingJSON(t *testing.T) {
	want := []string{"inner", "inner.value", "stamp", "text", "Untagged", "list (list)", "list[].value", "stamps", "names"}
	if got := policyModelPaths(policyObjectOf(reflect.TypeOf(policyTestShape{})), ""); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", got, want)
	}

	// encoding/json moves the fields of an embedded struct into the outer
	// object, which the merge does not follow.
	defer func() {
		if recover() == nil {
			t.Error("an embedded field did not panic")
		}
	}()
	policyObjectOf(reflect.TypeOf(policyTestEmbedding{policyTestInner{Value: 1}}))
}

// Whatever was stored, the body decodes to exactly the policy written, as
// Kopia would decode it: every stored value of a modelled field is gone,
// in any case and of any type.
func TestMergePolicyJSONDecodesToThePolicy(t *testing.T) {
	stored := []string{
		policyJSONShape0223,
		policyDefaultGlobalJSON,
		`{"RETENTION":{"KEEPDAILY":1,"keepNew":2},"Retention":null,"files":{"IGNORE":["a"],"new":true},"newTop":{"x":[1,2]}}`,
		`{"retention":5,"files":[],"scheduling":"x","actions":{"beforeFolder":null,"afterFolder":7},` +
			`"logging":{"directories":[],"entries":{"cacheHit":"x"}},"osSnapshots":{"volumeShadowCopy":true},"noParent":"yes"}`,
		`{"scheduling":{"timeOfDay":[{"hour":3,"min":15,"sec":1},{"HOUR":15,"Min":0}],"MANUAL":true},` +
			`"actions":{"beforeSnapshotRoot":{"PATH":"/bin/a","Args":["x"],"timeout":5,"new":1}}}`,
	}
	example := policyDecode(t, policyJSONShape0220)
	policies := []*Policy{
		{},
		DefaultGlobalPolicy(),
		&example,
		{
			Scheduling: &SchedulingPolicy{TimesOfDay: []TimeOfDay{{Hour: 3, Minute: 15}, {Hour: 15}}, RunMissed: policyPointer(false)},
			Actions: &ActionsPolicy{
				BeforeSnapshotRoot: &ActionCommand{Command: "/bin/a", Arguments: []string{"x"}},
				AfterFolder:        &ActionCommand{},
			},
			Files:    &FilesPolicy{MaxFileSize: -1},
			NoParent: true,
		},
	}
	for _, s := range stored {
		for _, policy := range policies {
			merged := policyMerge(t, s, policy)
			if got := policyDecode(t, merged); !reflect.DeepEqual(got, policy.Normalised()) {
				t.Errorf("%s over %s =\n%s\ndecodes to %s", policyMarshal(t, policy), s, merged, policyMarshal(t, got))
			}
		}
	}
}

// A time of day the policy has keeps the fields of the stored one with the
// same hour and minute that Policy does not model, wherever it was in the
// stored list; one the policy changes is a new time without them.
func TestMergePolicyJSONKeepsTheTimesOfDayItHas(t *testing.T) {
	stored := `{"scheduling":{"timeOfDay":[{"hour":15,"min":0,"sec":30},{"hour":3,"min":15,"zone":"UTC"},{"hour":3,"min":15,"zone":"CET"}],"runMissed":true}}`
	times := func(times ...TimeOfDay) *Policy { return &Policy{Scheduling: &SchedulingPolicy{TimesOfDay: times}} }

	tests := []struct {
		stored string
		policy *Policy
		want   string
	}{
		{stored, times(TimeOfDay{Hour: 3, Minute: 15}, TimeOfDay{Hour: 15}),
			`{"scheduling":{"timeOfDay":[{"hour":3,"min":15,"zone":"UTC"},{"hour":15,"min":0,"sec":30}]}}`},
		{stored, times(TimeOfDay{Hour: 4}, TimeOfDay{Hour: 15}),
			`{"scheduling":{"timeOfDay":[{"hour":4,"min":0},{"hour":15,"min":0,"sec":30}]}}`},
		{stored, &Policy{}, `{}`},
		// A matching time is written as Policy writes it, whatever the
		// spelling, order or gaps of the stored one; only its unknown
		// fields are taken from it.
		{`{"scheduling":{"timeOfDay":[{"sec":1,"MIN":15,"Hour":3},{"hour":4},null]}}`,
			times(TimeOfDay{Hour: 3, Minute: 15}, TimeOfDay{Hour: 4}, TimeOfDay{}),
			`{"scheduling":{"timeOfDay":[{"hour":3,"min":15,"sec":1},{"hour":4,"min":0},{"hour":0,"min":0}]}}`},
	}
	for _, tc := range tests {
		if got := policyMerge(t, tc.stored, tc.policy); got != tc.want {
			t.Errorf("%s over %s =\n%s\nwant\n%s", policyMarshal(t, tc.policy), tc.stored, got, tc.want)
		}
	}
	if got := KeptPolicyFields(json.RawMessage(tests[3].stored), tests[3].policy); strings.Join(got, ",") != "scheduling.timeOfDay.sec" {
		t.Errorf("KeptPolicyFields() = %v, want only the unknown field of the times", got)
	}

	// The canonical text of the stored policy shows them too.
	entry := PolicyEntry{PolicyJSON: json.RawMessage(stored)}
	decoded := policyDecode(t, stored)
	entry.Policy = &decoded
	got, err := entry.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"scheduling":{"timeOfDay":[{"hour":15,"min":0,"sec":30},{"hour":3,"min":15,"zone":"UTC"},{"hour":3,"min":15,"zone":"CET"}],"runMissed":true}}`
	if string(got) != want {
		t.Errorf("CanonicalJSON() =\n%s\nwant\n%s", got, want)
	}
}

// KeptPolicyFields names what a write keeps that Policy does not model, in
// the order it is sent.
func TestKeptPolicyFields(t *testing.T) {
	stored := json.RawMessage(`{"retention":{"keepDaily":7,"keepQuarterly":4},` +
		`"scheduling":{"manual":true,"jitter":30,"timeOfDay":[{"hour":3,"min":0,"sec":5},{"hour":3,"min":0,"sec":6}]},` +
		`"actions":{"beforeFolder":{"path":"/x","retries":1}},"encryption":{"mode":"a"},"newFlag":true}`)
	policy := &Policy{
		Scheduling: &SchedulingPolicy{TimesOfDay: []TimeOfDay{{Hour: 3}}},
		Actions:    &ActionsPolicy{BeforeFolder: &ActionCommand{Command: "/x"}},
	}
	want := []string{"retention.keepQuarterly", "scheduling.timeOfDay.sec", "scheduling.jitter", "actions.beforeFolder.retries", "encryption", "newFlag"}
	if got := KeptPolicyFields(stored, policy); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("KeptPolicyFields() = %v, want %v", got, want)
	}

	want = []string{"retention.keepQuarterly", "scheduling.jitter", "encryption", "newFlag"}
	if got := KeptPolicyFields(stored, &Policy{}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("KeptPolicyFields() with an empty policy = %v, want %v", got, want)
	}
	for _, none := range []json.RawMessage{nil, json.RawMessage(policyDefaultGlobalJSON), json.RawMessage(`[]`)} {
		if got := KeptPolicyFields(none, policy); len(got) != 0 {
			t.Errorf("KeptPolicyFields(%s) = %v, want none", none, got)
		}
	}
}

// The updates of a resolve are what a write would send, so that Kopia judges
// the kept scheduling fields too.
func TestResolvePolicyKeepingUnknown(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("POST /api/v1/policy/resolve", http.StatusOK,
		`{"effective":{},"definition":{},"defined":{},"upcomingSnapshotTimes":[],"schedulingError":"invalid scheduling policy: manual cannot be combined with other scheduling policies"}`)
	c := fake.client(t)

	stored := json.RawMessage(`{"scheduling":{"cron":["0 1 * * *"],"jitter":30},"newFlag":true}`)
	policy := &Policy{Scheduling: &SchedulingPolicy{Manual: true}}
	resolved, err := c.ResolvePolicyKeepingUnknown(context.Background(), SourceInfo{Host: "host1"}, policy, stored, 0)
	if err != nil {
		t.Fatalf("ResolvePolicyKeepingUnknown() error = %v", err)
	}
	if resolved.SchedulingError == "" {
		t.Error("SchedulingError is empty")
	}
	call := fake.lastCall(t, "POST /api/v1/policy/resolve")
	if got, want := string(call.body), `{"updates":{"scheduling":{"manual":true,"jitter":30},"newFlag":true},"numUpcomingSnapshotTimes":0}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}

	// Without a stored policy the updates are the policy alone, and without
	// updates the stored policy is used, whatever stored says.
	if _, err := c.ResolvePolicyKeepingUnknown(context.Background(), SourceInfo{}, policy, nil, 1); err != nil {
		t.Fatal(err)
	}
	if got, want := string(fake.lastCall(t, "POST /api/v1/policy/resolve").body), `{"updates":{"scheduling":{"manual":true}},"numUpcomingSnapshotTimes":1}`; got != want {
		t.Errorf("body without a stored policy = %s, want %s", got, want)
	}
	if _, err := c.ResolvePolicyKeepingUnknown(context.Background(), SourceInfo{}, nil, stored, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := string(fake.lastCall(t, "POST /api/v1/policy/resolve").body), `{"updates":null,"numUpcomingSnapshotTimes":0}`; got != want {
		t.Errorf("body without updates = %s, want %s", got, want)
	}
}
