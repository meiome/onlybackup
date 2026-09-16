# Client OnlyBackup nativo su Windows

Le release che includono il supporto Windows distribuiscono due programmi
nativi per Windows 10/11 AMD64:

- `onlybackup.exe` per cifrare e inviare;
- `onlybackup-recover.exe` per generare l'identità e recuperare da ricevuta.

Non servono WSL, Debian, Docker o una macchina virtuale. I servizi server e gli
strumenti amministrativi restano Linux-only.

Usare Windows 10/11 a 64 bit e un volume NTFS per configurazione e file di
recupero: le ACL private e la pubblicazione senza sovrascrittura dipendono dalle
funzioni di sicurezza del filesystem.

## Cosa serve

La credenziale `send.key` autorizza il deposito, ma non esegue alcuna operazione
da sola. `onlybackup.exe` gestisce cifratura age, SHA-256, TLS, autenticazione,
invio e ricevuta.

Usare per ogni PC reale una credenziale server dedicata, separata da collaudo e
da altri client. L'identità privata age serve invece a decifrare e deve essere
custodita anche fuori dal PC.

## 1. Verificare la release

Scaricare dalla stessa release:

- `onlybackup_VERSIONE_windows_amd64.zip`;
- `SHA256SUMS`.

Da PowerShell, nella directory di download:

```powershell
Get-FileHash .\onlybackup_VERSIONE_windows_amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS
gh attestation verify .\onlybackup_VERSIONE_windows_amd64.zip `
  --repo meiome/onlybackup
```

Il digest deve coincidere con la riga corrispondente di `SHA256SUMS` e
l'attestazione deve riuscire. Estrarre soltanto dopo entrambe le verifiche:

```powershell
Expand-Archive `
  -LiteralPath .\onlybackup_VERSIONE_windows_amd64.zip `
  -DestinationPath .\onlybackup-windows
```

## 2. Installare per l'utente corrente

```powershell
$source = Resolve-Path .\onlybackup-windows\onlybackup_VERSIONE_windows_amd64
$programDir = Join-Path $env:LOCALAPPDATA "Programs\OnlyBackup"
$configDir = Join-Path $env:LOCALAPPDATA "OnlyBackup"

New-Item -ItemType Directory -Force $programDir | Out-Null
New-Item -ItemType Directory -Force $configDir | Out-Null
Copy-Item -Recurse -Force "$source\bin", "$source\scripts" $programDir
```

Verificare:

```powershell
& "$programDir\bin\onlybackup.exe" --help
& "$programDir\bin\onlybackup-recover.exe" --help
```

## 3. Proteggere configurazione e chiavi con ACL

Rimuovere l'ereditarietà dalla directory privata e consentire accesso soltanto
all'utente corrente, LocalSystem e Administrators:

```powershell
$userSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls.exe $configDir /inheritance:r `
  /grant:r "*$($userSid):(OI)(CI)F" `
  "*S-1-5-18:(OI)(CI)F" `
  "*S-1-5-32-544:(OI)(CI)F"
if ($LASTEXITCODE -ne 0) { throw "Configurazione ACL non riuscita" }
```

Copiare poi nella directory:

```powershell
Copy-Item .\send-production.key "$configDir\send-production.key"
Copy-Item .\server.crt "$configDir\server.crt"
```

Il client accetta come proprietari soltanto l'utente corrente, LocalSystem o gli
Administrators locali e rifiuta ACL che concedono accesso ad altri account.
Dopo la copia verificata, eliminare l'esemplare consegnabile della credenziale
dal server.

## 4. Creare o importare l'identità age

Se non esiste già un'identità:

```powershell
& "$programDir\bin\onlybackup-recover.exe" keygen `
  --out "$configDir\age-identity.txt"
```

Il comando mostra il recipient pubblico da inserire in `client.json` e applica
alla nuova identità una ACL privata. Conservare un'altra copia protetta di
`age-identity.txt`; senza di essa i backup cifrati non sono recuperabili.

## 5. Configurare il client

Creare `$configDir\client.json`:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send-production.key",
  "ca_file": "server.crt",
  "encrypt_to": "age1..."
}
```

I percorsi relativi sono risolti rispetto a `client.json`. Se il certificato è
emesso da una CA già fidata da Windows, `ca_file` può essere omesso.

## 6. Eseguire un primo invio

```powershell
$receiptDir = Join-Path $configDir "receipts"
New-Item -ItemType Directory -Force $receiptDir | Out-Null

& "$programDir\bin\onlybackup.exe" send `
  --config "$configDir\client.json" `
  --description "Primo backup Windows" `
  --receipt "$receiptDir\receipt-manuale.json" `
  "C:\Dati\export\backup.zip"
```

Il client cifra e invia un singolo file già pronto. Non seleziona directory,
non crea snapshot e non pianifica l'esecuzione.

## 7. Automatizzare con PowerShell

La release contiene `scripts\backup-file.ps1`:

```powershell
& "$programDir\scripts\backup-file.ps1" `
  -Path "C:\Dati\export\backup.zip" `
  -Description "Backup Windows giornaliero" `
  -Client "$programDir\bin\onlybackup.exe" `
  -Config "$configDir\client.json" `
  -ReceiptDirectory "$configDir\receipts"
```

Lo script genera una ricevuta con nome univoco e demanda al client cifratura,
digest, TLS e invio. Eseguirlo manualmente con l'utente destinato prima di
registrarlo nell'Utilità di pianificazione di Windows.

Per la pianificazione creare un'attività che esegua:

```text
powershell.exe -NoProfile -File "C:\...\backup-file.ps1" -Path "C:\Dati\export\backup.zip"
```

Configurare l'attività con lo stesso account proprietario di configurazione e
chiavi. Non inserire la credenziale OnlyBackup negli argomenti o nello script.

## 8. Provare il recupero

L'API è deposit-only e non permette download. Ottenere tramite la procedura di
ripristino una copia del file `.backup` corrispondente alla ricevuta:

```powershell
& "$programDir\bin\onlybackup-recover.exe" `
  --receipt "$configDir\receipts\receipt-manuale.json" `
  --archive "D:\Ripristino\ID_BACKUP.backup" `
  --identity-file "$configDir\age-identity.txt" `
  --out "D:\Ripristino\file-recuperato.zip"
```

Il recuperatore verifica dimensione e SHA-256 della ricevuta prima di
decifrare, non sovrascrive file esistenti e applica un'ACL privata all'output.

## Checklist

- ZIP, checksum e attestazione verificati;
- credenziale di produzione unica per il PC;
- directory privata protetta con ACL e accettata dal client;
- identità age copiata anche su un supporto separato;
- invio manuale e ricezione della ricevuta completati;
- attività pianificata eseguita con lo stesso account;
- recupero periodico provato da una copia indipendente.
