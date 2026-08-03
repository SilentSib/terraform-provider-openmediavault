# Bring your own key pair, e.g. generated with the community
# hashicorp/tls provider.
#
# IMPORTANT: OMV only accepts a narrower set of formats than "any valid
# SSH key" -- see omv_ssh_certificate's attribute descriptions. In
# particular, private_key_openssh (NOT private_key_pem) is the attribute
# to use from tls_private_key for non-RSA algorithms, since OMV rejects
# the generic PKCS8 PEM that private_key_pem produces for ed25519/ECDSA
# keys.
resource "tls_private_key" "this" {
  algorithm = "ED25519"
}

resource "omv_ssh_certificate" "this" {
  public_key_openssh = tls_private_key.this.public_key_openssh
  private_key_pem     = tls_private_key.this.private_key_openssh
  comment              = "Managed by Terraform"
}

# Use it, e.g. for an omv_rsync_job's pubkey authentication:
resource "omv_rsync_job" "offsite_backup" {
  type    = "remote"
  mode    = "push"
  enabled = true

  src_shared_folder_id = "fs-uuid-of-the-source-shared-folder"
  dest_uri              = "backup-user@offsite.example.com:/srv/backups"

  authentication     = "pubkey"
  ssh_certificate_id = omv_ssh_certificate.this.id
}
