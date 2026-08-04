resource "omv_shared_folder" "media" {
  name           = "media"
  mount_point_id = "fs-uuid-of-the-target-filesystem"
  relative_path  = "media/"
}

resource "omv_smb_share" "media" {
  enabled          = true
  shared_folder_id = omv_shared_folder.media.id
  comment          = "Media library"

  guest       = "no"
  read_only   = false
  browseable  = true

  recycle_bin                = true
  recycle_bin_max_size_bytes = 1073741824 # 1 GiB; 0 = unrestricted
  recycle_bin_retention_days = 30         # 0 = keep forever (manual deletion only)
}

# A Time Machine backup target, restricted to a subnet, with SMB transport
# encryption required.
resource "omv_shared_folder" "backups" {
  name           = "backups"
  mount_point_id = "fs-uuid-of-the-target-filesystem"
  relative_path  = "backups/"
}

resource "omv_smb_share" "time_machine" {
  enabled          = true
  shared_folder_id = omv_shared_folder.backups.id
  comment          = "Time Machine backups"

  time_machine           = true
  time_machine_max_size  = "500G"
  transport_encryption   = true

  hosts_allow = "192.168.1.0/24"
}
