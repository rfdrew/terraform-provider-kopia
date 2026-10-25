# Kopia's own notifications, about the snapshots each server runs: scheduled
# ones and those the watcher asks for. Profiles live in the repository, so
# every server sends through this one. The watcher's NOTIFY_WEBHOOK_URL covers
# what Kopia cannot see, such as labels it refuses.
resource "kopia_notification_profile" "ops" {
  name         = "ops"
  min_severity = "warning"

  webhook = {
    endpoint = "https://ntfy.example.com/kopia-backups"
  }
}

# Throttle limits live in each server's config file, so each server gets its
# own. Leave the watcher's KOPIA_THROTTLE_ variables unset, so that the limits
# have one owner.
resource "kopia_throttle" "host1" {
  max_upload_speed_bytes_per_second = 10485760
}

resource "kopia_throttle" "host2" {
  provider = kopia.host2

  max_upload_speed_bytes_per_second = 5242880
}
