package provider

import (
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// algorithmsReleaseJSON is GET /api/v1/repo/algorithms from Kopia 0.23.1,
// shortened, indented as the releases write it.
const algorithmsReleaseJSON = `{
  "defaultHash": "BLAKE2B-256-128",
  "defaultEncryption": "AES256-GCM-HMAC-SHA256",
  "defaultEcc": "REED-SOLOMON-CRC32",
  "defaultSplitter": "DYNAMIC-4M-BUZHASH",
  "hash": [
    {
      "id": "BLAKE2B-256",
      "deprecated": false
    },
    {
      "id": "BLAKE2B-256-128",
      "deprecated": false
    }
  ],
  "encryption": [
    {
      "id": "AES256-GCM-HMAC-SHA256",
      "deprecated": false
    },
    {
      "id": "CHACHA20-POLY1305-HMAC-SHA256",
      "deprecated": false
    }
  ],
  "ecc": [
    {
      "id": "REED-SOLOMON-CRC32",
      "deprecated": false
    }
  ],
  "splitter": [
    {
      "id": "DYNAMIC",
      "deprecated": false
    },
    {
      "id": "FIXED-4M",
      "deprecated": false
    }
  ],
  "compression": [
    {
      "id": "zstd",
      "deprecated": false
    },
    {
      "id": "lz4",
      "deprecated": true
    },
    {
      "id": "zstd-best-compression",
      "deprecated": true
    }
  ]
}
`

func TestAlgorithmsDataSourceSchemaIsValid(t *testing.T) {
	requireValidDataSourceSchema(t, &algorithmsDataSource{})
}

func TestAlgorithmsDataSourceReadsTheReleaseShape(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/algorithms", http.StatusOK, algorithmsReleaseJSON)
	d := &algorithmsDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	state := resp.State.Raw

	for attribute, want := range map[string]string{
		"default_hash":       "BLAKE2B-256-128",
		"default_encryption": "AES256-GCM-HMAC-SHA256",
		"default_ecc":        "REED-SOLOMON-CRC32",
		"default_splitter":   "DYNAMIC-4M-BUZHASH",
	} {
		if got := stringAt(t, state, attribute); got != want {
			t.Errorf("%s = %q, want %q", attribute, got, want)
		}
	}
	// Kopia's order is kept: the algorithms that are not deprecated first.
	compression := []struct {
		id         string
		deprecated bool
	}{{"zstd", false}, {"lz4", true}, {"zstd-best-compression", true}}
	for i, want := range compression {
		if got := stringAt(t, state, "compression", i, "id"); got != want.id {
			t.Errorf("compression[%d].id = %q, want %q", i, got, want.id)
		}
		if got := boolAt(t, state, "compression", i, "deprecated"); got != want.deprecated {
			t.Errorf("compression[%d].deprecated = %v, want %v", i, got, want.deprecated)
		}
	}
	if got := stringAt(t, state, "splitter", 1, "id"); got != "FIXED-4M" {
		t.Errorf("splitter[1].id = %q", got)
	}
	if got := stringAt(t, state, "encryption", 1, "id"); got != "CHACHA20-POLY1305-HMAC-SHA256" {
		t.Errorf("encryption[1].id = %q", got)
	}
}

// master writes compact JSON; a list Kopia ever sent as null would read as
// empty.
func TestAlgorithmsDataSourceReadsTheCompactShapeOfMaster(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/repo/algorithms", http.StatusOK, `{"defaultHash":"BLAKE2B-256-128","defaultEncryption":`+
		`"AES256-GCM-HMAC-SHA256","defaultEcc":"REED-SOLOMON-CRC32","defaultSplitter":"DYNAMIC-4M-BUZHASH",`+
		`"hash":[{"id":"BLAKE3-256","deprecated":false}],"encryption":[{"id":"AES256-GCM-HMAC-SHA256","deprecated":false}],`+
		`"ecc":null,"splitter":[{"id":"DYNAMIC-4M-BUZHASH","deprecated":false}],"compression":[{"id":"s2-default","deprecated":false}]}`+"\n")
	d := &algorithmsDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireNoErrors(t, resp.Diagnostics)
	if got := stringAt(t, resp.State.Raw, "hash", 0, "id"); got != "BLAKE3-256" {
		t.Errorf("hash[0].id = %q", got)
	}
	// knownAt fails the test for a null list.
	var ecc []tftypes.Value
	if err := knownAt(t, resp.State.Raw, "ecc").As(&ecc); err != nil || len(ecc) != 0 {
		t.Errorf("ecc = %v (%v), want an empty list", ecc, err)
	}
}

// --- acceptance --------------------------------------------------------------

func TestAccAlgorithmsDataSource(t *testing.T) {
	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
data "kopia_algorithms" "this" {}
`,
				Check: acc.ComposeAggregateTestCheckFunc(
					acc.TestCheckResourceAttr("data.kopia_algorithms.this", "default_hash", "BLAKE2B-256-128"),
					acc.TestCheckResourceAttr("data.kopia_algorithms.this", "default_encryption", "AES256-GCM-HMAC-SHA256"),
					acc.TestCheckResourceAttr("data.kopia_algorithms.this", "default_splitter", "DYNAMIC-4M-BUZHASH"),
					acc.TestCheckTypeSetElemNestedAttrs("data.kopia_algorithms.this", "compression.*",
						map[string]string{"id": "zstd", "deprecated": "false"}),
					acc.TestCheckTypeSetElemNestedAttrs("data.kopia_algorithms.this", "compression.*",
						map[string]string{"id": "lz4", "deprecated": "true"}),
					acc.TestCheckTypeSetElemNestedAttrs("data.kopia_algorithms.this", "splitter.*",
						map[string]string{"id": "FIXED-4M"}),
				),
			},
		},
	})
}
