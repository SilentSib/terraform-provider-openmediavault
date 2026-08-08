package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccNotificationEventResource is a template acceptance test, same
// caveats as the other TestAcc* tests in this package. Uses
// "smartmontools", one of the built-in event IDs verified to exist on
// stock OMV 8.5.5 with no additional plugins.
func TestAccNotificationEventResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "omv_notification_event" "test" {
  event_id = "smartmontools"
  enabled  = true
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omv_notification_event.test", "event_id", "smartmontools"),
					resource.TestCheckResourceAttr("omv_notification_event.test", "enabled", "true"),
					resource.TestCheckResourceAttrSet("omv_notification_event.test", "id"),
				),
			},
		},
	})
}
