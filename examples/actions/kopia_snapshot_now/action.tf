# Actions need Terraform 1.14 or newer. The action only asks Kopia for a
# snapshot: the server queues it and takes it in the background, and the action
# does not wait for it. The kopia_sources data source shows how it went.

# A path policy makes the server know the source, so its first snapshot can be
# taken as soon as the policy exists rather than at the first scheduled time.
# The source must belong to the server's own host, kopia1 here.
resource "kopia_policy" "app" {
  target = "root@kopia1:/data/app"

  scheduling = {
    cron = ["0 3 * * *"]
  }

  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.kopia_snapshot_now.app]
    }
  }
}

action "kopia_snapshot_now" "app" {
  config {
    target = kopia_policy.app.target
  }
}

# A snapshot on demand, such as before an upgrade, with:
#
#   terraform apply -invoke=action.kopia_snapshot_now.postgres
#
# The target is always one user@host:path: (global), @host and user@host are
# refused, because Kopia would snapshot every source that matches them.
action "kopia_snapshot_now" "postgres" {
  config {
    target = "root@kopia1:/srv/postgres"
  }
}
