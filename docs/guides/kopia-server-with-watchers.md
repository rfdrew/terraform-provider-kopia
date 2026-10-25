---
page_title: "Kopia server with kopia-docker-watcher"
subcategory: ""
description: |-
  Split the configuration of Kopia servers between Terraform and kopia-docker-watcher: which policies Terraform owns, which it leaves to the watcher, and how the watcher's path policies inherit from Terraform's.
---

# Kopia server with kopia-docker-watcher

[kopia-docker-watcher](https://github.com/rfdrew/kopia-docker-watcher) backs
up Docker containers from their labels: it registers the paths a container
names as snapshot sources of a Kopia server, keeps the policies of those paths
in line with the labels, and pauses them while the container is stopped. It
talks to the Kopia server through the same HTTP API as this provider, and signs
in the same way, as the server's UI user with the same `KOPIA_SERVER_*`
variables.

The two work side by side when each owns its own part of the configuration:
Terraform what is set once for the repository, a server or a host, the watcher
what follows the containers.

## The setup

The Kopia server reads the files it backs up, so every Docker host runs a
Kopia server of its own, with a watcher next to it. To back up several hosts
into one place, connect all their servers to the same repository.

Where Kopia keeps a setting decides through which server Terraform manages it:

* **Policies and notification profiles live in the repository.** One provider
  configuration, pointed at any of the servers, manages them for every host.
  The server written to applies a change at once; the others pick it up at
  their next refresh (`kopia server start --refresh-interval`, four hours by
  default) or after `kopia server refresh`. Write the policy of a server's own
  sources through that server, so that a new schedule takes effect straight
  away.
* **Throttle limits and the repository description live in each server's
  config file.** Manage them with one provider configuration per server, using
  provider aliases.

Each server takes snapshots under the identity of its repository connection,
`user@host`, and the watcher registers the container paths under it: with the
official image on a container whose host name is `host1`, a path becomes
`root@host1:/srv/app/data`. The `username` and `hostname` attributes of the
`kopia_repository_status` data source report that identity. Give every server
a stable host name — `hostname:` on its container, or `--override-hostname`
when the repository is connected — or a recreated container registers every
path again under a new name, and the policies below no longer apply to them.

## Who owns what

| Configuration | Owner | Why |
| --- | --- | --- |
| The global policy, `(global)` | Terraform | Retention, compression, ignore rules and error handling: the defaults every source of every server inherits |
| Host policies, `@host` | Terraform | Defaults for every source of one host, whichever user it belongs to |
| The policy of a server's own `user@host` | Terraform | Defaults for that server's container paths, such as the schedule of containers that set none, and `upload.max_parallel_snapshots` |
| The policies of container paths, `user@host:/path` | kopia-docker-watcher | Their schedule, pause and the fields of the policy labels follow the containers |
| Policies of paths outside Docker | Terraform | Keep them outside the watcher's `WATCHER_PATH_PREFIX`, so that the two never meet on a path |
| Notification profiles | Terraform | Every server sends its own notifications, about the snapshots it runs, through the profiles in the repository |
| Throttle limits | Terraform, per server | One set per server; leave the watcher's `KOPIA_THROTTLE_` variables unset, so that the limits have one owner |

Two rules follow from how each side writes a policy:

* **Never describe a path policy the watcher manages in Terraform.**
  `kopia_policy` owns the whole policy of its target and replaces it on every
  apply, while the watcher writes the fields of its labels and the pause into
  it: each would undo the other's changes, at every apply and at every full
  reconcile of the watcher. Creating a `kopia_policy` for a path the watcher
  has already registered fails, because the policy exists; importing it would
  only start the tug of war.
* **The watcher never writes `(global)`, `@host` or `user@host` policies.** It
  only reads the effective policy of its paths, so whatever Terraform sets
  there reaches the containers through inheritance.

## How container paths inherit

Kopia resolves the policy of a path by merging, from the most specific to the
most general, the policy of the path, those of its parent directories, then
`user@host`, `@host`, `(global)` and finally Kopia's built-in defaults. Most
values are taken from the most specific policy that sets them, so a container's
labels override what Terraform sets above it. A few behave differently, and
they decide what belongs above the watcher's paths:

* **`user@host` is a user policy, not a host policy.** A target such as
  `root@host1` sits between the paths of that user and `@host1`; every
  container path of the server inherits from it first.
* **A `scheduling.cron` above is replaced** by a path's own `cron`, the
  `kopia.schedule` label. A cron entry on the server's `user@host` is therefore
  the schedule of exactly those containers that set none.
* **`scheduling.time_of_day` above is added** to the times of every path
  below it, unless a path sets `no_parent_time_of_day`. Times of day on
  `user@host`, `@host` or `(global)` make every container snapshot at those
  times on top of its own schedule.
* **An interval and a cron schedule both apply.** A path's own
  `interval_seconds` does not stop an inherited `cron`, nor its `cron` an
  inherited interval, and no path can clear an inherited `interval_seconds`.
  Keep intervals off the levels above the watcher's paths, and if some
  containers schedule with an interval, expect an inherited cron entry to
  snapshot them as well.
* **`scheduling.manual = true` above pauses every path below it**, and nothing
  below can resume them. Kopia combines `manual` from every level, so no path,
  and so no watcher, can undo it: set it on `@host` or `user@host` only to
  pause a whole host on purpose, and never on `(global)`.
* **The default global policy has no schedule.** A container path that sets
  no schedule is snapshotted only by what it inherits; without a schedule on
  `user@host`, `@host` or `(global)`, it is never snapshotted automatically.
* **`upload.max_parallel_snapshots` is read only from the effective policy of
  the server's own `user@host`**, so it has an effect on that target, on its
  `@host` and on `(global)`, and nowhere else; the provider refuses it on a
  path.
* **Some values cannot be cleared below.** A path can replace inherited ignore
  rules (`files.ignore`, `files.ignore_dot_files`) and `splitter.algorithm`
  with its own, but not remove them, so keep those generic above. Retention
  counts, error handling and the compressor can be overridden, the compressor
  with `none` and size limits with `-1`.

## Example

Two hosts, each with a Kopia server and a watcher, both servers connected to
the same repository. The provider configurations: one per server, the first
also writing everything the repository holds.

```terraform
terraform {
  required_providers {
    kopia = {
      source  = "rfdrew/kopia"
      version = "~> 0.1"
    }
  }
}

# The Kopia server on host1. Both servers are connected to the same
# repository, so the policies and notification profiles written through this
# one apply to every host.
provider "kopia" {
  url                  = "https://host1.example.com:51515"
  password             = var.kopia_passwords["host1"]
  tls_cert_fingerprint = var.kopia_cert_fingerprints["host1"]
}

# The Kopia server on host2: for what each server keeps for itself, the
# throttle limits, and for the policy of its own sources, which it then
# applies at once instead of at its next refresh.
provider "kopia" {
  alias                = "host2"
  url                  = "https://host2.example.com:51515"
  password             = var.kopia_passwords["host2"]
  tls_cert_fingerprint = var.kopia_cert_fingerprints["host2"]
}

variable "kopia_passwords" {
  description = "Password of each server's UI user, as given to `kopia server start --server-password`."
  type        = map(string)
  sensitive   = true
}

variable "kopia_cert_fingerprints" {
  description = "SHA-256 fingerprint of each server's TLS certificate, as `kopia server start` prints it."
  type        = map(string)
}
```

The global policy for every source, and a policy for the container paths of
each server, written through that server.

```terraform
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
```

A notification profile for every server, and throttle limits for each.

```terraform
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
```

## Checking the result

The `kopia_effective_policy` data source resolves what Kopia applies to a
container path, which target each value comes from, and when the next
snapshots are due; the `kopia_sources` data source lists the sources a server
knows, the watcher's among them. In a `check` block, the effective policy warns
on every plan when a path would not be snapshotted.

```terraform
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
```

## Changing hands

* **A host or user policy set up in the Kopia UI** makes creating the
  `kopia_policy` for it fail. Import it instead, for example with
  `terraform import kopia_policy.host1 'root@host1'`.
* **A path policy Terraform managed that the watcher should take over**: remove
  the resource from the configuration with a `removed` block and
  `lifecycle { destroy = false }`, so that the policy stays and the watcher
  writes its labels into it. Destroying the resource would delete the policy,
  and with it everything the labels do not set.
