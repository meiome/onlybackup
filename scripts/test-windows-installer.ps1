[CmdletBinding()]
param([string]$SourceRoot = "")

$ErrorActionPreference = "Stop"
if ([string]::IsNullOrWhiteSpace($SourceRoot)) {
    $SourceRoot = Split-Path -Parent $PSScriptRoot
}
$SourceRoot = [System.IO.Path]::GetFullPath($SourceRoot)

$runRoot = Join-Path ([System.IO.Path]::GetTempPath()) `
    ("onlybackup-windows-installer-{0}" -f [Guid]::NewGuid().ToString("N"))
$programDirectory = Join-Path $runRoot "program"
$dataDirectory = Join-Path $runRoot "data"

try {
    & (Join-Path $SourceRoot "scripts\install-windows-client.ps1") `
        -Scope CurrentUser `
        -SourceRoot $SourceRoot `
        -ProgramDirectory $programDirectory `
        -DataDirectory $dataDirectory | Out-Null

    foreach ($path in @(
        (Join-Path $programDirectory "bin\onlybackup.exe"),
        (Join-Path $programDirectory "bin\onlybackup-recover.exe"),
        (Join-Path $programDirectory "scripts\backup-file.ps1"),
        (Join-Path $dataDirectory "receipts"),
        (Join-Path $dataDirectory "encrypted-temp")
    )) {
        if (-not (Test-Path -LiteralPath $path)) {
            throw "Installer did not create: $path"
        }
    }

    $probe = Join-Path $dataDirectory "client.json"
    Set-Content -LiteralPath $probe -Value "{}" -Encoding ASCII
    if ((Get-Content -LiteralPath $probe -Raw) -notmatch "\{\}") {
        throw "A child of the private directory is not readable."
    }

    $currentSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $allowed = @($currentSid, "S-1-5-18", "S-1-5-32-544")
    $acl = Get-Acl -LiteralPath $probe
    $rules = $acl.GetAccessRules(
        $true,
        $true,
        [System.Security.Principal.SecurityIdentifier]
    )
    $actual = @()
    foreach ($rule in $rules) {
        $sid = $rule.IdentityReference.Value
        if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) {
            throw "Unexpected deny rule on private child: $sid"
        }
        if ($sid -notin $allowed) {
            throw "Unexpected account on private child ACL: $sid"
        }
        $actual += $sid
    }
    foreach ($sid in $allowed) {
        if ($sid -notin $actual) {
            throw "Required account missing from private child ACL: $sid"
        }
    }

    Write-Output "Windows installer and private child ACL: OK"
}
finally {
    if (Test-Path -LiteralPath $runRoot) {
        Remove-Item -LiteralPath $runRoot -Recurse -Force
    }
}
