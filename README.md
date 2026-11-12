# OnlyBackup

[![Server Linux](https://github.com/meiome/onlybackup/actions/workflows/server-linux.yml/badge.svg?branch=main)](https://github.com/meiome/onlybackup/actions/workflows/server-linux.yml)
[![Client Windows](https://github.com/meiome/onlybackup/actions/workflows/client-windows.yml/badge.svg?branch=main)](https://github.com/meiome/onlybackup/actions/workflows/client-windows.yml)
[![Release](https://img.shields.io/github/v/release/meiome/onlybackup)](https://github.com/meiome/onlybackup/releases/latest)
[![License: AGPL-3.0](https://img.shields.io/github/license/meiome/onlybackup)](LICENSE)

OnlyBackup is a **deposit-only** backup server for Linux, with native Linux and
Windows clients.

Clients can upload new files, but the public API cannot read, modify, or delete
them. A compromised client or a stolen upload credential can add backups, but
cannot use the API to read, alter or remove the ones already stored.

## How it works

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

A separate maintenance service monitors expected backups, sends mail reports
and, once enabled, applies retention when the disk fills up. Destructive
operations require durable, verified permissions; see the
[retention guide](docs/RETENTION.md).

> **Status:** OnlyBackup has been used in production on Debian 13 since
> September 2026. The deposit path, protocol and archive format are stable
> within the 0.5 series; automatic retention is newer and documented in detail
> in the [retention guide](docs/RETENTION.md). Upgrades replace all binaries and
> systemd units together; see the [upgrade procedure](docs/INSTALL.md#upgrade).

## Quick start

1. Download a release and verify it as described in
   [Verifiable releases](#verifiable-releases).
2. Install the server by following the [installation guide](docs/INSTALL.md).
   It covers users, systemd units, TLS, profiles and deposit credentials.
3. Set up a [Debian](docs/CLIENT-DEBIAN.md) or
   [Windows](docs/CLIENT-WINDOWS.md) client. A client configuration looks like
   this:

   ```json
   {
     "url": "https://backup.example.com:8443",
     "key_file": "send.key",
     "encrypt_to": "age1..."
   }
   ```

4. Send a file:

   ```bash
   onlybackup send --config client.json \
     --description "Daily accounting backup" backup.sql
   ```

The command returns a JSON receipt only after the server verifies and stores the
backup. Use `--plaintext` only when you intentionally want the server to store
readable content. OnlyBackup accepts files that have already been prepared; use
[scripts/backup-mysql.sh](scripts/backup-mysql.sh) to create MySQL dumps.

Upgrading from a version before 0.5.0 requires an offline archive migration
with all services stopped; follow the
[upgrade procedure](docs/INSTALL.md#upgrade).

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

Require an explicit `OK` line for the downloaded Linux archive. The checksum
file also lists the Windows archive, which need not be present. The checksum
detects accidental changes. The attestation links the archive to the workflow
and commit that produced it; it is not a guarantee that the software has no
vulnerabilities.

On Windows, verify `onlybackup_VERSION_windows_amd64.zip` in the same way. The
complete PowerShell commands are in the
[Windows client guide](docs/CLIENT-WINDOWS.md).

Then extract the Linux archive and enter its directory:

```bash
tar -xzf onlybackup_VERSION_linux_amd64.tar.gz
cd onlybackup_VERSION_linux_amd64
```

## Documentation

- [Installation, upgrade and migration](docs/INSTALL.md)
- [Debian client](docs/CLIENT-DEBIAN.md)
- [Native Windows client](docs/CLIENT-WINDOWS.md)
- [Production operations](docs/OPERATIONS.md)
- [Monitoring and retention](docs/RETENTION.md)
- [Security model](docs/SECURITY.md)
- [Public protocol](docs/PROTOCOL.md)
- [Restore testing](docs/RESTORE-TEST.md)
- [Contributing](CONTRIBUTING.md)
- [Release changes](CHANGELOG.md)

## Build and test from source

Building requires Linux, Go 1.27.4 or later, and GCC.

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

## License

Licensed under the [GNU AGPL-3.0](LICENSE).
