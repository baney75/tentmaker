# Windows-only steps for the scenario harness (lib.sh calls these through
# pwsh). Each function prints plain text on stdout and throws on failure.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$ScnSvc = 'Tentmaker'
$ScnHosts = "$env:SystemRoot\System32\drivers\etc\hosts"
$ScnWork = Join-Path $env:RUNNER_TEMP 'scn'

# Install-Scn BASE: the real one-line installer against the fake release.
function Install-Scn([string]$Base) {
    $env:TENTMAKER_DOWNLOAD_BASE = $Base
    $env:TENTMAKER_NO_SETUP = '1'
    Invoke-RestMethod -TimeoutSec 60 "$Base/install.ps1" | Invoke-Expression
    $exe = Join-Path $env:ProgramFiles 'Tentmaker\tentmaker.exe'
    if (-not (Test-Path $exe)) { throw "no $exe after the install" }
}

function Get-DaemonPid {
    (Get-CimInstance Win32_Service -Filter "Name='$ScnSvc'").ProcessId
}

# Kill-Daemon: what a crash looks like to the service manager.
function Kill-Daemon {
    $p = Get-DaemonPid
    if (-not $p) { throw 'the service has no process' }
    Stop-Process -Id $p -Force
    Write-Output $p
}

function Restart-Daemon {
    Restart-Service -Name $ScnSvc -Force
}

# Set-DaemonEnv "A=1 B=2": the service's environment, then a restart.
function Set-DaemonEnv([string]$Pairs) {
    $vals = [string[]]@($Pairs -split '\s+' | Where-Object { $_ })
    $key = "HKLM:\SYSTEM\CurrentControlSet\Services\$ScnSvc"
    New-ItemProperty -Path $key -Name Environment -PropertyType MultiString -Value $vals -Force | Out-Null
    Restart-Service -Name $ScnSvc -Force
}

# Runner-Procs: processes owned by the sandbox user, one per line.
function Runner-Procs {
    Get-Process -IncludeUserName -ErrorAction SilentlyContinue |
        Where-Object { $_.UserName -and $_.UserName -like '*\tentmaker-runner' } |
        ForEach-Object { "$($_.Id) $($_.ProcessName)" }
}

$ScnTcp = @'
using System;
using System.Runtime.InteropServices;
public static class ScnTcp {
    [StructLayout(LayoutKind.Sequential)]
    public struct MIB_TCPROW { public uint state, localAddr, localPort, remoteAddr, remotePort; }
    [DllImport("iphlpapi.dll")]
    public static extern uint SetTcpEntry(ref MIB_TCPROW row);
    public static uint Reset(uint la, uint lp, uint ra, uint rp) {
        var r = new MIB_TCPROW { state = 12, localAddr = la, localPort = lp, remoteAddr = ra, remotePort = rp };
        return SetTcpEntry(ref r);
    }
}
'@

function Addr32([string]$ip) { [BitConverter]::ToUInt32(([Net.IPAddress]::Parse($ip)).GetAddressBytes(), 0) }
function Port32([int]$port) { [uint32]((($port -band 0xff) -shl 8) -bor (($port -shr 8) -band 0xff)) }

# Block-Api: api.github.com unreachable for the daemon: the hosts file points
# it at 127.0.0.1, a firewall rule blocks its addresses, and the daemon's open
# connections to them are reset.
function Block-Api {
    $ips = @([Net.Dns]::GetHostAddresses('api.github.com') | Where-Object { $_.AddressFamily -eq 'InterNetwork' } | ForEach-Object { $_.ToString() })
    $daemon = Get-DaemonPid
    $conns = @(Get-NetTCPConnection -State Established -RemotePort 443 -ErrorAction SilentlyContinue | Where-Object { $_.OwningProcess -eq $daemon })
    # git push goes to github.com, which must stay reachable.
    $keep = @([Net.Dns]::GetHostAddresses('github.com') | ForEach-Object { $_.ToString() })
    $conns = @($conns | Where-Object { $keep -notcontains $_.RemoteAddress })
    $ips = @($ips + @($conns | ForEach-Object { $_.RemoteAddress }) | Where-Object { $_ -match '^\d+\.\d+\.\d+\.\d+$' -and $keep -notcontains $_ } | Sort-Object -Unique)
    Copy-Item $ScnHosts (Join-Path $ScnWork 'hosts.bak') -Force
    Add-Content -Path $ScnHosts -Value "`r`n127.0.0.1 api.github.com"
    Clear-DnsClientCache
    Set-NetFirewallProfile -Profile Domain, Public, Private -Enabled True
    New-NetFirewallRule -DisplayName 'tm-scn-block-api' -Direction Outbound -Action Block -RemoteAddress $ips | Out-Null
    Add-Type -TypeDefinition $ScnTcp
    foreach ($c in $conns) {
        if ($c.RemoteAddress -notmatch '^\d+\.\d+\.\d+\.\d+$') { continue }
        $rc = [ScnTcp]::Reset((Addr32 $c.LocalAddress), (Port32 $c.LocalPort), (Addr32 $c.RemoteAddress), (Port32 $c.RemotePort))
        Write-Output "reset $($c.LocalAddress):$($c.LocalPort) -> $($c.RemoteAddress):443 rc=$rc"
    }
    Write-Output "blocked $($ips -join ' ')"
}

function Unblock-Api {
    Remove-NetFirewallRule -DisplayName 'tm-scn-block-api' -ErrorAction SilentlyContinue
    $bak = Join-Path $ScnWork 'hosts.bak'
    if (Test-Path $bak) { Copy-Item $bak $ScnHosts -Force }
    Clear-DnsClientCache
    Write-Output 'unblocked'
}

# Free-GiB: free space on the state directory's drive.
function Free-GiB {
    $d = Get-PSDrive -Name ($env:ProgramData.Substring(0, 1))
    [math]::Floor($d.Free / 1GB)
}

# Fill-Disk LEAVE_GIB: a file that leaves LEAVE_GIB free on the state drive.
function Fill-Disk([int]$LeaveGiB) {
    $d = Get-PSDrive -Name ($env:ProgramData.Substring(0, 1))
    $size = [int64]$d.Free - [int64]$LeaveGiB * 1GB
    if ($size -le 0) { throw "only $([math]::Floor($d.Free / 1GB)) GiB free" }
    $f = "$($env:ProgramData.Substring(0, 2))\tm-scn-fill"
    & fsutil file createnew $f $size | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "fsutil exited $LASTEXITCODE" }
    Write-Output (Free-GiB)
}

function Unfill-Disk {
    Remove-Item -Force "$($env:ProgramData.Substring(0, 2))\tm-scn-fill" -ErrorAction SilentlyContinue
    Write-Output (Free-GiB)
}
