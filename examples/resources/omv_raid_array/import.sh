#!/bin/bash
# This script is a template for importing the openmediavault_raid_array resource.
# Customize the resource address and arguments below.

RESOURCE_ADDRESS="omv_raid_array.example"
# Example arguments to pass during import (these should match the resource definition)
# ARG1="value1"
# ARG2="value2"

terraform import ${RESOURCE_ADDRESS} "resource_id_from_omv"

# Note: The actual import ID format (e.g., 'resource_id_from_omv') must match how OMV identifies the RAID array.
