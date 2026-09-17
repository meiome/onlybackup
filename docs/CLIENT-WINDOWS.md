# Native OnlyBackup client on Windows

OnlyBackup releases include two native Windows AMD64 programs:

- `onlybackup.exe` encrypts and uploads files;
- `onlybackup-recover.exe` creates an age identity and recovers files from a
  receipt.

They do not require WSL, Debian, Docker, or a virtual machine. Server services
and administrative tools remain Linux-only. Continuous integration tests the
clients on Windows Server 2022. Validate other 64-bit Windows editions,
including Windows Server 2019, with a manual upload and recovery test before
production use.

Use an NTFS volume for configuration, temporary encrypted files, receipts, and
recovery output. Private ACLs and no-overwrite file creation depend on NTFS
security features and may not be preserved by FAT, exFAT, or network shares.

## Requirements

The `send.key` credential authorizes deposits but cannot read, modify, or delete
backups. Use a dedicated credential for each production computer, separate from
test credentials and other clients. Never put the credential in a script,
command-line argument, log, or scheduled-task argument.

`onlybackup.exe` handles age encryption, SHA-256, TLS, authentication, upload,
and receipt creation. Before connecting, it creates the complete encrypted file
in a private temporary directory. Allow temporary disk space slightly larger
than the source file as well as room for the source backup itself.

Only the public age recipient is required on a computer that uploads backups.
The private age identity decrypts every backup encrypted to that recipient;
keep it on a separate trusted recovery system or protected offline storage.

## 1. Verify the release

Download these files from the same release:

- `onlybackup_VERSION_windows_amd64.zip`;
- `SHA256SUMS`.

Use a current GitHub CLI with attestation support. Authenticate it if requested,
then run these commands in PowerShell from the download directory:

```powershell
Get-FileHash .\onlybackup_VERSION_windows_amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS
gh attestation verify .\onlybackup_VERSION_windows_amd64.zip `
  --repo meiome/onlybackup
```

The digest must match the corresponding line in `SHA256SUMS`, and attestation
verification must succeed. Verification may instead be performed on a trusted
staging computer before the package is transferred to an isolated server.
Extract the archive only after both checks pass:

```powershell
Expand-Archive `
  -LiteralPath .\onlybackup_VERSION_windows_amd64.zip `
  -DestinationPath .\onlybackup-windows
```

## 2. Choose an installation scope

Use one of the following layouts. All later commands use the `$programDir` and
`$configDir` selected here.

After release verification, if local execution policy blocks the packaged
script, allow it only for the current PowerShell process:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
```

### Current-user installation

This layout does not require elevation. The same Windows account must perform
manual uploads and run the scheduled task.

```powershell
$source = Resolve-Path .\onlybackup-windows\onlybackup_VERSION_windows_amd64
$programDir = Join-Path $env:LOCALAPPDATA "Programs\OnlyBackup"
$configDir = Join-Path $env:LOCALAPPDATA "OnlyBackup"

& "$source\scripts\install-windows-client.ps1" -Scope CurrentUser
```

### Machine-wide installation

Run PowerShell as Administrator. This layout keeps programs in `Program Files`
and private runtime data in the normally hidden `ProgramData` directory. The
account performing the installation must also run uploads and scheduled tasks.

```powershell
$source = Resolve-Path .\onlybackup-windows\onlybackup_VERSION_windows_amd64
$programDir = Join-Path $env:ProgramFiles "OnlyBackup"
$configDir = Join-Path $env:ProgramData "OnlyBackup"

& "$source\scripts\install-windows-client.ps1" -Scope Machine
```

`ProgramData` is hidden by default. Verify it from PowerShell instead of relying
on its visibility in File Explorer:

```powershell
Test-Path $configDir
Get-ChildItem -Force $configDir
```

Verify the installation:

```powershell
& "$programDir\bin\onlybackup.exe" --help
& "$programDir\bin\onlybackup-recover.exe" --help
```

## 3. Protect configuration and keys with ACLs

The installer creates the private configuration directory before any secret is
copied. It builds a protected ACL containing only the current user, LocalSystem,
and local Administrators, then repairs inheritance on existing children.

For diagnosis, the three grants below are the ones enforced by the installer.
They may also be applied to a newly created, empty configuration directory:

```powershell
$userSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls.exe $configDir /inheritance:r `
  /grant:r "*$($userSid):(OI)(CI)F" `
  "*S-1-5-18:(OI)(CI)F" `
  "*S-1-5-32-544:(OI)(CI)F"
if ($LASTEXITCODE -ne 0) { throw "Failed to configure private ACLs" }
```

Copy the credential and, when required, the private CA certificate. The TLS
server private key must never be copied to a client.

```powershell
Copy-Item .\send-production.key "$configDir\send-production.key"
Copy-Item .\backup-ca.crt "$configDir\backup-ca.crt"
New-Item -ItemType Directory -Force `
  "$configDir\receipts", "$configDir\encrypted-temp" | Out-Null

& icacls.exe "$configDir\*" /inheritance:e /T /C | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Failed to inherit private ACLs" }
```

Do not recursively use `/inheritance:r` on the child files without adding
explicit grants to every child: that can leave `client.json` and the credential
unreadable. Verify the effective ACLs before the first upload:

```powershell
& icacls.exe $configDir
& icacls.exe "$configDir\send-production.key"
```

Both outputs must contain only the intended user, LocalSystem, and local
Administrators. The client rejects a credential whose owner or DACL grants
access to another account. After verifying the client copy, remove every
transferable credential copy from staging directories and from the server.

The installer is parsed and exercised on Windows Server 2022 in CI, including a
read/write check through the resulting private child ACLs.

## 4. Keep the age identity off an upload-only server

On a separate trusted recovery computer, generate an identity if one does not
already exist:

```powershell
& ".\onlybackup-recover.exe" keygen `
  --out ".\age-identity.txt"
```

The command prints the public `age1...` recipient. Put only that public value in
the uploader's `client.json`. Preserve `age-identity.txt` in protected storage
and test recovery regularly; encrypted backups cannot be recovered without it.

If a computer is intentionally both an uploader and a recovery workstation,
the identity can be stored in `$configDir`, but compromise of that computer then
exposes both the deposit credential and the decryption identity.

## 5. Configure the client and temporary space

Create `$configDir\client.json`:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send-production.key",
  "ca_file": "backup-ca.crt",
  "encrypt_to": "age1...",
  "encrypted_temp_limit_bytes": 107374182400,
  "encrypted_temp_dir": "encrypted-temp"
}
```

Relative paths are resolved from the directory containing `client.json`. Omit
`ca_file` when Windows already trusts the certificate issuer. Select a temporary
limit appropriate for the credential profile and available local disk space;
the example is 100 GiB. The encrypted output must fit both this local limit and
the server's maximum size for one backup.

## 6. Run and observe the first upload

Use a unique receipt path because the client never overwrites receipts:

```powershell
$stamp = [DateTime]::UtcNow.ToString("yyyyMMddTHHmmssZ")
$receipt = Join-Path $configDir "receipts\manual-$stamp.json"

& "$programDir\bin\onlybackup.exe" send `
  --config "$configDir\client.json" `
  --description "First Windows backup" `
  --receipt $receipt `
  "C:\Data\export\backup.zip"
if ($LASTEXITCODE -ne 0) { throw "OnlyBackup upload failed" }
```

The command has two phases: it first writes the complete age-encrypted temporary
file, then uploads it with progress information. Large files can therefore take
time before network activity begins. Do not interrupt the process merely because
the encryption phase is quiet. From a second PowerShell window, inspect activity
without reading any secret:

```powershell
Get-Process onlybackup -ErrorAction SilentlyContinue
Get-ChildItem "$configDir\encrypted-temp" |
  Select-Object Name, Length, LastWriteTime
```

The temporary file is removed after normal success or a handled error. A power
loss or forced process termination can leave a private residual file that must
be inspected before a later run. Success requires exit code zero and a new JSON
receipt. The receipt is returned only after the server has verified and stored
the backup.

The client accepts one prepared file. It does not select directories, create
database dumps or snapshots, schedule jobs, or shut down the computer.

## 7. Automate uploads with PowerShell

The release includes `scripts\backup-file.ps1` for a file that another process
has already prepared:

```powershell
& "$programDir\scripts\backup-file.ps1" `
  -Path "C:\Data\export\backup.zip" `
  -Description "Daily Windows backup" `
  -Client "$programDir\bin\onlybackup.exe" `
  -Config "$configDir\client.json" `
  -ReceiptDirectory "$configDir\receipts"
```

The script creates a unique receipt name and delegates encryption, hashing,
TLS, and upload to the client. It uses quiet mode, so a long pause during local
encryption is expected. Run it manually as the intended task account before
registering it in Windows Task Scheduler.

For an interactive wrapper that must show progress, omit `--quiet` and invoke
the native client directly: do not capture its output and do not merge or
redirect standard error. Keep quiet mode for unattended tasks when console
progress is not useful; use the receipt and exit code as the success criteria.

For a database or application backup, use a separate wrapper that:

- creates the source backup with native operating-system authentication;
- verifies that the output is new, regular, non-empty, and within the size cap;
- prevents overlapping executions;
- invokes `onlybackup.exe` and propagates its exit code;
- records a local log and preserves the receipt;
- removes temporary source data only after confirmed success.

Do not put database passwords or the OnlyBackup credential in the wrapper or
task arguments.

In Task Scheduler, create a task with these properties:

- run as the same account that owns the configuration and credential;
- run whether or not that account is logged on;
- use the highest privileges when using the machine-wide layout;
- do not start another instance while the previous run is active;
- choose a trigger that cannot overlap the source-backup job or a shutdown;
- retain task history and inspect non-zero exit codes.

Use this action:

```text
Program:   C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe
Arguments: -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "C:\path\backup-wrapper.ps1"
Start in:  C:\path
```

The task must not run as a different account merely because that account can
elevate to Administrator; the private configuration ACL is bound to the account
selected during installation.

## 8. Test recovery on the trusted recovery computer

The API is deposit-only and does not provide downloads. Obtain an independent
copy of the `.backup` file identified by the receipt through the local restore
procedure, then run:

```powershell
& ".\onlybackup-recover.exe" `
  --receipt ".\manual-receipt.json" `
  --archive "D:\Recovery\BACKUP_ID.backup" `
  --identity-file ".\age-identity.txt" `
  --out "D:\Recovery\recovered-file.zip"
```

The recovery tool verifies the exact size and SHA-256 value in the receipt
before decrypting. It does not overwrite existing files and applies a private
ACL to the output. Complete the application-level restore test as well; a
successfully decrypted database file is not by itself proof of recoverability.

## Troubleshooting private ACLs

If `onlybackup.exe` reports access denied for `client.json`, inspect the
directory and file without printing their contents:

```powershell
whoami /user
& icacls.exe $configDir
& icacls.exe "$configDir\client.json"
```

If the directory has the intended private entries but its children have none,
restore inheritance from the already protected directory:

```powershell
& icacls.exe "$configDir\*" /inheritance:e /T /C
```

Never solve a private-file error by granting access to `Everyone`, `Users`, or
`Authenticated Users`.

## Checklist

- The ZIP, checksum, and provenance attestation have been verified.
- The Windows edition and NTFS environment passed a manual compatibility test.
- The production credential is unique to this computer.
- The private directory and every child have restricted ACLs accepted by the
  client.
- The age identity is protected separately from an upload-only computer.
- Temporary disk space and the encrypted-size limit are appropriate.
- A manual upload completed, returned exit code zero, and produced a receipt.
- The scheduled task uses the same account and cannot overlap another run.
- Transferable credential copies have been removed after installation.
- Recovery from an independent copy is tested regularly.

See [OPERATIONS.md](OPERATIONS.md) for monitoring, capacity planning,
credential rotation, TLS renewal, cold copies, and incident handling.
