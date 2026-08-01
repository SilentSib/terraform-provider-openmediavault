package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccSharedFolderResource is a template acceptance test: it only runs
// against a real OMV instance when TF_ACC=1 and the OMV_HOST/OMV_USERNAME/
// OMV_PASSWORD environment variables are set (see provider_test.go). The
// RPC service/method/field names it exercises were verified against the
// OMV 8.5.5 source (see the doc comment on SharedFolderResource), but it
// should still be expanded (update-in-place coverage, import coverage,
// error cases) before relying on it as full regression coverage.
func TestAccSharedFolderResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_shared_folder" "test" {
  name           = "tf-acc-test"
  mount_point_id = "REPLACE_WITH_A_REAL_MOUNT_POINT_UUID"
  relative_path  = "tf-acc-test/"
  comment        = "created by terraform acceptance tests"
  mode           = "755"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_shared_folder.test", "name", "tf-acc-test"),
					resource.TestCheckResourceAttr("omv_shared_folder.test", "mode", "755"),
					resource.TestCheckResourceAttrSet("omv_shared_folder.test", "id"),
				),
			},
		},
	})
}
