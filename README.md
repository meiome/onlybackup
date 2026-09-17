# OnlyBackup

OnlyBackup is a production-ready, **deposit-only** backup server for Linux.
Clients can upload new files, but the public API cannot read, modify, or delete
them.

Transfers use HTTPS with TLS 1.3. The client normally encrypts content with
[age](https://age-encryption.org/) before upload; storing plaintext requires an
explicit choice. The receiver and writer run as separate Linux users, so the
public receiver cannot access the archive.

```text
client -> HTTPS receiver -> Unix socket -> writer -+- .backup file
                                                   `- SQLite catalog
```

> **Status:** stable and running in production. The automated test suite, race
> detector, system tests, and isolation tests cover the supported workflow. The
> supplied systemd services have been deployed on a dedicated Debian 13 AMD64
> server.

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
sha256sum --check SHA256SUMS
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

The checksum detects accidental changes. The attestation links the archive to
the workflow and commit that produced it; it is not a guarantee that the
software has no vulnerabilities.

## Install and use

For a new server, upgrade, or migration, follow the single
[installation guide](docs/INSTALL.md). Separate guides cover
[Debian clients](docs/CLIENT-DEBIAN.md) and
[native Windows clients](docs/CLIENT-WINDOWS.md), including Bash and PowerShell
automation. Use the [operations runbook](docs/OPERATIONS.md) for monitoring,
capacity, credential rotation, TLS renewal, cold copies, and recovery exercises.

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
- [Security model](docs/SECURITY.md)
- [Public protocol](docs/PROTOCOL.md)
- [Restore testing](docs/RESTORE-TEST.md)

Licensed under the [GNU AGPL-3.0](LICENSE).
