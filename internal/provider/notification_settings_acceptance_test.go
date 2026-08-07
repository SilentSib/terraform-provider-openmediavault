package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccNotificationSettingsResource is a template acceptance test, same
// caveats as the other TestAcc* tests in this package. Keeps enabled =
// false so it doesn't require a real, working SMTP server to pass.
func TestAccNotificationSettingsResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_notification_settings" "test" {
  enabled = false
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_notification_settings.test", "enabled", "false"),
					resource.TestCheckResourceAttr("omv_notification_settings.test", "id", "settings"),
				),
			},
		},
	})
}
