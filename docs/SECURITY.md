# Sicurezza e confini verificati

OnlyBackup ha un endpoint pubblico di solo deposito. L'assenza di download e
cancellazione riduce i poteri della credenziale client, ma le garanzie sul disco
dipendono dalla modalità di esecuzione e dai permessi Linux.

## Modalità protetta

Il receiver parla HTTP su HTTPS e inoltra alla socket writer. Il writer usa
`--vault-socket` e non apre l'archivio. Il vault, con un utente distinto, riceve
un envelope limitato e byte tramite socket Unix: nessuna operazione, nome fisico,
ID, profilo o percorso scelto dal writer. Verifica autonomamente credenziale,
quote, dimensione, hash e spazio disponibile. Solo il vault possiede contenuti,
database, chiavi memorizzate come hash e metadati necessari al recupero.

La logica fidata di salvataggio è condivisa con la modalità locale per mantenere
un'unica implementazione di pubblicazione e riconciliazione. Nel vault non c'è
un parser HTTP di rete: il protocollo privato costruisce una richiesta interna
validata per quella logica. Il vault rimane un componente fidato con accesso
completo all'archivio; la separazione non elimina questa fiducia.

| Minaccia | Protezione e limite |
|---|---|
| Credenziale di deposito rubata | Può inviare entro quota; non può elencare, leggere, modificare o cancellare via protocollo. Può consumare quota con dati inutili. |
| Receiver compromesso | Con le unità fornite non accede all'archivio o alla socket vault. Può osservare credenziali e dati in transito, quindi i file sensibili vanno cifrati dal client. |
| Writer compromesso senza root | Con utenti/permessi corretti non può leggere o alterare contenuti e catalogo. Può inoltrare nuovi depositi usando credenziali viste e disturbare il servizio. |
| Vault o root compromesso | Non coperto da immutabilità: può cancellare l'archivio. Occorre storage indipendente con WORM/retention o una copia offline per un livello ulteriore. |
| Lettura del disco del server | I file in chiaro sono leggibili. I file cifrati con age richiedono la chiave privata, se questa è custodita fuori dal server. Nome, descrizione, dimensioni e tempi restano osservabili. |
| Interruzione o corruzione del deposito | Hash, dimensione, fsync, commit e riconciliazione impediscono il normale successo di un invio incompleto. La ricevuta persa resta un esito incerto. |
| Dump applicativo già errato | Il checksum non lo scopre. Servono prove di importazione e controlli dei dati; il server non esegue i contenuti ricevuti. |

Il controllo di accesso alla socket privata dipende da utenti e gruppi Unix,
non da una firma del writer. Ogni processo a cui si concede accesso alla socket
può tentare depositi; occorre comunque una credenziale valida verificata dal vault.
Non esistono ricevute firmate autonomamente o protezione antireplay che impedisca
a chi possiede una credenziale di creare depositi duplicati.

## Protezione amministrativa del server

La sicurezza del vault dipende anche dalla difficoltà di ottenere privilegi
amministrativi sul server. Se è disponibile una console sicura del provider o
un accesso fisico affidabile, è preferibile disabilitare SSH. Se SSH è necessario,
esporlo solo tramite VPN o a indirizzi autorizzati, disabilitare password e login
diretto di root, usare chiavi protette da passphrase e limitare i tentativi di
accesso. Mantenere sistema e servizi aggiornati e concedere `sudo` soltanto agli
amministratori che ne hanno necessità.

L'amministratore può inoltre configurare Object Lock, storage WORM, retention
non modificabile o una copia offline indipendente, se l'infrastruttura scelta li
supporta. Queste misure aggiungono protezione dopo una compromissione di root:
i permessi `0400` e la separazione dei processi, da soli, non possono fermarla.

## Cifratura

Cifratura richiesta normalmente dal client con la libreria age e destinatario X25519.
L'invio in chiaro richiede `--plaintext` o `plaintext: true`; senza destinatario age
o consenso esplicito il client fallisce prima della connessione. La chiave pubblica
consente di cifrare nuovi file, non di decifrare i precedenti. La chiave privata,
generata con `onlybackup-recover keygen`,
non è richiesta ai processi di deposito. Custodirne una copia separata e provarla.
Le dipendenze sono fissate con checksum nel modulo Go; non è un audit indipendente
del codice crittografico o dell'intero prodotto.

Il client crea soltanto un temporaneo cifrato e non passa all'invio in chiaro se
la cifratura fallisce. Il recupero verifica il cifrato su una copia temporanea,
poi decifra e pubblica un file nuovo solo a verifica conclusa. Il contenuto
originale sul client e quello recuperato localmente restano in chiaro.

## Prove riproducibili

- `make race`: suite Go con controllo delle corse, incluse quote concorrenti,
  idempotenza, framing ostile, SHA-256, riconciliazione e SIGTERM durante un upload reale.
- `make check`: suite e analisi statica.
- `make smoke`: compatibilità della modalità locale e delle API precedenti.
- `make system-test`: sette binari, HTTPS, vault, cifratura/chiaro esplicito,
  recupero, revoca, riavvio e copia a freddo di contenuti e catalogo.
- `make isolation-test`: utenti Linux distinti in un contenitore senza rete
  esterna; tentativi reali di lettura, modifica, chmod, unlink, rename e
  sostituzione negati a writer e receiver. Receiver escluso dalla socket vault.
- `make mysql-test`: dump/import MariaDB su istanze temporanee, sia in chiaro
  sia cifrato; dettagli e limiti in [RESTORE-TEST](RESTORE-TEST.md).

La prova Docker verifica il filesystem Linux, non l'avvio delle unità systemd
né uno storage fisico WORM. Non sono ancora dimostrati guasti reali del disco,
resistenza a root, audit esterno o prestazioni a carico produttivo. I test non
certificano la recuperabilità di ogni backup futuro.

## Compatibilità

La modalità writer locale senza vault mantiene l'accesso diretto all'archivio:
non protegge da un writer compromesso. Usare la catena protetta e i permessi di
[INSTALL](INSTALL.md) per ottenere il confine descritto sopra.
Il contenuto dei vecchi backup resta invariato; la migrazione del catalogo
aggiunge solo il formato e richiede una copia a freddo prima dell'aggiornamento.
