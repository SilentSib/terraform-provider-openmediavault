package provider

import (
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// RAIDResourceModel represents the configuration data model for an OMV RAID array.
type RAIDResourceModel struct {
	Name      string   `json:"name"`
	DeviceFile string   `json:"devicefile"`
	Level     string   `json:"level"`
	NumDevices int      `json:"numdevices"`
	Devices   []string `json:"devices"`
	DirtiedBy []string `json:"dirtied_by"` // OMV engine modules that this resource affects
}

// ResourceSchema defines the schema for the omv_raid_array resource.
func ResourceSchema() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"name": {
			Type:     schema.TypeString,
			Required: true,
			Description: "The name of the RAID array.",
		},
		"devicefile": {
			Type:     schema.TypeString,
			Required: true,
			Description: "The device file path for the RAID array.",
			// In a real provider, this would likely use a custom type or format validator
			// based on the 'devicefile' format from the OMV data model.
		},
		"level": {
			Type:     schema.TypeString,
			Required: true,
			Description: "The RAID level of the array.",
			Enum:     []string{"stripe", "mirror", "raid0", "raid1", "raid10", "raid5", "raid6"},
		},
		"numdevices": {
			Type:     schema.TypeInt,
			Required: true,
			Description: "The number of devices in the array.",
		},
		"devices": {
			Type:     schema.TypeList,
			Required: true,
			Description: "A list of device files included in the RAID array.",
			Elem: &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
			},
		},
	}
}