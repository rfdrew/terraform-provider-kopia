package provider

import (
	"net/http"
	"os"
	"testing"

	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/rfdrew/terraform-provider-kopia/internal/kopiaclient"
)

// repositoryStatusECCJSON is GET /api/v1/repo/status from Kopia 0.23.1 for a
// repository with error correction on and throttle limits in the config file
// at start-up, indented as the releases write it.
const repositoryStatusECCJSON = `{
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

func TestRepositoryStatusDataSourceSchemaIsValid(t *testing.T) {
	requireValidDataSourceSchema(t, &repositoryStatusDataSource{})
}

func TestRepositoryStatusDataSourceReadsTheReleaseShape(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, repositoryStatusECCJSON)
	d := &repositoryStatusDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	want := dataSourceValue(t, d, map[string]any{
		"connected":                    true,
		"config_file":                  "/app/config/repository.config",
		"format_version":               3,
		"hash":                         "BLAKE2B-256-128",
		"encryption":                   "AES256-GCM-HMAC-SHA256",
		"ecc":                          "REED-SOLOMON-CRC32",
		"ecc_overhead_percent":         2,
		"splitter":                     "DYNAMIC-4M-BUZHASH",
		"max_pack_size":                20971520,
		"storage":                      "filesystem",
		"supports_content_compression": true,
		"hostname":                     "kopia",
		"username":                     "root",
		"description":                  "Repository in Filesystem: /repository",
		"readonly":                     false,
		"enable_actions":               false,
		"format_blob_cache_duration":   "15m0s",
	})
	if !resp.State.Raw.Equal(want) {
		diffs, _ := resp.State.Raw.Diff(want)
		t.Errorf("state differs: %v", diffs)
	}
}

// master writes compact JSON. This connection is read-only, has actions
// enabled and was made with --disable-repository-format-cache, which Kopia
// reports as -1 nanoseconds. Error correction is off, so the overhead is 0
// even though Kopia leaves it out.
func TestRepositoryStatusDataSourceReadsTheCompactShapeOfMaster(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, `{"connected":true,"configFile":"/app/config/repository.config",`+
		`"formatVersion":2,"hash":"BLAKE3-256","encryption":"CHACHA20-POLY1305-HMAC-SHA256","splitter":"FIXED-4M",`+
		`"maxPackSize":33554432,"storage":"s3","supportsContentCompression":false,"hostname":"nas","username":"backup",`+
		`"readonly":true,"description":"Offsite","enableActions":true,"formatBlobCacheDuration":-1}`+"\n")
	d := &repositoryStatusDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	state := resp.State.Raw
	if !boolAt(t, state, "readonly") || !boolAt(t, state, "enable_actions") || boolAt(t, state, "supports_content_compression") {
		t.Errorf("flags = %s", state)
	}
	if got := stringAt(t, state, "format_blob_cache_duration"); got != "-1ns" {
		t.Errorf("format_blob_cache_duration = %q, want -1ns", got)
	}
	if !isNullAt(t, state, "ecc") || int64At(t, state, "ecc_overhead_percent") != 0 {
		t.Errorf("ecc = %s, overhead = %s; want null and 0", valueAt(t, state, "ecc"), valueAt(t, state, "ecc_overhead_percent"))
	}
	if got := int64At(t, state, "max_pack_size"); got != 33554432 {
		t.Errorf("max_pack_size = %d", got)
	}
	if !isNullAt(t, state, "api_server_url") {
		t.Errorf("api_server_url = %s, want null for a direct connection", valueAt(t, state, "api_server_url"))
	}
}

// Kopia itself decides that error correction is off when the overhead is 0;
// a repository made by an old release may still name an algorithm.
func TestRepositoryStatusDataSourceReportsNoECCWithoutOverhead(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, `{"connected":true,"hash":"BLAKE2B-256-128","ecc":"REED-SOLOMON-CRC32",`+
		`"storage":"filesystem","supportsContentCompression":true,"hostname":"kopia","username":"root","enableActions":false}`)
	d := &repositoryStatusDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	if !isNullAt(t, resp.State.Raw, "ecc") || int64At(t, resp.State.Raw, "ecc_overhead_percent") != 0 {
		t.Errorf("state = %s, want no ECC", resp.State.Raw)
	}
}

// Without a repository Kopia sends zero values for the fields without
// omitempty; they describe nothing, and read as null.
func TestRepositoryStatusDataSourceWithoutARepository(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, "{\n  \"connected\": false,\n  \"supportsContentCompression\": false,\n"+
		"  \"hostname\": \"\",\n  \"username\": \"\",\n  \"enableActions\": false,\n  \"initTaskID\": \"1\"\n}\n")
	d := &repositoryStatusDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	if want := dataSourceValue(t, d, map[string]any{"connected": false}); !resp.State.Raw.Equal(want) {
		t.Errorf("state = %s, want only connected = false", resp.State.Raw)
	}
}

// Through a Kopia repository server, Kopia reports the address of that server
// and none of the repository's format or storage.
func TestRepositoryStatusDataSourceThroughARepositoryServer(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/status", http.StatusOK, "{\n  \"connected\": true,\n  \"apiServerURL\": \"https://repo.example.com:51515\",\n"+
		"  \"supportsContentCompression\": true,\n  \"hostname\": \"laptop\",\n  \"username\": \"alice\",\n  \"enableActions\": false\n}\n")
	d := &repositoryStatusDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	want := dataSourceValue(t, d, map[string]any{
		"connected":                    true,
		"api_server_url":               "https://repo.example.com:51515",
		"supports_content_compression": true,
		"hostname":                     "laptop",
		"username":                     "alice",
		"readonly":                     false,
		"enable_actions":               false,
	})
	if !resp.State.Raw.Equal(want) {
		diffs, _ := resp.State.Raw.Diff(want)
		t.Errorf("state differs: %v", diffs)
	}
}

func TestRepositoryStatusDataSourceReportsErrors(t *testing.T) {
	fake := newFakeKopia()
	fake.replyError("GET /api/v1/repo/status", http.StatusForbidden, kopiaclient.CodeAccessDenied, "access is denied")
	d := &repositoryStatusDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireErrorContaining(t, resp.Diagnostics, "Unable to read Kopia repository status")
	requireErrorContaining(t, resp.Diagnostics, "access is denied")
}

// --- acceptance --------------------------------------------------------------

func TestAccRepositoryStatusDataSource(t *testing.T) {
	checks := []acc.TestCheckFunc{
		acc.TestCheckResourceAttr("data.kopia_repository_status.this", "connected", "true"),
		acc.TestCheckResourceAttr("data.kopia_repository_status.this", "storage", "filesystem"),
		acc.TestCheckResourceAttr("data.kopia_repository_status.this", "format_version", "3"),
		acc.TestCheckResourceAttrSet("data.kopia_repository_status.this", "hash"),
		acc.TestCheckResourceAttrSet("data.kopia_repository_status.this", "encryption"),
		acc.TestCheckResourceAttrSet("data.kopia_repository_status.this", "splitter"),
		acc.TestCheckResourceAttrSet("data.kopia_repository_status.this", "config_file"),
		acc.TestCheckResourceAttr("data.kopia_repository_status.this", "ecc_overhead_percent", "0"),
		acc.TestCheckNoResourceAttr("data.kopia_repository_status.this", "ecc"),
		acc.TestCheckNoResourceAttr("data.kopia_repository_status.this", "api_server_url"),
		acc.TestCheckResourceAttr("data.kopia_repository_status.this", "readonly", "false"),
		acc.TestCheckResourceAttr("data.kopia_repository_status.this", "format_blob_cache_duration", "15m0s"),
	}
	// The connection identity the bootstrap script creates the repository
	// with, when the tests run against its server.
	if host := os.Getenv(envAccSourceHost); host != "" {
		checks = append(checks, acc.TestCheckResourceAttr("data.kopia_repository_status.this", "hostname", host))
	}
	if user := os.Getenv(envAccSourceUser); user != "" {
		checks = append(checks, acc.TestCheckResourceAttr("data.kopia_repository_status.this", "username", user))
	}

	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
data "kopia_repository_status" "this" {}
`,
				Check: acc.ComposeAggregateTestCheckFunc(checks...),
			},
		},
	})
}
