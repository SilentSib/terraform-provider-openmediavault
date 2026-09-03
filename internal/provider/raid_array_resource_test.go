package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestResourceCRUD(t *testing.T) {
	// Setup a mock resource instance for testing
	r := &Resource{}

	// Setup a mock state instance
	mockState := &resource.InstanceState{
		Id: "my-raid-array",
		// Other state fields would be populated here
	}

	// Test Create
	t.Run("Create", func(t *testing.T) {
		// Mock input map[string]interface{} representing desired state
		mockInput := map[string]interface{}{
			"name": "my-raid-array",
			"devicefile": "/dev/sdb",
			"level": "raid1",
			"numdevices": 2,
			"devices": []string{"/dev/sdb", "/dev/sdc"},
		}

		// Execute Create method
		updatedState, err := r.Create(mockState, mockInput)
		if err != nil {
			t.Errorf("Create failed: %v", err)
		}
		if updatedState.Id != "my-raid-array" {
			t.Errorf("Expected ID 'my-raid-array', got '%s'", updatedState.Id)
		}
	})

	// Test Read
	t.Run("Read", func(t *testing.T) {
		// Execute Read method
		updatedState, err := r.Read(mockState, nil)
		if err != nil {
			t.Errorf("Read failed: %v", err)
		}
		if updatedState != mockState {
			t.Errorf("Read did not return the expected state")
		}
	})

	// Test Update
	t.Run("Update", func(t *testing.T) {
		// Mock input map[string]interface{} representing new desired state
		mockInput := map[string]interface{}{
			"name": "my-raid-array",
			"devicefile": "/dev/sdb",
			"level": "raid10", // Changed level
			"numdevices": 4,
			"devices": []string{"/dev/sdb", "/dev/sdc", "/dev/sdd", "/dev/sde"},
		}

		// Execute Update method
		updatedState, err := r.Update(mockState, mockInput)
		if err != nil {
			t.Errorf("Update failed: %v", err)
		}
		// In a real test, we would assert that the state reflects the changes
	})

	// Test Delete
	t.Run("Delete", func(t *testing.T) {
		// Execute Delete method
		updatedState, err := r.Delete(mockState, nil)
		if err != nil {
			t.Errorf("Delete failed: %v", err)
		}
		if updatedState != mockState {
			t.Errorf("Delete did not return the expected state")
		}
	})
}