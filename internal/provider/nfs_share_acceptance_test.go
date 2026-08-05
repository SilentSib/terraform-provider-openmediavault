package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccNFSShareResource is a template acceptance test, same caveats as
// the other TestAcc* tests in this package: needs a real shared folder
// UUID (with a space-free relative path -- see this resource's docs)
// substituted in before it will pass.
func TestAccNFSShareResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_nfs_share" "test" {
  shared_folder_id = "REPLACE_WITH_A_REAL_SHARED_FOLDER_UUID"
  client            = "127.0.0.1"
  options           = "ro"
  comment           = "created by terraform acceptance tests"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_nfs_share.test", "options", "ro"),
					resource.TestCheckResourceAttrSet("omv_nfs_share.test", "id"),
					resource.TestCheckResourceAttrSet("omv_nfs_share.test", "mount_entry_id"),
				),
			},
		},
	})
}
