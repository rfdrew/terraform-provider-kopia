# Defaults for every source of every server connected to the repository. A
# container path overrides any of them with its own policy labels.
resource "kopia_policy" "global" {
  target = "(global)"

  retention = {
    keep_latest  = 10
    keep_daily   = 14
    keep_weekly  = 8
    keep_monthly = 12
    keep_annual  = 3
  }

  compression = {
    compressor_name = "zstd"
  }

  files = {
    ignore_cache_dirs = true
  }
}

# The identity each server takes snapshots as: user@host of its repository
# connection, under which kopia-docker-watcher registers container paths.
data "kopia_repository_status" "host1" {}

data "kopia_repository_status" "host2" {
  provider = kopia.host2
}

locals {
  host1 = "${data.kopia_repository_status.host1.username}@${data.kopia_repository_status.host1.hostname}"
  host2 = "${data.kopia_repository_status.host2.username}@${data.kopia_repository_status.host2.hostname}"
}

# Defaults for the container paths of host1. A container's kopia.schedule
# label replaces this cron entry, so it is the schedule of the containers
# that set none. Times of day or an interval here would instead be added to
# every container's own schedule.
resource "kopia_policy" "host1" {
  target = local.host1

  scheduling = {
    cron = ["0 3 * * * # nightly"]
  }

  # Kopia reads this only from the effective policy of the server's own
  # user@host, so it belongs here or above, never on a path.
  upload = {
    max_parallel_snapshots = 2
  }
}

# The same for host2, written through its own server.
resource "kopia_policy" "host2" {
  provider = kopia.host2

  target = local.host2

  scheduling = {
    cron = ["30 3 * * * # nightly, after host1"]
  }
}
