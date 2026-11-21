# Attacks a Windows check job makes on the device that runs it, for the
# abuse test (.github/workflows/abuse.yml, through tentmaker-abuse.yml).
# Every attack prints exactly one line:
#
#   ATTACK <name> BLOCKED [detail]
#   ATTACK <name> SUCCEEDED [detail]
#
# and the harness fails on any SUCCEEDED or any missing line. CONTROL lines
# show that the job still has what it needs (the internet, DNS), so a
# blocked attack means the sandbox stopped it, not that nothing works.
#
#   attack.ps1 main | sibling | oom | pids | lan | closed
param([string]$Mode = 'main')
$ErrorActionPreference = 'Continue'
$ProgressPreference = 'SilentlyContinue'

function Say([string]$Name, [string]$Result, [string]$Detail = '') {
    Write-Output (("ATTACK $Name $Result $Detail").TrimEnd())
}
function Now { [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() }

# HttpOk: the decoy (or any server) answered 200.
function HttpOk([string]$Url, [hashtable]$Headers = @{}) {
    try {
        $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 6 -Headers $Headers -Uri $Url -ErrorAction Stop
        return $r.StatusCode -eq 200
    } catch { return $false }
}
function Reach([string]$Name, [string]$Url, [hashtable]$Headers = @{}) {
    if (HttpOk $Url $Headers) { Say $Name SUCCEEDED "$Url answered" } else { Say $Name BLOCKED }
}
# UdpOk: a UDP decoy (or DNS server) answered a DNS query for github.com.
function UdpOk([string]$Addr, [int]$Port) {
    try {
        $ip = [System.Net.IPAddress]::Parse($Addr)
        $u = [System.Net.Sockets.UdpClient]::new($ip.AddressFamily)
        $u.Client.ReceiveTimeout = 4000
        $q = [byte[]](0x7a, 0x11, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 6, 103, 105, 116, 104, 117, 98, 3, 99, 111, 109, 0, 0, 1, 0, 1)
        [void]$u.Send($q, $q.Length, [System.Net.IPEndPoint]::new($ip, $Port))
        $any = if ($ip.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetworkV6) { [System.Net.IPAddress]::IPv6Any } else { [System.Net.IPAddress]::Any }
        $ep = [System.Net.IPEndPoint]::new($any, 0)
        $r = $u.Receive([ref]$ep)
        $u.Close()
        return $r.Length -gt 0
    } catch { return $false }
}
function UdpReach([string]$Name, [string]$Addr, [int]$Port) {
    if (UdpOk $Addr $Port) { Say $Name SUCCEEDED "udp ${Addr}:$Port answered" } else { Say $Name BLOCKED }
}
# OwnTcp and OwnUdp: the job's own server on a loopback address answers the
# job, as a test's server (httptest, a test database, a Gradle daemon) does
# on GitHub's runners. Each listens on a free port, connects, and checks
# bytes come back.
function OwnTcp([string]$Addr) {
    $l = $null; $c = $null; $a = $null
    try {
        $ip = [System.Net.IPAddress]::Parse($Addr)
        $l = [System.Net.Sockets.TcpListener]::new($ip, 0)
        $l.Start()
        $c = [System.Net.Sockets.TcpClient]::new($ip.AddressFamily)
        if (-not ($c.ConnectAsync($ip, $l.LocalEndpoint.Port).Wait(5000) -and $c.Connected)) { return 'connect timed out' }
        $t = $l.AcceptTcpClientAsync()
        if (-not $t.Wait(5000)) { return 'accept timed out' }
        $a = $t.Result
        $a.GetStream().Write([byte[]](116, 109, 10), 0, 3)
        $s = $c.GetStream()
        $s.ReadTimeout = 5000
        $buf = [byte[]]::new(3)
        $n = $s.Read($buf, 0, 3)
        if ($n -ne 3) { return "read $n bytes" }
        return ''
    } catch { return $_.Exception.Message } finally {
        foreach ($x in $a, $c) { if ($x) { $x.Close() } }
        if ($l) { $l.Stop() }
    }
}
function OwnUdp([string]$Addr) {
    $srv = $null; $cl = $null
    try {
        $ip = [System.Net.IPAddress]::Parse($Addr)
        $srv = [System.Net.Sockets.UdpClient]::new([System.Net.IPEndPoint]::new($ip, 0))
        $srv.Client.ReceiveTimeout = 5000
        $cl = [System.Net.Sockets.UdpClient]::new($ip.AddressFamily)
        [void]$cl.Send([byte[]](116, 109, 10), 3, [System.Net.IPEndPoint]::new($ip, $srv.Client.LocalEndPoint.Port))
        $ep = [System.Net.IPEndPoint]::new($ip, 0)
        $r = $srv.Receive([ref]$ep)
        if ($r.Length -ne 3) { return "got $($r.Length) bytes" }
        return ''
    } catch { return $_.Exception.Message } finally {
        foreach ($x in $srv, $cl) { if ($x) { $x.Close() } }
    }
}
function OwnLoopback([string]$Name, [string]$Why) {
    if ($Why) { Write-Output "CONTROL $Name FAILED $Why" } else { Write-Output "CONTROL $Name OK" }
}
function HostIP {
    try {
        $s = [System.Net.Sockets.Socket]::new([System.Net.Sockets.AddressFamily]::InterNetwork, [System.Net.Sockets.SocketType]::Dgram, [System.Net.Sockets.ProtocolType]::Udp)
        $s.Connect('192.0.2.1', 53)
        $ip = $s.LocalEndPoint.Address.ToString()
        $s.Close()
        return $ip
    } catch { return '' }
}
function CanWrite([string]$Dir) {
    $f = Join-Path $Dir "tm-abuse-$PID.txt"
    try {
        [System.IO.File]::WriteAllText($f, 'tentmaker abuse test')
        Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue
        return $true
    } catch { return $false }
}

$jobDir = Split-Path -Parent $env:USERPROFILE
$jobsDir = Split-Path -Parent $jobDir
$tm = Join-Path $env:ProgramData 'Tentmaker'
$state = Join-Path $tm 'state'

switch ($Mode) {
    'sibling' {
        $secret = 'tm-sibling-secret-' + [Guid]::NewGuid().ToString('N')
        [System.IO.File]::WriteAllText((Join-Path $env:USERPROFILE 'tm-sibling-secret'), $secret)
        Write-Output "SIBLING start $(Now) dir $jobDir"
        Start-Sleep -Seconds 150
        Write-Output "SIBLING end $(Now)"
        exit 0
    }
    'oom' {
        # Allocate past the job's limit (2 GiB in the harness). The sandbox
        # must stop the job; a line here means it didn't.
        # Native memory, not .NET arrays: .NET caps its own heap at 75% of a
        # job object's limit and fails first, so arrays never test the
        # sandbox's limit. AllocHGlobal commits outside that heap, against
        # the job object's limit alone.
        # A failed allocation ends the script with an error, as it would end
        # any program.
        $ErrorActionPreference = 'Stop'
        $blocks = [System.Collections.Generic.List[IntPtr]]::new()
        for ($i = 0; $i -lt 128; $i++) { $blocks.Add([System.Runtime.InteropServices.Marshal]::AllocHGlobal(64MB)) }
        if ($blocks.Count -eq 128) { Write-Output "ATTACK memory-limit SUCCEEDED allocated $($blocks.Count * 64) MiB" }
        exit 0
    }
    'pids' {
        # A process bomb that stops at 3000. Reaching that many means nothing
        # bounds the job's processes. It runs in a child shell: when the job's
        # memory runs out, that shell can die (stack overflow), and this one
        # still reports.
        $count = Join-Path $env:TEMP "tm-bomb-$PID.txt"
        $bomb = {
            param($count)
            $procs = [System.Collections.Generic.List[System.Diagnostics.Process]]::new()
            $err = ''
            for ($i = 0; $i -lt 3000; $i++) {
                try {
                    $si = [System.Diagnostics.ProcessStartInfo]::new("$env:SystemRoot\System32\PING.EXE", '-n 120 127.0.0.1')
                    $si.UseShellExecute = $false
                    $si.CreateNoWindow = $true
                    $si.RedirectStandardOutput = $true
                    $procs.Add([System.Diagnostics.Process]::Start($si))
                    if ($procs.Count % 50 -eq 0) { Set-Content -Path $count -Value $procs.Count }
                } catch {
                    $err = $_.Exception.Message
                    break
                }
            }
            Set-Content -Path $count -Value "$($procs.Count) $err"
            foreach ($p in $procs) { try { $p.Kill() } catch {} }
        }
        $shell = (Get-Process -Id $PID).Path
        & $shell -NoProfile -NonInteractive -Command $bomb -args $count *> $null
        $code = $LASTEXITCODE
        Get-Process PING -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
        $line = ''
        try { $line = (Get-Content -Path $count -Raw -ErrorAction Stop).Trim() } catch {}
        $n = 0
        $err = ''
        if ($line -match '^(\d+)\s*(.*)$') { $n = [int]$Matches[1]; $err = $Matches[2] }
        if ($code -ne 0) { $err = "its shell died (exit $code) $err".Trim() }
        if ($n -ge 3000) { Say fork-bomb SUCCEEDED "started $n processes" } else { Say fork-bomb BLOCKED "after $n processes: $err" }
        exit 0
    }
    'lan' {
        # Run with [guards] allow_lan = true: the tailnet and this host stay shut.
        if (HttpOk 'https://api.github.com/zen') { Write-Output 'CONTROL internet OK' } else { Write-Output 'CONTROL internet FAILED' }
        $ip = HostIP
        Reach lan-decoy-tailnet-v4 'http://100.64.77.1:8080/'
        Reach lan-mesh-port-host-ip "http://${ip}:7433/"
        Reach lan-host-own-v4 'http://192.0.2.77:8767/'
        Reach lan-host-own-v6 'http://[2001:db8:77::1]:8767/'
        Reach lan-host-ip-other-port "http://${ip}:8767/"
        exit 0
    }
    'closed' {
        # Queued while the harness has removed the network rules: the device
        # must refuse to start it, so any line here means it ran.
        Write-Output 'ATTACK fail-closed SUCCEEDED the job ran without the network rules'
        Reach closed-decoy-tailnet-v4 'http://100.64.77.1:8080/'
        exit 0
    }
    'main' {}
    default { Write-Error 'usage: attack.ps1 main|sibling|oom|pids|lan|closed'; exit 2 }
}

Write-Output "tentmaker abuse: job dir $jobDir, user $([Security.Principal.WindowsIdentity]::GetCurrent().Name)"

# Controls: a sandbox that blocks everything would pass every attack.
if (HttpOk 'https://api.github.com/zen') { Write-Output 'CONTROL internet OK' } else { Write-Output 'CONTROL internet FAILED' }
try { [System.Net.Dns]::GetHostAddresses('github.com') | Out-Null; Write-Output 'CONTROL dns OK' } catch { Write-Output 'CONTROL dns FAILED' }
# The job's own loopback works, TCP and UDP, 127.0.0.1 and ::1. The host's
# loopback services stay closed (loopback-other-port and the rest below).
OwnLoopback own-loopback-v4 (OwnTcp '127.0.0.1')
OwnLoopback own-loopback-v6 (OwnTcp '::1')
OwnLoopback own-loopback-udp-v4 (OwnUdp '127.0.0.1')
OwnLoopback own-loopback-udp-v6 (OwnUdp '::1')

# Forge tokens and the control token.
$got = ''
foreach ($f in @(Get-ChildItem -LiteralPath (Join-Path $tm 'secrets') -File -ErrorAction SilentlyContinue) + @(Get-Item -LiteralPath (Join-Path $tm 'secrets\github-token') -ErrorAction SilentlyContinue)) {
    if ($f) { try { [System.IO.File]::ReadAllBytes($f.FullName) | Out-Null; $got = $f.FullName } catch {} }
}
try { [System.IO.File]::ReadAllBytes((Join-Path $tm 'secrets\github-token')) | Out-Null; $got = 'secrets\github-token' } catch {}
if ($got) { Say read-secrets SUCCEEDED "read $got" } else { Say read-secrets BLOCKED }
try { [System.IO.File]::ReadAllText((Join-Path $state 'control.token')) | Out-Null; Say read-control-token SUCCEEDED } catch { Say read-control-token BLOCKED }

# The control API's POSTs need the token.
function TryPause([string]$Token = '') {
    $h = @{}
    if ($Token) { $h['Authorization'] = "Bearer $Token" }
    try {
        $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 6 -Method Post -ContentType 'application/json' -Headers $h `
            -Body '{"reason":"abuse test","for_s":600}' -Uri 'http://127.0.0.1:7434/v1/pause' -ErrorAction Stop
        return $r.StatusCode -eq 200
    } catch { return $false }
}
# The relay port answers the job, so the pause attacks are refused by the
# API, not cut off by the network (Tentmaker's WFP filters permit only it on
# this host).
# The source archive relay answers the job: on 7435 once it has a listener
# of its own, on 7434 while it shares the control API's.
$relayOk = $false
foreach ($p in 7435, 7434) {
    try {
        $c = [System.Net.Sockets.TcpClient]::new([System.Net.Sockets.AddressFamily]::InterNetwork)
        $relayOk = $c.ConnectAsync('127.0.0.1', $p).Wait(4000) -and $c.Connected
        $c.Close()
    } catch { $relayOk = $false }
    if ($relayOk) { break }
}
if ($relayOk) { Write-Output 'CONTROL relay OK' } else { Write-Output 'CONTROL relay FAILED' }
# The control API's GETs need no token, so the job must not reach it.
Reach control-api-read 'http://127.0.0.1:7434/v1/status'
if (TryPause) { Say pause-no-token SUCCEEDED } else { Say pause-no-token BLOCKED }
$guessed = $false
foreach ($t in @(('0' * 64), ('f' * 64), [Guid]::NewGuid().ToString('N') + [Guid]::NewGuid().ToString('N'))) {
    if (TryPause $t) { $guessed = $true }
}
if ($guessed) { Say pause-guessed-token SUCCEEDED } else { Say pause-guessed-token BLOCKED }

# The network: tailnet, MagicDNS, LAN, multicast, link-local, this host's
# own addresses, the mesh port on every address, loopback services.
Reach decoy-tailnet-v4 'http://100.64.77.1:8080/'
Reach decoy-tailnet-v6 'http://[fd7a:115c:a1e0::77]:8080/'
Reach magicdns-http 'http://100.100.100.100/'
UdpReach magicdns-dns '100.100.100.100' 53
Reach decoy-lan-v4 'http://10.77.0.1:8080/'
UdpReach lan-multicast '239.77.77.77' 7777
Reach cloud-metadata 'http://169.254.169.254/metadata/instance?api-version=2021-02-01' @{ Metadata = 'true' }
Reach host-own-v4 'http://192.0.2.77:8767/'
Reach host-own-v6 'http://[2001:db8:77::1]:8767/'
$ip = HostIP
if ($ip) { Reach host-ip-other-port "http://${ip}:8767/" } else { Say host-ip-other-port BLOCKED 'no host address found' }
if ($ip) { Reach mesh-port-host-ip "http://${ip}:7433/" } else { Say mesh-port-host-ip BLOCKED 'no host address found' }
Reach mesh-port-lan 'http://10.77.0.1:7433/'
Reach mesh-port-tailnet 'http://100.64.77.1:7433/'
Reach mesh-port-tailnet-v6 'http://[fd7a:115c:a1e0::77]:7433/'
Reach mesh-port-host-v6 'http://[2001:db8:77::1]:7433/'
Reach mesh-port-loopback 'http://127.0.0.1:7433/'
Reach mesh-port-loopback-v6 'http://[::1]:7433/'
Reach loopback-other-port 'http://127.0.0.1:8766/'
Reach loopback-v6-other-port 'http://[::1]:8766/'
# A host service bound to every address ([::] and 0.0.0.0), by loopback.
Reach loopback-wildcard 'http://127.0.0.1:8767/'
Reach loopback-v6-wildcard 'http://[::1]:8767/'
# A host UDP service on loopback (a DNS cache, a metrics agent).
UdpReach loopback-udp '127.0.0.1' 8768
UdpReach loopback-v6-udp '::1' 8768
# tailscaled's local API on its named pipe: Windows lets only
# administrators and the signed-in owner use it.
try {
    $pipe = [System.IO.Pipes.NamedPipeClientStream]::new('.', 'ProtectedPrefix\Administrators\Tailscale\tailscaled', [System.IO.Pipes.PipeDirection]::InOut)
    $pipe.Connect(3000)
    $w = [System.IO.StreamWriter]::new($pipe)
    $w.Write("GET /localapi/v0/status HTTP/1.0`r`nHost: local-tailscaled.sock`r`nSec-Tailscale: localapi`r`n`r`n")
    $w.Flush()
    $first = [System.IO.StreamReader]::new($pipe).ReadLine()
    $pipe.Dispose()
    if ($first -match '^HTTP/1\.\d 200') { Say tailscaled-socket SUCCEEDED "the pipe answered $first" } else { Say tailscaled-socket BLOCKED "the pipe answered $first" }
} catch { Say tailscaled-socket BLOCKED $_.Exception.Message }

# Privilege.
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
& net.exe session *> $null
if ($admin -or $LASTEXITCODE -eq 0 -or (CanWrite "$env:SystemRoot")) { Say admin SUCCEEDED } else { Say admin BLOCKED }
$wrote = ''
foreach ($h in Get-ChildItem -LiteralPath 'C:\Users' -Directory -Force -ErrorAction SilentlyContinue) {
    if ($h.Name -in @('Public', 'Default', 'Default User', 'All Users')) { continue }
    if (CanWrite $h.FullName) { $wrote = $h.FullName }
}
if ($wrote) { Say write-other-home SUCCEEDED "wrote in $wrote" } else { Say write-other-home BLOCKED }
$wrote = ''
foreach ($d in @($tm, $state, (Join-Path $state 'checks'), (Join-Path $tm 'logs'), (Join-Path $tm 'secrets'), $jobsDir, (Join-Path $env:ProgramFiles 'Tentmaker'))) {
    if (CanWrite $d) { $wrote = $d }
}
if ($wrote) { Say write-state-dir SUCCEEDED "wrote in $wrote" } else { Say write-state-dir BLOCKED }

# The daemon runs as SYSTEM.
$svc = Get-CimInstance Win32_Service -Filter "Name='Tentmaker'" -ErrorAction SilentlyContinue
if (-not $svc -or -not $svc.ProcessId) {
    Say signal-daemon BLOCKED 'daemon pid not visible'
} else {
    try { Stop-Process -Id $svc.ProcessId -Force -ErrorAction Stop; Say signal-daemon SUCCEEDED "stopped $($svc.ProcessId)" } catch { Say signal-daemon BLOCKED }
}

# Leftovers: the harness checks after the job that these are gone.
try {
    Start-Process -WindowStyle Hidden -FilePath "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" `
        -ArgumentList '-NoProfile', '-Command', "Start-Sleep -Seconds 600; 'tm-abuse-leftover'" -ErrorAction Stop
} catch { Write-Output "start leftover: $($_.Exception.Message)" }
try {
    $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -ErrorAction Stop -Arguments @{
        CommandLine = "`"$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe`" -NoProfile -Command `"Start-Sleep -Seconds 600; 'tm-abuse-leftover-wmi'`""
    }
    Write-Output "wmi leftover: return $($r.ReturnValue) pid $($r.ProcessId)"
} catch { Write-Output "wmi leftover: $($_.Exception.Message)" }
Write-Output 'PLANTED background-process'

# Another running job's files: found through the process list.
$t0 = Now
$found = ''
$seen = 'no'
while ((Now) - $t0 -lt 100 -and -not $found) {
    $cands = @()
    foreach ($d in Get-ChildItem -LiteralPath $jobsDir -Directory -ErrorAction SilentlyContinue) {
        if ($d.FullName -ne $jobDir) { $cands += $d.FullName }
    }
    foreach ($p in Get-CimInstance Win32_Process -ErrorAction SilentlyContinue) {
        if ($p.CommandLine -match '--out\s+"?([^"\s]+)\\out') {
            $d = $Matches[1]
            if ($d -ne $jobDir) { $cands += $d }
        }
    }
    foreach ($d in $cands | Select-Object -Unique) {
        $seen = 'yes'
        try {
            $v = [System.IO.File]::ReadAllText((Join-Path $d 'home\tm-sibling-secret'))
            if ($v.StartsWith('tm-sibling-secret-')) { $found = $d; break }
        } catch {}
    }
    if (-not $found) { Start-Sleep -Seconds 2 }
}
Write-Output "READ-OTHER-JOB window $t0 $(Now) sibling-visible $seen"
if ($found) { Say read-other-job SUCCEEDED "read $found" } else { Say read-other-job BLOCKED }

Write-Output 'tentmaker abuse: done'
exit 0
