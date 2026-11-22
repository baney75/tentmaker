<#
.SYNOPSIS
  The part of test-install.ps1's system install case that checks a folder this
  user could change is set aside, not repaired. test-install.ps1 calls it with
  the folder its earlier steps installed into.
#>
param(
    [Parameter(Mandatory = $true)][string]$Installer,
    [Parameter(Mandatory = $true)][string]$Sys,
    [Parameter(Mandatory = $true)][string]$Me
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0
$failures = 0
function Pass([string]$m) { Write-Host "ok   $m" }
function Bad([string]$m) { Write-Host "FAIL $m"; $script:failures++ }
$sys = $Sys; $me = $Me; $installer = $Installer
$sysParent = Split-Path -Parent $Sys

# A folder Administrators own but this user could change (they
# might hold a WRITE_DAC handle a new DACL can't revoke), with a
# staged file and a DLL planted in it: it is set aside, not
# repaired, and nothing planted is in the new folder.
Set-Content -LiteralPath (Join-Path $sys 'tentmaker.new.exe') -Value 'planted'
Set-Content -LiteralPath (Join-Path $sys 'version.dll') -Value 'planted'
$out = & icacls.exe $sys /grant '*S-1-5-32-545:(OI)(CI)M' "*${me}:(OI)(CI)F" /Q 2>&1
if ($LASTEXITCODE -ne 0) { throw "icacls $sys /grant: $out" }
$out = & icacls.exe $sys /setowner '*S-1-5-32-544' /T /Q 2>&1
if ($LASTEXITCODE -ne 0) { throw "icacls $sys /setowner: $out" }
& $installer -InstallDir $sys -NoSetup
if (@(Get-ChildItem -LiteralPath $sysParent -Directory -Filter 'Tentmaker.untrusted-*').Count -gt 0) { Pass 'system install set aside a folder this user could change' } else { Bad 'system install repaired a folder this user could change in place' }
if (Test-Path -LiteralPath (Join-Path $sys 'version.dll')) { Bad 'system install kept a planted version.dll' } else { Pass 'system install left a planted DLL behind' }
# A clean folder Administrators own is kept, with its rollback copy.
$prev = Join-Path $sys 'tentmaker.exe.prev'
Set-Content -LiteralPath $prev -Value 'kept'
$out = & icacls.exe $prev /setowner '*S-1-5-32-544' /Q 2>&1
if ($LASTEXITCODE -ne 0) { throw "icacls $prev /setowner: $out" }
$asides = @(Get-ChildItem -LiteralPath $sysParent -Directory -Filter 'Tentmaker.untrusted-*').Count
& $installer -InstallDir $sys -NoSetup
if (@(Get-ChildItem -LiteralPath $sysParent -Directory -Filter 'Tentmaker.untrusted-*').Count -eq $asides -and (Test-Path -LiteralPath $prev)) { Pass 'system reinstall kept a clean folder in place' } else { Bad 'system reinstall set aside a clean folder' }

exit $failures
