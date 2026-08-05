# Import an existing NFS export rule using its OMV-assigned UUID.
# mount_entry_id is populated automatically on the next refresh/apply
# (it's entirely OMV-managed, never set in configuration).
terraform import omv_nfs_share.media_lan 5f2f4e9e-1234-4a5b-8abc-1234567890ab
