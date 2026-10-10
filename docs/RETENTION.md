# Monitoring and retention

New archives enable automatic retention by default, with deletion held until
mail proof, learning and a valid regular check are complete. Administrators
can opt out or pause it explicitly. The current catalog schema is v12. An upgrade
that passes through v4 creates the cold copy `metadata.db.v4.cold-copy`; the
v6-to-v7 migration adds the monitoring history without inventing earlier
checks. The v7-to-v8 migration adds lease generations, invalidates the old
lease, and leaves existing authorized work for physical reconciliation.
The v8-to-v9 migration adds a durable final-permission marker; existing tickets
remain provisional. The v9-to-v10 migration separates manual pauses from automatic
blocks and copies the existing archive minimum to each existing key. It does not
recalculate or reduce that minimum. Since old blocks do not reliably identify
their original cause, every existing block also becomes an administrative hold;
after a regular check, use `retention resume` to release it explicitly.
The v10-to-v11 migration preserves existing enablement, minima and every hold;
it does not infer consent to automatic activation on existing archives. Use
`retention resume --when-ready` to request activation or release an existing
hold after the next valid check, without waiting for the ten-minute window.
The v11-to-v12 migration versions the mail configuration without changing
existing mail proof or retention choices. Each setup advances that version.
Successful test delivery certifies only the version actually used for sending,
checked atomically with the delivery confirmation. If setup changes while a test
is in flight, its delivery is recorded but a test of the current configuration
is still required. Update writer and maintenance together; confirmations without
the configuration version cannot provide mail proof.
Stop maintenance before upgrading and upgrade writer and
maintenance together: older maintenance binaries do not take the execution lock.
The migrations preserve existing enablement and blocks; upgrading does not
enable deletion on an archive where it was disabled. The receiver and existing
clients keep using the unchanged deposit protocol.

The receiver only forwards deposits. The writer owns SQLite and serves separate
deposit, administrator, and maintenance Unix sockets. Maintenance performs
checks and mail delivery and is the only service that moves, restores, or
unlinks completed backups. It never opens SQLite. Its account can access only
`STATE/archives/backups`, `STATE/archives/quarantine`, and `STATE/maintenance`; the receiver is
not in that group and cannot traverse the archive.

## First activation

Run retention administration as root. The CLI supports a local terminal, SSH,
Mosh, piped input, and scripts; the same confirmations, audit records, anomaly
blocks, and quarantine rules apply in every case. Keep the Unix administrative
socket local to the server and rely on the normal SSH access policy when
working remotely.

Configure non-secret SMTP values. Optional credentials live in a protected
JSON file referenced by the maintenance environment, never in arguments,
SQLite, reports, or logs.

```bash
sudo onlybackup-admin --state /var/lib/onlybackup automation setup \
  --smtp-host smtp.example.net --smtp-port 587 --smtp-tls \
  --from onlybackup@example.net --to admin@example.net \
  --timezone Europe/Rome --retention auto

sudo install -d -o root -g onlybackup-maintenance -m 0750 \
  /etc/onlybackup-maintenance
sudo install -o onlybackup-maintenance -g onlybackup-maintenance -m 0600 /dev/null \
  /etc/onlybackup-maintenance/mail-credentials.json
sudoedit /etc/onlybackup-maintenance/mail-credentials.json
# {"username":"onlybackup","password":"..."}
sudo install -o root -g onlybackup-maintenance -m 0640 /dev/null \
  /etc/onlybackup-maintenance/maintenance.env
sudoedit /etc/onlybackup-maintenance/maintenance.env
# ONLYBACKUP_MAIL_CREDENTIALS=/etc/onlybackup-maintenance/mail-credentials.json

sudo systemctl restart onlybackup-maintenance.service
sudo onlybackup-admin --state /var/lib/onlybackup mail test
sudo onlybackup-admin --state /var/lib/onlybackup automation status
```

New archives default to automatic retention. When `--retention` is omitted,
setup preserves the existing enablement and pending initial consent, including
a previous `manual` choice. Use `--retention auto` or `--retention manual` to
explicitly change the mode. Manual mode leaves retention disabled until an
explicit administrative activation. Administrative pauses survive either mode.

Setup and the state commands require an action-specific confirmation. The CLI
explains the action and asks for the exact text, also over SSH, Mosh or pipes.
EOF, a wrong answer, or a mismatched `--confirm` value sends no mutation.
For scripts, provide the same text through `--confirm`:

| Command | Confirmation |
| --- | --- |
| `automation setup` with mode omitted | `CONFIGURA` |
| `automation setup --retention auto` | `CONFIGURA AUTO` |
| `automation setup --retention manual` | `CONFIGURA MANUAL` |
| `retention enable` | `ABILITA` |
| `retention pause` | `SOSPENDI` |
| `retention resume` | `RIPRENDI` |
| `retention resume --when-ready` | `PRENOTA` |

For example, `retention resume --when-ready --confirm PRENOTA` records the
request immediately; no further confirmation is needed when the valid check
completes. Automatic safety blocks still apply without user confirmation.
The writer's administrator API requires the same text in the JSON
`confirmation` field, rejecting missing or mismatched consent before mutation.
A generic `CONFIGURA` does not authorize a payload selecting a new mode.

`mail test` only queues a message. Maintenance must successfully send it before
`mail_tested` becomes true. Existing catalog history is used, but monitoring
remains `APPRENDIMENTO` until 14 complete concordant days are available. With
automatic mode selected, a subsequent completed regular check activates
retention without further `enable` or `resume` commands. Cleanup still starts
only at the disk threshold, and removes the oldest eligible copies first.
Check the maintenance journal if the test mail does not arrive.

Use `automation setup` for initial configuration or an intentional reset only:
it resets mail proof and learning, records an explicitly chosen mode or preserves
the existing choice, cancels deferred resume, and rejects pending retention
operations. It atomically invalidates checks already in progress; their obsolete
results cannot restore old revisions or activate retention. It never clears an
administrative pause. A mode-omitting reset does not create a new automatic
activation consent: if the previous consent was consumed or cancelled, use an
explicitly confirmed `--retention auto` or a deferred resume when appropriate. To opt out after installation:

```bash
sudo onlybackup-admin --state /var/lib/onlybackup retention pause --reason 'pausa amministrativa'
```

## Deferred activation and resume

On an existing archive, or after a manual pause, request a single release:

```bash
sudo onlybackup-admin --state /var/lib/onlybackup retention resume --when-ready
sudo onlybackup-admin --state /var/lib/onlybackup automation status
```

This explicit request enables automation but holds deletion until a subsequent
valid check completes, including a check already running when requested.
Mail proof, reliable current models for every active key, no blocking anomaly,
and no uncertain operation are required. A failed or obsolete check cannot
release a hold. The writer consumes the request atomically with the successful
check; it survives restarts and can wait through learning or an existing incident.
A new blocking incident, a new manual pause, cancellation of a quarantine
request, acknowledgement of a missing file, another setup, or relearning cancels
the deferred request. A new blocking incident also cancels automatic first
activation; after investigation, request a new deferred or immediate resume.
Non-blocking warnings do not cancel it. Use `retention pause` to withdraw a
pending request. Consumed consent never removes a later administrative pause.

`metadata.db`, in `automation_state`, persists `enabled`, `activation_required`
(initial safety hold), `activation_pending` (automatic initial activation),
`manual_paused` and its reason, plus `resume_pending` and
`resume_requested_at`. These fields are exposed by `automation status` (the
request timestamp is named `resume_requested_at_unix` in JSON).
The initial hold and a pending deferred request remain effective even if an
anomaly exclusion clears its own automatic block. Reports distinguish initial waiting, deferred resume, and
administrative pauses.

The immediate `retention resume` command remains available, with its ten-minute
freshness requirement. If installation used manual mode, either use
`retention resume --when-ready` or, after successful mail proof, explicitly
`retention enable` followed by immediate `retention resume` when ready.
`resume` requires a recent regular check, reliable versioned models, no active
blocking anomaly, and no uncertain operation. Non-blocking extra-copy warnings
and the 48-hour free-space margin warning do not prevent it. A legitimate
schedule change uses `monitoring relearn`; it does not silently widen tolerances.

The 72-hour report and anomaly notices include a short HTML view and a plain
text alternative. A large green `TUTTO OK` badge means that no anomaly or
operator action is currently known; the expected initial learning phase is
shown as information. A red `ATTENZIONE` badge marks anomalies, missing first
copies, disabled automation, or a regular monitor that still needs an explicit
`retention resume`. The same status appears at the start of the email subject,
so the inbox can be scanned without opening a green report. The report lists
the latest completed copy per active key,
disk use, retention state, and the next action without exposing key IDs or
raw quota/model internals. Mail-test messages remain plain text.

## Automatic policy

Maintenance checks at startup and every five minutes. It learns per key and
weekday from server completion times and stored byte sizes. The current civil
day is excluded. UTC instants and the saved IANA timezone handle 23/25-hour
days. Schedule tolerance is 30 minutes and size tolerance is 20%; systematic
size decreases require review instead of becoming accepted negative growth.

Every attempt is recorded with start, finish, verified period, model revisions,
outcome, and a short summary. A completed check advances a separate coverage
limit for each key; a crash or failed check does not. At the next start,
maintenance marks an abandoned running attempt as failed and resumes from the
last completed limit. Thus, after a five-day stop, it checks every appointment
in the gap against backups that actually arrived instead of treating the stop
as proof that copies are missing. Model changes split the period at the saved
validity boundary, new keys start at their first reliable model, and revoked
keys require no future coverage. Anomalies and completion are committed in one
writer transaction. Repeating work after a crash uses the same incident keys
and does not create duplicate incidents.

The first real check after a v6-to-v7 upgrade becomes the initial reference;
no rows are reconstructed from `last_check_at`. Completed history is retained
for 90 days. The latest completed checkpoint needed to resume an active
key/revision is kept even when it is older, so a stop longer than 90 days does
not lose continuity.

If that first attempt is interrupted, its planned starting boundary is reused.
If it stopped before planning, its recorded start time anchors the initial
window. Subsequent retries and restarts do not shift that window forward.
A check completing after a concurrent `monitoring relearn` is rejected when
its model revision is obsolete; its findings and coverage are not committed,
and the next cycle uses the new revision.

Inspect the latest 20 attempts, or a larger portion of the retained history,
through the writer's administrative API:

```bash
sudo onlybackup-admin --state /var/lib/onlybackup monitoring checks
sudo onlybackup-admin --state /var/lib/onlybackup monitoring checks --limit 100
```

The outcomes are `completed_clean`, `completed_with_anomalies`, and `failed`;
a live attempt is shown as `running`. Detailed incidents remain in
`monitoring anomalies`.

The administrator sets a minimum number of complete civil days **per key**;
the current day is additionally protected. Configure it before enabling cleanup:

```bash
sudo onlybackup-admin --state /var/lib/onlybackup keys list
sudo onlybackup-admin --state /var/lib/onlybackup retention minimum --key KEY_ID --days 30
sudo onlybackup-admin --state /var/lib/onlybackup automation status
```

`key_retention_days` shows the effective minimum of every key. New archives
default to seven days; upgraded archives preserve the previous archive default
for new keys until an explicit per-key value is set. Accepted values are 7 to
365000 days. Learning and relearning never change these settings. The minimum
applies to every backup associated with that key, including manual deletion.
The writer rechecks it at the request, provisional authorization, and final
permission stages. Increasing a minimum also protects queued work that has not
received final permission; already committed physical operations may finish.

The actual retained history may be much longer than this minimum: no automatic
cleanup begins below the disk trigger. As backup sizes grow, history can shrink
only down to the configured minima. If these minima no longer fit, monitoring
reports the capacity problem and does not lower them.

For each expected copy, the disk forecast applies learned non-negative growth
and a 20% safety margin. It sums multiple copies and uses the largest rolling
48-hour total whose window starts during the next seven days, together with the
writer's unchanged free-space reserve. This forecast guides cleanup volume and
capacity warnings, never the configured minima. Periodic quota checks project
the next unsatisfied scheduled backup and copies still pending within the
30-minute tolerance, using the largest predicted size. Received copies are
matched with the same one-to-one rules used by monitoring, including across
midnight. Predictions include growth accumulated since the model's learning
date; insufficient quota produces an anomaly notice with the available and
predicted bytes, even when physical disk space is still available.

New automatic requests begin only when the filesystem containing `archives/backups` is
at least 80.00% physically occupied. At 79.99% nothing is selected. Each cycle
selects enough of the oldest complete eligible days to release at least 10% of
the filesystem capacity, or more when required to return below 80% after the
48-hour forecast. Pending bytes count toward that amount only while still outside the current
minimum and belonging to a non-revoked key. Current-day
files, active uploads, revoked-key files, days protected by each key's minimum, uncertain
files, and the last recoverable copy of a key are excluded. If every legal
candidate is still insufficient, all legal candidates are requested and a
daily deduplicated anomaly mail reports required, selectable, and missing
bytes. This notification does not add or remove any retention block.

Each file follows:

```text
complete -> deleting -> quarantined -> purging -> deleted
```

The writer records a request and issues a fresh random ticket for one operation.
Maintenance verifies the fixed ID-derived path, regular-file identity, size,
and SHA-256; moves without replacement; syncs directories; and confirms the
ticket. The ticket is initially provisional. After SHA-256 verification, the
final `/v1/validate` transaction checks the current lease, revocation, policy
revision, blocking conditions and uploads, then durably sets
`execution_committed=1`. That is the definitive permission for this one
operation: later revocation, policy changes or lease expiry do not cancel it.
No user command cancels a committed operation. `purge_not_before` is the confirmed
quarantine time plus 48 hours.
Purge needs a new authorization and sync. Quota is freed only after confirmed
unlink. Request, authorization, result, and failures remain in the catalog.
Revocation prevents new permissions. Pending requests are cancelled; provisional
tickets are reconciled before being reset or cancelled. Committed operations
are completed even if the final permission response was lost. After a crash,
maintenance first reconciles the physical state: completed work is confirmed;
an intact source retains its permission and gets a fresh transport ticket;
uncertain state requires repair and is never treated as successful deletion.
Committed work is retried before starting new monitoring/policy decisions.

Every maintenance cycle holds an exclusive kernel `flock` on the existing
`maintenance` directory through reconciliation, physical operations and mail.
Another maintenance process cannot enter even after a protocol lease expires.
Process death releases the lock; a new cycle may still wait for the previous
ten-minute protocol lease to expire. Generations continue to protect protocol
tickets, but lease expiry alone cannot admit a second physical executor.
A hung executor prevents progress until it exits or is restarted. This is a
single-host, local-filesystem design, not distributed failover. Do not replace
the state/maintenance directory or run an old maintenance binary while active.

An anomaly queues immediate mail and is deduplicated between five-minute
checks; unresolved incidents get a daily reminder. Blocking anomalies suspend
new quarantine and purge authorizations. Recovery and reconciliation of
physical work already performed, and completion of final permissions already
granted, remain possible while blocked.

The specific `capacity:quarantine-margin` warning (not enough available space
for the next 48 hours of deposits plus the reserve) remains visible and sends
mail, but does not suspend quarantine or purge. Otherwise the warning would
prevent retention from reclaiming the space it needs. Resolving this warning
does not introduce a new pause. The configured per-key minima, current-day and
last-copy protections, at least 48 hours of quarantine, and all other blocking
conditions still apply. Other capacity incidents, including insufficient space
for configured retention minima and per-key quota forecasts, remain blocking.

This classification also applies to an existing margin incident without changing
the schema or rewriting its history. A previously persisted retention block or
administrative pause is preserved: after a fresh regular check, an explicit
`retention resume` is still required.

If a physical operation reports an error, its `maintenance-operation` anomaly
blocks new destructive work. An intact source permits retry of the authorized
operation, but is not evidence of success: the error incident remains open
until physical completion is confirmed or the operation is validly cancelled.
A committed operation cannot be cancelled. Reconciliation may resolve an
`operation-uncertain` incident after verification establishes a certain physical
state, including an intact source; this does not close the corresponding
`maintenance-operation` error. Confirmed completion or valid cancellation closes
only the matching error. After a fresh complete regular monitoring check, its
operation block may be removed when no other hold remains. Manual pauses,
other incidents and activation requirements retain their normal recovery.
Manual pause and its reason are stored independently of automatic blocks.
Anomaly exclusions, resolutions, regular checks, restarts, and relearning never
clear it. Only a successful administrative `retention resume`, immediate or
explicitly requested with `--when-ready`, releases it.
`automation status` exposes `manual_paused` and `manual_pause_reason`; effective
`deletion_blocked` remains true while either kind of hold is active.
This also covers errors after a physical change, such as a directory sync
failure: verify and confirm the durable physical result before closing the
operation error. Never label an intact source alone as a resolved operation.
Before final permission, an intact-source retry is scoped to that operation:
the writer rechecks the current lease, revision, minima, revocation and upload
state. The operation's own maintenance error may be bypassed only when it is
the sole blocking anomaly, a fresh check and reliable current models are
available, and no administrative or independent hold exists. New operations
remain blocked. Both ticket issuance and final validation enforce these checks;
long source verification retains the freshness decision made when issuing the
ticket. No error-resolution mail is sent until verified completion or valid
cancellation. After a provisional retry succeeds, the next fresh regular check
can release its global hold.

Full verification and STOP:

Full SHA-256 verification must be cancellable and must read the complete file;
large archives must not be accepted using partial hashes. Matching updated
maintenance client and writer server allow long requests on the
`/v1/reconcile` and `/v1/confirm` endpoints without the short-request deadline.
Short maintenance requests retain their two-minute timeout. Lease renewal
continues during long verification; allowing a long request does not disable
lease fencing or the exclusive directory lock. STOP cancels ongoing
verification and its requests, allowing orderly shutdown; interrupted checks
must not be reported as successful physical confirmation.

Install matching updated writer and maintenance binaries together; older binaries
retain the short deadline and non-cancellable verification.

Operational anomalies (`upload`, `time`, and `schedule_missing`) are warnings
on their first occurrence for a key and kind in the rolling 14-day window. The
second unresolved occurrence blocks deletion. Extra copies are reported as
explicit non-blocking warnings as soon as they can no longer satisfy any model
slot, including slots whose 30-minute window crosses midnight. They never block
retention by themselves, even when repeated; quota and disk protections remain
independent. Immediate and periodic checks use the same incident identity, so
one extra copy produces one incident and one initial mail. Physical or catalogue
integrity anomalies, including `file_missing`, block immediately. An
administrator may recognize an operational incident with `monitoring exclude`;
the audited interval must contain the originating anomaly and cannot exceed
seven days. Excluded days neither generate the selected operational findings
nor contribute observations to model learning. Integrity findings can never be
excluded.

Actual arrivals in excluded intervals still satisfy their appointments and are
checked for size anomalies. Excluding one operational kind cannot invent a
missing backup or hide another kind of anomaly.

Copies are matched chronologically to the earliest compatible free slot, with
backup ID breaking equal-time ties. Later arrivals cannot replace an earlier
copy already covering a slot behind a completed monitoring checkpoint. Matching
includes the observed historical prefix independently of report boundaries;
arrival checks consider all affected extra copies and retain their original
event times. Active warning summaries update without duplicating initial mail.
The SHA-256 scanning frequency is unchanged.

Each check covers appointments only up to its recorded start minus the
30-minute arrival tolerance. Appointments becoming due during a long archive
scan remain for the next check, rather than being marked as already checked.

```bash
sudo onlybackup-admin --state /var/lib/onlybackup monitoring exclude \
  --id 152 \
  --from 2026-10-03T00:00:00+02:00 \
  --to 2026-10-05T23:59:59+02:00 \
  --kinds upload,time,schedule_missing,extra \
  --reason 'guasto di rete verificato'
```

Use `monitoring exclusions` to inspect the permanent audit. Findings suppressed
by an exclusion are retained as resolved records and do not send duplicate
alerts.

A physical backup reported as `file_missing` is never cleared merely because
the civil day has changed. Inspect active incidents with `onlybackup-admin monitoring anomalies`,
then acknowledge a confirmed missing file with `onlybackup-admin monitoring
acknowledge --id ID`. The acknowledgement closes the incident but deliberately
keeps deletion blocked; run
`onlybackup-admin retention resume` only after the fresh monitoring check is
regular and the cause has been understood.

## Simulation and manual operations

Simulation has no file or deletion-state side effects:

```bash
sudo onlybackup-admin --state /var/lib/onlybackup retention simulate
```

The guided deletion command lists key names and IDs, then backups, accepts one
backup per run, and requires the exact confirmation `CANCELLA`. Input may come
from a terminal or an intentionally constructed pipe or script:

```bash
sudo onlybackup-admin --state /var/lib/onlybackup cancellazione
```

A manual request may be below 80%, but never inside the key's configured minimum,
for today, the last recoverable copy, or while blocked. The writer rechecks after
selection and before final permission.

Use the quarantine console to cancel a not-yet-authorized request or request a
restore before purge:

```bash
sudo onlybackup-admin --state /var/lib/onlybackup quarantena
```

Cancellation suspends future deletion until `retention resume`. A quarantined
file is recoverable until purge begins; a `deleted` row is historical only.

## Incident checklist

```bash
sudo onlybackup-admin --state /var/lib/onlybackup automation status
sudo systemctl status onlybackup-writer onlybackup-maintenance --no-pager
sudo journalctl -u onlybackup-writer -u onlybackup-maintenance --since -24h
sudo onlybackup-admin --state /var/lib/onlybackup retention pause \
  --reason 'investigazione amministrativa'
```

Never move or unlink archive files manually. Preserve the entire state,
including `archives/backups`, `archives/quarantine`, `maintenance`, SQLite
sidecars, permissions, ACLs, and
the pre-v5 cold copy. An intact source allows retry but leaves a
`maintenance-operation` error open until physical confirmation or valid
cancellation. After that, a complete regular check can remove its block if it
is the only hold; verified uncertainty alone does not close the error. For
other blocking incidents, wait for a complete regular check and run `retention
resume`; do not relearn merely to hide an incident.
