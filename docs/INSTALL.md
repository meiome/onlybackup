# Install and migrate on Debian 13 AMD64

This guide installs the two OnlyBackup services on a dedicated Debian 13
server. The procedure is used in production on Debian 13 (`x86_64`, glibc 2.41,
systemd 257).

The same document also covers clean reinstalls, upgrades, and migration from an
older installation that includes `onlybackup-vault`. Do not combine fragments
from older installation guides.

## Before you start

You need:

- SSH access through an administrative account authorized to use `sudo`;
- a Debian 13 AMD64 server with enough space for the archive and temporary
  files;
- a TLS certificate valid for the DNS name or IP address used by clients;
- a trusted upload computer for the deposit credential and a separate protected
  recovery location for the age identity;
- a release archive and `SHA256SUMS` downloaded from the same release.

The server listens on TCP port 8443. Do not install the private age key on the
server; without it, the deposit server cannot decrypt encrypted backups.

The commands below assume that you have extracted the release archive and are
in the `onlybackup_VERSION_linux_amd64` directory.

## 1. Verify the release on a trusted computer

Download both files from the same GitHub release:

- `onlybackup_VERSION_linux_amd64.tar.gz`;
- `SHA256SUMS`.

Use a recent, authenticated GitHub CLI. Confirm that `gh attestation` is
available because older Debian packages may not include it.

```bash
gh auth status
gh attestation --help >/dev/null
sha256sum --check SHA256SUMS
gh attestation verify onlybackup_VERSION_linux_amd64.tar.gz \
  --repo meiome/onlybackup
```

Do not install an archive if either verification fails. Extract it only after
both checks pass:

```bash
tar -xzf onlybackup_VERSION_linux_amd64.tar.gz
cd onlybackup_VERSION_linux_amd64
file bin/*
```

## 2. Check the Debian server

Before changing the server, verify its identity, architecture, operating
system, available space, and any existing OnlyBackup installation:

```bash
hostnamectl
uname -m
getconf GNU_LIBC_VERSION
df -h /var/lib /var/tmp
systemctl list-unit-files 'onlybackup-*'
getent passwd onlybackup-writer onlybackup-receiver
getent group onlybackup-ingest onlybackup-receiver
```

This guide expects Debian 13 on `x86_64` with no OnlyBackup units already
installed. If services or data already exist, do not initialize the state; use
the clean-reinstall or migration section later in this document.

Install only the required system tools:

```bash
sudo apt-get update
sudo apt-get install --no-install-recommends ca-certificates file openssl
```

## 3. Create isolated users and groups

```bash
sudo groupadd --system onlybackup-ingest
sudo groupadd --system onlybackup-receiver

sudo useradd \
  --system \
  --no-create-home \
  --shell /usr/sbin/nologin \
  --gid onlybackup-ingest \
  onlybackup-writer

sudo useradd \
  --system \
  --no-create-home \
  --shell /usr/sbin/nologin \
  --gid onlybackup-ingest \
  --groups onlybackup-receiver \
  onlybackup-receiver
```

Verify the accounts before continuing:

```bash
id onlybackup-writer
id onlybackup-receiver
```

The writer must belong only to `onlybackup-ingest`. The receiver must use
`onlybackup-ingest` as its primary group and `onlybackup-receiver` as a
supplementary group.

## 4. Install programs and systemd units

Run these commands from the root of the extracted archive:

```bash
sudo install -m 0755 \
  bin/onlybackup \
  bin/onlybackup-admin \
  bin/onlybackup-inspect \
  bin/onlybackup-recover \
  bin/onlybackup-receiver \
  bin/onlybackup-writer \
  /usr/local/bin/

sudo install -m 0644 \
  deploy/onlybackup-writer.service \
  deploy/onlybackup-receiver.service \
  /etc/systemd/system/

sudo systemd-analyze verify \
  /etc/systemd/system/onlybackup-writer.service \
  /etc/systemd/system/onlybackup-receiver.service

sudo systemctl daemon-reload
```

`systemd-analyze verify` must complete without errors.

## 5. Initialize state

Run these commands only for a new installation. Never run `init` over an
archive that must be preserved.

```bash
sudo install -d \
  -o onlybackup-writer \
  -g onlybackup-ingest \
  -m 0700 \
  /var/lib/onlybackup

sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup init
```

Check ownership and permissions:

```bash
sudo stat -c '%A %a %U:%G %n' \
  /var/lib/onlybackup \
  /var/lib/onlybackup/incoming \
  /var/lib/onlybackup/backups \
  /var/lib/onlybackup/metadata.db
```

The three directories must have mode `0700`, and the database must have mode
`0600`. Everything must belong to `onlybackup-writer:onlybackup-ingest`.

## 6. Install the TLS certificate

Install the certificate and private key obtained from your CA:

```bash
sudo install -d -o root -g onlybackup-receiver -m 0750 /etc/onlybackup
sudo install -o root -g onlybackup-receiver -m 0640 server.crt /etc/onlybackup/server.crt
sudo install -o root -g onlybackup-receiver -m 0640 server.key /etc/onlybackup/server.key
```

Check the validity dates and Subject Alternative Name. The SAN must contain the
exact DNS name or IP address used in the client URL:

```bash
sudo openssl x509 \
  -in /etc/onlybackup/server.crt \
  -noout -subject -issuer -dates -ext subjectAltName

sudo openssl x509 \
  -in /etc/onlybackup/server.crt \
  -checkend 0 -noout

sudo -u onlybackup-receiver test \
  -r /etc/onlybackup/server.crt \
  -a -r /etc/onlybackup/server.key
```

For a self-signed certificate, copy only the public `server.crt` to each client
and configure it as `ca_file`. Never copy `server.key` to a client.

## 7. Create a profile and deposit credential

List the default profiles or create a dedicated one. This example allows 200
GiB in total, 100 GiB per backup, 24 uploads in each rolling 24-hour window,
and two concurrent uploads:

```bash
sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup profiles set \
  --name BACKUP \
  --total 200GiB \
  --max-backup 100GiB \
  --daily 24 \
  --concurrent 2
```

Create a credential without printing it to the terminal:

```bash
sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup keys create \
  --name primary-client \
  --profile BACKUP \
  --out /tmp/onlybackup-send.key

sudo install -o "$(id -un)" -g "$(id -gn)" -m 0600 \
  /tmp/onlybackup-send.key \
  ./onlybackup-send.key

sudo rm -f -- /tmp/onlybackup-send.key
```

Transfer `onlybackup-send.key` from the current directory to the trusted
computer over SSH, verify its mode is `0600`, and remove the transferable copy
from the server. Only the credential hash remains in the catalog.

## 8. Start and verify the services

```bash
sudo systemctl enable --now \
  onlybackup-writer.service \
  onlybackup-receiver.service

sudo systemctl status \
  onlybackup-writer.service \
  onlybackup-receiver.service \
  --no-pager

sudo ss -ltnp 'sport = :8443'
sudo journalctl \
  -u onlybackup-writer.service \
  -u onlybackup-receiver.service \
  --since '-10 minutes' \
  --no-pager
```

Also verify that the receiver cannot traverse or read the state directory:

```bash
sudo -u onlybackup-receiver test ! -x /var/lib/onlybackup
sudo -u onlybackup-receiver test ! -r /var/lib/onlybackup/metadata.db
```

Allow TCP port 8443 through the firewall only from networks or addresses that
need to upload backups. OnlyBackup does not change firewall configuration.

## 9. Prepare a client

Complete setup, automation, and recovery instructions are available for the
[Debian client](CLIENT-DEBIAN.md) and the
[native Windows client](CLIENT-WINDOWS.md).

On a trusted recovery computer, install `onlybackup-recover` from the same
release and create the age identity once:

```bash
onlybackup-recover keygen --out age-identity.txt > age-public.json
chmod 0600 age-identity.txt onlybackup-send.key
```

Keep `age-identity.txt` separate from the deposit server. Create `client.json`
on the upload computer with the public recipient from `age-public.json`. Do not
copy the private identity to an upload-only computer:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "onlybackup-send.key",
  "ca_file": "server.crt",
  "encrypt_to": "age1..."
}
```

Upload a small file and retain the receipt:

```bash
onlybackup send \
  --config client.json \
  --description "Initial production verification" \
  sample-file.bin > receipt.json
```

On the server, verify that the ID, size, and SHA-256 digest match the receipt
and that the backup file has mode `0400`:

```bash
sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin --state /var/lib/onlybackup \
  backups status --id RECEIPT_ID

sudo stat -c '%A %a %U:%G %s %n' \
  /var/lib/onlybackup/backups/RECEIPT_ID.backup

sudo sha256sum \
  /var/lib/onlybackup/backups/RECEIPT_ID.backup
```

Complete verification by recovering from a cold copy of the entire
`/var/lib/onlybackup` directory and comparing the decrypted file with the
original. See [RESTORE-TEST.md](RESTORE-TEST.md) for the automated MariaDB
restore test.

## Clean reinstall of a disposable environment

Use this procedure only when the existing state contains no backups, profiles,
or credentials that must be preserved. Stop the current units and any legacy
vault, then verify that every removal target belongs exclusively to OnlyBackup:

```bash
sudo systemctl disable --now \
  onlybackup-receiver.service \
  onlybackup-writer.service
sudo systemctl disable --now onlybackup-vault.service 2>/dev/null || true

sudo rm -rf -- /var/lib/onlybackup
sudo rm -f -- /etc/systemd/system/onlybackup-vault.service
sudo rm -f -- /usr/local/bin/onlybackup-vault
sudo gpasswd --delete \
  onlybackup-writer onlybackup-vault-ingest 2>/dev/null || true
sudo systemctl daemon-reload
```

Keep `/etc/onlybackup` only when its certificate and key are still valid. Reuse
the current users and groups, repeat steps 4 through 8, initialize a new state,
and create a new profile and credential. A credential from the deleted catalog
cannot be recovered or imported into the new catalog.

## Migrate from a vault-based installation

Removing `onlybackup-vault` does not change the backup format or state path.
Schedule a maintenance window and:

1. Stop the receiver, writer, and old vault and confirm that no process still
   has the state open.
2. Create and verify a cold copy of the complete state, including
   `metadata.db-wal` and `metadata.db-shm` when present.
3. Install all six current programs and both current systemd units together.
4. Recursively assign the state to
   `onlybackup-writer:onlybackup-ingest`, keeping directories at `0700`, the
   database at `0600`, and completed backups at `0400`.
5. Remove ACLs and groups that granted access to the old vault, disable and
   remove `onlybackup-vault.service`, and reload systemd.
6. Start the writer and receiver, then verify a replay, a new upload, catalog
   inspection, and recovery from the independent copy.

Never run the old vault and the current writer against the same archive. The
writer lock does not replace an orderly shutdown and process check.

## SQLite schema and existing archives

Current programs open v1, v2, v3, and v4 catalogs read-only. The first write
access upgrades an older catalog transactionally to v4, progressively adding
the content format, idempotency key, and rolling attempt history.

The v3-to-v4 migration imports the one attempt reconstructable from
`backups.started_at`; older retries that were already overwritten cannot be
reconstructed. Existing backup files are not changed or re-encrypted.

Do not open a v4 catalog with older programs. There is no automatic downgrade;
rollback requires the complete verified copy made before the upgrade.

## Upgrade

Before upgrading, verify the new release as described in step 1 and create a
complete cold copy of the state. Then:

1. Stop the receiver and writer.
2. Install all six programs and both systemd units from the new release.
3. Run `systemd-analyze verify` and `systemctl daemon-reload`.
4. Start the writer and receiver.
5. Check the units, port, journal, a new upload, and recovery.

Do not run `init` again, delete the state, or open an upgraded catalog with
older programs. Use the dedicated sections above for a clean reinstall or
migration from an older vault-based architecture.

## Completion criteria

The installation is ready when:

- the writer and receiver are active and enabled;
- port 8443 is listening and TLS validates the client URL hostname;
- the receiver cannot read the archive or catalog;
- no transferable copy of the credential remains on the server;
- an encrypted upload, its receipt, archive file, catalog entry, and SHA-256
  digest are consistent;
- recovery from a cold copy produces a file identical to the original;
- the service journals contain no relevant errors.

Continue with the monitoring, certificate renewal, credential rotation,
capacity, cold-copy, and recovery schedule in [OPERATIONS.md](OPERATIONS.md).
