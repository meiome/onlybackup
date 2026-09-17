# Production operations

This runbook covers recurring checks after installation. It does not replace
application-specific backup or restore procedures. Run administrative commands
locally as `onlybackup-writer`; do not expose the administrative tools or state
directory over the network.

## Monitoring cadence

At least daily, verify service state, recent errors, filesystem capacity, and
the most recent deposits:

```bash
sudo systemctl is-active \
  onlybackup-writer.service \
  onlybackup-receiver.service

sudo journalctl \
  -u onlybackup-writer.service \
  -u onlybackup-receiver.service \
  --since '-24 hours' \
  --priority warning \
  --no-pager

df -h /var/lib/onlybackup
df -i /var/lib/onlybackup

sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-inspect \
  --state /var/lib/onlybackup list --limit 20
```

Investigate an unexpected gap, repeated failed attempt, low-space response, or
service restart. A successful scheduler exit on a client is not sufficient by
itself: retain the receipt and periodically compare it with server metadata.

At least weekly, inspect every active credential and its quota:

```bash
sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup keys list

sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-inspect \
  --state /var/lib/onlybackup quota --key KEY_ID
```

Also check that `incoming` contains no old partial files. A recent file may
belong to an active upload; correlate it with the catalog and journal before
taking action. Do not manually delete state files while services are running.

## Capacity and retention

OnlyBackup does not implement automatic retention or a supported command for
deleting completed backups. Profile totals are admission limits, not cleanup
policies. Manually removing `.backup` files or catalog rows breaks consistency
and is unsupported.

Plan storage from backup size, frequency, intended retention period, growth,
temporary incoming data, cold copies, and a free-space safety margin. Increasing
a profile limit does not create physical disk capacity.

Quota usage is tracked per credential. Creating a replacement credential starts
a separate quota history; it does not transfer the old credential's used-byte
total. Include all current and revoked credentials when estimating physical
archive growth.

Before capacity becomes critical, expand the filesystem or migrate the complete
state during a maintenance window. If a shorter retention period is required,
design and test a separate lifecycle procedure before production use; do not
improvise deletion in the archive.

## Credential rotation

Rotate a deposit credential after suspected exposure and on the organization's
normal secret-rotation schedule. A credential cannot be reprinted from the
catalog, so rotation creates a new one.

1. Record the old key ID and profile from `keys list`.
2. Create a new credential in a new file without printing its value:

   ```bash
   sudo -u onlybackup-writer \
     /usr/local/bin/onlybackup-admin \
     --state /var/lib/onlybackup keys create \
     --name CLIENT_NAME_NEXT \
     --profile PROFILE_NAME \
     --out /tmp/onlybackup-send-next.key

   sudo install -o "$(id -un)" -g "$(id -gn)" -m 0600 \
     /tmp/onlybackup-send-next.key \
     ./onlybackup-send-next.key
   sudo rm -f -- /tmp/onlybackup-send-next.key
   ```

3. Transfer `./onlybackup-send-next.key` through a trusted channel, install it
   with private permissions, and remove that transferable server copy.
4. Update the client configuration atomically and perform a small encrypted
   upload. Retain and verify its receipt.
5. Revoke the old credential only after every intended client has switched:

   ```bash
   sudo -u onlybackup-writer \
     /usr/local/bin/onlybackup-admin \
     --state /var/lib/onlybackup keys revoke --id OLD_KEY_ID
   ```

6. Confirm that the old credential is rejected and remove all remaining copies
   from clients, staging directories, password stores, and transfer media.

Revocation prevents new deposits but does not remove backups already associated
with the old key. For suspected theft, revoke first and accept the brief outage
rather than waiting for the replacement test.

## TLS certificate renewal

Monitor expiry with sufficient lead time:

```bash
sudo openssl x509 \
  -in /etc/onlybackup/server.crt \
  -checkend 2592000 -noout
```

Before replacement, verify the new certificate's validity period, issuer, and
Subject Alternative Name. Confirm that its public key matches the private key.
Install both with the ownership and mode documented in [INSTALL.md](INSTALL.md),
then restart only the receiver and inspect its journal:

```bash
sudo systemctl restart onlybackup-receiver.service
sudo systemctl status onlybackup-receiver.service --no-pager
sudo journalctl \
  -u onlybackup-receiver.service \
  --since '-10 minutes' --no-pager
```

Perform a small client upload after renewal. If the issuing CA changes, deploy
the new trust chain to clients as a coordinated change; otherwise they will
correctly reject the renewed certificate.

## Cold copies

A consistent state copy contains the catalog, any SQLite sidecar files, incoming
state, completed backups, ownership, modes, ACLs, and extended attributes.

1. Stop the receiver and writer and verify both are inactive.
2. Confirm that no OnlyBackup process or administrative command still has the
   state open.
3. Copy the complete `/var/lib/onlybackup` tree to independent storage with a
   tool that preserves metadata.
4. Record the copy time, release version, total size, file count, and a digest
   manifest protected separately from the copy.
5. Restart writer and receiver and verify a new upload.
6. Test recovery from the copy on another path or system; never use the live
   archive as the recovery-test workspace.

Encrypt and physically separate cold copies according to the data's sensitivity.
A copy on the same writable filesystem does not protect against root compromise,
ransomware, controller failure, or site loss.

## Recovery exercises

Run a representative recovery on a fixed schedule and after significant
upgrades. A complete exercise verifies:

- the receipt matches the copied archive's size and SHA-256;
- the age identity decrypts the selected backup;
- the recovered file matches the expected type and structure;
- a database dump imports into an isolated disposable database;
- application-level rows, relationships, and invariants are correct;
- recovery time and required operator steps are recorded.

Do not keep the only age identity on an upload client or deposit server. Test
the protected copy that would actually be used during a disaster.

## Incident handling

For an interrupted or uncertain upload, preserve the client log and any receipt,
then inspect the catalog and journal. The absence of a receipt does not prove
that the backup is absent; protocol v2 clients retry with the same idempotency
key during the same operation.

For unexpected archive changes, stop public ingestion, preserve a cold forensic
copy, and avoid repair commands until the catalog, filesystem, and logs have
been examined together. OnlyBackup does not claim immutability against root or
a fully compromised writer; independent WORM storage or an offline copy is the
recovery boundary for that threat.
