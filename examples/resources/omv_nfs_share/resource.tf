resource "omv_shared_folder" "media" {
  name           = "media"
  mount_point_id = "fs-uuid-of-the-target-filesystem"
  relative_path  = "media/" # must not contain spaces -- OMV rejects NFS exports of paths that do
}

# Read-only for the whole LAN...
resource "omv_nfs_share" "media_lan" {
  shared_folder_id = omv_shared_folder.media.id
  client            = "192.168.1.0/24"
  options           = "ro"
}

# ...and read/write for one trusted host. NFS shares are many-to-one: both
# of these reference the same shared folder, since each "share" here is
# really just one (client, options) export rule -- OMV places no
# uniqueness constraint on shared_folder_id (unlike omv_smb_share).
resource "omv_nfs_share" "media_trusted_host" {
  shared_folder_id = omv_shared_folder.media.id
  client            = "192.168.1.50"
  options           = "rw"
  extra_options     = "no_root_squash,subtree_check,insecure"
}
