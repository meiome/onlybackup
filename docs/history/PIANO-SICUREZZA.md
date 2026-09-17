# Piano di consolidamento e cifratura opzionale

> **Documento storico.** Conserva il piano approvato l'11 settembre 2026,
> compresa la successiva architettura con vault, ma non descrive la versione
> corrente a due processi. Per le garanzie e le istruzioni correnti leggere
> [SECURITY](../SECURITY.md) e [INSTALL](../INSTALL.md).

Data: 11 settembre 2026.
Stato aggiornato: le quattro fasi applicative sono implementate e collaudate.
Il 12 settembre 2026 una decisione successiva al piano ha reso obbligatoria una
scelta: destinatario age per la cifratura oppure consenso esplicito al chiaro.
Una configurazione priva di entrambe viene rifiutata prima della connessione.
Sono passati test Go/race/vet, deposito e recupero in chiaro/cifrato, isolamento
con UID distinti in Docker e dump/import MariaDB reale lungo la catena vault.
Il collaudo systemd su server dedicato e la protezione WORM da root/vault
restano attività operative o sviluppi separati, non garanzie già fornite.
Per lo stato effettivo leggere [SECURITY](../SECURITY.md) e
[OPERATIONS](../OPERATIONS.md). Il testo seguente conserva il piano approvato.

## Obiettivi e requisiti

L'utente ha autorizzato il consolidamento, la protezione dei backup completati,
le prove di ripristino e l'aggiunta della cifratura opzionale. Il comportamento
predefinito resta il deposito in chiaro. Ha chiesto di preparare il piano e
valutare sviluppo con GPT-5.6 Sol tramite cambio modello o agente dedicato.

Restano Go, HTTPS obbligatorio, credenziale di solo deposito, recupero locale,
nessuna API pubblica di lettura, elenco, amministrazione o cancellazione.
Nomi fisici dei contenuti: <ID>.backup; nome originale nei metadati del database.
I backup precedenti devono restare leggibili. Nessuna conversione automatica
dei backup esistenti o installazione di servizi sulla macchina di sviluppo.

Baseline verificata l'11 settembre: `./scripts/go.sh test -race ./...` e
`./scripts/go.sh vet ./...` superati. La directory non è un repository Git valido.

## 1. Correggere arresto e gestione degli errori

- Attendere esplicitamente il completamento di Shutdown prima di rilasciare
  database, lock del writer e risorse del processo. Alla scadenza interrompere
  le richieste e attendere la conclusione degli handler prima della chiusura DB.
- Provare SIGTERM durante invio e finalizzazione: una ricevuta positiva deve
  corrispondere a un backup durevole; un invio incompleto non diventa completo.
- Rendere la riconciliazione ripetibile anche dopo un arresto durante la sua
  stessa esecuzione; gestire temporanei orfani senza cancellare file finali.
- Completare test di certificati non attendibili, redirect reali, timeout,
  cancellazione, header duplicati e metadati al limite.
- Mostrare sul client l'attesa della conferma dopo il trasferimento dei byte.

Accettazione: test dei processi reali e test di errore tra pubblicazione e commit,
suite con race detector e analisi statica superati.

## 2. Cifratura opzionale prima dell'invio

Proposta tecnica: usare la libreria Go di age con destinatario a chiave pubblica,
senza inventare algoritmi o formati crittografici. Fissare una release compatibile
e verificarne dipendenze e documentazione prima dell'integrazione.

- Senza opzione: protocollo e contenuto in chiaro attuali.
- Con opzione, ad esempio `--encrypt-to age1...`: il client cifra e invia il
  risultato tramite il normale HTTPS. Supportare la stessa scelta nel JSON client.
- Chiave pubblica sui client; chiave privata di recupero custodita separatamente,
  assente da receiver e writer. Non confonderla con la credenziale di deposito.
- Per conservare il protocollo a dimensione e SHA-256 noti, creare inizialmente
  un temporaneo cifrato con permessi 0600 sul client. Invio dopo chiusura corretta
  del cifratore; nessun fallback silenzioso all'invio in chiaro in caso di errore.
- Quote e ricevuta attestano i byte realmente archiviati, quindi cifrati quando
  l'opzione è attiva. Distinguere chiaramente integrità del deposito e verifica
  del contenuto decifrato. Limitare lo spazio temporaneo e documentarne il costo.
- Aggiungere metadati versionati per il formato, mantenendo compatibilità per
  record e client precedenti. Una dichiarazione di formato non dimostra da sola
  che il client abbia cifrato correttamente o per il destinatario desiderato.
- Recupero: esportazione del contenuto archiviato oppure decifratura esplicita
  con file della chiave privata; output nuovo 0600, pubblicato solo dopo verifica
  completa. Rimuovere l'output incompleto in caso di autenticazione fallita.
- Test: default in chiaro, round trip cifrato, file grande, chiave errata,
  alterazione, troncamento, errori I/O, configurazione invalida, vecchi backup.

Limiti: descrizione e nome nel catalogo restano visibili agli amministratori;
dimensioni e tempi restano osservabili. Il file sorgente sul client resta in chiaro.
La perdita di tutte le copie della chiave privata rende irrecuperabili i file
cifrati; verificare una copia separata della chiave e il suo recupero.
La rotazione cambia destinatario per i nuovi invii, conservando le vecchie chiavi
per leggere i backup precedenti. Cifratura e protezione dalle cancellazioni sono
due controlli distinti.

## 3. Proteggere archivio e catalogo dal writer

Obiettivo di questa fase: compromissione del processo writer senza privilegi root.
I soli permessi 0400 non soddisfano questo obiettivo.

Proposta: introdurre un archiviatore locale minimale sotto un utente distinto,
proprietario dell'archivio definitivo e del catalogo autorevole di recupero.
Il writer gestisce l'ingresso ma non può modificare o cancellare l'archivio.
La socket privata dell'archiviatore offre soltanto un'operazione di deposito,
con controlli propri di dimensione, integrità, spazio e pubblicazione durevole.
Nessun percorso arbitrario, operazione amministrativa o lettura remota.

Prima di implementare definire il contratto tra writer e archiviatore: autenticazione,
limiti non aggirabili, identificativi, retry, proprietà dei file, transazioni e
riconciliazione. Non basta spostare lo stesso writer dietro una seconda socket.
Il componente fidato deve essere piccolo, controllare i byte che riceve e
possedere metadati sufficienti a recuperare senza fidarsi del DB modificabile
dal writer. Evitare hard link da inode ancora modificabili dal processo di ingresso.

La ricevuta positiva arriva solo dopo finalizzazione di contenuto e metadati
protetti. Provare da un processo con identità del writer i tentativi di modifica,
chmod, unlink, rename e alterazione del catalogo. Devono fallire; invii nuovi e
recupero locale autorizzato devono continuare a funzionare.

Questa separazione aggiunge un componente fidato: non garantisce resistenza alla
compromissione dell'archiviatore stesso o di root. Per coprire quel rischio serve
un ulteriore livello indipendente, come storage con retention WORM/Object Lock
o copia offline. Non dichiarare tale garanzia senza backend e prove reali.
L'eventuale migrazione di un archivio reale richiede copia verificata e un piano
di ritorno; iniziare su un archivio nuovo di prova.

## 4. Dimostrare il ripristino

- Prova completa per file in chiaro e cifrati: invio, arresto, riavvio, recupero
  e confronto byte per byte, con ricevute e controlli di integrità coerenti.
- Copia consistente dell'archivio e del catalogo e ripristino su directory nuova;
  includere le condizioni WAL e le chiavi private custodite separatamente.
- Database MySQL usa e getta: dati e relazioni note, dump con lo script reale,
  deposito, recupero e importazione in una seconda istanza isolata; verificare
  dati e relazioni. Usare esclusivamente database creati per la prova.
- Distinguere nei risultati «ricevuto», «integrità verificata» e «ripristino
  applicativo verificato». Non importare automaticamente dump non fidati sul
  server di deposito. La prova MySQL deve usare ambiente effimero e isolato.
- Collaudo separato su Linux dedicato per utenti, unità systemd, permessi,
  saturazione e interruzioni. Dichiarare come non verificate le prove non eseguite.

## Esecuzione proposta

Un agente GPT-5.6 Sol per implementare una fase delimitata alla volta; l'agente
principale cura progetto, revisione delle differenze e validazione indipendente.
Nessuna scrittura concorrente sugli stessi file. Fasi 1 e 2 preparano la baseline;
la fase 3 richiede il contratto definito prima di modificare lo storage; fase 4
verifica l'intero risultato. Aggiornare README, protocollo e istruzioni a ogni fase.

Questo piano può essere usato anche cambiando il modello della conversazione
a GPT-5.6 Sol. La scelta dell'agente conserva separati implementazione e revisione.

## Riferimenti

- age, progetto e libreria Go: https://github.com/FiloSottile/age
- Subagenti Codex: https://learn.chatgpt.com/docs/agent-configuration/subagents
- GPT-5.6 Sol: https://developers.openai.com/api/docs/models/gpt-5.6-sol
