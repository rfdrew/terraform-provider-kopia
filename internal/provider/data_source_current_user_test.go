package provider

import (
	"net/http"
	"os"
	"testing"

	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestCurrentUserDataSourceSchemaIsValid(t *testing.T) {
	requireValidDataSourceSchema(t, &currentUserDataSource{})
}

// GET /api/v1/current-user in the releases' indented shape and master's
// compact one.
func TestCurrentUserDataSourceReadsTheUser(t *testing.T) {
	for name, body := range map[string]string{
		"release": "{\n  \"username\": \"root\",\n  \"hostname\": \"kopia-acc\"\n}\n",
		"master":  `{"username":"root","hostname":"kopia-acc"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeKopia()
			fake.reply("GET /api/v1/current-user", http.StatusOK, body)
			d := &currentUserDataSource{client: fake.client(t)}

			resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
			requireNoErrors(t, resp.Diagnostics)
			if want := dataSourceValue(t, d, map[string]any{"username": "root", "hostname": "kopia-acc"}); !resp.State.Raw.Equal(want) {
				t.Errorf("state = %s, want %s", resp.State.Raw, want)
			}
		})
	}
}

func TestCurrentUserDataSourceReportsErrors(t *testing.T) {
	fake := newFakeKopia()
	fake.reply("GET /api/v1/current-user", http.StatusOK, `{"username":`)
	d := &currentUserDataSource{client: fake.client(t)}

	resp := readDataSource(t, d, dataSourceValue(t, d, map[string]any{}))
	requireErrorContaining(t, resp.Diagnostics, "Unable to read Kopia current user")
}

// --- acceptance --------------------------------------------------------------

// The official image runs kopia as root, and the bootstrap script gives the
// container the host name it also connects the repository as.
func TestAccCurrentUserDataSource(t *testing.T) {
	checks := []acc.TestCheckFunc{
		acc.TestCheckResourceAttrSet("data.kopia_current_user.this", "username"),
		acc.TestCheckResourceAttrSet("data.kopia_current_user.this", "hostname"),
	}
	if host := os.Getenv(envAccSourceHost); host != "" {
		checks = append(checks, acc.TestCheckResourceAttr("data.kopia_current_user.this", "hostname", host))
	}

	acc.Test(t, acc.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []acc.TestStep{
			{
				Config: providerConfig + `
data "kopia_current_user" "this" {}
`,
				Check: acc.ComposeAggregateTestCheckFunc(checks...),
			},
		},
	})
}
