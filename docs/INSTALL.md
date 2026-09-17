# Install on a dedicated Linux server

For a new, production-tested installation on Debian 13 AMD64, follow the
step-by-step [Debian installation guide](INSTALL-DEBIAN.md). This document
covers the architecture, clean reinstalls, and migration of existing data.

The supplied systemd units run two processes under separate users:

```text
client -> HTTPS receiver -> writer over Unix socket -> archive and catalog
```

The service units are running in production on Debian 13. The Docker isolation
test also verifies the Unix permissions independently. These permissions
protect the archive from the receiver, but they do not provide WORM immutability
against root or the writer, which must create files and update the catalog. See
[SECURITY.md](SECURITY.md).

## Users and paths

| Component | User | Primary group | Supplementary groups |
|---|---|---|---|
| Writer | `onlybackup-writer` | `onlybackup-ingest` | none |
| Receiver | `onlybackup-receiver` | `onlybackup-ingest` | `onlybackup-receiver` for TLS |

The writer owns `/var/lib/onlybackup` with mode 0700 and accesses `incoming`,
`backups`, and `metadata.db`. The receiver can open only the writer's socket.

- `/run/onlybackup`: `onlybackup-writer:onlybackup-ingest`, mode 0750; socket
  mode 0660.
- `/etc/onlybackup`: `root:onlybackup-receiver`, mode 0750; TLS private key
  mode 0640.
- Private age key: stored separately and never required by the services.

The socket directory and writer lock must not be writable by the receiver. Do
not grant the receiver ACLs or group memberships that allow it to traverse the
archive directory.

## New installation

Run these commands from the extracted binary archive, or from the source tree
after `make build`:

```bash
sudo groupadd --system onlybackup-ingest
sudo groupadd --system onlybackup-receiver
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --gid onlybackup-ingest onlybackup-writer
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --gid onlybackup-ingest --groups onlybackup-receiver onlybackup-receiver
sudo install -m 0755 bin/onlybackup bin/onlybackup-admin bin/onlybackup-inspect bin/onlybackup-recover bin/onlybackup-receiver bin/onlybackup-writer /usr/local/bin/
sudo install -d -o onlybackup-writer -g onlybackup-ingest -m 0700 /var/lib/onlybackup
sudo -u onlybackup-writer /usr/local/bin/onlybackup-admin --state /var/lib/onlybackup init
sudo install -d -o root -g onlybackup-receiver -m 0750 /etc/onlybackup
```

Create a deposit credential, for example with the `M` profile:

```bash
sudo -u onlybackup-writer /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup keys create \
  --name primary-client --profile M \
  --out /var/lib/onlybackup/send-primary-client.key
```

Transfer the `.key` file to the client over a secure channel and keep it with
mode 0600. After verifying the copy, remove the plaintext credential file from
the server; only its hash remains in the catalog. Never leave it accessible to
the receiver.

Install `onlybackup` and `onlybackup-recover` on the client, then create and
store the age identity separately:

```bash
onlybackup-recover keygen --out age-identity.txt > age-public.json
chmod 0600 age-identity.txt
```

Encrypted backups cannot be recovered without `age-identity.txt`. Copy the
`recipient` value from `age-public.json` to `encrypt_to` in the client
configuration. Never install the private identity on the deposit server.

Install the TLS certificate and private key as `/etc/onlybackup/server.crt` and
`/etc/onlybackup/server.key`, owned by `root:onlybackup-receiver`, with mode 0640
on the private key. Then install and start the services:

```bash
sudo install -m 0644 deploy/onlybackup-writer.service deploy/onlybackup-receiver.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now onlybackup-writer.service onlybackup-receiver.service
```

Systemd creates `/run/onlybackup`. The writer uses `PrivateNetwork=yes` and has
write access to the state. The receiver exposes HTTPS on port 8443, accesses the
Unix socket, and has `/var/lib/onlybackup` marked as inaccessible. Configure the
firewall and certificate renewal for your environment.

Create `client.json` on the client, with `send.key` in the same directory:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send.key",
  "encrypt_to": "age1..."
}
```

Run an initial upload and retain its JSON receipt:

```bash
onlybackup send --config client.json \
  --description "Initial production backup" backup.sql > receipt.json
```

## Clean reinstall of a disposable environment

Use this procedure only when the existing state contains no backups, profiles,
or credentials that must be preserved. The commands affect only OnlyBackup
services and paths and do not require a server reboot.

Stop the old units and disable the optional vault:

```bash
sudo systemctl disable --now onlybackup-receiver.service onlybackup-writer.service
sudo systemctl disable --now onlybackup-vault.service 2>/dev/null || true
```

Remove the disposable state and only the components no longer distributed:

```bash
sudo rm -rf -- /var/lib/onlybackup
sudo rm -f -- /etc/systemd/system/onlybackup-vault.service
sudo rm -f -- /usr/local/bin/onlybackup-vault
sudo systemctl daemon-reload
```

Install the programs and both current units together by following "New
installation", then initialize `/var/lib/onlybackup` as
`onlybackup-writer:onlybackup-ingest`. Existing users and groups can be reused.
Remove the writer from the old vault group if necessary:

```bash
sudo gpasswd --delete onlybackup-writer onlybackup-vault-ingest 2>/dev/null || true
```

Keep the TLS certificate in `/etc/onlybackup` if it is still valid. Create a new
profile and deposit credential because credentials from the old catalog are not
present in the newly initialized one. After updating the client, start and
verify both services:

```bash
sudo systemctl enable --now onlybackup-writer.service onlybackup-receiver.service
sudo systemctl status onlybackup-writer.service onlybackup-receiver.service --no-pager
```

## Migrate from a vault-based version

Removing `onlybackup-vault` does not change the backup format or state path.
Schedule a maintenance window and:

1. Stop the receiver, writer, and old vault.
2. Create and verify a cold copy of the entire state directory, including any
   `metadata.db-wal` and `metadata.db-shm` files.
3. Install all current programs and both current units together.
4. Recursively assign the state to `onlybackup-writer:onlybackup-ingest`, keeping
   directories at 0700, the database at 0600, and backups at 0400.
5. Remove ACLs and groups that granted access to old components, disable
   `onlybackup-vault.service`, then start the writer and receiver.
6. Verify an idempotent replay, a new upload, and recovery from the copy.

Never run the old vault and current writer against the same archive. The
`writer.lock` file prevents concurrent use by compatible programs but does not
replace an orderly shutdown and process check.

## SQLite schema and existing archives

Current programs open v1, v2, v3, and v4 catalogs in read-only mode. The first
write access upgrades older catalogs to v4 in transactions, progressively
adding `content_format`, the idempotency key, and the attempt history required
for the rolling 24-hour window. Expired attempt rows may be removed when the
same key is next admitted.

The v3-to-v4 migration imports the one attempt that can be reconstructed from
`backups.started_at`. Older retries that were already overwritten cannot be
reconstructed.

Existing backup files are not changed or re-encrypted. Do not open a v4 schema
with older programs. Downgrading requires the complete pre-upgrade copy; there
is no automatic reverse migration.

## Production checks and cold copies

Verify the following on the dedicated server:

- both services start and accept an HTTPS upload with certificate validation;
- the receiver cannot read, modify, rename, or delete the archive or catalog;
- over-quota uploads, low disk space, interrupted uploads, and reconciliation
  behave as expected;
- plaintext and encrypted files can be recovered from an independent copy.

For a consistent cold copy, stop the receiver and writer, run no administrative
commands during the copy, copy the complete state, and restart both services.
`make system-test` exercises this sequence with temporary archives.

The application allows 15 seconds for an orderly shutdown before canceling
connections and waiting for handlers. Systemd allows 25 seconds. An I/O stall
beyond that period can result in SIGKILL and require reconciliation at restart.
The absence of a receipt does not prove that a deposit is absent; retry the same
operation with the same idempotency key.
