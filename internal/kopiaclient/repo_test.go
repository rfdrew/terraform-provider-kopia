package kopiaclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// repoStatusReleaseJSON is GET /api/v1/repo/status from Kopia 0.23.1 for a
// filesystem repository with ECC enabled and limits in the config file at
// start-up: indented, in struct order, with the ClientOptions fields
// promoted to the top level.
const repoStatusReleaseJSON = `{
  "connected": true,
  "configFile": "/app/config/repository.config",
  "formatVersion": 3,
  "hash": "BLAKE2B-256-128",
  "encryption": "AES256-GCM-HMAC-SHA256",
  "ecc": "REED-SOLOMON-CRC32",
  "eccOverheadPercent": 2,
  "splitter": "DYNAMIC-4M-BUZHASH",
  "maxPackSize": 20971520,
  "storage": "filesystem",
  "supportsContentCompression": true,
  "hostname": "kopia",
  "username": "root",
  "description": "Repository in Filesystem: /repository",
  "enableActions": false,
  "formatBlobCacheDuration": 900000000000,
  "throttlingLimits": {
    "maxUploadSpeedBytesPerSecond": 1048576
  }
}
`

// repoStatusMasterJSON is the same endpoint from Kopia master, which writes
// compact JSON, for a read-only connection made with
// --disable-repository-format-cache.
const repoStatusMasterJSON = `{"connected":true,"configFile":"/app/config/repository.config","formatVersion":3,` +
	`"hash":"BLAKE3-256","encryption":"CHACHA20-POLY1305-HMAC-SHA256","splitter":"FIXED-4M","maxPackSize":33554432,` +
	`"storage":"s3","supportsContentCompression":true,"hostname":"nas","username":"backup","readonly":true,` +
	`"description":"Offsite","enableActions":true,"formatBlobCacheDuration":-1}` + "\n"

func TestRepoStatusDecodesEveryField(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, repoStatusReleaseJSON)

	status, err := fake.client(t).RepoStatus(context.Background())
	if err != nil {
		t.Fatalf("RepoStatus() error = %v", err)
	}
	want := RepoStatus{
		Connected:                  true,
		ConfigFile:                 "/app/config/repository.config",
		FormatVersion:              3,
		Hash:                       "BLAKE2B-256-128",
		Encryption:                 "AES256-GCM-HMAC-SHA256",
		ECC:                        "REED-SOLOMON-CRC32",
		ECCOverheadPercent:         2,
		Splitter:                   "DYNAMIC-4M-BUZHASH",
		MaxPackSize:                20971520,
		Storage:                    "filesystem",
		SupportsContentCompression: true,
		Hostname:                   "kopia",
		Username:                   "root",
		Description:                "Repository in Filesystem: /repository",
		FormatBlobCacheDuration:    15 * time.Minute,
	}
	if status.ThrottlingLimits == nil || *status.ThrottlingLimits != (ThrottleLimits{UploadBytesPerSecond: 1048576}) {
		t.Errorf("ThrottlingLimits = %+v, want the upload limit", status.ThrottlingLimits)
	}
	status.ThrottlingLimits = nil
	if *status != want {
		t.Errorf("RepoStatus() = %+v, want %+v", *status, want)
	}
}

// Kopia sends a time.Duration as a count of nanoseconds; master's compact
// encoding decodes the same way, and -1 is the "no cache" marker.
func TestRepoStatusDecodesTheCompactEncodingOfMaster(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, repoStatusMasterJSON)

	status, err := fake.client(t).RepoStatus(context.Background())
	if err != nil {
		t.Fatalf("RepoStatus() error = %v", err)
	}
	if !status.ReadOnly || !status.EnableActions || status.FormatBlobCacheDuration != -time.Nanosecond ||
		status.MaxPackSize != 33554432 || status.Storage != "s3" || status.ECC != "" || status.ThrottlingLimits != nil {
		t.Errorf("RepoStatus() = %+v", *status)
	}
}

// Without a repository Kopia still answers 200, with only the fields that
// have no omitempty, and the task opening it while one runs.
func TestRepoStatusWithoutARepository(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, "{\n  \"connected\": false,\n  \"supportsContentCompression\": false,\n"+
		"  \"hostname\": \"\",\n  \"username\": \"\",\n  \"enableActions\": false,\n  \"initTaskID\": \"1a\"\n}\n")

	status, err := fake.client(t).RepoStatus(context.Background())
	if err != nil {
		t.Fatalf("RepoStatus() error = %v", err)
	}
	if *status != (RepoStatus{InitTaskID: "1a"}) {
		t.Errorf("RepoStatus() = %+v, want only the task id", *status)
	}
}

func TestAlgorithms(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/algorithms", http.StatusOK, `{
  "defaultHash": "BLAKE2B-256-128",
  "defaultEncryption": "AES256-GCM-HMAC-SHA256",
  "defaultEcc": "REED-SOLOMON-CRC32",
  "defaultSplitter": "DYNAMIC-4M-BUZHASH",
  "hash": [{"id": "BLAKE2B-256", "deprecated": false}, {"id": "BLAKE2B-256-128", "deprecated": false}],
  "encryption": [{"id": "AES256-GCM-HMAC-SHA256", "deprecated": false}],
  "ecc": [{"id": "REED-SOLOMON-CRC32", "deprecated": false}],
  "splitter": [{"id": "DYNAMIC", "deprecated": false}, {"id": "FIXED-4M", "deprecated": false}],
  "compression": [{"id": "zstd", "deprecated": false}, {"id": "lz4", "deprecated": true}]
}
`)

	algorithms, err := fake.client(t).Algorithms(context.Background())
	if err != nil {
		t.Fatalf("Algorithms() error = %v", err)
	}
	if algorithms.DefaultHash != "BLAKE2B-256-128" || algorithms.DefaultEncryption != "AES256-GCM-HMAC-SHA256" ||
		algorithms.DefaultECC != "REED-SOLOMON-CRC32" || algorithms.DefaultSplitter != "DYNAMIC-4M-BUZHASH" {
		t.Errorf("defaults = %+v", *algorithms)
	}
	if len(algorithms.Hash) != 2 || len(algorithms.Encryption) != 1 || len(algorithms.ECC) != 1 || len(algorithms.Splitter) != 2 {
		t.Errorf("lists = %+v", *algorithms)
	}
	if got := algorithms.Compression; len(got) != 2 || got[0] != (AlgorithmInfo{ID: "zstd"}) || got[1] != (AlgorithmInfo{ID: "lz4", Deprecated: true}) {
		t.Errorf("compression = %+v, want zstd, then the deprecated lz4", got)
	}
}

func TestCurrentUser(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/current-user", http.StatusOK, "{\n  \"username\": \"root\",\n  \"hostname\": \"kopia\"\n}\n")

	user, err := fake.client(t).CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser() error = %v", err)
	}
	if *user != (CurrentUser{Username: "root", Hostname: "kopia"}) {
		t.Errorf("CurrentUser() = %+v", *user)
	}
}

func TestGetThrottleDecodesEveryLimit(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/throttle", http.StatusOK, `{
  "readsPerSecond": 10,
  "writesPerSecond": 5.5,
  "listsPerSecond": 1,
  "maxUploadSpeedBytesPerSecond": 1048576,
  "maxDownloadSpeedBytesPerSecond": 2e+21,
  "concurrentReads": 8,
  "concurrentWrites": 4
}
`)

	limits, err := fake.client(t).GetThrottle(context.Background())
	if err != nil {
		t.Fatalf("GetThrottle() error = %v", err)
	}
	want := ThrottleLimits{
		ReadsPerSecond: 10, WritesPerSecond: 5.5, ListsPerSecond: 1,
		UploadBytesPerSecond: 1048576, DownloadBytesPerSecond: 2e21,
		ConcurrentReads: 8, ConcurrentWrites: 4,
	}
	if *limits != want {
		t.Errorf("GetThrottle() = %+v, want %+v", *limits, want)
	}
}

// Kopia leaves unlimited (zero) limits out, so no limits at all is {}.
func TestGetThrottleOfAnUnlimitedServer(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/throttle", http.StatusOK, "{}\n")

	limits, err := fake.client(t).GetThrottle(context.Background())
	if err != nil {
		t.Fatalf("GetThrottle() error = %v", err)
	}
	if *limits != (ThrottleLimits{}) {
		t.Errorf("GetThrottle() = %+v, want no limits", *limits)
	}
}

// The body mirrors throttling.Limits: zero limits are left out, so a PUT of
// no limits is exactly {}, the request that removes them all.
func TestSetThrottleSendsTheLimitsAsKopiaEncodesThem(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("PUT /api/v1/repo/throttle", http.StatusOK, "{}\n")
	c := fake.client(t)

	if err := c.SetThrottle(context.Background(), ThrottleLimits{UploadBytesPerSecond: 10485760, ConcurrentReads: 8}); err != nil {
		t.Fatalf("SetThrottle() error = %v", err)
	}
	if got := string(fake.lastCall(t, "PUT /api/v1/repo/throttle").body); got != `{"maxUploadSpeedBytesPerSecond":10485760,"concurrentReads":8}` {
		t.Errorf("body = %s", got)
	}

	if err := c.SetThrottle(context.Background(), ThrottleLimits{}); err != nil {
		t.Fatalf("SetThrottle() error = %v", err)
	}
	if got := string(fake.lastCall(t, "PUT /api/v1/repo/throttle").body); got != "{}" {
		t.Errorf("body for no limits = %s, want {}", got)
	}
}

// PUT /api/v1/repo/throttle reports both a refused limit and a config file it
// could not write as 400 MALFORMED_REQUEST; only the text tells a request
// that changed nothing from one whose limits are in effect but not saved.
func TestSetThrottleClassifiesItsFailures(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		code         string
		message      string
		rejected     bool
		notPersisted bool
	}{
		{
			name: "negative rate", status: 400, code: CodeMalformedRequest,
			message:  "unable to set limits: UploadBytesPerSecond: limit cannot be negative",
			rejected: true,
		},
		{
			name: "negative concurrency", status: 400, code: CodeMalformedRequest,
			message:  "unable to set limits: ConcurrentWrites: invalid limit",
			rejected: true,
		},
		{
			name: "undecodable body", status: 400, code: CodeMalformedRequest,
			message:  "unable to decode request: json: cannot unmarshal number 2.5 into Go struct field Limits.concurrentReads of type int",
			rejected: true,
		},
		{
			name: "unwritable config file", status: 400, code: CodeMalformedRequest,
			message:      "unable to set limits: error writing file: open /app/config/repository.config.tmp: read-only file system",
			notPersisted: true,
		},
		{
			name: "permissive cache loading", status: 400, code: CodeMalformedRequest,
			message:      "unable to set limits: must have set KOPIA_UPGRADE_LOCK_ENABLED when connecting to repository with permissive cache loading",
			notPersisted: true,
		},
		{
			name: "not connected", status: 400, code: CodeNotConnected, message: "not connected",
		},
		{
			name: "remote repository server", status: 400, code: CodeStorageConnection, message: "no direct storage connection",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.replyError("PUT /api/v1/repo/throttle", tc.status, tc.code, tc.message)

			err := fake.client(t).SetThrottle(context.Background(), ThrottleLimits{ReadsPerSecond: 1})
			if err == nil {
				t.Fatal("SetThrottle() succeeded")
			}
			if errors.Is(err, ErrThrottleRejected) != tc.rejected || errors.Is(err, ErrThrottleNotPersisted) != tc.notPersisted {
				t.Errorf("SetThrottle() error = %v; rejected = %v, not persisted = %v, want %v and %v",
					err, errors.Is(err, ErrThrottleRejected), errors.Is(err, ErrThrottleNotPersisted), tc.rejected, tc.notPersisted)
			}
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Errorf("SetThrottle() error = %v, want the *Error with code %s inside", err, tc.code)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("Error() = %q, want Kopia's message in it", err)
			}
			if IsStorageConnection(err) != (tc.code == CodeStorageConnection) || IsNotConnected(err) != (tc.code == CodeNotConnected) {
				t.Errorf("IsStorageConnection = %v, IsNotConnected = %v for %s", IsStorageConnection(err), IsNotConnected(err), tc.code)
			}
		})
	}
}

func TestGetThrottleOfARemoteRepositoryServer(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/repo/throttle", http.StatusBadRequest, CodeStorageConnection, "no direct storage connection")

	_, err := fake.client(t).GetThrottle(context.Background())
	if !IsStorageConnection(err) {
		t.Fatalf("GetThrottle() error = %v, want STORAGE_CONNECTION", err)
	}
}

// POST /api/v1/repo/description takes a ClientOptions object of which only the
// description counts, and answers with the whole repository status.
func TestSetDescription(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("POST /api/v1/repo/description", http.StatusOK,
		strings.Replace(repoStatusReleaseJSON, "Repository in Filesystem: /repository", "Production backups", 1))

	status, err := fake.client(t).SetDescription(context.Background(), "Production backups")
	if err != nil {
		t.Fatalf("SetDescription() error = %v", err)
	}
	if status.Description != "Production backups" || !status.Connected {
		t.Errorf("SetDescription() = %+v, want the status with the new description", *status)
	}
	if got := string(fake.lastCall(t, "POST /api/v1/repo/description").body); got != `{"description":"Production backups"}` {
		t.Errorf("body = %s", got)
	}
}

func TestSetDescriptionReportsKopiasError(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("POST /api/v1/repo/description", http.StatusInternalServerError, CodeInternal,
		"internal server error: error writing file: permission denied")

	_, err := fake.client(t).SetDescription(context.Background(), "x")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeInternal || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("SetDescription() error = %v, want Kopia's INTERNAL error", err)
	}
}
