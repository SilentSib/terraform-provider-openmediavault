package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccRsyncJobResource is a template acceptance test, same caveats as
// TestAccSharedFolderResource: only runs with TF_ACC=1 and OMV_HOST/
// OMV_USERNAME/OMV_PASSWORD set, and needs a real shared folder UUID
// substituted in below before it will pass.
func TestAccRsyncJobResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_rsync_job" "test" {
  type                   = "local"
  enabled                = false
  comment                = "created by terraform acceptance tests"
  src_shared_folder_id   = "REPLACE_WITH_A_REAL_SHARED_FOLDER_UUID"
  dest_shared_folder_id  = "REPLACE_WITH_ANOTHER_REAL_SHARED_FOLDER_UUID"
  minute                 = ["0"]
  hour                   = ["3"]
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_rsync_job.test", "type", "local"),
					resource.TestCheckResourceAttr("omv_rsync_job.test", "enabled", "false"),
					resource.TestCheckResourceAttrSet("omv_rsync_job.test", "id"),
				),
			},
		},
	})
}
