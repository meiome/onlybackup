# OnlyBackup changes

This file records release changes. It does not establish which binaries
are installed on a server. Published versions and their artifacts are listed in
[GitHub releases](https://github.com/meiome/onlybackup/releases).

## Unreleased

## 0.5.3

This patch rebuilds all binaries with Go 1.27.4 for a standard-library security
fix and publishes them from a tag whose commit, binaries and build attestation
all match. Protocol, catalogue and archive formats and the systemd units are
unchanged from 0.5.2: replace all binaries together and restart the services.

- Require Go 1.27.4 so rebuilt binaries include the `net/http` fix for
  GO-2026-6859, an HTTP/1 server crash reachable from the receiver and writer.
- `--help` on `onlybackup-writer`, `onlybackup-receiver`,
  `onlybackup-maintenance` and `onlybackup-recover` now prints usage and exits
  with status 0 instead of reporting `Errore: flag: help requested`.
- Release notes are taken from this changelog; the release workflow stops
  before building if the tagged version has no entry.
- Add a code of conduct, issue and pull request templates, and Dependabot
  updates for Go modules and GitHub Actions.
- Clarify the project status in the README and move CI details to
  CONTRIBUTING.

## 0.5.2

This patch reduces maintenance load on large archives and rebuilds all binaries
with Go 1.27.3 for a standard-library security fix. Protocol and catalogue
formats are unchanged from 0.5.1. Replace all binaries together and install the
updated `deploy/onlybackup-maintenance.service`, then run
`systemctl daemon-reload` before restarting maintenance. The first cycle after
the upgrade hashes every archived file once to build the verification cache.

- Stop maintenance from re-hashing the whole archive on every five-minute
  cycle. Unchanged files are trusted from a verification cache keyed on file
  identity; new or changed files are hashed at once and a rolling scrub
  re-verifies every file within `--scrub-period` (default 7 days) under a
  per-cycle budget. Destructive operations still hash before acting. The
  maintenance unit now runs at low CPU and I/O priority.
- Require Go 1.27.3 so rebuilt binaries include the `crypto/x509` fix for
  GO-2026-6809, reachable from maintenance SMTP delivery over TLS.
- `onlybackup-admin backups status`, `onlybackup-admin quota` and the matching
  `onlybackup-inspect` commands now report a missing `--id`/`--key` or an
  unknown ID instead of `sql: no rows in result set`.

## 0.5.1

This patch fixes the Windows installer. Existing working installations do not
need to be reinstalled; client/server code, protocol, and stored data formats
are unchanged from 0.5.0. Upgrades from versions before 0.5.0 still require the
[server upgrade procedure](docs/INSTALL.md#upgrade).

- Reject files occupying Windows installer directory destinations before creating
  directories or changing permissions, including `receipts` and `encrypted-temp`.
  Resolve relative installation paths against the PowerShell working directory.

- Fix Windows PowerShell 5.1 installation failing when the client's successful
  `--help` probe writes to stderr. Check the process exit code directly and run
  installer regression tests in both Windows PowerShell 5.1 and PowerShell 7.

## 0.5.0

Existing upload clients remain compatible; updating them is optional. The
release includes Linux server and Windows client packages built with Go 1.27.2.

Server upgrades require a verified cold copy, all services stopped, the offline
`migrate-archives` command, and matching updated binaries and systemd units.
Catalogs from 0.4.1 upgrade from schema v10 to v12 while preserving existing
retention choices, holds, minima and models. Follow [the upgrade procedure](docs/INSTALL.md#upgrade).
Use the updated Linux recovery tool for `--state` on the migrated layout;
receipt/file recovery retains its existing format.

- Require Go 1.27.2 to include standard-library security fixes in rebuilt binaries.
- Bound monitoring coverage by the writer's recorded check start, so long archive
  scans cannot skip missing-backup alerts that become due during the scan.
- Preserve actual arrivals when matching appointments and checking sizes during
  excluded incidents; only model learning omits these observations. The writer
  continues to suppress only the selected operational anomaly kinds.
- Page the maintenance backup catalogue over the local socket, retaining deleted
  history and existing ordering without exceeding the per-response size limit.
  Failed pages abort the check before a partial catalogue can be used. Upgrade
  writer and maintenance together; no schema or public protocol change is needed.
- Resolve maintenance incidents atomically when key revocation cancels pending
  quarantine or purge requests, allowing retention to resume after a clean check
  while preserving independent holds and unfinished authorized operations.
- Store archives in `STATE/archives/{backups,quarantine}` and give maintenance
  one writable parent mount, fixing cross-mount rename failures under systemd.
  Existing installations require the offline, resumable `migrate-archives`
  command and matching binaries/units; directory renames preserve backup bytes,
  ownership, modes, catalog and pending operations.
- Allow reconciled provisional retries through only their own maintenance
  hold, while rechecking current safety rules and preserving all independent
  holds. Keep physical-operation failures open across intact-source retries. Send one
  resolution only after verified completion or valid cancellation; clarify
  physical uncertainty independently without falsely reporting success.
- Allow long whole-file reconciliation and confirmation requests on both ends
  of the local maintenance protocol. Full SHA-256 verification is cancellable
  on shutdown or request cancellation, with lease renewal still active.
- Test actual systemd mount rules, including an EXDEV reproduction, atomic
  quarantine/recovery/purge and writer publication; cover migration interruption,
  collisions, active executors and repeated-error mail behavior.
  Apply the shipped capability limits when the CI probe runs as root.
- Default new archives to automatic retention, held until successful mail proof,
  reliable models and a completed regular check. Add
  `automation setup --retention auto|manual` and report the selected mode during setup.
- Add `retention resume --when-ready` for a persistent, one-shot release at the
  next valid check. New administrative pauses, blocking incidents, cancellations,
  missing-file acknowledgements, setup and relearning cancel deferred requests.
- Require action-specific confirmation for setup, enable, pause and both resume
  forms, through a CLI prompt or `--confirm`, enforced also by the writer API.
  Mode-omitting setup preserves existing choices and invalidates running checks.
  Pending resume remains a separate hold across anomaly exclusions and recovery.
- Persist initial activation separately from administrative holds in schema v11.
  Upgrades preserve existing enablement, holds and configured minima; they do not
  silently activate previously disabled retention. Upgrade writer, maintenance
  and administrator binaries together.
- Version SMTP configuration in schema v12 and atomically bind successful mail
  proof to the configuration actually used for delivery. An in-flight test from
  before setup, a replayed confirmation, or the legacy mail-kind header cannot
  certify replacement settings or activate retention. Existing proof and
  retention choices are preserved by migration.
- Preserve disk thresholds, oldest-first selection, per-key minima, last-copy
  protection and at least 48 hours of quarantine. Show pending activation and
  deferred resume in status and email reports.

## 0.4.1

Existing upload and recovery clients remain compatible. Catalogs already on
schema v10 need no schema migration; backup contents and learned models are
preserved. The release includes Linux server and Windows client packages.

- Keep the specific 48-hour free-space margin alert visible and mailed without
  blocking quarantine or purge. Other capacity and integrity incidents retain
  their existing blocking behavior.
- Preserve per-key retention minima, current-day and last-copy protections, and
  at least 48 hours of quarantine. Resolving the margin warning no longer
  introduces a new retention pause.
- Preserve existing administrative pauses and persisted blocks. After a fresh
  regular check, explicitly run `retention resume` to release them, including
  when the only remaining incident is the non-blocking margin warning.
- Clarify margin-warning emails and periodic reports, identify the client and
  appointment in missing-backup alerts, and base resume advice on a recently
  completed regular check.
- Add regression tests for automatic quarantine and purge under low-space
  warnings, existing incident history, independent holds, and operator reports.

Upgrade writer and maintenance together using [the installation guide](docs/INSTALL.md).
Updating installed upload and recovery clients is not required.

## 0.4.0

Existing upload and recovery clients remain compatible; updating installed clients
is not required. The release includes Linux server and Windows client packages.

- Persist administrative retention pauses independently of automatic blocks;
  resolving or excluding an anomaly cannot clear an administrative pause.
- Add `retention minimum --key KEY_ID --days N` for administrator-configured
  per-key minima. Learning and relearning never change these values, and space
  shortages are reported without lowering them.
- Recheck each key's minimum at the request, provisional authorization, and final
  permission stages, including manual destructive requests. Already committed
  final permissions retain their existing behavior.
- Migrate catalogs to schema v10, preserving existing minima and conservatively
  retaining legacy blocks as administrative holds until explicit resume.
- Include growth since model learning in quota predictions, consider copies
  still pending within schedule tolerance, and exclude appointments satisfied
  under the monitoring rules, including across midnight.
- Add Linux CI for pushes to `main` and pull requests: tests, static analysis,
  vulnerability scanning, race detection, smoke, and system checks.
- Update retention and upgrade guides and add contributor documentation.

Upgrade writer and maintenance together, stop maintenance before upgrading, and
create an independent cold copy first. See [the installation guide](docs/INSTALL.md)
for migration details. These changes do not alter backup contents or the public
deposit protocol.
