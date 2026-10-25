# What Kopia applies to one container path, merged from everything above it.
data "kopia_effective_policy" "app" {
  target                      = "${local.host1}:/srv/app/data"
  num_upcoming_snapshot_times = 1
}

# Where the schedule of the path comes from: the path itself when its
# container sets kopia.schedule, otherwise the server's user@host.
output "app_schedule_from" {
  value = lookup(data.kopia_effective_policy.app.definition, "scheduling.cron", null)
}

# A path the watcher has paused, because all its containers are stopped, has
# no upcoming snapshot either.
check "app_is_scheduled" {
  assert {
    condition     = length(data.kopia_effective_policy.app.upcoming_snapshot_times) > 0
    error_message = "${local.host1}:/srv/app/data has no upcoming scheduled snapshot."
  }
}
