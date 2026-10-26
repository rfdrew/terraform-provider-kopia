# Terraform Provider for Kopia

Manages the configuration of a running [Kopia](https://kopia.io) server —
snapshot policies, notification profiles, throttling and the repository
description — as code, in Terraform and OpenTofu, through the HTTP API behind
the server's web UI. With Terraform 1.14 it also finds existing policies and
profiles for `terraform query`, and offers actions that send a test
notification or take a snapshot now.

## Versions

**Provider 0.1.0 — built and verified against Kopia 0.23.1.**
The 0.22 and 0.23 releases are supported; the provider warns when the server
reports another release.

| Provider | Released | Verified against | Supported Kopia | Terraform | OpenTofu |
| --- | --- | --- | --- | --- | --- |
| 0.1.0 | 2026-10-26 | Kopia 0.23.1 | 0.22.x, 0.23.x | 1.9+ | 1.9+ |

Terraform 1.9 is enough for everything except importing by resource identity,
which needs Terraform 1.12 or newer, and the list resources and actions, which
need Terraform 1.14 or newer.

Changes between releases are listed in [`CHANGELOG.md`](CHANGELOG.md).

## Why the surface is small

Kopia has no public API. `kopia server start` serves the JSON endpoints its own
web UI calls, and that is what this provider uses — undocumented and
unversioned, which is why each provider release names the Kopia releases it is
verified against. Repository server users (`kopia server user`), their access
rules (`kopia server acl`) and maintenance settings (`kopia maintenance set`)
exist only in the `kopia` command line, so they cannot be managed here.

Creating or connecting a repository is possible over the API but deliberately
left out: the storage credentials would end up in Terraform state, creating a
repository is a one-off, and destroying one could only disconnect it. Connect
the server to its repository before `kopia server start`. Snapshots are left
out too, as data rather than configuration; the `kopia_snapshots` and
`kopia_sources` data sources read them, and the `kopia_snapshot_now` action
asks the server for one.

## Resources, data sources, list resources and actions

| Resource | Lifecycle | Notes |
| --- | --- | --- |
| `kopia_policy` | create, read, update, delete | Owns the policy of one target: `(global)`, `@host`, `user@host` or a directory, and keeps the settings it has no attribute for, such as those of a later Kopia release. Creating refuses a target that already has a policy, except `(global)`, which is adopted and reset to Kopia's defaults on destroy. Cron entries and algorithm names are checked with the server at plan time. Identity `target`; import by target |
| `kopia_notification_profile` | create, read, update, delete | Email, Pushover or webhook, switchable in place. Creating refuses a name that exists. Secrets are in state; `send_test_on_apply` sends Kopia's test notification. Identity `name`; import by name |
| `kopia_throttle` | create, read, update (singleton); destroy saves "unlimited" | Owns all seven limits of the server's repository connection; saved in the server's config file. Import id `throttle` |
| `kopia_repository_description` | create, read, update (singleton); destroy leaves it | Saved in the server's config file, after which the provider saves the throttle limits again. Import id `description` |

| Data source | Purpose |
| --- | --- |
| `kopia_policies` | List the targets that have a policy of their own, filtered by host, user and path, with each stored policy as JSON |
| `kopia_effective_policy` | Resolve what Kopia applies to a target: the merged policy, which target each value comes from, the next snapshot times and any scheduling error |
| `kopia_notification_profiles` | List the notification profiles by name, method and minimum severity, without their secrets |
| `kopia_repository_status` | Whether a repository is connected, its format, algorithms and storage, and the `user@host` identity the server takes snapshots as |
| `kopia_current_user` | The operating-system user and host the server process runs as — neither the UI user nor necessarily the snapshot identity |
| `kopia_algorithms` | The hash, encryption, error-correction, splitter and compression algorithms the server's release supports, with its defaults |
| `kopia_sources` | The snapshot sources the server knows, with status, effective schedule, next snapshot time and last snapshot |
| `kopia_snapshots` | The snapshots of one host, optionally of one user and path, with retention reasons and pins |

| List resource | Purpose |
| --- | --- |
| `kopia_policy` | The targets that have a policy, filtered by host, user and path, for `terraform query`; each target once, and without the malformed targets other clients can store, with a warning naming them |
| `kopia_notification_profile` | The email, Pushover and webhook profiles, for `terraform query`; with `include_resource`, every profile is read in full, secrets included |

| Action | Purpose |
| --- | --- |
| `kopia_notification_test` | Has Kopia send its test notification through a stored profile, by name; errors come with the profile's secrets redacted |
| `kopia_snapshot_now` | Asks the server for a snapshot of one source, `user@host:path`, now. Kopia only queues it, so the action does not wait for it |

### Import by identity and `terraform query`

`kopia_policy` and `kopia_notification_profile` carry a resource identity —
`target` and `name` — so an `import` block can name them with `identity`
instead of an import id (Terraform 1.12+). Older Terraform releases keep
importing by id. The two singletons have no identity; their import ids are
fixed.

The same two can be listed in a `.tfquery.hcl` file (Terraform 1.14+):
`terraform query` shows the policies and profiles the server has, and
`terraform query -generate-config-out=generated.tf` writes an `import` block and
a resource block for each one. With `include_resource = true` the generated
blocks hold every setting, as importing and refreshing would record it.

```hcl
list "kopia_policy" "docker_host1" {
  provider         = kopia
  include_resource = true

  config {
    host      = "host1"
    user_name = "docker"
  }
}
```

### Actions

Actions (Terraform 1.14+) keep no state. They run when a resource's
`lifecycle { action_trigger { ... } }` fires, or on demand with
`terraform apply -invoke=action.<type>.<name>`:

```hcl
resource "kopia_policy" "app" {
  target = "root@kopia1:/data/app"

  # Take the first snapshot as soon as the server knows the path.
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
```

### Requirements on the Kopia server

* **The web UI must be enabled.** `--no-ui` turns off the API this provider
  uses together with the UI.
* **The provider signs in as the UI user**, the `--server-username` (default
  `kopia`) and `--server-password` of `kopia server start`. Users added with
  `kopia server user add` and the server-control user are refused.
* **CSRF protection stays on.** The provider fetches a CSRF token as the web UI
  does, and a new one after a server restart has invalidated it, so
  `--disable-csrf-token-checks` is never needed.
* **A repository must be connected.** Every resource and data source except
  `kopia_current_user` and `kopia_algorithms` needs one; the provider warns
  when the server has none.
* **For fingerprint pinning, the TLS certificate must persist.** With
  `--tls-generate-cert` and no certificate files, Kopia makes a new
  certificate, and so a new fingerprint, on every start. Generate it once into
  `--tls-cert-file` and `--tls-key-file` on a volume, then start the server
  with those files and without `--tls-generate-cert`.
* **The server's config file must be on persistent, writable storage** for
  `kopia_throttle` and `kopia_repository_description`: `repository.config`, on
  a volume on `/app/config` for the official Docker image.

### Working with kopia-docker-watcher

[kopia-docker-watcher](https://github.com/rfdrew/kopia-docker-watcher) keeps
the policies of container paths in line with Docker labels, through the same
API and with the same credentials. Let Terraform own `(global)`, the `@host`
and `user@host` policies, the notification profiles and the throttle, and
leave the container paths to the watcher. The guide
[Kopia server with kopia-docker-watcher](docs/guides/kopia-server-with-watchers.md)
explains the split and how the watcher's path policies inherit from
Terraform's.

### Known limitations

These follow from Kopia's API, not from the provider's design.

* **Policies and notification profiles live in the repository; the throttle
  and the description in the server's config file.** Every server connected
  to the same repository shares the former: the server the provider writes to
  applies a change at once, the others at their next refresh
  (`--refresh-interval`, four hours by default) or after
  `kopia server refresh`. The latter belong to one server each.
* **Kopia cannot store `false` for some policy settings.** The `no_parent*`
  flags and `scheduling.manual` are kept only when `true`, so `false` means the
  same as leaving them out and cannot switch off what is inherited. After an
  import, a configured `false` of that kind shows once as an update that
  changes nothing on the server.
* **Deleting the global policy would remove it for good.** Destroying a
  `kopia_policy` for `(global)` therefore writes the policy
  `kopia repository create` starts with instead.
* **Kopia overwrites an existing policy or profile without a word.** Creating a
  `kopia_policy` (other than for `(global)`) or a
  `kopia_notification_profile` that already exists fails, so that one set up
  in the Kopia UI or by kopia-docker-watcher is not overwritten unseen; import
  it instead.
* **Kopia accepts any compressor or splitter name and any notification
  setting.** It silently compresses nothing, falls back to the repository's
  splitter, or skips the profile when a notification goes out. The provider
  checks names against the server's algorithms and every notification setting
  at plan time; `lz4` is refused from Kopia 0.23 on, where it no longer works.
* **Upcoming snapshot times are not planned.** They change with the clock, so
  `kopia_policy` only checks the schedule; `kopia_effective_policy` reports
  them.
* **Notification secrets are stored in state.** Kopia returns every setting
  when a profile is read — SMTP password, Pushover tokens, webhook URL and
  headers — and Kopia itself writes them to its log files at debug level, the
  level those files record by default.
* **Notification delivery has quirks.** Kopia counts a webhook as delivered
  only on HTTP 200. Email uses STARTTLS on a plain connection, so implicit TLS
  on port 465 does not work, and Kopia ignores `cc`. Message templates can only
  be changed with `kopia notification template`, and are not deleted with a
  profile. The test notification of `send_test_on_apply` is bounded by the
  provider's `request_timeout`.
* **Throttle limits cannot be removed, only set to unlimited.** Destroying
  `kopia_throttle` saves "unlimited", and limits from the storage
  configuration (`--max-upload-speed`, `--max-download-speed`) do not come
  back. Disconnecting or reconnecting the repository discards the saved
  limits. A server connected through a Kopia repository server has no limits
  of its own: Kopia refuses every throttle request there.
* **Changing the repository description rewrites the server's config file**
  with the settings the server started with, which would put the saved
  throttle limits back. The provider saves the limits in effect again right
  after; a description changed in the Kopia UI does not, and the server then
  loses the limits at its next restart. The Kopia UI and the `kopia` command
  line are not coordinated with the provider's writes.
* **The description can be set but not removed.** Destroying
  `kopia_repository_description` leaves it in place.
* **The Kopia version comes from the title of the web UI page.** A build that
  reports none triggers no warning about an untested release.
* **`kopia_snapshot_now` does not wait for the snapshot.** Kopia queues it and
  reports neither a task nor the outcome; `kopia_sources` shows how it went.
  The server snapshots only the sources it knows (those with a snapshot or a
  path policy of their own) on its own host, and none on a read-only
  repository connection; Kopia reads a missing host, user or path as "any",
  so the action takes exactly one `user@host:path`.
* **The test notification ignores `min_severity`.** Kopia sends it whatever the
  profile's minimum severity, and every successful `kopia_notification_test`
  delivers a real message.

## Usage

```hcl
terraform {
  required_providers {
    kopia = {
      source  = "rfdrew/kopia"
      version = "~> 0.1"
    }
  }
}

provider "kopia" {
  url = "https://kopia.example.com:51515"
  # username and password come from KOPIA_SERVER_USERNAME and
  # KOPIA_SERVER_PASSWORD.
  tls_cert_fingerprint = "cd50843049f0c6da48255e8022728d28124cb5b72fc3a2d81be496219c2cccf3"
}

resource "kopia_policy" "global" {
  target = "(global)"

  retention = {
    keep_latest = 10
    keep_daily  = 14
  }

  compression = {
    compressor_name = "zstd"
  }
}
```

### Provider configuration

| Argument | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `url` | `KOPIA_SERVER_URL` | — | Root URL of the kopia server. Required. |
| `username` | `KOPIA_SERVER_USERNAME` | `kopia` | The server's UI user (`--server-username`). |
| `password` | `KOPIA_SERVER_PASSWORD` | — | The UI user's password (`--server-password`). Required, sensitive. |
| `tls_cert_fingerprint` | `KOPIA_SERVER_CERT_FINGERPRINT` | — | SHA-256 of the server's certificate, to trust that certificate alone. |
| `ca_cert_file` | `KOPIA_SERVER_CA_FILE` | — | PEM file of CA certificates to verify the server with. |
| `insecure_skip_verify` | `KOPIA_SERVER_INSECURE_SKIP_VERIFY` | `false` | Skip certificate verification. |
| `request_timeout` | `KOPIA_SERVER_REQUEST_TIMEOUT` | `30s` | Time limit for each request, as a Go duration. |

The configuration wins over the environment, and an empty variable counts as
unset. `tls_cert_fingerprint`, `ca_cert_file` and `insecure_skip_verify` are
mutually exclusive.

### Credentials

The provider signs in as the server's UI user: the one `kopia server start`
names with `--server-username` (default `kopia`) and `--server-password`. The
server-control user and repository users are refused by the UI API, and a
server started with `--no-ui` does not serve it.

A server started with `--tls-generate-cert` prints its certificate's
fingerprint as `SERVER CERT SHA256: …`; pass it as `tls_cert_fingerprint`. The
certificate, and with it the fingerprint, changes on every start unless the
server is given `--tls-cert-file` and `--tls-key-file`.

Kopia's own client commands (`kopia server status` and the like) read
`KOPIA_SERVER_USERNAME` and `KOPIA_SERVER_PASSWORD` as the server-control
credentials, not the UI user's, so keep the two apart.

## Development

Requires Go 1.26+, and Terraform 1.12+ and Docker for the acceptance tests;
those of the list resources and actions skip themselves below Terraform 1.14.

```shell
make build          # build the provider binary
make test           # unit tests, no kopia server needed
make lint           # gofmt + go vet + terraform fmt, and golangci-lint if installed
make docs           # regenerate docs/ through Terraform
make docs-offline   # regenerate docs/ without Terraform, from an in-process schema dump
make docs-validate  # tfplugindocs validate
```

`make docs` downloads Terraform 1.14.0 rather than using the one on `PATH`,
because older releases leave newer schema features out of the pages; set
`TERRAFORM_DOCS_VERSION` to pick another exact release. `make docs-offline`
renders the same pages without Terraform: `tools/providerschema` dumps the
provider's schema in process in the format of `terraform providers schema
-json`, and tfplugindocs reads that file instead. CI runs both and fails if
they disagree. The provider page comes from `templates/index.md.tmpl`, and the
guides from `templates/guides/`; the resource and data source pages are made
from the schema descriptions and `examples/` alone.

### Working without network or sockets

The unit tests never open a socket. The client's `Transport` option, and the
provider's `transport` field, take an `http.RoundTripper`, and an in-process
fake answers Kopia's `GET /` and CSRF handshake and the API routes each test
registers. Resources are tested by calling their methods directly, or through
the framework's protocol server, the one Terraform talks to, so plans,
private state and identities are exercised without launching the provider as
a plugin. `make test` therefore runs in a sandbox without network, Docker or
local ports; the acceptance tests only compile there
(`go test -count=1 -run '^$' ./...`).

With `GOPROXY=off`, `make docs-offline` and `make docs-validate` resolve
tfplugindocs from the local module cache. If `go build` cannot stamp version
control information, add `-buildvcs=false`.

### Running against a local kopia server

`make kopia-up` creates a filesystem repository in a Docker volume and starts
`kopia server start` on it, with a UI password and CSRF protection, on
`127.0.0.1:51515`.

```shell
eval "$(make kopia-up)"   # exports KOPIA_SERVER_URL, KOPIA_SERVER_USERNAME, KOPIA_SERVER_PASSWORD
make testacc              # acceptance tests against that server
make kopia-down           # remove the container, volume and state

# Another release, HTTPS with a pinned certificate, or a server restart:
eval "$(KOPIA_TEST_IMAGE=kopia/kopia:0.22.3 make kopia-up)"
eval "$(KOPIA_TEST_TLS=1 make kopia-up)"
eval "$(./scripts/bootstrap-test-kopia.sh --restart)"
```

The script header in `scripts/bootstrap-test-kopia.sh` lists every setting.
Besides the `KOPIA_SERVER_*` variables it exports what some acceptance tests
need to know about the server, such as the identity it takes snapshots as
(`KOPIA_ACC_SOURCE_HOST`, `KOPIA_ACC_SOURCE_USER`). CI runs the acceptance
tests against Kopia 0.22.3 and 0.23.1 on Terraform 1.12 and 1.14, once over
HTTPS, once with the latest OpenTofu, and once against Kopia 0.22.0, whose
policy JSON leaves empty sections out.

Tests that restart the server, such as the one checking that throttle limits
survive a restart, run only with `KOPIA_ACC_RESTART=1`. They restart it with
`scripts/bootstrap-test-kopia.sh --restart`, which restarts only a server it
started from the same image in the same mode, so give them the `KOPIA_TEST_*`
settings the server was started with. Run them on their own, as CI does,
because a restart fails whatever else reaches the server meanwhile. With TLS
the server has a new certificate afterwards, so export its fingerprint again
by running the boot command once more, which reuses the server:

```shell
KOPIA_TEST_TLS=1 KOPIA_ACC_RESTART=1 TF_ACC=1 go test ./internal/provider -run '^TestAcc.*_restart$' -count=1 -v
eval "$(KOPIA_TEST_TLS=1 make kopia-up)"   # reuses the server, exports its new fingerprint
```

> Acceptance tests create, change and delete real policies, notification
> profiles and settings, and reset the global policy to Kopia's defaults.
> Only ever point them at a disposable server.

### Manual testing with dev overrides

```shell
go build -o /tmp/tfbin/terraform-provider-kopia .

cat > /tmp/dev.tfrc <<'EOF'
provider_installation {
  dev_overrides {
    "rfdrew/kopia" = "/tmp/tfbin"
  }
  direct {}
}
EOF

export TF_CLI_CONFIG_FILE=/tmp/dev.tfrc
terraform plan   # no `terraform init` — dev overrides bypass the registry
```

## Releasing

In [`CHANGELOG.md`](CHANGELOG.md), replace `Unreleased` in the newest heading
with the release date and point its compare link at the new tag instead of
`HEAD`; put the same date in the Released column of the
[version table](#versions) at the top of this README. Then tag a semver release
and push it; the `release` workflow runs GoReleaser and publishes signed
artefacts for the Terraform Registry and the OpenTofu Registry.

```shell
git tag v0.1.0
git push origin v0.1.0
```

The repository needs two secrets: `GPG_PRIVATE_KEY` (ASCII-armoured, its public
half uploaded to the registries) and `PASSPHRASE`.

## License

[MPL-2.0](LICENSE)
