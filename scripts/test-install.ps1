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
} finally {
    Remove-Item Env:TENTMAKER_DOWNLOAD_BASE -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures -gt 0) { Write-Host "$failures failed"; exit 1 }
Write-Host 'all passed'
