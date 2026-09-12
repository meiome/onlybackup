# OnlyBackup

OnlyBackup è un server di backup **deposit-only** per Linux: i client possono
inviare nuovi file, ma l'API pubblica non permette di leggerli, modificarli o
cancellarli.

Il trasferimento usa HTTPS con TLS 1.3. Normalmente il client cifra il contenuto
con [age](https://age-encryption.org/) prima dell'invio; il salvataggio in chiaro
deve essere scelto esplicitamente. Receiver, writer e vault possono essere
eseguiti con utenti Linux separati, così solo il vault accede all'archivio.

```text
client → HTTPS receiver → socket Unix → writer → socket privata → vault
                                                               ├─ file .backup
                                                               └─ SQLite
```

> **Stato:** progetto in sviluppo. Suite, race detector, test di sistema e
> isolamento sono disponibili; le unità systemd devono ancora essere collaudate
> su un server dedicato reale.

## Prova rapida

Servono Linux, Go 1.27 o successivo e GCC.

```bash
make build
make check
make smoke
```

`make smoke` avvia un ambiente HTTPS temporaneo, invia e recupera un backup,
verifica integrità e API vietate, quindi rimuove i dati di prova.

## Installazione e uso

Per installare i tre servizi protetti seguire
[docs/INSTALL.md](docs/INSTALL.md). Dopo aver ricevuto dal server il file della
credenziale e il destinatario pubblico age:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send.key",
  "encrypt_to": "age1..."
}
```

```bash
bin/onlybackup send --config client.json \
  --description "Backup gestionale" backup.sql
```

Il comando restituisce una ricevuta JSON soltanto dopo il salvataggio verificato.
Per conservare volutamente il file leggibile sul server usare `--plaintext`.
OnlyBackup riceve file già preparati; per MySQL è disponibile
[scripts/backup-mysql.sh](scripts/backup-mysql.sh).

## Documentazione

- [Installazione protetta](docs/INSTALL.md)
- [Modello di sicurezza](docs/SECURITY.md)
- [Protocollo pubblico](docs/PROTOCOL.md)
- [Protocollo privato del vault](docs/VAULT-PROTOCOL.md)
- [Prove di ripristino](docs/RESTORE-TEST.md)

Distribuito con licenza [GNU AGPL-3.0](LICENSE).
