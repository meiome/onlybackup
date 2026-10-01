# OnlyBackup changes

This file records release changes. It does not establish which binaries
are installed on a server. Published versions and their artifacts are listed in
[GitHub releases](https://github.com/meiome/onlybackup/releases).

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
