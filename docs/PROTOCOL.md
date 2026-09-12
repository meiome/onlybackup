# Protocollo di deposito v2

Il percorso corrente è `POST /v2/backups`, senza query string. `POST /v1/backups`
rimane disponibile per i client precedenti, che non garantiscono ritenti idempotenti.
Sono richiesti HTTPS, TLS 1.3, file non vuoto e dimensione nota. Non ci sono endpoint
di amministrazione, lettura, elenco, stato, ripristino o cancellazione.

## Richiesta

| Header | Contenuto |
|---|---|
| Authorization | `Bearer <chiave>`; massimo 255 caratteri ASCII stampabili senza spazi. |
| Content-Type | `application/octet-stream` |
| Content-Length | Dimensione positiva del contenuto in byte. |
| X-Onlybackup-Metadata | JSON UTF-8 codificato base64url senza padding. |
| X-Onlybackup-Sha256 | SHA-256 del contenuto, 64 caratteri esadecimali minuscoli. |
| X-Onlybackup-Idempotency-Key | Obbligatoria in v2: chiave casuale dell'operazione, tra 16 e 128 caratteri alfanumerici, `-` o `_`. |
| Expect | Il client usa `100-continue` per consentire il rifiuto prima dell'invio del corpo. |

Il JSON dei metadati richiede `description` e `original_name`; può contenere
`content_format`, omesso o vuoto per il contenuto in chiaro, oppure `age-v1`
per il formato age cifrato dal client. Altri valori sono rifiutati.
La credenziale è trasmessa nell'header di autenticazione.
Dimensione e checksum sono generati automaticamente dal client sui byte archiviati.

```json
{"description":"Backup gestionale","original_name":"gestionale.sql.gz"}
```

Descrizione obbligatoria, massimo 1000 code point Unicode; nome obbligatorio, massimo 255.
Il nome non può essere `.` o `..`, contenere slash, backslash o caratteri di controllo.
Non si applicano filtri alle estensioni. Il client ricava il nome dal file locale se omesso.
I metadati non possono contenere proprietà aggiuntive o JSON concatenati.
Il corpo è il contenuto grezzo, senza multipart, estrazione, conversione o decompressione.
`Content-Encoding` non è accettato. Il nome originale è persistito solo nel database.

I dati transitano nel receiver in streaming, senza copia su disco o accesso all'archivio.
In modalità protetta il writer inoltra al vault tramite il [protocollo privato](VAULT-PROTOCOL.md).
Il vault ripete validazione e autorizzazione, applica le quote dal proprio catalogo
e calcola l'hash ricevuto. In modalità locale tali operazioni restano nel writer.
Le risposte non espongono percorso fisico, credenziale o contenuto del backup.

Con cifratura attiva il corpo è il file age e non il testo originale; quote, lunghezza
e hash attestano quel corpo. Il formato dichiarato non garantisce che il contenuto sia
decifrabile: il vault non possiede la chiave privata. Nome e descrizione rimangono
metadati in chiaro nel catalogo. Solo il recupero con identità age può verificare
l'autenticazione crittografica e restituire il contenuto decifrato.

## Risposta

`201 Created` soltanto dopo salvataggio e conferma nel catalogo:

```json
{
  "id": "0123456789abcdef0123456789abcdef",
  "status": "complete",
  "size_bytes": 12345,
  "sha256": "...64 caratteri esadecimali...",
  "received_at": "2026-09-10T15:00:00Z"
}
```

L'identificativo è casuale a 128 bit, assegnato dal server. Il timestamp usa l'orologio UTC
del server. SHA-256 è un controllo di integrità, non una firma della ricevuta. TLS autentica
la connessione; per verifica indipendente/offline della provenienza serve una futura firma.

Gli errori hanno forma `{"error":"messaggio"}`:

| HTTP | Significato |
|---|---|
| 400 | Metadati, header o trasferimento non validi/incompleti. |
| 401 | Credenziale sconosciuta o revocata. |
| 404 / 405 | Percorso o metodo non disponibili. |
| 411 | Dimensione sconosciuta o file vuoto. |
| 413 | File oltre il limite del profilo. |
| 415 | Tipo di corpo o codifica non consentiti. |
| 409 | Chiave di idempotenza già in uso per dati diversi o per un invio ancora in corso. |
| 422 | SHA-256 del contenuto non corrispondente. |
| 429 | Quota, frequenza o concorrenza della chiave superate. |
| 502 / 503 | Writer non disponibile, server occupato o esito non confermato. |
| 507 | Spazio disco o operazione di salvataggio non disponibili. |

Una disconnessione o un errore prima della ricevuta non dimostra l'assenza del backup.
Il client ufficiale genera una chiave di idempotenza casuale per ogni operazione e,
in caso di errore di rete o risposta temporaneamente non disponibile, ripete al massimo
due volte lo stesso invio. Il vault associa la chiave alla credenziale, ai metadati,
alla dimensione e allo SHA-256: se il deposito è già completo restituisce la ricevuta
originale senza creare un altro backup. Un riuso della chiave con dati diversi viene
rifiutato. I client precedenti sul percorso v1 possono omettere l'header e conservano
il comportamento precedente: ogni richiesta riuscita crea un deposito distinto.

## Transazione logica

1. Validazione e prenotazione atomica nel DB: stato `receiving`.
2. Creazione esclusiva di `incoming/<ID>.part`; copia con verifica della dimensione.
3. Verifica SHA-256, permessi 0400, sincronizzazione e chiusura del file.
4. Hard link a `backups/<ID>.backup` senza sostituzione, sincronizzazione della directory.
5. Rimozione del temporaneo, sincronizzazione della directory di ingresso.
6. Stato `complete` nel DB e risposta 201.

Il filesystem e SQLite non costituiscono un'unica transazione. Se un arresto avviene tra
pubblicazione e conferma DB, la riconciliazione all'avvio verifica il file e completa il record.
Se il file finale non esiste, il record diventa `failed` e il temporaneo viene eliminato.
I backup finali non vengono cancellati durante questa procedura.
