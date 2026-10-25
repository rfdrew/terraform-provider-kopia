# The snapshots of one source. Give user_name and path whenever the retention
# reasons matter: Kopia works them out from the retention policy of the source
# asked for. With all = true, a snapshot whose contents match an older one is
# listed too, rather than hidden behind it.
data "kopia_snapshots" "app" {
  host      = "host1"
  user_name = "docker"
  path      = "/data/app"
  all       = true
}

locals {
  # Oldest first, so the last one is the newest.
  app_complete_snapshots = [for s in data.kopia_snapshots.app.snapshots : s if s.incomplete_reason == null]
  app_latest_snapshot    = try(local.app_complete_snapshots[length(local.app_complete_snapshots) - 1], null)
}

output "app_latest_snapshot_time" {
  value = try(local.app_latest_snapshot.start_time, null)
}

# Warn when the nightly backup has not completed for more than a day.
check "app_backed_up_recently" {
  assert {
    condition     = try(timecmp(local.app_latest_snapshot.start_time, timeadd(plantimestamp(), "-26h")) > 0, false)
    error_message = "No complete snapshot of docker@host1:/data/app in the last 26 hours."
  }
}
