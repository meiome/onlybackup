# Contributing to OnlyBackup

Changes should preserve the deposit-only public protocol, service isolation,
verified publication without replacement, and conservative retention safeguards.
Read [the security model](docs/SECURITY.md) and
[the retention guide](docs/RETENTION.md) before changing those components.

## Build and validation

Use Linux with Go as specified in `go.mod`, GCC, Python 3, OpenSSL, and curl.
The commands below use temporary test archives; never point tests at production
state or use real deposit credentials or private age identities.

```bash
make check
make race
make smoke
make system-test
```

For filesystem permissions or service isolation changes, also run
`make isolation-test` with Docker available. For database backup or restore
changes, run `make mysql-test`; see [the restore guide](docs/RESTORE-TEST.md).
Windows CI tests the native clients, installer, and private file ACLs.

Linux CI runs check, race, smoke, and system tests on pushes to `main` and pull
requests. Adding a workflow does not make its checks mandatory for merging;
repository branch rules are configured separately.

## Proposing a change

Keep changes focused. Describe the concrete problem, resulting behavior, and
validation in the pull request. Include a regression test for a corrected bug
and update the relevant guide when commands or operational behavior change.

Retention minima belong to the administrator and must not be derived or lowered
automatically. Manual pauses must survive anomaly resolution and restarts.
Already committed final permissions retain their documented meaning; quarantine
and purge require separate permissions. Test migrations for preserved settings
and rollback on failure, and document any upgrade requirements.

For ordinary bugs, include the version or commit, operating system, reproduction
steps, and expected and actual behavior. Remove credentials, private keys,
personal data, and sensitive archive metadata from examples and logs. For a
suspected vulnerability, follow [the security guidance](docs/SECURITY.md#reporting-a-vulnerability).
