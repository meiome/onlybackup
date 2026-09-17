[CmdletBinding()]
param(
    [ValidateSet("CurrentUser", "Machine")]
    [string]$Scope = "CurrentUser",

    [string]$SourceRoot = "",
    [string]$ProgramDirectory = "",
    [string]$DataDirectory = ""
)

$ErrorActionPreference = "Stop"

$identity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [System.Security.Principal.WindowsPrincipal]::new($identity)
$adminRole = [System.Security.Principal.WindowsBuiltInRole]::Administrator

if ($Scope -eq "Machine" -and -not $principal.IsInRole($adminRole)) {
    throw "Machine scope requires PowerShell running as Administrator."
}

if ([string]::IsNullOrWhiteSpace($SourceRoot)) {
    $SourceRoot = Split-Path -Parent $PSScriptRoot
}
$SourceRoot = [System.IO.Path]::GetFullPath($SourceRoot)

if ([string]::IsNullOrWhiteSpace($ProgramDirectory)) {
    if ($Scope -eq "Machine") {
        $ProgramDirectory = Join-Path $env:ProgramFiles "OnlyBackup"
    }
    else {
        $ProgramDirectory = Join-Path $env:LOCALAPPDATA "Programs\OnlyBackup"
    }
}
if ([string]::IsNullOrWhiteSpace($DataDirectory)) {
    if ($Scope -eq "Machine") {
        $DataDirectory = Join-Path $env:ProgramData "OnlyBackup"
    }
    else {
        $DataDirectory = Join-Path $env:LOCALAPPDATA "OnlyBackup"
    }
}

$clientSource = Join-Path $SourceRoot "bin\onlybackup.exe"
$recoverSource = Join-Path $SourceRoot "bin\onlybackup-recover.exe"
$uploadScriptSource = Join-Path $SourceRoot "scripts\backup-file.ps1"
foreach ($path in @($clientSource, $recoverSource, $uploadScriptSource)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Required package file not found: $path"
    }
}

$programBin = Join-Path $ProgramDirectory "bin"
$programScripts = Join-Path $ProgramDirectory "scripts"
New-Item -ItemType Directory -Force $programBin | Out-Null
New-Item -ItemType Directory -Force $programScripts | Out-Null
New-Item -ItemType Directory -Force $DataDirectory | Out-Null

# Build a protected DACL from scratch. This avoids locale-dependent account
# names and removes inherited or explicit grants left by a parent directory.
$directoryAcl = [System.Security.AccessControl.DirectorySecurity]::new()
$directoryAcl.SetAccessRuleProtection($true, $false)
$inheritance = [System.Security.AccessControl.InheritanceFlags] "ContainerInherit, ObjectInherit"
$propagation = [System.Security.AccessControl.PropagationFlags]::None
$allow = [System.Security.AccessControl.AccessControlType]::Allow
$fullControl = [System.Security.AccessControl.FileSystemRights]::FullControl
$trustedSids = @(
    $identity.User,
    [System.Security.Principal.SecurityIdentifier]::new("S-1-5-18"),
    [System.Security.Principal.SecurityIdentifier]::new("S-1-5-32-544")
)
foreach ($sid in $trustedSids) {
    $rule = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $sid,
        $fullControl,
        $inheritance,
        $propagation,
        $allow
    )
    [void]$directoryAcl.AddAccessRule($rule)
}
$directoryAcl.SetOwner($identity.User)
Set-Acl -LiteralPath $DataDirectory -AclObject $directoryAcl

foreach ($name in @("receipts", "encrypted-temp")) {
    New-Item -ItemType Directory -Force (Join-Path $DataDirectory $name) | Out-Null
}

# Existing children must inherit only the protected parent DACL. Never disable
# inheritance recursively: doing so can leave files with no usable access rule.
& icacls.exe (Join-Path $DataDirectory "*") /reset /T /C | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "Failed to apply the private ACL to configuration children."
}
& icacls.exe (Join-Path $DataDirectory "*") /setowner "*$($identity.User.Value)" /T /C | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "Failed to set the owner of configuration children."
}

Copy-Item -LiteralPath $clientSource `
    -Destination (Join-Path $programBin "onlybackup.exe") -Force
Copy-Item -LiteralPath $recoverSource `
    -Destination (Join-Path $programBin "onlybackup-recover.exe") -Force
Copy-Item -LiteralPath $uploadScriptSource `
    -Destination (Join-Path $programScripts "backup-file.ps1") -Force

& (Join-Path $programBin "onlybackup.exe") --help *> $null
if ($LASTEXITCODE -ne 0) {
    throw "The installed OnlyBackup client did not start successfully."
}

Write-Output "OnlyBackup client installation completed."
Write-Output "Programs: $ProgramDirectory"
Write-Output "Private configuration: $DataDirectory"
Write-Output "Copy the deposit credential and CA only after reviewing the ACL."
