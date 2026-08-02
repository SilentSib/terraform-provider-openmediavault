# There is exactly one of these per OMV instance -- see this resource's
# description for why "id" is a fixed literal rather than an OMV-assigned
# UUID.
#
# WARNING: applying this resource changes how OMV's web UI (and this
# provider's own connection) is reached. If you change `port`,
# `enable_ssl`, `ssl_port`, or `force_ssl_only`, you will likely also need
# to update the `omv` provider block's own `port`/`scheme` to match before
# the next `terraform plan`/`apply` can connect.
resource "omv_workbench_settings" "this" {
  port                 = 80
  auto_logout_minutes  = 15

  enable_ssl         = true
  ssl_port           = 443
  force_ssl_only     = false
  ssl_certificate_id = "cert-uuid-of-an-existing-ssl-certificate"
}
