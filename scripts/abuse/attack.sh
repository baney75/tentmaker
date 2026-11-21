#!/bin/bash
# Attacks a Linux check job makes on the device that runs it, for the abuse
# test (.github/workflows/abuse.yml, through tentmaker-abuse.yml). Every
# attack prints exactly one line:
#
#   ATTACK <name> BLOCKED [detail]
#   ATTACK <name> SUCCEEDED [detail]
#
# and the harness fails on any SUCCEEDED or any missing line. CONTROL lines
# show that the job still has what it needs (the internet, DNS), so a
# blocked attack means the sandbox stopped it, not that nothing works.
#
#   attack.sh main | sibling | oom | pids
set -u

mode=${1:-main}
say() { printf 'ATTACK %s %s\n' "$1" "$2${3:+ $3}"; }
now() { date +%s; }

# http_ok URL: the decoy (or any server) answered 200 with a body.
http_ok() {
	local code
	code=$(curl -gsS -o /dev/null -w '%{http_code}' --connect-timeout 3 --max-time 6 "$@" 2>/dev/null) || return 1
	[ "$code" = 200 ]
}

reach() { # reach NAME URL...
	local name=$1
	shift
	if http_ok "$@"; then say "$name" SUCCEEDED "$1 answered"; else say "$name" BLOCKED; fi
}

host_ip() { # the source address of this host's default route
	python3 -c 'import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.connect(("192.0.2.1", 53))
print(s.getsockname()[0])' 2>/dev/null
}

job_dir=$(dirname "${HOME:?}")
jobs_dir=$(dirname "$job_dir")
state=/var/lib/tentmaker

case $mode in
sibling)
	# Another job's secret: main tries to read it while this job runs.
	secret="tm-sibling-secret-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
	printf '%s\n' "$secret" >"$HOME/tm-sibling-secret"
	chmod 600 "$HOME/tm-sibling-secret"
	echo "SIBLING start $(now) dir $job_dir"
	sleep 150
	echo "SIBLING end $(now)"
	exit 0
	;;
oom)
	# Allocate and touch memory well past the job's limit (2 GiB in the
	# harness). The sandbox must kill the job; a line here means it didn't.
	python3 -c '
chunks = []
for i in range(128):          # 128 x 64 MiB = 8 GiB
    chunks.append(b"\x01" * (64 << 20))
print("ATTACK memory-limit SUCCEEDED allocated %d MiB" % (len(chunks) * 64), flush=True)
'
	exit 0
	;;
pids)
	# A fork bomb that stops at 6000 tasks. Reaching that many means nothing
	# bounds the job's tasks. It starts threads with small stacks: the pids
	# limit counts threads like processes, and 4096 sleep processes cost
	# about 2 GiB, which hits the memory limit first and proves nothing about
	# the pids limit.
	python3 -c '
import threading
threading.stack_size(64 * 1024)
stop = threading.Event()
ts, err = [], ""
for i in range(6000):
    t = threading.Thread(target=stop.wait, daemon=True)
    try:
        t.start()
    except RuntimeError as e:
        err = str(e)
        break
    ts.append(t)
n = len(ts)
stop.set()
for t in ts:
    t.join()
if n >= 6000:
    print("ATTACK fork-bomb SUCCEEDED started %d tasks" % n, flush=True)
else:
    print("ATTACK fork-bomb BLOCKED after %d tasks: %s" % (n, err), flush=True)
'
	exit 0
	;;
main) ;;
*)
	echo "usage: attack.sh main|sibling|oom|pids" >&2
	exit 2
	;;
esac

echo "tentmaker abuse: job dir $job_dir, uid $(id -u), groups $(id -G)"

# Controls: a sandbox that blocks everything would pass every attack.
if http_ok https://api.github.com/zen; then echo "CONTROL internet OK"; else echo "CONTROL internet FAILED"; fi
if getent hosts github.com >/dev/null; then echo "CONTROL dns OK"; else echo "CONTROL dns FAILED"; fi

# Forge tokens and the control token.
got=""
for f in /etc/tentmaker/secrets/* /etc/tentmaker/secrets/github-token; do
	[ -f "$f" ] && cat -- "$f" >/dev/null 2>&1 && got="$f"
done
if [ -n "$got" ]; then say read-secrets SUCCEEDED "read $got"; else say read-secrets BLOCKED; fi
if cat "$state/control.token" >/dev/null 2>&1; then say read-control-token SUCCEEDED; else say read-control-token BLOCKED; fi

# The control API's POSTs need the token.
pause() { # pause [TOKEN]
	local code
	code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 6 -X POST -H 'Content-Type: application/json' \
		${1:+-H "Authorization: Bearer $1"} -d '{"reason":"abuse test","for_s":600}' http://127.0.0.1:7434/v1/pause 2>/dev/null)
	[ "$code" = 200 ]
}
if pause; then say pause-no-token SUCCEEDED; else say pause-no-token BLOCKED; fi
guessed=""
for t in "$(printf '0%.0s' $(seq 64))" "$(printf 'f%.0s' $(seq 64))" "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"; do
	pause "$t" && guessed=1
done
if [ -n "$guessed" ]; then say pause-guessed-token SUCCEEDED; else say pause-guessed-token BLOCKED; fi

# The network: tailnet, LAN, link-local, the mesh port, loopback services.
reach decoy-tailnet-v4 http://100.64.77.1:8080/
reach decoy-tailnet-v6 'http://[fd7a:115c:a1e0::77]:8080/'
reach decoy-lan-v4 http://10.77.0.1:8080/
reach cloud-metadata -H Metadata:true 'http://169.254.169.254/metadata/instance?api-version=2021-02-01'
ip=$(host_ip)
if [ -n "$ip" ]; then
	reach mesh-port-host-ip "http://$ip:7433/"
else
	say mesh-port-host-ip BLOCKED "no host address found"
fi
reach mesh-port-tailnet http://100.64.77.1:7433/
reach mesh-port-loopback http://127.0.0.1:7433/
reach loopback-other-port http://127.0.0.1:8766/

# Rootless podman: a container's traffic leaves through pasta or
# slirp4netns, which run as this job's user, so the same rules must stop it.
img=docker.io/library/busybox:1.36
if timeout 180 podman pull -q "$img" >/dev/null 2>&1; then
	in_pod() { timeout 30 podman run --rm "$img" wget -q -O /dev/null -T 5 "$1" >/dev/null 2>&1; }
	if in_pod http://example.com/; then echo "CONTROL podman OK"; else echo "CONTROL podman FAILED"; fi
	for t in "podman-decoy-tailnet-v4 http://100.64.77.1:8080/" "podman-decoy-lan-v4 http://10.77.0.1:8080/" \
		"podman-mesh-port-host-ip http://${ip:-192.0.2.1}:7433/" "podman-loopback-other-port http://127.0.0.1:8766/"; do
		set -- $t
		if in_pod "$2"; then say "$1" SUCCEEDED "$2 answered"; else say "$1" BLOCKED; fi
	done
else
	echo "CONTROL podman FAILED (could not pull $img)"
fi

# Privilege.
if sudo -n true >/dev/null 2>&1; then say sudo SUCCEEDED; else say sudo BLOCKED; fi
wrote=""
for h in /home/*; do
	[ -d "$h" ] || continue
	if touch "$h/tm-abuse-$$" 2>/dev/null; then
		wrote="$h"
		rm -f "$h/tm-abuse-$$"
	fi
done
if [ -n "$wrote" ]; then say write-other-home SUCCEEDED "wrote in $wrote"; else say write-other-home BLOCKED; fi
wrote=""
for d in "$state" "$state/checks" "$state/linux" "$jobs_dir" /etc/tentmaker /usr/local/lib/tentmaker; do
	if touch "$d/tm-abuse-$$" 2>/dev/null; then
		wrote="$d"
		rm -f "$d/tm-abuse-$$"
	fi
done
if [ -n "$wrote" ]; then say write-state-dir SUCCEEDED "wrote in $wrote"; else say write-state-dir BLOCKED; fi

# The daemon runs as root.
daemon=$(pgrep -o -u 0 -f 'tentmaker daemon' || true)
if [ -z "$daemon" ]; then
	say signal-daemon BLOCKED "daemon pid not visible"
elif kill -TERM "$daemon" 2>/dev/null; then
	say signal-daemon SUCCEEDED "sent SIGTERM to $daemon"
else
	say signal-daemon BLOCKED
fi

# Leftovers: the harness checks after the job that these are gone.
setsid bash -c 'exec -a tm-abuse-leftover sleep 600' </dev/null >/dev/null 2>&1 &
(exec -a tm-abuse-leftover-bg sleep 600) </dev/null >/dev/null 2>&1 &
disown -a
echo "PLANTED background-process"

# Another running job's files: through the jobs dir, or through /proc.
t0=$(now)
found="" read_from="" seen=""
while [ "$(($(now) - t0))" -lt 100 ] && [ -z "$found" ]; do
	cands=""
	for d in "$jobs_dir"/*; do
		[ -d "$d" ] && [ "$d" != "$job_dir" ] && cands="$cands $d"
	done
	for p in /proc/[0-9]*; do
		c=$(readlink "$p/cwd" 2>/dev/null) || continue
		case $c in "$jobs_dir"/*) ;; *) continue ;; esac
		d=$jobs_dir/$(printf '%s' "${c#"$jobs_dir"/}" | cut -d/ -f1)
		[ "$d" != "$job_dir" ] && cands="$cands $d:$p"
	done
	for c in $cands; do
		d=${c%%:*}
		p=""
		[ "$c" != "$d" ] && p=${c#*:}
		seen=1
		for f in "$d/home/tm-sibling-secret" ${p:+"$p/root$d/home/tm-sibling-secret"}; do
			if v=$(cat -- "$f" 2>/dev/null) && [ "${v#tm-sibling-secret-}" != "$v" ]; then
				found=1 read_from=$f
				break 2
			fi
		done
	done
	[ -z "$found" ] && sleep 2
done
echo "READ-OTHER-JOB window $t0 $(now) sibling-visible ${seen:-no}"
if [ -n "$found" ]; then say read-other-job SUCCEEDED "read $read_from"; else say read-other-job BLOCKED; fi

echo "tentmaker abuse: done"
