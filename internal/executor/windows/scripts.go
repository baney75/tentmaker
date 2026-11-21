package windows

// jobRunnerPS1 starts actions/runner for one job. The JIT config arrives on
// stdin, goes to a file the job's ACL limits to the runner user, SYSTEM and
// Administrators, and is read back and deleted before the runner starts.
// The runner takes it from ACTIONS_RUNNER_INPUT_JITCONFIG, which it clears.
const jobRunnerPS1 = `param([Parameter(Mandatory = $true)][string]$Runner)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3
if (-not (Test-Path -LiteralPath (Join-Path $Runner 'run.cmd') -PathType Leaf)) {
    throw "no runner in $Runner"
}
$jit = Join-Path $Runner '.jit'
$utf8 = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText($jit, [Console]::In.ReadToEnd().Trim(), $utf8)
$cfg = [System.IO.File]::ReadAllText($jit, $utf8)
Remove-Item -LiteralPath $jit -Force
if ($cfg.Length -eq 0) { throw 'no JIT config on stdin' }
$env:ACTIONS_RUNNER_INPUT_JITCONFIG = $cfg
Remove-Variable cfg
Set-Location -LiteralPath $Runner
& (Join-Path $Runner 'run.cmd')
exit $LASTEXITCODE
`
