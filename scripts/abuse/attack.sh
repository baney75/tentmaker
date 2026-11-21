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
#   attack.sh main | sibling | oom | pids | lan | closed
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

udp_reach() { # udp_reach NAME ADDR PORT: a UDP decoy (or DNS server) answered
	if python3 "$here/udpask.py" "$2" "$3" >/dev/null 2>&1; then say "$1" SUCCEEDED "udp $2:$3 answered"; else say "$1" BLOCKED; fi
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

here=$(cd "$(dirname "$0")" && pwd)
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
lan)
	# Run with [guards] allow_lan = true: the physical LAN is open, a
	# Tailscale subnet route and the tailnet are not.
	if http_ok http://10.78.0.1:8080/; then echo "CONTROL lan-v4 OK"; else echo "CONTROL lan-v4 FAILED"; fi
	if http_ok 'http://[2001:db8:78::1]:8080/'; then echo "CONTROL lan-v6 OK"; else echo "CONTROL lan-v6 FAILED"; fi
	if http_ok https://api.github.com/zen; then echo "CONTROL internet OK"; else echo "CONTROL internet FAILED"; fi
	ip=$(host_ip)
	reach lan-subnet-route-v4 http://10.99.0.1:8080/
	reach lan-decoy-tailnet-v4 http://100.64.77.1:8080/
	reach lan-mesh-port-lan http://10.78.0.1:7433/
	reach lan-mesh-port-host-ip "http://${ip:-192.0.2.1}:7433/"
	reach lan-host-private-v4 http://10.77.0.1:8080/
	reach lan-host-own-v4 http://192.0.2.77:8767/
	reach lan-host-own-v6 'http://[2001:db8:77::1]:8767/'
	reach lan-host-ip-other-port "http://${ip:-192.0.2.1}:8767/"
	reach lan-host-lan-v4 http://10.78.0.2:8767/
	reach lan-host-global-v6 'http://[2001:db8:78::2]:8767/'
	exit 0
	;;
closed)
	# Queued while the harness has removed the network rules: the device
	# must refuse to start it, so any line here means it ran.
	echo "ATTACK fail-closed SUCCEEDED the job ran without the network rules"
	reach closed-decoy-tailnet-v4 http://100.64.77.1:8080/
	exit 0
	;;
main) ;;
*)
	echo "usage: attack.sh main|sibling|oom|pids|lan|closed" >&2
	exit 2
	;;
esac

echo "tentmaker abuse: job dir $job_dir, uid $(id -u), groups $(id -G)"

# Controls: a sandbox that blocks everything would pass every attack.
if http_ok https://api.github.com/zen; then echo "CONTROL internet OK"; else echo "CONTROL internet FAILED"; fi
if getent hosts github.com >/dev/null; then echo "CONTROL dns OK"; else echo "CONTROL dns FAILED"; fi
# The job's own loopback works: a test that starts a server on 127.0.0.1
# or ::1 (httptest, a database) reaches it, as on GitHub's runners. The
# host's loopback stays closed (loopback-other-port and the rest below).
own_lo() { # own_lo NAME BIND URLHOST PORT
	python3 -m http.server "$4" --bind "$2" >/dev/null 2>&1 &
	local pid=$! ok=FAILED
	for _ in $(seq 20); do
		if http_ok "http://$3:$4/"; then ok=OK; break; fi
		sleep 0.25
	done
	kill "$pid" 2>/dev/null
	echo "CONTROL $1 $ok"
}
if [ "$(uname -s)" = Linux ]; then
	own_lo own-loopback-v4 127.0.0.1 127.0.0.1 18771
	own_lo own-loopback-v6 ::1 '[::1]' 18772
fi

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
# The relay port answers the job (a 401 for no token), so the pause attacks
# are refused by the API, not cut off by the network.
relay_code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 6 -X POST http://127.0.0.1:7434/v1/pause 2>/dev/null)
case "$relay_code" in "" | 000) echo "CONTROL relay FAILED" ;; *) echo "CONTROL relay OK ($relay_code)" ;; esac
if pause; then say pause-no-token SUCCEEDED; else say pause-no-token BLOCKED; fi
guessed=""
for t in "$(printf '0%.0s' $(seq 64))" "$(printf 'f%.0s' $(seq 64))" "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"; do
	pause "$t" && guessed=1
done
if [ -n "$guessed" ]; then say pause-guessed-token SUCCEEDED; else say pause-guessed-token BLOCKED; fi

# The network: tailnet, MagicDNS, LAN, multicast, link-local, this host's
# own addresses, the mesh port on every address, loopback services, and
# tailscaled's local API.
reach decoy-tailnet-v4 http://100.64.77.1:8080/
reach decoy-tailnet-v6 'http://[fd7a:115c:a1e0::77]:8080/'
reach magicdns-http http://100.100.100.100/
udp_reach magicdns-dns 100.100.100.100 53
reach decoy-lan-v4 http://10.77.0.1:8080/
udp_reach lan-multicast 239.77.77.77 7777
reach cloud-metadata -H Metadata:true 'http://169.254.169.254/metadata/instance?api-version=2021-02-01'
# This host's own addresses, on a port only the host serves: a
# non-private IPv4 and a global IPv6 (stand-ins for a public address).
reach host-own-v4 http://192.0.2.77:8767/
reach host-own-v6 'http://[2001:db8:77::1]:8767/'
ip=$(host_ip)
if [ -n "$ip" ]; then reach host-ip-other-port "http://$ip:8767/"; else say host-ip-other-port BLOCKED "no host address found"; fi
if [ -n "$ip" ]; then
	reach mesh-port-host-ip "http://$ip:7433/"
else
	say mesh-port-host-ip BLOCKED "no host address found"
fi
reach mesh-port-lan http://10.77.0.1:7433/
reach mesh-port-tailnet http://100.64.77.1:7433/
reach mesh-port-tailnet-v6 'http://[fd7a:115c:a1e0::77]:7433/'
reach mesh-port-host-v6 'http://[2001:db8:77::1]:7433/'
reach mesh-port-loopback http://127.0.0.1:7433/
reach mesh-port-loopback-v6 'http://[::1]:7433/'
reach loopback-other-port http://127.0.0.1:8766/
reach loopback-v6-other-port 'http://[::1]:8766/'
# The LAN by its global IPv6 addresses: an on-link prefix setup saw, and
# one the LAN gained after setup.
reach lan-global-v6 'http://[2001:db8:78::1]:8080/'
reach lan-global-v6-new 'http://[2001:db8:79::1]:8080/'
# This host's own addresses on the LAN interface.
reach host-lan-v4 http://10.78.0.2:8767/
reach host-lan-global-v6 'http://[2001:db8:78::2]:8767/'
# A device behind a Tailscale subnet route, out through tailscale0.
reach subnet-route-v4 http://10.99.0.1:8080/
# Any local user may read tailscaled's status (every device on the tailnet,
# its addresses and logins) through its socket; a job must not.
ts_api() { # ts_api PATH: tailscaled's local API answered
	http_ok -H 'Sec-Tailscale: localapi' --unix-socket "$1" "http://local-tailscaled.sock/localapi/v0/$2"
}
got=""
for sock in /run/tailscale/tailscaled.sock /var/run/tailscale/tailscaled.sock; do
	for q in status whois?addr=100.100.100.100:53; do
		ts_api "$sock" "$q" && got="$got $sock:$q"
	done
	# whois answers 4xx while logged out; any HTTP answer means the API is open.
	code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 6 -H 'Sec-Tailscale: localapi' --unix-socket "$sock" \
		'http://local-tailscaled.sock/localapi/v0/whois?addr=100.100.100.100:53' 2>/dev/null) || code=000
	[ "$code" != 000 ] && [ -z "$got" ] && got=" $sock:whois answered $code"
done
if [ -n "$got" ]; then say tailscaled-socket SUCCEEDED "read$got"; else say tailscaled-socket BLOCKED; fi

# Rootless podman: a container's traffic leaves through pasta or
# slirp4netns, which run as this job's user, so the same rules must stop it.
# A container on the host network opens its own sockets, as a subordinate
# uid when it runs as a non-root user; the podman service (DOCKER_HOST)
# runs outside the job's unit and can mount any path this user can read.
img=docker.io/library/busybox:1.36
curl_img=docker.io/curlimages/curl:8.11.1
if timeout 180 podman pull -q "$img" >/dev/null 2>&1; then
	in_pod() { timeout 30 podman run --rm "$img" wget -q -O /dev/null -T 5 "$1" >/dev/null 2>&1; }
	if in_pod http://example.com/; then echo "CONTROL podman OK"; else echo "CONTROL podman FAILED"; fi
	for t in "podman-decoy-tailnet-v4 http://100.64.77.1:8080/" "podman-decoy-lan-v4 http://10.77.0.1:8080/" \
		"podman-mesh-port-host-ip http://${ip:-192.0.2.1}:7433/" "podman-loopback-other-port http://127.0.0.1:8766/"; do
		set -- $t
		if in_pod "$2"; then say "$1" SUCCEEDED "$2 answered"; else say "$1" BLOCKED; fi
	done
	hostnet() { # hostnet USER URL: a host-network container through the job's podman and the service
		local how
		for how in podman "podman --remote --url ${DOCKER_HOST:-unix:///nonexistent}"; do
			if timeout 30 $how run --rm --network host --user "$1" "$img" wget -q -O /dev/null -T 5 "$2" >/dev/null 2>&1; then
				echo "$how as $1"
				return 0
			fi
		done
		return 1
	}
	for u in 0 1000:1000; do
		name=podman-hostnet-root
		[ "$u" = 0 ] || name=podman-hostnet-subuid
		got=""
		for url in http://100.64.77.1:8080/ http://10.77.0.1:8080/ http://127.0.0.1:8766/ "http://${ip:-192.0.2.1}:7433/"; do
			if how=$(hostnet "$u" "$url"); then got="$url answered ($how)"; break; fi
		done
		if [ -n "$got" ]; then say "$name" SUCCEEDED "$got"; else say "$name" BLOCKED; fi
	done
	# The curl image: through the job's own podman, and through the service.
	ways=""
	timeout 180 podman pull -q "$curl_img" >/dev/null 2>&1 && ways="podman"
	if pull_err=$(timeout 180 podman --remote --url "${DOCKER_HOST:-unix:///nonexistent}" pull -q "$curl_img" 2>&1 >/dev/null); then
		ways="$ways|podman --remote --url ${DOCKER_HOST:-unix:///nonexistent}"
		echo "CONTROL podman-remote OK"
	else
		echo "CONTROL podman-remote FAILED (could not pull $curl_img through the podman service at ${DOCKER_HOST:-no DOCKER_HOST}: $(printf '%s' "$pull_err" | tail -n 3 | tr '\n' ' '))"
		ls -la "${XDG_RUNTIME_DIR:-/nonexistent}" "${XDG_RUNTIME_DIR:-/nonexistent}/podman" 2>&1 | sed 's/^/PODMAN-REMOTE /'
	fi
	ways=${ways#|}
	if [ -n "$ways" ]; then
		got=""
		IFS='|'
		for how in $ways; do
			unset IFS
			if timeout 30 $how run --rm -v /run/tailscale:/ts "$curl_img" -fsS --max-time 5 -H 'Sec-Tailscale: localapi' --unix-socket /ts/tailscaled.sock \
				http://local-tailscaled.sock/localapi/v0/status >/dev/null 2>&1; then
				got="$how"
				break
			fi
		done
		unset IFS
		if [ -n "$got" ]; then say podman-tailscaled-socket SUCCEEDED "mounted /run/tailscale ($got)"; else say podman-tailscaled-socket BLOCKED; fi
		# The curl image runs as its own user (uid 100): a subordinate uid on the host.
		got=""
		IFS='|'
		for how in $ways; do
			unset IFS
			for url in http://100.64.77.1:8080/ http://10.77.0.1:8080/ http://127.0.0.1:8766/ "http://${ip:-192.0.2.1}:7433/"; do
				if timeout 30 $how run --rm --network host "$curl_img" -fsS -o /dev/null --max-time 5 "$url" >/dev/null 2>&1; then
					got="$url answered ($how)"
					break 2
				fi
			done
		done
		unset IFS
		if [ -n "$got" ]; then say podman-hostnet-image-user SUCCEEDED "$got"; else say podman-hostnet-image-user BLOCKED; fi
	else
		echo "CONTROL podman-curl FAILED (could not pull $curl_img)"
	fi
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
