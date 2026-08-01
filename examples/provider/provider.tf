terraform {
  required_providers {
    openmediavault = {
      source  = "example/openmediavault" # update once published to a registry
      version = "~> 0.1"
    }
  }
}

provider "openmediavault" {
  host = "nas.example.lan"
  # port                  = 443
  # scheme                = "https"
  # username              = "admin"        # or set OMV_USERNAME
  # password              = "changeme"     # or set OMV_PASSWORD (recommended)
  # insecure_skip_verify  = true           # OMV ships a self-signed cert by default
}
