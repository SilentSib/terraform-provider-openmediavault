resource "omv_shared_folder" "media" {
  name           = "media"
  mount_point_id = "fs-uuid-of-the-target-filesystem"
  relative_path  = "media/"
  comment        = "Managed by Terraform"
  # mode defaults to "775" (OMV's own default); only takes effect the
  # first time the shared folder's directory is created.
  # mode = "755"
}
