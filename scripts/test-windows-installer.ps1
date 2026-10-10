[CmdletBinding()]
param([string]$SourceRoot = "")

$ErrorActionPreference = "Stop"
if ([string]::IsNullOrWhiteSpace($SourceRoot)) {
    $SourceRoot = Split-Path -Parent $PSScriptRoot
}
$SourceRoot = [System.IO.Path]::GetFullPath($SourceRoot)

function Assert-PrivateFileAcl([string]$Path) {
    $currentSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $allowed = @($currentSid, "S-1-5-18", "S-1-5-32-544")
    $acl = Get-Acl -LiteralPath $Path
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
    if ($acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value -notin $allowed) {
        throw "Unexpected owner on private child: $Path"
    }
}

$runRoot = Join-Path ([System.IO.Path]::GetTempPath()) `
    ("onlybackup-windows-installer-{0}" -f [Guid]::NewGuid().ToString("N"))
$programDirectory = Join-Path $runRoot "program with spaces [literal]"
$dataDirectory = Join-Path $runRoot "data with spaces [literal]"

try {
    Push-Location -LiteralPath $SourceRoot
    try {
        & (Join-Path $SourceRoot "scripts\install-windows-client.ps1") `
            -Scope CurrentUser `
            -SourceRoot "." `
            -ProgramDirectory $programDirectory `
            -DataDirectory $dataDirectory | Out-Null
    }
    finally { Pop-Location }

    foreach ($path in @(
        (Join-Path $programDirectory "bin\onlybackup.exe"),
        (Join-Path $programDirectory "bin\onlybackup-recover.exe"),
        (Join-Path $programDirectory "scripts\backup-file.ps1")
    )) {
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            throw "Installer did not create a file: $path"
        }
    }
    foreach ($path in @(
        $programDirectory,
        $dataDirectory,
        (Join-Path $programDirectory "bin"),
        (Join-Path $programDirectory "scripts"),
        (Join-Path $dataDirectory "receipts"),
        (Join-Path $dataDirectory "encrypted-temp")
    )) {
        if (-not (Test-Path -LiteralPath $path -PathType Container)) {
            throw "Installer did not create a directory: $path"
        }
    }

    $probe = Join-Path $dataDirectory "client.json"
    Set-Content -LiteralPath $probe -Value "{}" -Encoding ASCII
    if ((Get-Content -LiteralPath $probe -Raw) -notmatch "\{\}") {
        throw "A child of the private directory is not readable."
    }

    Assert-PrivateFileAcl $probe

    # Reinstall using paths relative to PowerShell's location, not the process CWD.
    $probeHash = (Get-FileHash -LiteralPath $probe -Algorithm SHA256).Hash
    Push-Location -LiteralPath $runRoot
    try {
        & (Join-Path $SourceRoot "scripts\install-windows-client.ps1") `
            -SourceRoot $SourceRoot `
            -ProgramDirectory ".\program with spaces [literal]" `
            -DataDirectory ".\data with spaces [literal]" | Out-Null
    }
    finally { Pop-Location }
    if ((Get-FileHash -LiteralPath $probe -Algorithm SHA256).Hash -ne $probeHash) {
        throw "Reinstallation changed existing private file content."
    }
    Assert-PrivateFileAcl $probe

    foreach ($collisionKind in @("program", "bin", "scripts", "data", "receipts", "encrypted-temp")) {
        $collisionRoot = Join-Path $runRoot "collision $collisionKind"
        $collisionProgram = Join-Path $collisionRoot "program with spaces [literal]"
        $collisionData = Join-Path $collisionRoot "data with spaces [literal]"
        $collisionPath = switch ($collisionKind) {
            "program" { $collisionProgram }
            "bin" { Join-Path $collisionProgram "bin" }
            "scripts" { Join-Path $collisionProgram "scripts" }
            "data" { $collisionData }
            "receipts" { Join-Path $collisionData "receipts" }
            "encrypted-temp" { Join-Path $collisionData "encrypted-temp" }
        }
        [void][System.IO.Directory]::CreateDirectory((Split-Path -Parent $collisionPath))
        Set-Content -LiteralPath $collisionPath -Value "Preserve this file" -Encoding ASCII
        $collisionHash = (Get-FileHash -LiteralPath $collisionPath -Algorithm SHA256).Hash
        $beforeItems = @(Get-Item -LiteralPath $collisionRoot) +
            @(Get-ChildItem -LiteralPath $collisionRoot -Recurse -Force)
        $beforeAcls = @{}
        foreach ($item in $beforeItems) {
            $beforeAcls[$item.FullName] = (Get-Acl -LiteralPath $item.FullName).Sddl
        }
        $collisionRejected = $false
        $collisionOutput = [System.Collections.Generic.List[string]]::new()
        try {
            & (Join-Path $SourceRoot "scripts\install-windows-client.ps1") `
                -SourceRoot $SourceRoot `
                -ProgramDirectory $collisionProgram `
                -DataDirectory $collisionData |
                ForEach-Object { $collisionOutput.Add([string]$_) }
        }
        catch {
            if ($_.Exception.Message -ne "Destination exists but is not a directory: $collisionPath") {
                throw
            }
            $collisionRejected = $true
        }
        if (-not $collisionRejected) { throw "Installer accepted a file at $collisionPath" }
        if ($collisionOutput -match 'installation completed') {
            throw "Installer reported success for a directory collision: $collisionPath"
        }
        if (-not (Test-Path -LiteralPath $collisionPath -PathType Leaf) -or
            (Get-FileHash -LiteralPath $collisionPath -Algorithm SHA256).Hash -ne $collisionHash) {
            throw "Installer changed the colliding file: $collisionPath"
        }
        $afterItems = @(Get-Item -LiteralPath $collisionRoot) +
            @(Get-ChildItem -LiteralPath $collisionRoot -Recurse -Force)
        if ($afterItems.Count -ne $beforeItems.Count) {
            throw "Installer changed destinations before rejecting $collisionPath"
        }
        foreach ($item in $afterItems) {
            if (-not $beforeAcls.ContainsKey($item.FullName) -or
                (Get-Acl -LiteralPath $item.FullName).Sddl -ne $beforeAcls[$item.FullName]) {
                throw "Installer changed a path or its permissions before rejecting $collisionPath"
            }
        }
    }

    # A successful stderr-writing --help must not hide real process failures.
    $fixtureRoot = Join-Path $runRoot "failing package"
    $fixtureBin = Join-Path $fixtureRoot "bin"
    $fixtureScripts = Join-Path $fixtureRoot "scripts"
    New-Item -ItemType Directory -Force $fixtureBin, $fixtureScripts | Out-Null
    Copy-Item (Join-Path $SourceRoot "bin\onlybackup-recover.exe") $fixtureBin
    Copy-Item (Join-Path $SourceRoot "scripts\backup-file.ps1") $fixtureScripts
    $fixtureSource = Join-Path $runRoot "FailingClient.cs"
    @'
public static class FailingClient {
    public static int Main() {
        System.Console.Error.WriteLine("Deliberate startup failure");
        return 23;
    }
}
'@ | Set-Content -LiteralPath $fixtureSource -Encoding ASCII
    $fixtureExe = Join-Path $fixtureBin "onlybackup.exe"
    $compiler = Join-Path $env:WINDIR "Microsoft.NET\Framework64\v4.0.30319\csc.exe"
    & $compiler /nologo /target:exe "/out:$fixtureExe" $fixtureSource
    if ($LASTEXITCODE -ne 0) { throw "Failed to build the startup-failure fixture." }
    $rejected = $false
    try {
        & (Join-Path $SourceRoot "scripts\install-windows-client.ps1") `
            -SourceRoot $fixtureRoot `
            -ProgramDirectory (Join-Path $runRoot "failed program") `
            -DataDirectory (Join-Path $runRoot "failed data") | Out-Null
    }
    catch {
        if ($_.Exception.Message -notmatch 'exit code 23') { throw }
        $rejected = $true
    }
    if (-not $rejected) { throw "Installer accepted a client exiting with code 23." }

    Write-Output "Windows installer, private ACLs, reinstallation, directory collisions and startup-failure rejection: OK"
}
finally {
    if (Test-Path -LiteralPath $runRoot) {
        Remove-Item -LiteralPath $runRoot -Recurse -Force
    }
}
