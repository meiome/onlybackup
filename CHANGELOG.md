# OnlyBackup changes

This file records release changes. It does not establish which binaries
are installed on a server. Published versions and their artifacts are listed in
[GitHub releases](https://github.com/meiome/onlybackup/releases).

## Unreleased

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
