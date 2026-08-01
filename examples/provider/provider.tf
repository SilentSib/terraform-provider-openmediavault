terraform {
  required_providers {
    # This key ("omv") must match the prefix of this provider's resource
    # types (omv_shared_folder, omv_rsync_job, ...) -- Terraform uses it,
    # not the "source" address below, to decide which provider a
    # "resource \"omv_...\"" block belongs to. See README.md's "Using the
    # provider" section for what goes wrong if these don't match.
    omv = {
      source  = "example/openmediavault" # update once published to a registry
      version = "~> 0.1"
    }
  }
}

provider "omv" {
  host = "nas.example.lan"
  # port                  = 443
  # scheme                = "https"
  # username              = "admin"        # or set OMV_USERNAME
  # password              = "changeme"     # or set OMV_PASSWORD (recommended)
  # insecure_skip_verify  = true           # OMV ships a self-signed cert by default
}
