# Sicurezza e confini verificati

OnlyBackup espone un endpoint pubblico di solo deposito. La credenziale client
non può leggere, elencare, modificare o cancellare backup tramite il protocollo.

## Separazione dei processi

Il receiver termina TLS e inoltra in streaming alla socket Unix del writer. Non
apre file di backup, catalogo o credenziali persistenti. Il writer autentica,
applica quote, frequenza e concorrenza, registra i tentativi, verifica dimensione
e SHA-256, pubblica i file e aggiorna SQLite.

Le unità systemd usano utenti distinti. `/var/lib/onlybackup` è 0700 e appartiene
al writer; il receiver condivide soltanto il gruppo necessario ad aprire la
socket 0660. Questa separazione limita una compromissione del processo esposto a
Internet, purché non siano presenti ACL, gruppi o mount aggiuntivi.

Il writer è un componente fidato con accesso completo all'archivio. I file 0400,
la pubblicazione senza sostituzione e l'assenza di API distruttive proteggono dal
comportamento ordinario e dagli errori del percorso di upload. Non impediscono a
root o a chi controlla completamente l'utente writer di modificare o cancellare
i dati. Per quel rischio serve una copia offline o storage WORM indipendente.

| Minaccia | Protezione e limite |
|---|---|
| Credenziale di deposito rubata | Può inviare entro quota, ma non leggere o cancellare via API. Può consumare quota con dati inutili. |
| Receiver compromesso | Non accede all'archivio con le unità fornite. Può osservare credenziali e dati in transito; cifrare sul client i contenuti sensibili. |
| Writer o root compromesso | Può alterare l'archivio; questo scenario non è coperto da immutabilità. |
| Lettura del disco | I file in chiaro sono leggibili. I file age richiedono la chiave privata custodita altrove; metadati, dimensioni e tempi restano visibili. |
| Interruzione del deposito | Dimensione, hash, fsync, commit e riconciliazione impediscono una ricevuta positiva per un file incompleto. |
| Dump applicativo già errato | Il checksum non lo rileva. Servono prove periodiche di importazione e controlli applicativi. |

## Interruzioni, finalizzazione e retry

Il receiver limita a 32 gli inoltri concorrenti. Se il writer si disconnette
mentre il receiver attende un corpo client bloccato, l'errore sulla socket Unix
interrompe quella lettura e libera lo slot. Una risposta anticipata del writer
viene letta interamente entro 64 KiB e 5 secondi prima di interrompere il corpo,
così il suo JSON autorevole non viene perso. Una reale disconnessione client
cancella l'inoltro e libera le risorse.

Il writer non usa la cancellazione del contesto HTTP per annullare una
finalizzazione già possibile. Se ha ricevuto e verificato tutti i byte, completa
pubblicazione durevole e catalogo anche quando la risposta non può più arrivare
al client. Se mancano byte, marca il tentativo fallito, elimina il temporaneo e
libera prenotazione e slot. Il tentativo ammesso resta nello storico della
finestra mobile di 24 ore; le righe più vecchie, non più usate dai limiti,
vengono eliminate alla successiva ammissione della stessa chiave.

La pubblicazione usa un hard link e non sostituisce un percorso esistente. Dopo
la pubblicazione, un errore di sincronizzazione o del catalogo produce un esito
non confermato e non elimina il file finale. Al riavvio il writer verifica file,
dimensione e digest e completa il record. Un replay v2 già completo restituisce
la ricevuta originale senza nuovo corpo, file o tentativo; una chiave riutilizzata
con dati differenti o ancora in corso restituisce conflitto.

Gli errori di scrittura e di finalizzazione dovuti a spazio esaurito sono distinti
dagli errori di lettura del client e arrivano come HTTP 507 con JSON esplicito.
Altri errori che rendono incerto l'esito non producono una ricevuta positiva.

## Cifratura

Il client cifra normalmente con age e destinatario X25519. L'invio in chiaro
richiede `--plaintext` o `plaintext: true`; senza destinatario age o consenso
esplicito il client fallisce prima della connessione. La chiave privata, generata
con `onlybackup-recover keygen`, non è richiesta ai servizi e va custodita e
provata separatamente.

Il client rifiuta chiavi di deposito e identità age con permessi troppo ampi.
Su Linux richiede che gruppo e altri non abbiano accesso; i file creati dal
programma sono limitati a 0600. Su Windows verifica il proprietario corrente e
accetta nella DACL soltanto l'utente, LocalSystem e Administrators; i file
creati dal programma ricevono una DACL protetta equivalente. Questa verifica
non sostituisce la cifratura del disco né protegge da un amministratore locale.

Il recupero verifica il cifrato su una copia temporanea, poi decifra e pubblica
un file nuovo solo a verifica conclusa. Le dipendenze sono fissate con checksum
nel modulo Go; questo non equivale a un audit indipendente.

## Prove riproducibili

- `make check`: suite Go, analisi statica e controllo vulnerabilità aggiornato.
- `make race`: regressioni di concorrenza, interruzione, idempotenza e quote.
- `make smoke`: binari reali, HTTPS, socket Unix, catalogo e recupero.
- `make system-test`: due processi, cifratura/chiaro, revoca, riavvio e copia a freddo.
- `make isolation-test`: utenti Linux distinti in un contenitore senza rete; il receiver tenta realmente di leggere e alterare l'archivio.
- `make mysql-test`: dump e import MariaDB temporanei; dettagli in [RESTORE-TEST](RESTORE-TEST.md).
- `.github/workflows/client-windows.yml`: test di client, ACL, cifratura e
  recupero su Windows Server 2022, più compilazione degli eseguibili AMD64.

La prova Docker verifica i permessi del filesystem Linux, non l'avvio delle unità
systemd né storage WORM. Non dimostra resistenza a root, guasti fisici reali o
recuperabilità di ogni backup futuro. La prova Windows non copre tutte le
versioni desktop, i filesystem non NTFS o le policy aziendali locali.

## Compatibilità

Il contenuto dei vecchi backup resta invariato. Lo schema v4 aggiunge lo storico
dei tentativi necessario alla finestra mobile e importa una volta il
`started_at` ricostruibile per ogni backup v3. I tentativi scaduti possono essere
rimossi alla successiva ammissione della stessa chiave. L'apertura readonly dei
cataloghi v1-v3 non migra né scrive e usa il miglior conteggio disponibile dai
record dei backup. Eseguire una copia a freddo prima dell'aggiornamento e
aggiornare insieme tutti i binari.
