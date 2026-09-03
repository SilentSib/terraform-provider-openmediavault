package provider

import (
	"fmt"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

// Schema returns the schema for the omv_raid_array resource.
func (r *Resource) Schema() schema.Schema {
	return schema.Schema{
		Fields: ResourceSchema(),
	}
}

// Create creates the RAID array in the OMV backend.
func (r *Resource) Create(d *resource.InstanceState, m interface{}) (*resource.InstanceState, error) {
	var model RAIDResourceModel
	if err := mapToModel(m, &model); err != nil {
		return nil, fmt.Errorf("failed to map desired state to model: %w", err)
	}

	// Call the OMV backend RPC to create the RAID array.
	// Assume r.client.CallOmvMkraidRPC handles the necessary OMV communication.
	createdID, err := r.client.CallOmvMkraidRPC(model)
	if err != nil {
		return nil, fmt.Errorf("failed to create RAID array %s: %w", model.Name, err)
	}

	// Update the state to reflect the newly created resource ID.
	d.SetId(createdID)
	return d, nil
}

// Update updates the resource in the backend.
func (r *Resource) Update(d *resource.InstanceState, m interface{}) (*resource.InstanceState, error) {
	var desiredModel RAIDResourceModel
	// Assuming mapToModel conversion is handled correctly for update.
	if err := mapToModel(m, &desiredModel); err != nil {
		return nil, fmt.Errorf("failed to map desired state to model: %w", err)
	}

	// Check for changes between desired state and current state.
	currentState := d.GetModel().(RAIDResourceModel)
	if !areRAIDModelsEqual(currentState, desiredModel) {
		// Construct the OMV command/RPC payload for modification.
		// The implementation must ensure the update is idempotent or handles state transitions.
		err := r.client.CallOmvUpdateRaidRPC(currentState, desiredModel)
		if err != nil {
			return nil, fmt.Errorf("failed to update RAID array %s: %w", desiredModel.Name, err)
		}
	}
	// State is implicitly updated if the operation succeeds.
	return d, nil
}

// Delete removes the resource from the backend.
func (r *Resource) Delete(d *resource.InstanceState, m interface{}) (*resource.InstanceState, error) {
	resourceID := d.Id()
	if resourceID == "" {
		return nil, fmt.Errorf("cannot delete RAID array: resource ID is missing")
	}

	// Call OMV CLI/API to remove the RAID array using the resource ID.
	err := r.client.CallOmvRmraidRPC(resourceID)
	if err != nil {
		return nil, fmt.Errorf("failed to delete RAID array %s: %w", resourceID, err)
	}

	// State is reset upon successful deletion.
	return d, nil
}
