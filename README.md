# OnlyBackup

OnlyBackup is a production-ready, **deposit-only** backup server for Linux.
Clients can upload new files, but the public API cannot read, modify, or delete
them.

Transfers use HTTPS with TLS 1.3. The client normally encrypts content with
[age](https://age-encryption.org/) before upload; storing plaintext requires an
explicit choice. The receiver and writer run as separate Linux users, so the
public receiver cannot access the archive.

```text
client -> HTTPS receiver -> deposit socket -> writer -+- .backup file
                                                       `- SQLite catalog
local admin ---------------------> admin socket -------'
maintenance <---------------- maintenance socket -----'
    `-> monitoring, SMTP, quarantine/recovery/purge
```

> **Status:** the deposit path is running in production. Retention changes in
> this repository must be checked against the installed version; local tests do
> not establish that they have been released or deployed. Go, race, smoke,
> system, and isolation tests passed for the documented retention worktree.
> Linux CI on pushes to `main` and pull requests runs `make check`, `make race`,
> `make smoke`, and `make system-test`. The release workflow runs `make check`
> and `make race`; isolation and database restore checks require separate runs.

## Build and test from source

Building requires Linux, Go 1.27 or later, and GCC.

```bash
make build
make check
make smoke
```

`make smoke` starts a temporary HTTPS environment, uploads and recovers a
backup, checks its integrity and the rejection of forbidden API operations,
then removes the test data.

To build the two native Windows AMD64 clients in `bin/windows-amd64`:

```bash
make windows-client
```

## Verifiable releases

GitHub Actions creates releases only for tags in the `vX.Y.Z` format, after the
test suite, static analysis, and race detector complete successfully. The Linux
AMD64 server package is built on Ubuntu 22.04 for glibc-based systems; it does
not support Alpine Linux or other musl-based distributions. The Windows AMD64
package contains the native `onlybackup.exe` and `onlybackup-recover.exe`
clients, also tested on Windows Server 2022. Each archive has an entry in
`SHA256SUMS` and a build provenance attestation.

After downloading an archive and `SHA256SUMS` from the same release, verify them
before extracting or running anything:

```bash
test -f onlybackup_VERSION_linux_amd64.tar.gz
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify onlybackup_VERSION_linux_amd64.tar.gz \
  --repo meiome/onlybackup
```

On Windows, verify `onlybackup_VERSION_windows_amd64.zip` in the same way. The
complete PowerShell commands are in the
[Windows client guide](docs/CLIENT-WINDOWS.md).

Then extract the Linux archive and enter its directory:

```bash
tar -xzf onlybackup_VERSION_linux_amd64.tar.gz
cd onlybackup_VERSION_linux_amd64
```

Require an explicit `OK` line for the downloaded Linux archive. The checksum
file also lists the Windows archive, which need not be present. The checksum
detects accidental changes. The attestation links the archive to
the workflow and commit that produced it; it is not a guarantee that the
software has no vulnerabilities.

## Install and use

For a new server, upgrade, or migration, follow the single
[installation guide](docs/INSTALL.md). Separate guides cover
[Debian clients](docs/CLIENT-DEBIAN.md) and
[native Windows clients](docs/CLIENT-WINDOWS.md), including Bash and PowerShell
automation. Use the [operations runbook](docs/OPERATIONS.md) for recurring work
and the [retention guide](docs/RETENTION.md) for monitoring, mail, simulation,
quarantine, recovery, and persistent anomaly blocks.

A client configuration looks like this:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send.key",
  "encrypt_to": "age1..."
}
```

```bash
onlybackup send --config client.json \
  --description "Daily accounting backup" backup.sql
```

The command returns a JSON receipt only after the server verifies and stores the
backup. Use `--plaintext` only when you intentionally want the server to store
readable content. OnlyBackup accepts files that have already been prepared; use
[scripts/backup-mysql.sh](scripts/backup-mysql.sh) to create MySQL dumps.

## Documentation

- [Installation and migration](docs/INSTALL.md)
- [Debian client](docs/CLIENT-DEBIAN.md)
- [Native Windows client](docs/CLIENT-WINDOWS.md)
- [Production operations](docs/OPERATIONS.md)
- [Monitoring and retention](docs/RETENTION.md)
- [Security model](docs/SECURITY.md)
- [Public protocol](docs/PROTOCOL.md)
- [Restore testing](docs/RESTORE-TEST.md)
- [Contributing](CONTRIBUTING.md)
- [Release changes](CHANGELOG.md)

Retention grants durable permission for one physical operation at final
validation. Revocation blocks new permissions but does not cancel committed
work; quarantine and purge need separate permissions. Manual pause and its reason
persist independently of automatic blocks and require an explicit successful
administrative resume. Administrator-configured per-key retention minima are
checked before each new destructive permission. These changes require schema
v10 and matching binaries; verify the installed version before relying on them.
See the retention guide for recovery after a crash and the remaining safety limits.

Licensed under the [GNU AGPL-3.0](LICENSE).
