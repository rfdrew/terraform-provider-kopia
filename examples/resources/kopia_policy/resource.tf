# The global policy always exists, so this adopts it. Everything it leaves out
# falls back to Kopia's built-in defaults, and destroying the resource resets
# the global policy to the one a new repository starts with.
resource "kopia_policy" "global" {
  target = "(global)"

  retention = {
    keep_latest  = 10
    keep_daily   = 14
    keep_weekly  = 8
    keep_monthly = 12
  }

  compression = {
    compressor_name = "zstd"
    never_compress  = [".gz", ".jpg", ".mp4", ".zip"]
  }

  files = {
    ignore_cache_dirs = true
    one_file_system   = true
  }

  # Kopia reads this only from the policies of the server's own user@host and
  # those above it, and refuses it on a path policy.
  upload = {
    max_parallel_snapshots = 2
  }
}

# The policy of every source user docker has on host1. kopia-docker-watcher
# keeps the path policies below it (docker@host1:/...) in line with container
# labels; they inherit what is set here unless they set it themselves.
resource "kopia_policy" "docker_host1" {
  target = "docker@host1"

  # Kopia reads cron entries in the server's time zone, and treats everything
  # from a hash sign on as a comment. A path policy's own cron replaces this
  # one, while times of day set here would be added to its own.
  scheduling = {
    cron = ["0 3 * * * # nightly"]
  }
}

# A directory policy with a database dump before every snapshot, on a host
# without kopia-docker-watcher: a path policy the watcher keeps must not be
# described here as well. Actions run only when the server's repository
# connection has actions enabled.
resource "kopia_policy" "postgres" {
  target = "root@db1:/srv/postgres"

  files = {
    ignore = ["pg_wal/", "*.tmp"]
  }

  actions = {
    before_snapshot_root = {
      script          = "#!/bin/sh\npg_dumpall -U postgres > /srv/postgres/dump.sql"
      timeout_seconds = 600
      mode            = "essential"
    }
  }
}

# Pausing scheduled snapshots of a host: manual cannot be combined with any
# other scheduling attribute, and no more specific policy can undo it.
resource "kopia_policy" "maintenance" {
  target = "@host2"

  scheduling = {
    manual = true
  }
}

# A Windows source, with Volume Shadow Copy where it is available. Windows
# paths are written with backslashes, which HCL needs doubled.
resource "kopia_policy" "windows" {
  target = "Administrator@WIN1:C:\\Users"

  os_snapshots = {
    volume_shadow_copy = "when-available"
  }

  error_handling = {
    ignore_file_errors = true
  }
}
