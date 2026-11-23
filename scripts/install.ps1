<#
.SYNOPSIS
  Install Tentmaker on Windows (PowerShell 5.1 and 7).

.DESCRIPTION
  Downloads the Tentmaker release for this CPU, refuses to install unless its
  sha256 matches the release's SHA256SUMS, checks that it runs, moves it into
  place, adds the install directory to PATH and, with -Yes, runs
  `tentmaker setup --yes`. It never prompts.

  One line, from an Administrator PowerShell:

    irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex

  Piped through iex the script can't take parameters, so it reads these
  environment variables instead (set them first, in the same session):

    $env:TENTMAKER_CHANNEL     = 'dev'       # stable (default) or dev
    $env:TENTMAKER_VERSION     = '0.1.0'     # a specific stable release
    $env:TENTMAKER_INSTALL_DIR = 'D:\Tools\Tentmaker'
    $env:TENTMAKER_NO_SETUP    = '1'         # only install the binary
    $env:TENTMAKER_YES         = '1'         # run tentmaker setup --yes
    $env:TENTMAKER_SETUP_ARGS  = '--memory 4GiB --boost-memory 12GiB --repo owner/name'
    $env:TENTMAKER_JSON        = '1'         # for agents: one JSON line of output

  From a checkout of your repo, this sets up the machine and opens a pull
  request that moves Windows CI onto it (setup runs in the current folder):

    $env:TENTMAKER_YES = '1'; $env:TENTMAKER_SETUP_ARGS = '--repo-here --pr'
    irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex

  TENTMAKER_SETUP_ARGS is split on spaces; an argument with a space in it
  needs the parameter form below. Saved as a file, or as a script block, it
  takes parameters:

    .\install.ps1 -Channel dev -Yes -SetupArgs '--memory','4GiB'
    & ([scriptblock]::Create((irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1))) -Channel dev -Yes

  TENTMAKER_DOWNLOAD_BASE replaces https://github.com/baney75/tentmaker/releases
  (a URL or a local directory with the same layout) for testing.

.PARAMETER Channel
  stable (default) or dev.
.PARAMETER Version
  A specific stable release, like 0.1.0.
.PARAMETER InstallDir
  Where tentmaker.exe goes. Default "$env:ProgramFiles\Tentmaker", which needs
  Administrator.
.PARAMETER NoSetup
  Only install the binary.
.PARAMETER Yes
  Run `tentmaker setup --yes` after installing, with -SetupArgs.
.PARAMETER SetupArgs
  Extra arguments for `tentmaker setup`.
.PARAMETER Json
  For agents: the only output is one JSON line, setup's result with -Yes,
  else what was installed and the next step; a failure writes
  {"ok":false,"error":...,"fix":...} before it throws. On too when
  -SetupArgs holds --json.
#>

function Install-Tentmaker {
    [CmdletBinding()]
    param(
        [string]$Channel = $(if ($env:TENTMAKER_CHANNEL) { $env:TENTMAKER_CHANNEL } else { 'stable' }),
        [string]$Version = $env:TENTMAKER_VERSION,
        [string]$InstallDir = $(if ($env:TENTMAKER_INSTALL_DIR) { $env:TENTMAKER_INSTALL_DIR } else { Join-Path $env:ProgramFiles 'Tentmaker' }),
        [switch]$NoSetup = $(@('1', 'true', 'yes') -contains "$env:TENTMAKER_NO_SETUP".ToLowerInvariant()),
        [switch]$Yes = $(@('1', 'true', 'yes') -contains "$env:TENTMAKER_YES".ToLowerInvariant()),
        [string[]]$SetupArgs = $(if ($env:TENTMAKER_SETUP_ARGS) { @($env:TENTMAKER_SETUP_ARGS -split '\s+' | Where-Object { $_ }) } else { @() }),
        [switch]$Json = $(@('1', 'true', 'yes') -contains "$env:TENTMAKER_JSON".ToLowerInvariant())
    )
    if ($SetupArgs -contains '--json') { $Json = [switch]$true } elseif ($Json) { $SetupArgs = @('--json') + $SetupArgs }
    # Once setup runs it prints the JSON line, even when it fails.
    $setupRan = $false

    # Scoped to this function, so a piped run doesn't change the caller's session.
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is far slower with the progress bar
    Set-StrictMode -Version 2.0

    $repoUrl = 'https://github.com/baney75/tentmaker'
    $rawInstall = 'irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex'

    function Say([string]$msg) { Write-Host "tentmaker-install: $msg" }
    function Fail([string]$msg, [string]$fix) {
        if ($Json -and -not $setupRan) {
            Write-Output ([ordered]@{ ok = $false; error = $msg; fix = $fix } | ConvertTo-Json -Compress)
        }
        $text = "tentmaker-install: error: $msg"
        if ($fix) { $text += "`ntentmaker-install: fix: $fix" }
        throw $text
    }

    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        Fail 'this is the Windows installer' 'on Linux or macOS run: curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh | sh'
    }

    $Channel = $Channel.Trim().ToLowerInvariant()
    if ($Channel -ne 'stable' -and $Channel -ne 'dev') {
        Fail "unknown channel '$Channel'" 'use -Channel stable or -Channel dev (there are no other channels)'
    }
    if ($Version) {
        $Version = $Version.Trim().TrimStart('v')
        if ($Channel -ne 'stable') { Fail "-Version picks a stable release, but -Channel is $Channel" 'drop -Channel dev, or drop -Version for the newest dev build' }
        if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$') { Fail "'$Version' is not a release version" 'use -Version X.Y.Z, like -Version 0.1.0' }
    }

    $arch = $env:PROCESSOR_ARCHITEW6432
    if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
    switch ($arch) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { Fail "unsupported CPU '$arch'" 'Tentmaker ships amd64 and arm64 builds' }
    }
    $asset = "tentmaker_windows_$arch.zip"

    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)
    $systemDirs = @($env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:ProgramData, $env:SystemRoot) | Where-Object { $_ }
    $isSystem = $false
    foreach ($d in $systemDirs) {
        if ($InstallDir.StartsWith($d.TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase) -or
            $InstallDir.Equals($d.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)) { $isSystem = $true }
    }
    if ($isSystem -and -not $isAdmin) {
        Fail "installing to $InstallDir needs Administrator" "open PowerShell as Administrator (Start, type PowerShell, right-click, Run as administrator) and run: $rawInstall"
    }

    # PowerShell 5.1 defaults to TLS 1.0; GitHub needs 1.2.
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    }

    $base = if ($env:TENTMAKER_DOWNLOAD_BASE) { $env:TENTMAKER_DOWNLOAD_BASE.TrimEnd('/', '\') } else { "$repoUrl/releases" }
    if ($Version) { $dl = "$base/download/v$Version"; $what = "v$Version" }
    elseif ($Channel -eq 'dev') { $dl = "$base/download/dev"; $what = 'the dev channel' }
    else { $dl = "$base/latest/download"; $what = 'the latest stable release' }

    function Icacls([string]$path) {
        $ErrorActionPreference = 'Continue'
        $out = & icacls.exe $path @args /Q 2>&1
        $code = $LASTEXITCODE
        $ErrorActionPreference = 'Stop'
        if ($code -ne 0) { Fail "icacls $path $($args -join ' ') failed: $out" 'run the installer from an Administrator PowerShell' }
    }

    function Fetch([string]$src, [string]$dst) {
        if ($src -notmatch '^https?://') {
            $local = $src -replace '/', '\'
            if (-not (Test-Path -LiteralPath $local -PathType Leaf)) { Fail "not found: $local" 'check TENTMAKER_DOWNLOAD_BASE' }
            Copy-Item -LiteralPath $local -Destination $dst -Force
            return
        }
        $last = $null
        for ($i = 1; $i -le 3; $i++) {
            try {
                Invoke-WebRequest -Uri $src -OutFile $dst -UseBasicParsing -TimeoutSec 300 -Headers @{ 'User-Agent' = 'tentmaker-install' }
                return
            } catch {
                $last = $_
                if ($i -lt 3) { Start-Sleep -Seconds (2 * $i) }
            }
        }
        Fail "download failed: $src ($($last.Exception.Message))" "check your network, or that the release exists at $repoUrl/releases"
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('tentmaker-install-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    $staged = $null
    try {
        Say "downloading $asset from $what"
        Fetch "$dl/SHA256SUMS" (Join-Path $tmp 'SHA256SUMS')
        $zipPath = Join-Path $tmp $asset
        Fetch "$dl/$asset" $zipPath

        $want = $null
        foreach ($line in Get-Content -LiteralPath (Join-Path $tmp 'SHA256SUMS')) {
            $f = @($line.Trim() -split '\s+')
            if ($f.Count -eq 2 -and ($f[1] -eq $asset -or $f[1] -eq "*$asset")) { $want = $f[0].ToLowerInvariant(); break }
        }
        if (-not $want) { Fail "SHA256SUMS has no entry for $asset" "the release may be incomplete; retry later or report it at $repoUrl/issues" }
        # The temp folder is the user's, so the archive could change after a
        # check. Read it once, and check and extract the same bytes.
        $zipBytes = [IO.File]::ReadAllBytes($zipPath)
        $sha = [Security.Cryptography.SHA256]::Create()
        try { $got = -join ($sha.ComputeHash($zipBytes) | ForEach-Object { $_.ToString('x2') }) } finally { $sha.Dispose() }
        if ($got -ne $want) {
            Fail "checksum mismatch for ${asset}: got $got, SHA256SUMS says $want; refusing to install" "retry; if it repeats, report it at $repoUrl/issues (the download was changed or corrupted)"
        }
        Say "verified sha256 $got"

        $target = Join-Path $InstallDir 'tentmaker.exe'
        $staged = Join-Path $InstallDir 'tentmaker.new.exe'
        # The service runs this binary as SYSTEM, and setup refuses one that
        # anyone but SYSTEM and Administrators could change. An owner can
        # always rewrite a DACL, and a handle opened while the user owned a
        # file or folder keeps working after the owner changes. So a system
        # install never lets the user own either one, even for a moment:
        # the folder is created with Administrators as owner and a DACL of
        # its own, and the OWNER RIGHTS entry takes the owner's implicit
        # rights away from every file made in it.
        $lock = $isAdmin -and $isSystem
        if ($lock) {
            if ((Split-Path -Leaf $InstallDir) -ne 'Tentmaker') {
                Fail "a system install goes in its own folder named Tentmaker, not $InstallDir" "use -InstallDir `"$(Join-Path $InstallDir 'Tentmaker')`""
            }
            $admins = New-Object Security.Principal.SecurityIdentifier 'S-1-5-32-544'
            $sec = New-Object Security.AccessControl.DirectorySecurity
            $sec.SetOwner($admins)
            $sec.SetAccessRuleProtection($true, $false)
            $tree = [Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
            foreach ($rule in @(@('S-1-5-18', 'FullControl'), @('S-1-5-32-544', 'FullControl'), @('S-1-5-32-545', 'ReadAndExecute'), @('S-1-3-4', 'ReadAndExecute'))) {
                $sec.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule (
                    (New-Object Security.Principal.SecurityIdentifier $rule[0]), [Security.AccessControl.FileSystemRights]$rule[1], $tree,
                    [Security.AccessControl.PropagationFlags]::None, [Security.AccessControl.AccessControlType]::Allow)))
            }
            $trusted = @('S-1-5-18', 'S-1-5-32-544')
            # Anyone else who could change the folder or anything in it may
            # still hold a handle that a new DACL can't revoke, or may have
            # left a file the service would load (a DLL beside the exe). Such
            # a folder is never repaired in place: it is set aside.
            $write = [Security.AccessControl.FileSystemRights]'WriteData, AppendData, WriteExtendedAttributes, WriteAttributes, DeleteSubdirectoriesAndFiles, Delete, ChangePermissions, TakeOwnership'
            function Get-Untrusted([string]$path) {
                $acl = Get-Acl -LiteralPath $path
                $owner = $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value
                if ($trusted -notcontains $owner) { return "it is owned by $owner" }
                foreach ($r in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
                    if ($r.AccessControlType -eq 'Allow' -and $trusted -notcontains $r.IdentityReference.Value -and ($r.FileSystemRights -band $write)) {
                        return "$($r.IdentityReference.Value) can change it"
                    }
                }
                return $null
            }
            if (Test-Path -LiteralPath $InstallDir) {
                $why = Get-Untrusted $InstallDir
                if (-not $why) {
                    foreach ($child in Get-ChildItem -LiteralPath $InstallDir -Force -Recurse) {
                        $because = Get-Untrusted $child.FullName
                        if ($because) { $why = "in $($child.FullName), $because"; break }
                    }
                }
                if ($why) {
                    $aside = "$InstallDir.untrusted-" + [DateTime]::UtcNow.Ticks
                    try { [IO.Directory]::Move($InstallDir, $aside) } catch {
                        Fail "$InstallDir can't be trusted ($why) and can't be moved aside: $_" "stop the service with 'Stop-Service tentmaker' and close anything using the folder, then delete it and run the installer again"
                    }
                    Say "moved $InstallDir to $aside, because $why"
                }
            }
            if (Test-Path -LiteralPath $InstallDir) {
                Set-Acl -LiteralPath $InstallDir -AclObject $sec
                # The rollback copy is one the service would run. Delete it
                # when anyone else owns it (an elevated `tentmaker update`
                # used to leave it owned by the user).
                $prev = "$target.prev"
                if (Test-Path -LiteralPath $prev) {
                    $prevOwner = (Get-Acl -LiteralPath $prev).GetOwner([Security.Principal.SecurityIdentifier]).Value
                    if ($trusted -notcontains $prevOwner) {
                        Remove-Item -LiteralPath $prev -Force
                        Say "removed $prev, which $prevOwner could have changed"
                    }
                }
            } else {
                $parent = Split-Path -Parent $InstallDir
                if (-not (Test-Path -LiteralPath $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
                if ($PSVersionTable.PSVersion.Major -lt 6) {
                    [void][IO.Directory]::CreateDirectory($InstallDir, $sec)
                } else {
                    [void][IO.FileSystemAclExtensions]::Create((New-Object IO.DirectoryInfo $InstallDir), $sec)
                }
            }
            $now = (Get-Acl -LiteralPath $InstallDir).GetOwner([Security.Principal.SecurityIdentifier]).Value
            if ($trusted -notcontains $now) { Fail "$InstallDir is owned by $now after the install made it" "report it at $repoUrl/issues" }
        } else {
            New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        }

        # Extract only tentmaker.exe from the top level of the archive, into
        # a new file: one left over could carry its own security descriptor.
        if (Test-Path -LiteralPath $staged) { Remove-Item -LiteralPath $staged -Force }
        Add-Type -AssemblyName System.IO.Compression, System.IO.Compression.FileSystem
        $zip = New-Object IO.Compression.ZipArchive((New-Object IO.MemoryStream (, $zipBytes)), [IO.Compression.ZipArchiveMode]::Read)
        try {
            $entry = $zip.Entries | Where-Object { $_.FullName -eq 'tentmaker.exe' } | Select-Object -First 1
            if (-not $entry) { Fail "$asset has no tentmaker.exe" "report it at $repoUrl/issues" }
            [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $staged, $false)
        } finally {
            $zip.Dispose()
        }
        if ($lock) {
            # Until the owner changes, only the inherited OWNER RIGHTS entry
            # keeps the user from rewriting this file's DACL. Check it is
            # there; Windows drops it once Administrators own the file.
            $limits = @((Get-Acl -LiteralPath $staged).GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]) | Where-Object { $_.IdentityReference.Value -eq 'S-1-3-4' })
            if ($limits.Count -eq 0) { Fail "$staged did not inherit the OWNER RIGHTS entry from $InstallDir; refusing to install" "report it at $repoUrl/issues" }
            Icacls $staged '/setowner' '*S-1-5-32-544'
        }
        # PowerShell 5.1 turns native stderr into errors under 'Stop'.
        $ErrorActionPreference = 'Continue'
        $out = & $staged version 2>&1
        $code = $LASTEXITCODE
        $ErrorActionPreference = 'Stop'
        if ($code -ne 0) {
            Fail "the downloaded binary doesn't run here: $out" 'check that antivirus did not block it, or try another -InstallDir'
        }
        $installedVersion = "$out".Trim()

        # A running exe (the service) can be renamed but not replaced.
        $old = "$target.old"
        if (Test-Path -LiteralPath $old) {
            try { Remove-Item -LiteralPath $old -Force } catch { $old = "$target.old-" + [DateTime]::UtcNow.Ticks }
        }
        $moved = $false
        if (Test-Path -LiteralPath $target) {
            [IO.File]::Move($target, $old)
            $moved = $true
        }
        try {
            [IO.File]::Move($staged, $target)
            $staged = $null
        } catch {
            if ($moved) { [IO.File]::Move($old, $target) }
            throw
        }
        Say "installed $installedVersion to $target"

        # PATH: machine-wide for a system install, this user's otherwise.
        $scope = if ($isAdmin) { 'Machine' } else { 'User' }
        $current = [Environment]::GetEnvironmentVariable('Path', $scope)
        $entries = @("$current" -split ';' | Where-Object { $_ } | ForEach-Object { $_.TrimEnd('\') })
        if ($entries -notcontains $InstallDir.TrimEnd('\')) {
            $newPath = if ($current) { $current.TrimEnd(';') + ';' + $InstallDir } else { $InstallDir }
            [Environment]::SetEnvironmentVariable('Path', $newPath, $scope)
            Say "added $InstallDir to the $($scope.ToLowerInvariant()) PATH (new terminals see it)"
        }
        if ((@($env:Path -split ';') | ForEach-Object { $_.TrimEnd('\') }) -notcontains $InstallDir.TrimEnd('\')) {
            $env:Path = $env:Path.TrimEnd(';') + ';' + $InstallDir
        }

        if ($Yes -and -not $NoSetup) {
            if (-not $isAdmin) {
                Fail 'setup needs Administrator' "open PowerShell as Administrator and run: & '$target' setup --yes $($SetupArgs -join ' ')"
            }
            Say "running: tentmaker setup --yes $($SetupArgs -join ' ')"
            $ErrorActionPreference = 'Continue'
            $setupRan = $true
            & $target setup --yes @SetupArgs
            $code = $LASTEXITCODE
            $ErrorActionPreference = 'Stop'
            if ($code -ne 0) {
                Fail "setup failed (exit $code)" "fix the error above, then run: tentmaker setup --yes $($SetupArgs -join ' ')"
            }
            Say 'done'
        } else {
            $hint = if ($isAdmin) { '' } else { ' (in an Administrator PowerShell)' }
            $next = ("tentmaker setup " + ($SetupArgs -join ' ')).TrimEnd()
            Say "next step${hint}: $next"
            Say '  (an agent can run: tentmaker setup --yes --json --memory 4GiB --boost-memory 12GiB --repo owner/name)'
            if ($Json) {
                Write-Output ([ordered]@{ ok = $true; installed = $target; version = $installedVersion; next = ("tentmaker setup --yes " + ($SetupArgs -join ' ')).TrimEnd() } | ConvertTo-Json -Compress)
            }
        }
    } finally {
        if ($staged -and (Test-Path -LiteralPath $staged)) { Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue }
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Install-Tentmaker @args
