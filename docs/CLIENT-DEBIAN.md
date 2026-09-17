# OnlyBackup client on Debian

This guide installs the native client on Debian AMD64, configures encryption
and credentials, and automates uploads of files that have already been created.
For server setup, see [INSTALL-DEBIAN.md](INSTALL-DEBIAN.md).

## Requirements

The client uses three separate elements:

- `onlybackup` encrypts the file, calculates its digest, authenticates the
  request, and uploads it;
- a `send.key` credential authorizes deposits but cannot perform an upload by
  itself;
- a public age recipient identifies the private identity that can decrypt the
  backup.

`age-identity.txt` is the private recovery key. Never store it on the deposit
server or confuse it with `send.key`.

For production backups, create a dedicated server profile and credential named
for the computer or workload. Do not reuse test credentials or share one key
between independent computers.

## 1. Verify and install the client

Download the Linux archive and `SHA256SUMS` from the same release. Verify the
checksum and attestation before extracting it:

```bash
sha256sum --check SHA256SUMS
gh attestation verify onlybackup_VERSION_linux_amd64.tar.gz \
  --repo meiome/onlybackup
tar -xzf onlybackup_VERSION_linux_amd64.tar.gz
```

Install the programs for the current user:

```bash
install -d -m 0700 "$HOME/.local/bin"
install -m 0755 \
  onlybackup_VERSION_linux_amd64/bin/onlybackup \
  onlybackup_VERSION_linux_amd64/bin/onlybackup-recover \
  "$HOME/.local/bin/"
```

Make sure `$HOME/.local/bin` is in `PATH`, then run:

```bash
onlybackup --help
onlybackup-recover --help
```

## 2. Prepare private directories

```bash
install -d -m 0700 "$HOME/.config/onlybackup"
install -d -m 0700 "$HOME/.local/state/onlybackup/receipts"
```

Copy these files into the configuration directory:

- the new production credential as `send-production.key`;
- the server's public certificate as `server.crt` when it uses a private CA.

```bash
install -m 0600 send-production.key \
  "$HOME/.config/onlybackup/send-production.key"
install -m 0644 server.crt \
  "$HOME/.config/onlybackup/server.crt"
```

After verifying the copy, remove the transferable credential file from the
server. Only its hash remains in the server catalog.

## 3. Create or import the age identity

If you do not already have a recovery identity:

```bash
onlybackup-recover keygen \
  --out "$HOME/.config/onlybackup/age-identity.txt" \
  > "$HOME/.config/onlybackup/age-public.json"
chmod 0600 "$HOME/.config/onlybackup/age-identity.txt"
```

Keep a second protected copy of `age-identity.txt`. Encrypted backups cannot be
recovered if every copy is lost. If a valid identity already exists, do not
regenerate it; copy its public recipient into the client configuration.

## 4. Configure the client

Create `$HOME/.config/onlybackup/client.json`, replacing the URL and recipient:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send-production.key",
  "ca_file": "server.crt",
  "encrypt_to": "age1..."
}
```

Relative paths are resolved from the directory containing `client.json`. Omit
`ca_file` when the certificate is issued by a CA already trusted by the system.

```bash
chmod 0600 "$HOME/.config/onlybackup/client.json"
```

## 5. Upload a file

The client accepts one prepared regular file. It does not create dumps or
snapshots, compress data, select directories, or schedule jobs.

```bash
onlybackup send \
  --config "$HOME/.config/onlybackup/client.json" \
  --description "Daily accounting backup" \
  --receipt "$HOME/.local/state/onlybackup/receipts/manual-receipt.json" \
  /srv/export/accounting.sql
```

The receipt is written only after server confirmation, and only to a new file.
The client never overwrites an existing receipt.

## 6. Automate uploads with Bash and cron

The release includes `scripts/backup-file.sh`. Install it with:

```bash
install -m 0755 scripts/backup-file.sh "$HOME/.local/bin/onlybackup-file"
```

Run it as follows:

```bash
onlybackup-file \
  /srv/export/accounting.sql \
  "$HOME/.config/onlybackup/client.json" \
  "$HOME/.local/state/onlybackup/receipts" \
  "Daily accounting backup"
```

The script creates a unique receipt name and delegates encryption, hashing,
TLS, and upload to the client. A cron entry with absolute paths can look like
this:

```cron
0 2 * * * /usr/bin/flock -n /home/user/.local/state/onlybackup/send.lock /home/user/.local/bin/onlybackup-file /srv/export/accounting.sql /home/user/.config/onlybackup/client.json /home/user/.local/state/onlybackup/receipts "Daily accounting backup" >>/home/user/.local/state/onlybackup/client.log 2>&1
```

The process that creates the source file must finish before the upload starts.
For databases, use an application-consistent dump or snapshot. The release also
includes `scripts/backup-mysql.sh` for MySQL.

## 7. Test recovery

The API is deposit-only: the client cannot download backups from the server. A
recovery test requires an independent copy of the `.backup` file, its receipt,
and the age identity.

```bash
onlybackup-recover \
  --receipt receipt.json \
  --archive BACKUP_ID.backup \
  --identity-file "$HOME/.config/onlybackup/age-identity.txt" \
  --out recovered-file
```

Compare the recovered file with the original or validate it with the relevant
application. Recovery directly from a complete copy of the Linux state is also
available with `--state DIRECTORY --id ID`.

## Checklist

- The credential is unique to this production workload and has mode 0600.
- The age identity also has a protected copy outside the sending computer.
- TLS verification is enabled, with no option that bypasses certificate checks.
- Receipts are retained and included in recovery procedures.
- Automation has been run manually as the same user that runs `cron`.
- Recovery from an independent copy is tested regularly.
