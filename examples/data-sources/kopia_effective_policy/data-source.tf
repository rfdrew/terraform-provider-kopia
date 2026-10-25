# What Kopia applies to a source: its own policy merged with everything it
# inherits. The target needs no policy of its own.
data "kopia_effective_policy" "postgres" {
  target                      = "docker@host1:/srv/postgres"
  num_upcoming_snapshot_times = 3
}

output "postgres_keep_daily" {
  value = data.kopia_effective_policy.postgres.effective.retention.keep_daily
}

# Which policy each value comes from; "(global)" also stands for Kopia's
# built-in defaults.
output "postgres_cron_from" {
  value = lookup(data.kopia_effective_policy.postgres.definition, "scheduling.cron", null)
}

output "postgres_next_snapshots" {
  value = data.kopia_effective_policy.postgres.upcoming_snapshot_times
}

# Warn on every plan when Kopia would refuse the schedule, or when the source
# would never be snapshotted on a schedule.
check "postgres_is_scheduled" {
  assert {
    condition     = data.kopia_effective_policy.postgres.scheduling_error == null
    error_message = "Kopia refuses the schedule: ${coalesce(data.kopia_effective_policy.postgres.scheduling_error, "unknown")}"
  }

  assert {
    condition     = length(data.kopia_effective_policy.postgres.upcoming_snapshot_times) > 0
    error_message = "docker@host1:/srv/postgres has no upcoming scheduled snapshot."
  }
}
