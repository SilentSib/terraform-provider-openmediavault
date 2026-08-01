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
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"openmediavault": providerserver.NewProtocol6WithError(New("test")()),
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
