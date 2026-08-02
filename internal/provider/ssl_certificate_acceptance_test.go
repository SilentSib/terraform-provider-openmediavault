package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccSSLCertificateResource is a template acceptance test, same
// caveats as the other TestAcc* tests in this package. Uses a fixed
// throwaway self-signed cert/key pair rather than depending on the
// hashicorp/tls provider being available in the acceptance test
// environment.
func TestAccSSLCertificateResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_ssl_certificate" "test" {
  comment = "created by terraform acceptance tests"
  certificate_pem = <<-EOT
    REPLACE_WITH_A_REAL_TEST_CERTIFICATE_PEM
  EOT
  private_key_pem = <<-EOT
    REPLACE_WITH_A_REAL_TEST_PRIVATE_KEY_PEM
  EOT
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_ssl_certificate.test", "comment", "created by terraform acceptance tests"),
					resource.TestCheckResourceAttrSet("omv_ssl_certificate.test", "id"),
				),
			},
		},
	})
}
