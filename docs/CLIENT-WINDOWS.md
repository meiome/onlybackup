# Native OnlyBackup client on Windows

OnlyBackup releases include two native programs for Windows 10/11 AMD64:

- `onlybackup.exe` encrypts and uploads files;
- `onlybackup-recover.exe` creates an age identity and recovers files from a
  receipt.

They do not require WSL, Debian, Docker, or a virtual machine. Server services
and administrative tools remain Linux-only.

Use 64-bit Windows 10/11 and an NTFS volume for configuration and recovery
files. Private ACLs and no-overwrite file creation depend on filesystem security
features provided by NTFS.

## Requirements

The `send.key` credential authorizes deposits but cannot perform an operation by
itself. `onlybackup.exe` handles age encryption, SHA-256, TLS, authentication,
upload, and receipt creation.

Use a dedicated server credential for each production computer, separate from
test credentials and other clients. The private age identity decrypts backups;
keep a protected copy outside the computer.

## 1. Verify the release

Download these files from the same release:

- `onlybackup_VERSION_windows_amd64.zip`;
- `SHA256SUMS`.

In PowerShell, from the download directory:

```powershell
Get-FileHash .\onlybackup_VERSION_windows_amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS
gh attestation verify .\onlybackup_VERSION_windows_amd64.zip `
  --repo meiome/onlybackup
```

The digest must match the corresponding line in `SHA256SUMS`, and attestation
verification must succeed. Extract the archive only after both checks pass:

```powershell
Expand-Archive `
  -LiteralPath .\onlybackup_VERSION_windows_amd64.zip `
  -DestinationPath .\onlybackup-windows
```

## 2. Install for the current user

```powershell
$source = Resolve-Path .\onlybackup-windows\onlybackup_VERSION_windows_amd64
$programDir = Join-Path $env:LOCALAPPDATA "Programs\OnlyBackup"
$configDir = Join-Path $env:LOCALAPPDATA "OnlyBackup"

New-Item -ItemType Directory -Force $programDir | Out-Null
New-Item -ItemType Directory -Force $configDir | Out-Null
Copy-Item -Recurse -Force "$source\bin", "$source\scripts" $programDir
```

Verify the installation:

```powershell
& "$programDir\bin\onlybackup.exe" --help
& "$programDir\bin\onlybackup-recover.exe" --help
```

## 3. Protect configuration and keys with ACLs

Disable inheritance on the private directory and allow access only to the
current user, LocalSystem, and Administrators:

```powershell
$userSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls.exe $configDir /inheritance:r `
  /grant:r "*$($userSid):(OI)(CI)F" `
  "*S-1-5-18:(OI)(CI)F" `
  "*S-1-5-32-544:(OI)(CI)F"
if ($LASTEXITCODE -ne 0) { throw "Failed to configure private ACLs" }
```

Then copy the credential and, when required, the private CA certificate:

```powershell
Copy-Item .\send-production.key "$configDir\send-production.key"
Copy-Item .\server.crt "$configDir\server.crt"
```

The client accepts only the current user, LocalSystem, or local Administrators
as owners, and rejects ACLs that grant access to other accounts. After verifying
the copy, remove the transferable credential file from the server.

## 4. Create or import the age identity

If an identity does not already exist:

```powershell
& "$programDir\bin\onlybackup-recover.exe" keygen `
  --out "$configDir\age-identity.txt"
```

The command prints the public recipient for `client.json` and applies a private
ACL to the new identity. Keep another protected copy of `age-identity.txt`;
encrypted backups cannot be recovered without it.

## 5. Configure the client

Create `$configDir\client.json`:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send-production.key",
  "ca_file": "server.crt",
  "encrypt_to": "age1..."
}
```

Relative paths are resolved from the directory containing `client.json`. Omit
`ca_file` when Windows already trusts the certificate issuer.

## 6. Run the first upload

```powershell
$receiptDir = Join-Path $configDir "receipts"
New-Item -ItemType Directory -Force $receiptDir | Out-Null

& "$programDir\bin\onlybackup.exe" send `
  --config "$configDir\client.json" `
  --description "First Windows backup" `
  --receipt "$receiptDir\manual-receipt.json" `
  "C:\Data\export\backup.zip"
```

The client encrypts and uploads one prepared file. It does not select
directories, create snapshots, or schedule jobs.

## 7. Automate uploads with PowerShell

The release includes `scripts\backup-file.ps1`:

```powershell
& "$programDir\scripts\backup-file.ps1" `
  -Path "C:\Data\export\backup.zip" `
  -Description "Daily Windows backup" `
  -Client "$programDir\bin\onlybackup.exe" `
  -Config "$configDir\client.json" `
  -ReceiptDirectory "$configDir\receipts"
```

The script creates a unique receipt name and delegates encryption, hashing,
TLS, and upload to the client. Run it manually as the intended user before
registering it in Windows Task Scheduler.

Create a scheduled task that runs:

```text
powershell.exe -NoProfile -File "C:\...\backup-file.ps1" -Path "C:\Data\export\backup.zip"
```

Run the task as the same account that owns the configuration and keys. Do not
put the OnlyBackup credential in task arguments or in the script.

## 8. Test recovery

The API is deposit-only and does not provide downloads. Obtain a copy of the
`.backup` file identified by the receipt through the local restore procedure:

```powershell
& "$programDir\bin\onlybackup-recover.exe" `
  --receipt "$configDir\receipts\manual-receipt.json" `
  --archive "D:\Recovery\BACKUP_ID.backup" `
  --identity-file "$configDir\age-identity.txt" `
  --out "D:\Recovery\recovered-file.zip"
```

The recovery tool verifies the size and SHA-256 value in the receipt before
decrypting. It does not overwrite existing files and applies a private ACL to
the output.

## Checklist

- The ZIP, checksum, and attestation have been verified.
- The production credential is unique to this computer.
- The private directory has restricted ACLs accepted by the client.
- A protected copy of the age identity exists on separate storage.
- A manual upload completed and produced a receipt.
- The scheduled task runs as the same account used during setup.
- Recovery from an independent copy is tested regularly.
