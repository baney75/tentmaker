<#
.SYNOPSIS
  Test scripts/install.ps1 against a fake release on Windows (CI runs it under
  PowerShell 5.1 and 7).

.DESCRIPTION
  Usage: .\scripts\test-install.ps1 -Binary path\to\tentmaker.exe -Expect "tentmaker 0.0.0-ci"

  Builds a local release tree around -Binary, points TENTMAKER_DOWNLOAD_BASE at
  it and checks: the script parses; a good install by parameters; a good
  install piped through iex with environment variables; a tampered archive is
  refused and installs nothing. Installs go to a temp directory.
#>
param(
    [Parameter(Mandatory = $true)][string]$Binary,
    [Parameter(Mandatory = $true)][string]$Expect
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$installer = Join-Path $PSScriptRoot 'install.ps1'
$failures = 0
function Pass([string]$m) { Write-Host "ok   $m" }
function Bad([string]$m) { Write-Host "FAIL $m"; $script:failures++ }

$tokens = $null; $errors = $null
[void][Management.Automation.Language.Parser]::ParseFile($installer, [ref]$tokens, [ref]$errors)
if ($errors.Count -eq 0) { Pass "install.ps1 parses under PowerShell $($PSVersionTable.PSVersion)" } else { Bad "parse errors: $($errors | Out-String)" }

$arch = $env:PROCESSOR_ARCHITEW6432
if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
$arch = if ($arch -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "tentmaker_windows_$arch.zip"

$work = Join-Path ([IO.Path]::GetTempPath()) ('tm-test-' + [Guid]::NewGuid().ToString('N'))
Add-Type -AssemblyName System.IO.Compression.FileSystem
function New-Release([string]$dir, [bool]$tamper) {
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    $stage = Join-Path $work ('stage-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $stage | Out-Null
    Copy-Item -LiteralPath $Binary -Destination (Join-Path $stage 'tentmaker.exe')
    Set-Content -LiteralPath (Join-Path $stage 'LICENSE') -Value 'MIT'
    $zip = Join-Path $dir $asset
    [IO.Compression.ZipFile]::CreateFromDirectory($stage, $zip)
    $hash = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $dir 'SHA256SUMS'), "$hash  $asset`n")
    if ($tamper) { [IO.File]::AppendAllText($zip, 'x') }
}

try {
    New-Release (Join-Path $work 'good\latest\download') $false
    New-Release (Join-Path $work 'good\download\dev') $false
    New-Release (Join-Path $work 'bad\latest\download') $true

    # 1. Parameters, from a file.
    $env:TENTMAKER_DOWNLOAD_BASE = Join-Path $work 'good'
    $dir1 = Join-Path $work 'install1'
    try {
        & $installer -InstallDir $dir1 -NoSetup
        $v = & (Join-Path $dir1 'tentmaker.exe') version
        if ("$v".Trim() -eq $Expect) { Pass 'install by parameters' } else { Bad "installed version '$v', want '$Expect'" }
    } catch { Bad "install by parameters threw: $_" }

    # 2. Upgrade in place, dev channel.
    try {
        & $installer -InstallDir $dir1 -NoSetup -Channel dev
        if (Test-Path (Join-Path $dir1 'tentmaker.exe')) { Pass 'upgrade over an existing install' } else { Bad 'upgrade lost the binary' }
    } catch { Bad "upgrade threw: $_" }

    # 3. Piped through iex, options from environment variables.
    $dir2 = Join-Path $work 'install2'
    $env:TENTMAKER_INSTALL_DIR = $dir2
    $env:TENTMAKER_NO_SETUP = '1'
    $env:TENTMAKER_CHANNEL = 'dev'
    try {
        Get-Content -LiteralPath $installer -Raw | Invoke-Expression
        if (Test-Path (Join-Path $dir2 'tentmaker.exe')) { Pass 'install piped through iex' } else { Bad 'iex install left no binary' }
    } catch { Bad "iex install threw: $_" }
    Remove-Item Env:TENTMAKER_INSTALL_DIR, Env:TENTMAKER_NO_SETUP, Env:TENTMAKER_CHANNEL

    # 4. Tampered archive.
    $env:TENTMAKER_DOWNLOAD_BASE = Join-Path $work 'bad'
    $dir3 = Join-Path $work 'install3'
    $threw = $false
    try { & $installer -InstallDir $dir3 -NoSetup } catch { $threw = $true; $msg = "$_" }
    if ($threw -and $msg -match 'checksum mismatch' -and -not (Test-Path (Join-Path $dir3 'tentmaker.exe'))) {
        Pass 'sha mismatch refuses'
    } else { Bad "tampered archive: threw=$threw" }

    # 5. Bad channel.
    $threw = $false
    try { & $installer -InstallDir $dir3 -NoSetup -Channel beta } catch { $threw = $true }
    if ($threw) { Pass 'rejects unknown channel' } else { Bad 'accepted channel beta' }

    # 6. A system install (Administrator) leaves nothing the user owns: the
    # SYSTEM service refuses a binary its user could change, and an owner can
    # always rewrite the DACL. A rollback copy planted while the user owned
    # the folder is removed.
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
    if ($isAdmin) {
        $env:TENTMAKER_DOWNLOAD_BASE = Join-Path $work 'good'
        $sysParent = Join-Path $env:ProgramFiles ('tm-test-' + [Guid]::NewGuid().ToString('N'))
        $sys = Join-Path $sysParent 'Tentmaker'
        $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
        try {
            New-Item -ItemType Directory -Path $sys -Force | Out-Null
            Set-Content -LiteralPath (Join-Path $sys 'tentmaker.exe.prev') -Value 'planted'
            # Whether an elevated process's new files belong to the user or
            # to Administrators differs between hosts and PowerShell
            # editions, so make both the user's, as an earlier install left
            # them on the scenario runner.
            $me = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
            foreach ($p in @($sys, (Join-Path $sys 'tentmaker.exe.prev'))) {
                $out = & icacls.exe $p /setowner "*$me" /Q 2>&1
                if ($LASTEXITCODE -ne 0) { throw "icacls $p /setowner: $out" }
                Write-Host "     before: $p is owned by $((Get-Acl -LiteralPath $p).GetOwner([Security.Principal.SecurityIdentifier]).Value)"
            }
            & $installer -InstallDir $sys -NoSetup
            foreach ($p in @($sys, (Join-Path $sys 'tentmaker.exe'))) {
                $acl = Get-Acl -LiteralPath $p
                $owner = $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value
                if ($owner -eq 'S-1-5-32-544') { Pass "system install: Administrators own $p" } else { Bad "system install: $p is owned by $owner" }
                $mine = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]) | Where-Object { $_.IdentityReference.Value -eq $me })
                if ($mine.Count -eq 0) { Pass "system install: no entry for this user on $p" } else { Bad "system install: $p has an entry for $me" }
            }
            if (Test-Path -LiteralPath (Join-Path $sys 'tentmaker.exe.prev')) { Bad 'system install kept a planted tentmaker.exe.prev' } else { Pass 'system install removed a planted tentmaker.exe.prev' }
            # A rollback copy only Administrators could change survives a
            # reinstall.
            $prev = Join-Path $sys 'tentmaker.exe.prev'
            Set-Content -LiteralPath $prev -Value 'kept'
            $out = & icacls.exe $prev /setowner '*S-1-5-32-544' /Q 2>&1
            if ($LASTEXITCODE -ne 0) { throw "icacls $prev /setowner: $out" }
            & $installer -InstallDir $sys -NoSetup
            if (Test-Path -LiteralPath $prev) { Pass 'system reinstall kept an Administrators-owned tentmaker.exe.prev' } else { Bad 'system reinstall removed an Administrators-owned tentmaker.exe.prev' }
            # A folder Administrators own, but with an entry planted while
            # someone else did, and a staged file planted beside it: the
            # install replaces the DACL outright and makes a new file.
            Set-Content -LiteralPath (Join-Path $sys 'tentmaker.new.exe') -Value 'planted'
            $out = & icacls.exe $sys /grant '*S-1-5-32-545:(OI)(CI)M' "*${me}:(OI)(CI)F" /Q 2>&1
            if ($LASTEXITCODE -ne 0) { throw "icacls $sys /grant: $out" }
            & $installer -InstallDir $sys -NoSetup
            $write = [Security.AccessControl.FileSystemRights]'WriteData, AppendData, WriteAttributes, WriteExtendedAttributes, Delete, ChangePermissions, TakeOwnership'
            foreach ($p in @($sys, (Join-Path $sys 'tentmaker.exe'))) {
                $rules = @((Get-Acl -LiteralPath $p).GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
                $loose = @($rules | Where-Object { $_.AccessControlType -eq 'Allow' -and @('S-1-5-18', 'S-1-5-32-544') -notcontains $_.IdentityReference.Value -and ($_.FileSystemRights -band $write) })
                if ($loose.Count -eq 0) { Pass "system install: only SYSTEM and Administrators can change $p" } else { Bad "system install: $p lets $($loose[0].IdentityReference.Value) change it ($($loose[0].FileSystemRights))" }
            }
            # install.ps1 checks that the staged file inherited this before it
            # changes the owner; Windows drops it from the file after that.
            $limit = @((Get-Acl -LiteralPath $sys).GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]) | Where-Object { $_.IdentityReference.Value -eq 'S-1-3-4' -and ($_.InheritanceFlags -band [Security.AccessControl.InheritanceFlags]::ObjectInherit) })
            if ($limit.Count -gt 0) { Pass 'system install: new files in the folder inherit OWNER RIGHTS' } else { Bad 'system install: the folder passes no OWNER RIGHTS entry to new files' }
            $got = (& (Join-Path $sys 'tentmaker.exe') version 2>&1 | Out-String).Trim()
            if ($got -eq $Expect) { Pass 'system install replaced a planted tentmaker.new.exe' } else { Bad "system install: tentmaker.exe says '$got'" }
            # A system install outside a Tentmaker folder would put the DACL
            # on a folder others share, so it is refused.
            $threw = $false
            try { & $installer -InstallDir (Join-Path $sysParent 'Other') -NoSetup } catch { $threw = $true }
            if ($threw -and -not (Test-Path -LiteralPath (Join-Path $sysParent 'Other\tentmaker.exe'))) { Pass 'system install refuses a folder not named Tentmaker' } else { Bad 'system install went into a folder not named Tentmaker' }
        } catch { Bad "system install threw: $_" } finally {
            Remove-Item -LiteralPath $sysParent -Recurse -Force -ErrorAction SilentlyContinue
            [Environment]::SetEnvironmentVariable('Path', $machinePath, 'Machine')
        }
    } else { Write-Host 'skip system install: not Administrator' }
} finally {
    Remove-Item Env:TENTMAKER_DOWNLOAD_BASE -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures -gt 0) { Write-Host "$failures failed"; exit 1 }
Write-Host 'all passed'
