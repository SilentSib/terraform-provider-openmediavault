package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccSSHCertificateResource is a template acceptance test, same
// caveats as the other TestAcc* tests in this package. Uses a fixed
// throwaway key pair rather than depending on the hashicorp/tls provider
// being available in the acceptance test environment.
func TestAccSSHCertificateResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_ssh_certificate" "test" {
  comment = "created by terraform acceptance tests"
  public_key_openssh = "REPLACE_WITH_A_REAL_TEST_SSH_PUBLIC_KEY"
  private_key_pem = <<-EOT
    REPLACE_WITH_A_REAL_TEST_OPENSSH_OR_RSA_PRIVATE_KEY_PEM
  EOT
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_ssh_certificate.test", "comment", "created by terraform acceptance tests"),
					resource.TestCheckResourceAttrSet("omv_ssh_certificate.test", "id"),
				),
			},
		},
	})
}
