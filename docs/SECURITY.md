# Security model and verified boundaries

OnlyBackup exposes one public deposit-only endpoint. A client credential cannot
read, list, modify, or delete backups through the protocol.

## Process isolation

The receiver terminates TLS and streams requests to the writer over a Unix
socket. It does not open backup files, the catalog, or persistent credentials.
The writer authenticates requests, applies quota, rate, and concurrency limits,
records attempts, verifies size and SHA-256, publishes files, and updates
SQLite.

The systemd units use separate users. `/var/lib/onlybackup` has mode 0700 and is
owned by the writer. The receiver shares only the group required to open the
0660 socket. This separation limits the effect of a compromised Internet-facing
process, provided no additional ACLs, groups, or mounts grant access.

The writer is a trusted component with full archive access. Mode 0400, publication
without replacement, and the absence of destructive API operations protect
against normal upload behavior and accidental errors. They do not prevent root
or a party with full control of the writer account from modifying or deleting
data. Protect against that risk with independent WORM storage or an offline copy.

| Threat | Protection and limitation |
|---|---|
| Stolen deposit credential | It can upload within quota but cannot read or delete through the API. It can consume quota with unwanted data. |
| Compromised receiver | With the supplied units, it cannot access the archive. It can observe credentials and data in transit; encrypt sensitive content on the client. |
| Compromised writer or root | It can alter the archive; OnlyBackup does not claim immutability for this scenario. |
| Disk access | Plaintext backups are readable. Age-encrypted files require the separately stored private key; metadata, sizes, and timestamps remain visible. |
| Interrupted deposit | Length and hash checks, fsync, catalog commit, and reconciliation prevent a positive receipt for an incomplete file. |
| Invalid application dump | The checksum cannot detect a logically bad dump. Run periodic imports and application-level checks. |

## Interruptions, finalization, and retries

The receiver allows at most 32 forwarded requests at once. If the writer
disconnects while the receiver waits on a stalled client body, the Unix socket
error stops that read and releases the slot. An early writer response is read in
full, within a 64 KiB and five-second limit, before the request body is stopped.
This preserves the authoritative JSON response. A real client disconnection
cancels forwarding and releases resources.

The writer does not use HTTP context cancellation to abort a finalization that
can still complete. Once all bytes have been received and verified, it finishes
durable publication and updates the catalog even if the response can no longer
reach the client. If bytes are missing, it marks the attempt as failed, removes
the temporary file, and releases the reservation and slot. The admitted attempt
remains in the rolling 24-hour history. Older rows that no longer affect limits
are removed on the next admission for the same key.

Publication uses a hard link and never replaces an existing path. After
publication, a directory-sync or catalog error produces an unconfirmed outcome
and does not remove the final file. At restart, the writer verifies the file,
size, and digest and completes the record. Replaying a completed v2 request
returns the original receipt without a new body, file, or counted attempt. A key
reused with different data, or while still in progress, returns a conflict.

Write and finalization failures caused by insufficient space return HTTP 507
with explicit JSON. Other errors that make the outcome uncertain never produce
a positive receipt.

## Encryption

The client normally encrypts with age for an X25519 recipient. Plaintext upload
requires `--plaintext` or `plaintext: true`. Without an age recipient or
explicit plaintext consent, the client fails before connecting. The private
key, created with `onlybackup-recover keygen`, is not required by the services;
store and test it separately.

The client rejects deposit credentials and age identities with permissions that
are too broad. On Linux, group and other users must have no access; files created
by the program use mode 0600. On Windows, only the current user, LocalSystem,
and Administrators may own or appear in the DACL; files created by the program
receive an equivalent protected DACL. These checks do not replace disk
encryption or protect against a local administrator.

Recovery verifies encrypted input through a temporary copy, then decrypts and
publishes a new file only after complete verification. Go module dependencies
are pinned with checksums; this is not equivalent to an independent security
audit.

Client-side encryption also uses a complete temporary file before upload. A
handled error removes it, but power loss or forced termination can leave a
private residual file. Place the temporary directory on NTFS or a private Unix
directory, monitor its capacity, and inspect residuals without weakening ACLs.

## Operational limits

OnlyBackup does not automatically delete completed backups and has no supported
retention or pruning command. Quotas reject new deposits when exhausted; they
do not reclaim space. Manual removal of archive files or catalog rows is
unsupported and can destroy consistency.

A replacement credential starts a separate per-key quota history. Rotation
must therefore account for both old and new key usage when sizing physical
storage. Revocation prevents future deposits but does not remove existing
backups.

TLS renewal, credential rotation, capacity monitoring, cold copies, and restore
exercises are operator responsibilities. Follow [OPERATIONS.md](OPERATIONS.md)
and protect against writer or root compromise with an independent WORM backend
or offline copy.

## Reproducible checks

- `make check`: Go test suite, static analysis, and current vulnerability scan.
- `make race`: concurrency, interruption, idempotency, and quota regressions.
- `make smoke`: real programs, HTTPS, Unix socket, catalog, and recovery.
- `make system-test`: two processes, encrypted and plaintext deposits,
  revocation, restart, and cold copy.
- `make isolation-test`: separate Linux users in a network-isolated container;
  the receiver actually attempts to read and alter the archive.
- `make mysql-test`: temporary MariaDB dump and import; see
  [RESTORE-TEST.md](RESTORE-TEST.md).
- `.github/workflows/client-windows.yml`: client, ACL, encryption, and recovery
  tests on Windows Server 2022, plus native AMD64 builds.

The Docker test verifies Linux filesystem permissions, not WORM storage or
resistance to root and physical disk failures. Production deployment separately
validates the supplied systemd units. No test can guarantee the recoverability
of future backups. Windows tests do not cover every desktop release, non-NTFS
filesystems, or local enterprise policies.

## Compatibility

Existing backup content remains unchanged. Schema v4 adds the attempt history
required for the rolling window and imports the one reconstructable
`started_at` value for each v3 backup. Expired attempts can be removed on the
next admission for the same key. Opening v1-v3 catalogs read-only does not
migrate or write them and uses the best available count from backup records.
Create a cold copy before upgrading and upgrade all programs together.
