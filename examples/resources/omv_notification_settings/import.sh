# omv_notification_settings is a singleton -- there is exactly one per
# OMV instance, and it must be imported using the fixed literal id
# "settings".
terraform import omv_notification_settings.this settings
