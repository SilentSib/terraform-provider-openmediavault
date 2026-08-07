# There is exactly one of these per OMV instance -- see this resource's
# description for why "id" is a fixed literal rather than an OMV-assigned
# UUID.
resource "omv_notification_settings" "this" {
  enabled     = true
  smtp_server = "smtp.example.com"
  smtp_port   = 587
  encryption_mode = "starttls"

  sender_email  = "nas@example.com"
  primary_email = "admin@example.com"

  auth_enabled  = true
  smtp_username = "nas@example.com"
  smtp_password = var.smtp_password # keep secrets out of your .tf files
}
