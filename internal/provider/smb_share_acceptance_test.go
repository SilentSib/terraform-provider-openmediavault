package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccSMBShareResource is a template acceptance test, same caveats as
// the other TestAcc* tests in this package: needs a real shared folder
// UUID substituted in before it will pass.
func TestAccSMBShareResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_smb_share" "test" {
  enabled           = false
  shared_folder_id  = "REPLACE_WITH_A_REAL_SHARED_FOLDER_UUID"
  comment           = "created by terraform acceptance tests"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_smb_share.test", "enabled", "false"),
					resource.TestCheckResourceAttr("omv_smb_share.test", "guest", "no"),
					resource.TestCheckResourceAttrSet("omv_smb_share.test", "id"),
				),
			},
		},
	})
}
