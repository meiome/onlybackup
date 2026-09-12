# Protocollo locale del vault

Il vault è il solo proprietario dell'archivio definitivo e del catalogo SQLite
autorevole. Il processo che riceve gli upload pubblici può aprire esclusivamente
la socket Unix del vault e non deve avere permessi sullo stato. La socket è creata
con modo `0660`; utenti e gruppi restano una responsabilità dell'installazione.

Ogni connessione contiene una sola richiesta e una sola risposta. Non è HTTP e
non offre operazioni, percorsi, identificativi scelti dal chiamante, letture,
elenchi, cancellazioni o amministrazione.

La richiesta è composta da:

1. un intero unsigned big-endian di 32 bit con la lunghezza dell'envelope JSON;
2. un envelope JSON UTF-8 lungo al massimo 16 KiB;
3. esattamente `size_bytes` byte di contenuto;
4. la chiusura della metà di scrittura della connessione Unix.

L'envelope ammette soltanto questi campi:

```json
{
  "token": "obk_...",
  "metadata": {
    "description": "esportazione database",
    "original_name": "database.sql"
  },
  "size_bytes": 1234,
  "sha256": "64 caratteri esadecimali minuscoli",
  "idempotency_key": "obi_..."
}
```

I campi sconosciuti, duplicati o i valori JSON successivi sono rifiutati. Il
vault valida nuovamente token e metadati, applica quote, frequenza, concorrenza e
riserva di spazio dal proprio catalogo, genera l'ID e calcola SHA-256 sui byte
ricevuti. Un byte in meno o in più rende il deposito non valido. Il chiamante non
può dichiarare un ID, un profilo, uno stato o un percorso.
La chiave di idempotenza è generata dal client pubblico e inoltrata senza modifiche.
Il vault la associa alla credenziale e restituisce la ricevuta originale quando
riceve nuovamente la stessa operazione già completata. Dati diversi con la stessa
chiave sono rifiutati.

La risposta usa lo stesso prefisso a 32 bit e un JSON lungo al massimo 64 KiB.
Un successo contiene `status_code: 201` e una `receipt`; un errore contiene uno
status HTTP equivalente e `error`. Il gateway tratta come non confermata ogni
risposta incoerente con dimensione e digest della richiesta.

La ricevuta positiva viene inviata soltanto dopo verifica del contenuto,
pubblicazione senza sovrascrittura, sincronizzazione delle directory e commit
del record completo nel catalogo autorevole. Il protocollo limita il danno di un
writer compromesso senza privilegi: una credenziale osservata può ancora essere
usata per nuovi depositi entro le sue quote, ma non permette di cambiare byte o
record precedenti. Non protegge dalla compromissione del vault o di root.
