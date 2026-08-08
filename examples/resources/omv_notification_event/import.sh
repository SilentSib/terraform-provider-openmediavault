# Import uses event_id (e.g. "smartmontools"), NOT OMV's internal UUID.
# Only works if OMV already has a persisted object for that event (i.e.
# it's been toggled at least once, via the web UI or a prior apply) --
# see this resource's description for what happens otherwise.
terraform import omv_notification_event.smart smartmontools
