package provider

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// testAccProtoV6ProviderFactories is used to instantiate the provider
// during acceptance tests, which run real Terraform CLI commands against
// this codebase (see https://developer.hashicorp.com/terraform/plugin/framework/acctests).
//
// The map key ("omv") is used by terraform-plugin-testing as the local
// name for the required_providers entry it implicitly wraps each
// resource.TestStep's Config in -- it MUST match the prefix of this
// provider's resource/data source types (see the comment on
// OMVProvider.Metadata in provider.go), or acceptance tests would fail
// with the same "provider not found" class of error a real Terraform
// config with a mismatched required_providers key would.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"omv": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck validates that all required environment variables for
// acceptance tests are set. Acceptance tests talk to a real OMV instance
// and are skipped unless TF_ACC=1 is set (standard Terraform SDK/framework
// convention), so this only needs to validate connection details.
func testAccPreCheck(t *testing.T) {
	t.Helper()
	for _, env := range []string{"OMV_HOST", "OMV_USERNAME", "OMV_PASSWORD"} {
		if os.Getenv(env) == "" {
			t.Fatalf("%s must be set for acceptance tests", env)
		}
	}
}
