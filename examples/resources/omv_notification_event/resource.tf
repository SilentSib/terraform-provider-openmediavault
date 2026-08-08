# event_id's valid values depend on which OMV modules/plugins are
# installed -- see this resource's description for the full list verified
# against stock OMV 8.5.5. Requires omv_notification_settings to be
# configured with a working SMTP setup for these to actually deliver
# anything.
resource "omv_notification_event" "smart" {
  event_id = "smartmontools"
  enabled  = true
}

resource "omv_notification_event" "apt_updates" {
  event_id = "apt"
  enabled  = true
}

resource "omv_notification_event" "auth" {
  event_id = "authentication"
  enabled  = true
}
