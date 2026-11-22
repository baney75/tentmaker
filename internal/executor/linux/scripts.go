package linux

import "strings"

// jobRunnerScript is the ExecStart of a runner job, installed root-owned
// 0755. systemd starts it as tentmaker-runner with the JIT config on stdin.
// It writes the config to a 0600 file in the job's own directory, reads it
// back and deletes it, and hands it to actions/runner through
// ACTIONS_RUNNER_INPUT_JITCONFIG, which the runner reads and then clears.
const jobRunnerScript = `#!/bin/bash
# Tentmaker job runner. Installed by tentmaker setup; root-owned, do not edit.
#   job-runner <jobs dir> <name>     stdin: the job's single-use JIT config
set -euo pipefail
umask 077
jobs=${1:-}
name=${2:-}
[[ $name =~ ^[a-z0-9][a-z0-9-]{2,62}$ ]] || { echo "tentmaker job-runner: bad name" >&2; exit 2; }
dir=$jobs/$name
[[ -d $dir && ! -L $dir && -O $dir ]] || { echo "tentmaker job-runner: no job dir" >&2; exit 2; }
rm -f -- "$dir/jit"
cat > "$dir/jit"
if [[ ! -s $dir/jit ]]; then rm -f -- "$dir/jit"; echo "tentmaker job-runner: empty JIT config" >&2; exit 2; fi
cfg=$(<"$dir/jit")
rm -f -- "$dir/jit"
exec </dev/null
export ACTIONS_RUNNER_INPUT_JITCONFIG=$cfg
unset cfg
cd "$dir/runner"
exec ./run.sh
`

// dockerShim is first on every job's PATH as docker. Podman's Docker API
// takes the PidsLimit 0 the Docker CLI sends on every create as a limit of
// one process, so a job container couldn't fork. Give create and run a real
// limit unless the caller set one; pass everything else through.
const dockerShimTemplate = `#!/bin/bash
# Tentmaker docker shim. Installed by tentmaker setup; root-owned, do not edit.
real=@DOCKER@
args=("$@")
i=0
[[ ${args[0]:-} == container ]] && i=1
case ${args[i]:-} in
  create|run)
    for a in "${args[@]:i+1}"; do
      [[ $a == --pids-limit || $a == --pids-limit=* ]] && exec "$real" "$@"
    done
    exec "$real" "${args[@]:0:i+1}" --pids-limit 4096 "${args[@]:i+1}"
    ;;
esac
exec "$real" "$@"
`

func dockerShim(realDocker string) string {
	return strings.ReplaceAll(dockerShimTemplate, "@DOCKER@", shellQuote(realDocker))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

const sliceUnit = `# Written by tentmaker setup. Every CI job, and the CI user's own manager with
# its rootless containers, runs here; the daemon sets MemoryMax and CPUQuota at
# runtime to the pool budget.
[Unit]
Description=Tentmaker CI jobs
Before=slices.target

[Slice]
MemoryAccounting=yes
CPUAccounting=yes
TasksAccounting=yes
`

// userManagerDropIn puts the CI user's manager (podman.service and every
// rootless container) in the pool's slice, and hides tailscaled's socket
// from it as the job units do. It starts after tailscaled so the socket
// directory exists when its mount namespace is built.
var userManagerDropIn = `# Written by tentmaker setup. The CI user's manager (podman.service and every
# rootless container) counts against the Tentmaker pool limit and can't reach
# tailscaled's local API.
[Unit]
After=tailscaled.service

[Service]
Slice=tentmaker.slice
` + inaccessible(TailscalePaths)

func inaccessible(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("InaccessiblePaths=-" + p + "\n")
	}
	return b.String()
}

const rsharedUnit = `# Written by tentmaker setup. WSL mounts / private; rootless podman needs it shared.
[Unit]
Description=Tentmaker: make / a shared mount for rootless podman (WSL)
DefaultDependencies=no
After=local-fs.target
Before=user@@UID@.service

[Service]
Type=oneshot
ExecStart=/usr/bin/mount --make-rshared /
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`
