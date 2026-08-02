package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccWorkbenchSettingsResource is a template acceptance test, same
// caveats as the other TestAcc* tests in this package: only runs with
// TF_ACC=1 and OMV_HOST/OMV_USERNAME/OMV_PASSWORD set.
//
// CAUTION: unlike the other resources' acceptance tests, this one
// modifies OMV's own web UI settings on a real instance. Keep enable_ssl
// disabled and the port at 80 in this test to avoid locking the test
// runner's own connection out mid-run -- see the resource's operational
// warning in its Description.
func TestAccWorkbenchSettingsResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_workbench_settings" "test" {
  port                = 80
  auto_logout_minutes = 10
  enable_ssl          = false
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_workbench_settings.test", "port", "80"),
					resource.TestCheckResourceAttr("omv_workbench_settings.test", "auto_logout_minutes", "10"),
					resource.TestCheckResourceAttr("omv_workbench_settings.test", "id", "settings"),
				),
			},
		},
	})
}
