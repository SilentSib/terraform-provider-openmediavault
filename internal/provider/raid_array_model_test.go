package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func TestResourceSchema(t *testing.T) {
	schema := ResourceSchema()

	// Test basic schema structure and required fields
	if _, ok := schema["name"]; !ok {
		t.Errorf("Schema missing 'name' field")
	}
	if _, ok := schema["devicefile"]; !ok {
		t.Errorf("Schema missing 'devicefile' field")
	}
	if _, ok := schema["level"]; !ok {
		t.Errorf("Schema missing 'level' field")
	}
	if _, ok := schema["numdevices"]; !ok {
		t.Errorf("Schema missing 'numdevices' field")
	}
	if _, ok := schema["devices"]; !ok {
		t.Errorf("Schema missing 'devices' field")
	}

	// Test specific field constraints
	nameSchema := schema["name"]
	if nameSchema.Type != schema.TypeString || !nameSchema.Required {
		t.Errorf("'name' field has incorrect type or required status")
	}

	levelSchema := schema["level"]
	if levelSchema.Type != schema.TypeString || !levelSchema.Required {
		t.Errorf("'level' field has incorrect type or required status")
	}
	expectedLevels := []string{"stripe", "mirror", "raid0", "raid1", "raid10", "raid5", "raid6"}
	if len(levelSchema.Enum) != len(expectedLevels) {
		t.Errorf("'level' field enum count mismatch. Expected %d, got %d", len(expectedLevels), len(levelSchema.Enum))
	}

	numDevicesSchema := schema["numdevices"]
	if numDevicesSchema.Type != schema.TypeInt || !numDevicesSchema.Required {
		t.Errorf("'numdevices' field has incorrect type or required status")
	}

	devicesSchema := schema["devices"]
	if devicesSchema.Type != schema.TypeList || !devicesSchema.Required {
		t.Errorf("'devices' field has incorrect type or required status")
	}

	// Test element schema for 'devices'
	if devicesSchema.Elem == nil {
		t.Fatalf("'devices' field element schema is nil")
	}
	elementSchema := devicesSchema.Elem
	if elementSchema.Type != schema.TypeString || !elementSchema.Required {
		t.Errorf("Devices element schema has incorrect type or required status")
	}
}
