[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string]$Path,

    [Parameter(Position = 1)]
    [string]$Description = "",

    [string]$Client = "",
    [string]$Config = "",
    [string]$ReceiptDirectory = ""
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($Client)) {
    $Client = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot "..\bin\onlybackup.exe"))
}
if ([string]::IsNullOrWhiteSpace($Config)) {
    $Config = Join-Path $env:LOCALAPPDATA "OnlyBackup\client.json"
}
if ([string]::IsNullOrWhiteSpace($ReceiptDirectory)) {
    $ReceiptDirectory = Join-Path $env:LOCALAPPDATA "OnlyBackup\receipts"
}

$source = (Resolve-Path -LiteralPath $Path).Path
if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
    throw "Il percorso non indica un file regolare: $source"
}
if (-not (Test-Path -LiteralPath $Client -PathType Leaf)) {
    throw "Client non trovato: $Client"
}
if (-not (Test-Path -LiteralPath $Config -PathType Leaf)) {
    throw "Configurazione non trovata: $Config"
}
if ([string]::IsNullOrWhiteSpace($Description)) {
    $Description = "Backup Windows: $([System.IO.Path]::GetFileName($source))"
}

[System.IO.Directory]::CreateDirectory($ReceiptDirectory) | Out-Null
$timestamp = [DateTime]::UtcNow.ToString("yyyyMMddTHHmmssfffffffZ")
$receipt = Join-Path $ReceiptDirectory "receipt-$timestamp-$PID.json"

& $Client send `
    --quiet `
    --config $Config `
    --description $Description `
    --receipt $receipt `
    $source | Out-Null

if ($LASTEXITCODE -ne 0) {
    throw "OnlyBackup ha restituito il codice $LASTEXITCODE"
}

Write-Output "Ricevuta: $receipt"
