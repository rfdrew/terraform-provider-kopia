# Changelog

All notable changes to this provider are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the provider is
at 0.x, a minor release may change behaviour; such changes are listed under
**Breaking and behaviour changes**.

## [0.1.0] - Unreleased

First release. Built and verified against Kopia 0.23.1.

### Requirements

- Kopia 0.22.x or 0.23.x, running `kopia server start` with its web UI (not
  `--no-ui`), signed in to as the UI user (`--server-username` and
  `--server-password`), with a repository connected.
- Terraform 1.9 or newer, or OpenTofu 1.9 or newer; import by resource identity
  needs Terraform 1.12.

### Added

- The provider, configured with `url`, `username`, `password`,
  `tls_cert_fingerprint`, `ca_cert_file`, `insecure_skip_verify` and
  `request_timeout`, or the matching `KOPIA_SERVER_*` environment variables.
  It renews Kopia's CSRF session on its own after a server restart, pins a
  self-generated certificate by its SHA-256 fingerprint, and warns about Kopia
  releases outside the supported ones and about a server without a connected
  repository.
- `kopia_policy`, which owns the whole snapshot policy of one target:
  `(global)`, `@host`, `user@host` or a directory, in the form Kopia writes it,
  checked by a strict parser. Every section of a Kopia policy is a nested
  attribute — `retention`, `files`, `error_handling`, `scheduling`,
  `compression`, `metadata_compression`, `splitter`, `actions`,
  `os_snapshots`, `logging` and `upload` — plus `no_parent`; whatever is left
  out is inherited. Creating refuses a target that already has a policy,
  except `(global)`, which is adopted, and destroying `(global)` resets it to
  the policy a new repository starts with instead of deleting it. At plan time
  the provider checks cron entries with the server and compressor and splitter
  names against the server's algorithms, and refuses `lz4` on Kopia 0.23 and
  later. Import by target, or by the `target` identity.
- `kopia_notification_profile`, for email, Pushover and webhook profiles, with
  `min_severity` by name, webhook headers as a map, and every setting
  validated at plan time, because Kopia stores invalid profiles and silently
  skips them. Creating refuses a name that exists. `send_test_on_apply` has
  Kopia send its test notification after every change. Secrets are marked
  sensitive and redacted from error messages. Import by name, or by the `name`
  identity.
- `kopia_throttle`, the singleton that owns the server's seven throttle
  limits: request rates, upload and download speeds, and concurrent reads and
  writes. Kopia saves them in the server's config file; destroying the
  resource saves "unlimited". An apply that Kopia carried out but could not
  save is saved again by the next one.
- `kopia_repository_description`, the singleton that sets the repository's
  description. As changing it makes Kopia rewrite its config file with the
  limits the server started with, the provider then saves the throttle limits
  in effect again. Destroying the resource leaves the description in place.
- The `kopia_policies` data source, which lists the targets that have a policy,
  filtered by host, user and path, with each stored policy as JSON.
- The `kopia_effective_policy` data source, which resolves what Kopia applies
  to a target, which target each value comes from, the next snapshot times and
  any scheduling error.
- The `kopia_notification_profiles` data source, which lists the profiles by
  name, method and minimum severity, without their secrets.
- The `kopia_repository_status`, `kopia_current_user` and `kopia_algorithms`
  data sources: the connected repository and the identity the server takes
  snapshots as, the account the server process runs as, and the algorithms the
  server's release supports.
- The `kopia_sources` and `kopia_snapshots` data sources: the snapshot sources
  the server knows, with their state and schedule, and the snapshots of one
  host with their retention reasons and pins.
- The guide "Kopia server with kopia-docker-watcher", which splits the
  configuration between Terraform and the watcher and explains how container
  path policies inherit from Terraform's.

[0.1.0]: https://github.com/rfdrew/terraform-provider-kopia/releases/tag/v0.1.0
