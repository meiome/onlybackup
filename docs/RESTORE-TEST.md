# Isolated MariaDB recovery test

`scripts/mysql-restore-test.sh` verifies the complete workflow with real
MariaDB instances. It creates known data and relationships, produces two dumps
with `scripts/backup-mysql.sh`, deposits them over HTTPS as plaintext and
age-encrypted content through the receiver and writer, stops both processes,
recovers both files locally, and imports them into a second temporary database.
It checks the data, UTF-8 characters, aggregate amounts, and foreign-key
constraint.

The test requires built project programs, Bash, OpenSSL, curl, Python 3, and
MariaDB with `mariadbd`, `mariadb-install-db`, `mariadb`, and `mysqldump` in the
Debian paths used by the script. Run:

```sh
make mysql-test
```

The two MariaDB instances use independent data directories under a directory
created by `mktemp`. They accept only Unix-socket connections with
`--skip-networking`. Every direct invocation uses `--no-defaults`. A temporary
wrapper runs the real `mysqldump` with one `--defaults-file` that identifies
only the source socket. The test does not read MariaDB sockets, credentials, or
databases from the host.

The receiver and writer run as the same user during this MariaDB test. The
dedicated isolation test separately verifies distinct UIDs and rejection of
receiver operations against the archive.

The exit trap stops only the process IDs started by the script and removes only
its own `/tmp/onlybackup-mysql-restore.*` directory. Set
`KEEP_MYSQL_RESTORE_FILES=1` to retain files for diagnosis. Those files contain
test data, temporary keys, and dump copies; remove them carefully afterward.

A passing result demonstrates application-level recovery of the small fixture
with the MariaDB version printed at the end of the run. It does not certify
compatibility with MySQL 8, production dumps, procedures, triggers, views,
large datasets, or other SQL configurations. Untrusted dumps are never imported
on the deposit server; the destination is a second isolated, temporary instance.
