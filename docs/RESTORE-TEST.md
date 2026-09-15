# Prova di ripristino MariaDB isolata

`scripts/mysql-restore-test.sh` verifica il percorso completo con MariaDB reale:
crea dati e relazioni noti, produce due dump tramite `scripts/backup-mysql.sh`, li
deposita via HTTPS in chiaro e con cifratura age attraversando receiver e writer,
arresta entrambi i processi, recupera entrambi i
contenuti localmente e li importa in un secondo database temporaneo. Confronta i
dati, i caratteri UTF-8, gli importi aggregati e il vincolo esterno.

La prova richiede i binari del progetto già compilati, Bash, OpenSSL, curl,
Python 3 e MariaDB con `mariadbd`, `mariadb-install-db`, `mariadb` e
`mysqldump` nei percorsi Debian usati dallo script. Esecuzione:

```sh
make mysql-test
```

Le due istanze MariaDB usano datadir indipendenti sotto una directory creata da
`mktemp`. Accettano soltanto connessioni su socket Unix con `--skip-networking`.
Ogni invocazione diretta usa `--no-defaults`; il vero `mysqldump` viene eseguito
da un wrapper temporaneo con un unico `--defaults-file` che indica esclusivamente
la socket sorgente. La prova non legge socket, credenziali o database MariaDB
dell'host.

Receiver e writer girano con lo stesso utente nel test MariaDB. La separazione
effettiva degli UID e il rifiuto delle operazioni del receiver sull'archivio
richiedono la prova di isolamento dedicata.

Il trap arresta soltanto i PID avviati dallo script e rimuove soltanto la propria
directory `/tmp/onlybackup-mysql-restore.*`. Impostando
`KEEP_MYSQL_RESTORE_FILES=1` conserva i file per una diagnosi. Questi contengono
dati di prova, chiavi temporanee e copie del dump e devono poi essere rimossi con
cura.

Il risultato dimostra un ripristino applicativo del piccolo fixture sulla
versione MariaDB riportata a fine esecuzione. Non certifica compatibilità con
MySQL 8, dump di produzione, procedure, trigger, viste, grandi volumi o altre
configurazioni SQL. Nessun dump non fidato viene importato nel server di deposito:
la destinazione è una seconda istanza effimera e isolata.
