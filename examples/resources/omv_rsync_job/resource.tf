# Local job: mirror one shared folder into another, nightly at 02:00.
resource "omv_rsync_job" "nightly_local_mirror" {
  type                  = "local"
  enabled               = true
  comment               = "Mirror media onto backup volume"
  src_shared_folder_id  = "fs-uuid-of-the-source-shared-folder"
  dest_shared_folder_id = "fs-uuid-of-the-destination-shared-folder"

  hour   = ["2"]
  minute = ["0"]
  # day_of_month, month, and day_of_week default to ["*"]

  option_archive   = true
  option_delete    = true
  option_compress  = false
}

# Remote job: push a shared folder to an off-site rsync server over SSH,
# authenticating with a pre-existing SSH certificate/key object rather
# than a password.
resource "omv_rsync_job" "offsite_push" {
  type    = "remote"
  mode    = "push"
  enabled = true
  comment = "Push backups off-site"

  src_shared_folder_id = "fs-uuid-of-the-source-shared-folder"
  dest_uri              = "backup-user@offsite.example.com:/srv/backups/media"

  authentication     = "pubkey"
  ssh_certificate_id = "cert-uuid-of-the-ssh-key"
  ssh_port           = 22

  # Weekly, Sunday at 03:30.
  minute       = ["30"]
  hour         = ["3"]
  day_of_week  = ["7"]

  option_archive  = true
  option_compress = true
  option_delete   = true
}
