package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// These tests look at the provider the way Terraform does, through the
// protocol server of New: what GetProviderSchema and friends serve for every
// registered resource and data source. The framework validates each schema
// only when it serves it, so a mistake in one that no other test registers
// would otherwise surface first in a user's `terraform init`.

// providerTestServer is the protocol server of the provider the binary runs.
func providerTestServer(t *testing.T) tfprotov6.ProviderServer {
	t.Helper()
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// Every resource and data source is registered, and the framework accepts
// every schema: GetProviderSchema runs ValidateImplementation on each one.
func TestProviderServesEveryResourceAndDataSource(t *testing.T) {
	ctx := context.Background()
	server := providerTestServer(t)

	resp, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireNoProtocolErrors(t, resp.Diagnostics)

	wantResources := []string{
		"kopia_notification_profile",
		"kopia_policy",
		"kopia_repository_description",
		"kopia_throttle",
	}
	if got := harnessSortedKeys(resp.ResourceSchemas); !reflect.DeepEqual(got, wantResources) {
		t.Errorf("resources = %v, want %v", got, wantResources)
	}
	wantDataSources := []string{
		"kopia_algorithms",
		"kopia_current_user",
		"kopia_effective_policy",
		"kopia_notification_profiles",
		"kopia_policies",
		"kopia_repository_status",
		"kopia_snapshots",
		"kopia_sources",
	}
	if got := harnessSortedKeys(resp.DataSourceSchemas); !reflect.DeepEqual(got, wantDataSources) {
		t.Errorf("data sources = %v, want %v", got, wantDataSources)
	}

	metadata, err := server.GetMetadata(ctx, &tfprotov6.GetMetadataRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireNoProtocolErrors(t, metadata.Diagnostics)
	if len(metadata.Resources) != len(wantResources) || len(metadata.DataSources) != len(wantDataSources) {
		t.Errorf("GetMetadata lists %d resources and %d data sources, want %d and %d",
			len(metadata.Resources), len(metadata.DataSources), len(wantResources), len(wantDataSources))
	}
}

// Resources with an identity name it by the attribute users import them by;
// the singletons have none, as in homarr.
func TestProviderServesTheResourceIdentities(t *testing.T) {
	resp, err := providerTestServer(t).GetResourceIdentitySchemas(context.Background(),
		&tfprotov6.GetResourceIdentitySchemasRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireNoProtocolErrors(t, resp.Diagnostics)

	want := map[string]string{
		"kopia_policy":               "target",
		"kopia_notification_profile": "name",
	}
	if got := harnessSortedKeys(resp.IdentitySchemas); !reflect.DeepEqual(got, harnessSortedKeys(want)) {
		t.Fatalf("resources with an identity = %v, want %v", got, harnessSortedKeys(want))
	}
	for typeName, attribute := range want {
		attrs := resp.IdentitySchemas[typeName].IdentityAttributes
		if len(attrs) != 1 || attrs[0].Name != attribute || !attrs[0].RequiredForImport || attrs[0].Description == "" {
			t.Errorf("%s identity = %+v, want one described attribute %q, required for import", typeName, attrs, attribute)
		}
	}
}

// Every attribute carries a Markdown description, because the generated
// documentation is made of nothing else.
func TestProviderSchemasDescribeEveryAttribute(t *testing.T) {
	resp, err := providerTestServer(t).GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireNoProtocolErrors(t, resp.Diagnostics)

	check := func(kind, typeName string, s *tfprotov6.Schema) {
		if s.Block.Description == "" || s.Block.DescriptionKind != tfprotov6.StringKindMarkdown {
			t.Errorf("%s %s has no Markdown description", kind, typeName)
		}
		providerTestCheckDescriptions(t, kind+" "+typeName, s.Block.Attributes)
		if len(s.Block.BlockTypes) != 0 {
			t.Errorf("%s %s uses blocks; the provider uses nested attributes only", kind, typeName)
		}
	}
	check("provider", "kopia", resp.Provider)
	for typeName, s := range resp.ResourceSchemas {
		check("resource", typeName, s)
	}
	for typeName, s := range resp.DataSourceSchemas {
		check("data source", typeName, s)
	}
}

// providerTestCheckDescriptions fails for every attribute under attrs, nested
// ones included, that has no Markdown description.
func providerTestCheckDescriptions(t *testing.T, where string, attrs []*tfprotov6.SchemaAttribute) {
	t.Helper()
	for _, a := range attrs {
		if a.Description == "" || a.DescriptionKind != tfprotov6.StringKindMarkdown {
			t.Errorf("%s: attribute %s has no Markdown description", where, a.Name)
		}
		if a.NestedType != nil {
			providerTestCheckDescriptions(t, where+"."+a.Name, a.NestedType.Attributes)
		}
	}
}
