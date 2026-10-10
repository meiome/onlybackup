# Production operations

This runbook covers recurring checks after installation. It does not replace
application-specific backup or restore procedures. Existing profile/key queries
may run as `onlybackup-writer`; retention mutations must run as root. The
administrative CLI may be used locally or through SSH, Mosh, pipes, and scripts,
but the Unix sockets and state must remain local to the server and must not be
exposed over the network.

## Monitoring cadence

At least daily, verify service state, recent errors, filesystem capacity, and
the most recent deposits:

```bash
sudo systemctl is-active \
  onlybackup-writer.service \
  onlybackup-receiver.service \
  onlybackup-maintenance.service

sudo journalctl \
  -u onlybackup-writer.service \
  -u onlybackup-receiver.service \
  -u onlybackup-maintenance.service \
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

OnlyBackup retention defaults to automatic on a new archive; deletion waits
for successful mail proof, learning and a completed valid regular check.
`automation setup --retention manual` opts out explicitly, and `retention pause`
persists an administrative pause. An upgrade preserves
existing enablement and blocks: it does not turn on deletion in a previously
disabled archive, and it does not automatically turn off an enabled one. Inspect
the actual state with:

```bash
sudo onlybackup-admin --state /var/lib/onlybackup automation status
sudo onlybackup-admin --state /var/lib/onlybackup retention simulate
```

Automatic cleanup starts only at 80% physical filesystem use, never at 79.99%,
and each cycle targets at least 10% of total filesystem capacity. It keeps the
current day, each key's configured minimum of complete days, the last available copy, and 48 hours
of quarantine. The forecast includes every expected copy, learned growth, and
a 20% margin, using the largest rolling 48-hour window in the next week. If the
legal candidates are insufficient, OnlyBackup requests all of them and mails
the remaining shortfall. Quarantined bytes still consume physical space and
quota. Profile totals remain admission limits and do not create disk capacity.
See [RETENTION.md](RETENTION.md) for activation, manual selection, recovery,
anomaly blocks, and the exact state machine. Manually deleting `.backup` files,
quarantine files, or catalog rows remains unsupported and triggers an anomaly.

`mail test` queues a message; the persisted successful delivery shown by
`automation status` is required for activation. Automatic mode needs no later
`retention enable` or `resume`. `automation setup` records the activation mode,
resets mail proof and learning, cancels deferred resume and rejects pending
operations, so use it only for initial configuration or an intentional reset.
Omitting `--retention` preserves the existing choice; explicitly changing the
mode requires its matching confirmation. Setup invalidates checks already in
progress, so old results cannot replace the new configuration.

A retention request receives a provisional ticket. The final validation commits
permission for that single physical move or unlink; later revocation stops new
permissions but does not cancel committed work. Quarantine and purge need
separate permissions. Maintenance resumes committed work before new monitoring
and selection. After a crash it may wait up to ten minutes for the old protocol
lease. Its directory lock prevents two physical executors, but a stuck executor
has no timed replacement. An uncertain physical outcome remains blocked until
verified or repaired. An intact source allows retry but leaves the
`maintenance-operation` error open until physical confirmation or valid
cancellation. Verification can resolve `operation-uncertain` independently. A retry before final
permission may bypass only its own error, after reconciliation and a fresh
check with reliable models; every independent hold and current policy check
remains effective. This does not release retention for other operations.
See the retention guide before restarting a stuck cycle.

SHA-256 verification is complete and cancellable. Both
maintenance client and server allow long `/v1/reconcile` and `/v1/confirm`
requests; short requests remain limited to two minutes. Lease renewal continues
and STOP cancels verification. Check installed binary support before relying on
this behavior. For filesystem layout upgrades, follow the offline
[inventory, migration and rollback procedure](INSTALL.md#upgrade): keep all
three services stopped, migrate by rename and install matching units. Archive
paths are `STATE/archives/{backups,quarantine}`; maintenance has writable
`STATE/archives` and `STATE/maintenance`, and cannot access the catalog, sidecars
or incoming. Writer keeps writable whole state without separate child mounts.

`retention pause` persists independently of automatic anomaly blocks until an
explicit successful `retention resume`. Excluding an incident does not remove
the administrative pause. Inspect `manual_paused` and `manual_pause_reason` in
`automation status`.

Use `retention resume --when-ready` to request a single release after the next
valid check, including on an existing disabled archive. The request persists in
`metadata.db`; it does not bypass mail proof, models, anomalies or uncertain
operations. New pauses, blocking incidents, quarantine cancellations, missing
file acknowledgements, setup and relearning cancel the request. A later pause
always needs a new explicit resume. `activation_required`, `activation_pending`
and `resume_pending` distinguish first activation from deferred release.

Setup, enable, pause and resume ask for an action-specific confirmation. For
scripts, use `--confirm` with the exact text listed in [RETENTION.md](RETENTION.md),
for example `retention resume --when-ready --confirm PRENOTA`. The writer also
checks the API `confirmation` field. Confirmation records the administrator's
choice immediately; destructive work still waits for its safety prerequisites.
Exclusions and maintenance recovery cannot release a pending deferred request.

Set a key's minimum with `retention minimum --key KEY_ID --days 30`. Learning
never recalculates this administrator-owned value. The status field
`key_retention_days` shows the effective minima; insufficient capacity is
reported without reducing them. See the retention guide for migration behavior.

Plan storage from backup size, frequency, intended retention period, growth,
temporary incoming data, cold copies, and a free-space safety margin. Increasing
a profile limit does not create physical disk capacity.

Quota usage is tracked per credential. Creating a replacement credential starts
a separate quota history; it does not transfer the old credential's used-byte
total. Include all current and revoked credentials when estimating physical
archive growth.

Before capacity becomes critical, expand the filesystem or migrate the complete
state during a maintenance window. The configured per-key minima are never
automatically reduced to accommodate growth; insufficient capacity for those
minima blocks deletion and requires administrative action. The separate 48-hour
free-space margin warning keeps sending alerts but allows retention to reclaim
space, subject to all other holds and the normal quarantine protections.

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

1. Stop the receiver, writer, and maintenance service and verify all are inactive.
2. Confirm that no OnlyBackup process or administrative command still has the
   state open.
3. Copy the complete `/var/lib/onlybackup` tree—including `archives/backups`, `archives/quarantine`, the
   maintenance state, and `metadata.db.v4.cold-copy` when present—to independent storage with a
   tool that preserves metadata.
4. Record the copy time, release version, total size, file count, and a digest
   manifest protected separately from the copy.
5. Restart writer, receiver, and maintenance and verify a new upload and the
   automation status.
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

With an offline copy, use the receipt, copied `.backup` file, and age identity
directly, or `onlybackup-recover --state DIRECTORY --id ID` when the copied
catalog marks the backup `complete`. A pending quarantine request (`deleting`)
must be cancelled through the live administrative console before state-based
recovery. A quarantined file needs a live administrative restore before purge;
`purging` and `deleted` cannot be recovered from that state. An offline copy
may still contain its file, so inspect it and use receipt/file recovery without
assuming the live writer or maintenance service is available.

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
