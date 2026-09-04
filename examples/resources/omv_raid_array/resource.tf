resource "omv_raid_array" "example" {
  name        = "my_raid_array"
  devices     = ["disk1", "disk2", "disk3"]
  raid_level  = "raid5"
  description = "A test RAID array for OMV"
}