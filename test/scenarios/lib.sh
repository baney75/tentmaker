# shellcheck shell=bash
# Shared harness for the real-host scenarios in .github/workflows/scenarios.yml.
#
# A scenario script sources this file, calls scn_begin NAME, and then drives a
# real install on a throwaway GitHub-hosted runner (Linux, or Windows under Git
# Bash; Windows-only steps go through lib.ps1). It records numbers with
# measure, records failed assertions with check or soft_fail (the scenario goes
# on measuring), and on exit writes metrics/<name>-<os>.json:
#   {scenario, os, pass, measurements, failures}
#
# Environment (set by the workflow): REPO, GH_TOKEN (the workflow's
# GITHUB_TOKEN; read only from the environment, never put in argv), RUN_ID,
# RUN_ATTEMPT, RUNNER_TEMP, RUNNER_OS. Optional: SCN_PORT (fake release server).
#
# Nothing waits forever: every call that can hang has a timeout, every wait
# loop has a deadline, and the workflow runs each script under `timeout`.
# Its TERM is recorded as a failure, so a hang ends with a message and the
# metrics file.
#
# Commits under test are throwaway: the harness writes the scenario's
# workflow to .tm-scn/wf.yml in a new commit on top of the checked-out commit
# and pushes it to tm-scn/<run_id>.<attempt>/<scenario>-<os>/<n>. Pushes made
# with GITHUB_TOKEN never trigger workflows. The branches are deleted when the
# scenario ends (and again by the workflow's cleanup job).

set -euo pipefail

SCN_LIB=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCN_ROOT=$(cd "$SCN_LIB/../.." && pwd)
: "${REPO:?REPO is required}" "${GH_TOKEN:?GH_TOKEN is required}" "${RUNNER_TEMP:?RUNNER_TEMP is required}"
RUN_ID=${RUN_ID:-local}
RUN_ATTEMPT=${RUN_ATTEMPT:-1}
PORT=${SCN_PORT:-8765}

case ${RUNNER_OS:-} in
	Linux) OS=linux ;;
	Windows) OS=windows ;;
	*) echo "lib.sh: unsupported RUNNER_OS '${RUNNER_OS:-}'" >&2; exit 2 ;;
esac

if [ "$OS" = windows ]; then
	W=$(cygpath -u "$RUNNER_TEMP")/scn
	PYTHON=python
	STATE=/c/ProgramData/Tentmaker/state
	RUNS_ON=windows-latest
	STEP_SHELL=pwsh
	export PATH="/c/Program Files/Tentmaker:$PATH"
else
	W=$RUNNER_TEMP/scn
	PYTHON=python3
	STATE=/var/lib/tentmaker
	RUNS_ON=ubuntu-latest
	STEP_SHELL=bash
fi
mkdir -p "$W"
METRICS=$SCN_ROOT/metrics
WF_PATH=.tm-scn/wf.yml
SCN=""
SCN_BG=() # background helpers scn_finish stops

# ---------------------------------------------------------------- logging, metrics

log() { printf '%s [%s] %s\n' "$(date -u +%H:%M:%S)" "${SCN:-scn}" "$*" >&2; }
now_ms() { date +%s%3N; }
now_s() { date +%s; }
# secs_since MS: seconds (one decimal) since a now_ms reading.
secs_since() { awk -v a="$1" -v b="$(now_ms)" 'BEGIN { printf "%.1f", (b - a) / 1000 }'; }
# iso_epoch ISO8601: seconds since the epoch.
iso_epoch() { date -u -d "$1" +%s; }

# jq without the carriage returns jq.exe adds on Windows.
jqr() { command jq "$@" | tr -d '\r'; }

scn_begin() {
	SCN=$1
	BR_PREFIX="tm-scn/$RUN_ID.$RUN_ATTEMPT/$SCN-$OS"
	MFILE=$W/measure.json
	FAILS=$W/failures.txt
	echo '{}' >"$MFILE"
	: >"$FAILS"
	: >"$W/branches"
	SCN_T0=$(now_ms)
	mkdir -p "$METRICS"
	trap scn_finish EXIT
	trap 'soft_fail "the scenario ran past its time limit (killed by timeout)"; exit 124' TERM
	log "scenario $SCN on $OS ($(uname -sr)), $(nproc 2>/dev/null || echo '?') CPUs"
	if [ "$OS" = linux ]; then
		df -h / /var/lib 2>/dev/null | sed 's/^/  /' >&2 || true
		free -g | sed 's/^/  /' >&2 || true
	fi
}

# measure KEY NUMBER, measure_s KEY STRING, measure_j KEY JSON
measure() { _put "$1" "$2"; }
measure_s() { _put "$1" "$(jqr -n --arg s "$2" '$s')"; }
measure_j() { _put "$1" "$2"; }
_put() {
	jqr --arg k "$1" --argjson v "$2" '.[$k] = $v' "$MFILE" >"$MFILE.t" && mv "$MFILE.t" "$MFILE"
	log "measure $1 = $2"
}

soft_fail() {
	echo "::error title=$SCN ($OS)::$*"
	printf '%s\n' "$*" >>"$FAILS"
}
die() {
	soft_fail "$*"
	exit 1
}
# check MESSAGE COMMAND...: records a failed assertion and goes on.
check() {
	local msg=$1
	shift
	if "$@"; then
		log "ok: $msg"
	else
		soft_fail "$msg"
	fi
}

scn_finish() {
	local rc=$?
	set +e
	trap - EXIT TERM
	local p
	for p in "${SCN_BG[@]}"; do kill "$p" 2>/dev/null; done
	local pass=false
	if [ "$rc" = 0 ] && [ ! -s "$FAILS" ]; then pass=true; fi
	measure total_s "$(secs_since "$SCN_T0")"
	jqr -n --arg s "$SCN" --arg os "$OS" --argjson pass "$pass" --slurpfile m "$MFILE" --rawfile f "$FAILS" \
		'{scenario: $s, os: $os, pass: $pass, measurements: $m[0], failures: ($f | split("\n") | map(select(length > 0)))}' \
		>"$METRICS/$SCN-$OS.json"
	cat "$METRICS/$SCN-$OS.json" >&2
	if [ "$pass" != true ]; then
		diag
	fi
	unblock_api_quiet
	unfill_disk >/dev/null 2>&1 || true
	delete_branches
	[ "$pass" = true ] || exit 1
	exit 0
}

# ---------------------------------------------------------------- tentmaker

# tm ARGS: tentmaker as root (Linux) or Administrator (the Windows runner).
tm() {
	if [ "$OS" = linux ]; then timeout 300 sudo tentmaker "$@"; else timeout 300 tentmaker "$@"; fi
}
# tm_env NAME=VALUE ARGS: tm with one more environment variable (a test hook).
tm_env() {
	local kv=$1
	shift
	if [ "$OS" = linux ]; then timeout 300 sudo env "$kv" tentmaker "$@"; else timeout 300 env "$kv" tentmaker "$@"; fi
}
tm_status() { timeout 30 tentmaker status --json | tr -d '\r'; }
# rcat FILE: read a root-only file.
rcat() {
	if [ "$OS" = linux ]; then sudo cat "$@"; else cat "$@"; fi
}
tm_logs() {
	local pre=()
	[ "$OS" = linux ] && pre=(sudo)
	timeout 60 "${pre[@]}" tentmaker logs -n "${1:-3000}" --json 2>/dev/null | jqr -r '.lines[]' 2>/dev/null || true
}

# tm_build VERSION OUTDIR [TAGS]: this host's release archive and SHA256SUMS,
# laid out like scripts/build.sh's (one archive, so it builds in seconds).
tm_build() {
	local version=$1 out=$2 tags=${3:-} goos goarch name stage bin
	goos=$(go env GOOS | tr -d '\r')
	goarch=$(go env GOARCH | tr -d '\r')
	name=tentmaker_${goos}_${goarch}
	stage=$W/stage-$version
	bin=tentmaker
	[ "$OS" = windows ] && bin=tentmaker.exe
	rm -rf "$stage"
	mkdir -p "$stage" "$out"
	(cd "$SCN_ROOT" && CGO_ENABLED=0 timeout 600 go build -trimpath ${tags:+-tags "$tags"} \
		-ldflags "-s -w -X github.com/baney75/tentmaker/internal/cli.Version=$version" -o "$stage/$bin" ./cmd/tentmaker)
	cp "$SCN_ROOT/LICENSE" "$SCN_ROOT/README.md" "$stage/"
	if [ "$OS" = windows ]; then
		rm -f "$out/$name.zip"
		pwsh -NoProfile -NonInteractive -Command "Compress-Archive -Path '$(cygpath -w "$stage")\\*' -DestinationPath '$(cygpath -w "$out")\\$name.zip'"
	else
		tar -C "$stage" --owner=0 --group=0 --numeric-owner -czf "$out/$name.tar.gz" "$bin" LICENSE README.md
	fi
	(cd "$out" && sha256sum tentmaker_* | sed 's/ \*/  /' >SHA256SUMS)
	log "built $version${tags:+ (tags $tags)} into $out"
}

# serve_release: start the fake release server (installers, release API).
# Builds go in $SRV/rel/<version>/; $SRV/current names the newest.
serve_release() {
	SRV=$W/srv
	mkdir -p "$SRV/rel"
	cp "$SCN_ROOT/scripts/install.sh" "$SCN_ROOT/scripts/install.ps1" "$SRV/"
	local root=$SRV
	[ "$OS" = windows ] && root=$(cygpath -w "$SRV")
	"$PYTHON" "$SCN_LIB/fake_release.py" "$root" "$PORT" >"$W/release-server.log" 2>&1 </dev/null &
	SCN_BG+=($!)
	for _ in $(seq 1 50); do
		if curl -fsS --max-time 2 -o /dev/null "http://127.0.0.1:$PORT/healthz"; then
			log "release server on 127.0.0.1:$PORT"
			return 0
		fi
		sleep 0.2
	done
	cat "$W/release-server.log" >&2
	die "the release server did not start"
}

# publish VERSION [TAGS]: build VERSION into the server and make it current.
publish() {
	tm_build "$1" "$SRV/rel/$1" "${2:-}"
	printf '%s\n' "$1" >"$SRV/current"
}

# tm_install: the real installer against the fake release.
tm_install() {
	if [ "$OS" = linux ]; then
		curl -fsSL --max-time 60 "http://127.0.0.1:$PORT/install.sh" |
			timeout 300 sudo env TENTMAKER_DOWNLOAD_BASE="http://127.0.0.1:$PORT" sh
		# The runner image leaves homes world-readable; setup refuses that
		# and prints this fix.
		for h in /home/*; do sudo chmod o-rwx "$h"; done
	else
		ps1 Install-Scn "http://127.0.0.1:$PORT"
	fi
	command -v tentmaker >/dev/null || die "tentmaker is not on PATH after the install"
}

# tm_setup ARGS: setup --yes --json --no-mesh --no-gh ARGS, which must succeed.
tm_setup() {
	local rc=0
	tm setup --yes --json --no-mesh --no-gh "$@" >"$W/setup.json" || rc=$?
	tr -d '\r' <"$W/setup.json" >&2
	[ "$rc" = 0 ] || die "setup exited $rc"
	jqr -e '.ok == true and .service == "running"' "$W/setup.json" >/dev/null || die "setup is not ok or the service is not running"
}

tm_connect() {
	printf '%s' "$GH_TOKEN" | tm connect github --token-stdin --json >"$W/connect.json" ||
		{ cat "$W/connect.json" >&2; die "connect failed"; }
	jqr -e '.ok == true' "$W/connect.json" >/dev/null || die "connect is not ok"
}

# tm_repo_add BRANCH...: checks mode for $REPO, the scenario workflow, the branches.
tm_repo_add() {
	local args=() b
	for b in "$@"; do args+=(--branch "$b"); done
	tm repo add "$REPO" --mode checks --allow-public --workflow "$WF_PATH" "${args[@]}" --json >"$W/repo.json" ||
		{ cat "$W/repo.json" >&2; die "repo add failed"; }
	jqr -e '.ok == true' "$W/repo.json" >/dev/null || die "repo add is not ok"
}

# wait_service_up SECONDS: the control API answers.
wait_service_up() {
	local deadline=$(($(now_s) + $1))
	while [ "$(now_s)" -lt "$deadline" ]; do
		if tm_status 2>/dev/null | jqr -e '.service == "running"' >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	return 1
}

# daemon_pid: the service's main process.
daemon_pid() {
	if [ "$OS" = linux ]; then
		systemctl show -p MainPID --value tentmaker
	else
		ps1 Get-DaemonPid
	fi
}

# daemon_restarts: how many times the service manager restarted the daemon.
daemon_restarts() {
	if [ "$OS" = linux ]; then systemctl show -p NRestarts --value tentmaker; else echo "?"; fi
}

# set_daemon_env NAME=VALUE...: the service's environment (test hooks), then restart.
set_daemon_env() {
	if [ "$OS" = linux ]; then
		sudo mkdir -p /etc/systemd/system/tentmaker.service.d
		{
			echo "[Service]"
			local kv
			for kv in "$@"; do echo "Environment=$kv"; done
		} | sudo tee /etc/systemd/system/tentmaker.service.d/50-scenario.conf >/dev/null
		timeout 60 sudo systemctl daemon-reload
		timeout 120 sudo systemctl restart tentmaker
	else
		ps1 Set-DaemonEnv "$*"
	fi
	wait_service_up 60 || die "the service did not come back after setting its environment"
}

winpath() { if [ "$OS" = windows ]; then cygpath -w "$1"; else printf '%s' "$1"; fi; }

# ps1 FUNCTION ARGS: a lib.ps1 function in PowerShell 7 (Windows only).
ps1() {
	local f=$1
	shift
	local qargs="" a
	for a in "$@"; do qargs="$qargs '${a//\'/\'\'}'"; done
	timeout 300 pwsh -NoProfile -NonInteractive -Command ". '$(cygpath -w "$SCN_LIB/lib.ps1")'; $f$qargs" | tr -d '\r'
}

# ---------------------------------------------------------------- git

git_init() {
	cd "$SCN_ROOT"
	git config user.name "tentmaker-scenarios"
	git config user.email "tentmaker-scenarios@users.noreply.github.com"
	# shellcheck disable=SC2034 # the scenarios build on BASE
	BASE=$(git rev-parse HEAD)
}

# mk_commit PARENT WORKFLOW_TEMPLATE MARK: a commit on PARENT whose
# .tm-scn/wf.yml is the template (a name in workflows/, or a path) with
# @RUNS_ON@ and @SHELL@ filled in.
# Prints the sha. The working tree is untouched.
mk_commit() {
	local parent=$1 tpl=$2 mark=$3 idx blob mblob tree
	idx=$W/index.$RANDOM
	rm -f "$idx"
	GIT_INDEX_FILE=$idx git read-tree "$parent"
	case $tpl in */*) ;; *) tpl=$SCN_LIB/workflows/$tpl ;; esac
	blob=$(sed -e "s/@RUNS_ON@/$RUNS_ON/g" -e "s/@SHELL@/$STEP_SHELL/g" "$tpl" | git hash-object -w --stdin)
	mblob=$(printf '%s %s %s\n' "$SCN" "$OS" "$mark" | git hash-object -w --stdin)
	GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$blob,$WF_PATH"
	GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$mblob,.tm-scn/marker"
	tree=$(GIT_INDEX_FILE=$idx git write-tree)
	rm -f "$idx"
	git commit-tree "$tree" -p "$parent" -m "tentmaker scenario $SCN ($OS): $mark"
}

# git_push REFSPEC...: push with the workflow token from the environment.
git_push() {
	local r
	for r in "$@"; do
		case $r in *:refs/heads/*) printf '%s\n' "${r#*:refs/heads/}" >>"$W/branches" ;; esac
	done
	timeout 120 git -c credential.helper= \
		-c 'credential.helper=!f() { test "$1" = get || exit 0; echo username=x-access-token; echo "password=$GH_TOKEN"; }; f' \
		push --force --quiet origin "$@"
}

branch() { printf '%s/%s' "$BR_PREFIX" "$1"; }

delete_branches() {
	[ -s "$W/branches" ] || return 0
	local refs=() b
	while read -r b; do refs+=(":refs/heads/$b"); done < <(sort -u "$W/branches")
	log "deleting ${#refs[@]} throwaway branches"
	timeout 120 git -c credential.helper= \
		-c 'credential.helper=!f() { test "$1" = get || exit 0; echo username=x-access-token; echo "password=$GH_TOKEN"; }; f' \
		push --quiet origin "${refs[@]}" || log "branch cleanup failed; the workflow's cleanup job retries"
	: >"$W/branches"
}

# ---------------------------------------------------------------- commit statuses

# statuses SHA: the commit's statuses (newest first) as JSON. Conditional
# requests: an unchanged answer is a 304, which costs no rate limit.
statuses() {
	local sha=$1 f code hdr=()
	f=$W/etag/$sha
	mkdir -p "$W/etag"
	if [ -s "$f.etag" ]; then hdr=(-H "If-None-Match: $(cat "$f.etag")"); fi
	code=$(printf 'header = "Authorization: Bearer %s"\n' "$GH_TOKEN" |
		curl -sS --max-time 20 -K - "${hdr[@]}" -H "Accept: application/vnd.github+json" \
			-D "$f.h" -o "$f.new" -w '%{http_code}' \
			"https://api.github.com/repos/$REPO/commits/$sha/statuses?per_page=100") || code=000
	case $code in
		200)
			# A commit's statuses only grow, but a lagging GitHub replica can
			# answer with an older, shorter list: keep the newer one we have.
			if [ -s "$f.json" ] && [ "$(command jq length <"$f.new" 2>/dev/null || echo 0)" -lt "$(command jq length <"$f.json" 2>/dev/null || echo 0)" ]; then
				rm -f "$f.new"
				tr -d '\r' <"$f.json"
				return 0
			fi
			mv "$f.new" "$f.json"
			{ grep -i '^etag:' "$f.h" || true; } | sed 's/^[^:]*: *//' | tr -d '\r' >"$f.etag"
			;;
		304) ;;
		*)
			log "statuses $sha: HTTP $code"
			return 1
			;;
	esac
	[ -s "$f.json" ] || echo '[]' >"$f.json"
	tr -d '\r' <"$f.json"
}

# state_of SHA CONTEXT: "<state> <epoch> <description>" of the newest status, or "none".
state_of() {
	local js
	js=$(statuses "$1") || { echo "api-error"; return 0; }
	printf '%s' "$js" | jqr -r --arg c "$2" \
		'[.[] | select(.context == $c)][0] | if . == null then "none" else "\(.state) \(.created_at | fromdateiso8601) \(.description)" end'
}

# count_states SHA CONTEXT STATE: how many STATE statuses CONTEXT has (each
# grant posts one pending; a duplicate run posts two).
count_states() {
	statuses "$1" | jqr --arg c "$2" --arg s "$3" '[.[] | select(.context == $c and .state == $s)] | length'
}

# first_epoch SHA CONTEXT STATE: when CONTEXT first got STATE, or empty.
first_epoch() {
	statuses "$1" | jqr -r --arg c "$2" --arg s "$3" '[.[] | select(.context == $c and .state == $s)] | last | if . == null then "" else (.created_at | fromdateiso8601) end'
}

# wait_state SHA CONTEXT STATES SECONDS [INTERVAL]: polls until the newest
# status of CONTEXT is one of STATES (a regex like "success|failure"); prints
# the "<state> <epoch> <description>" line. Fails on timeout.
wait_state() {
	local sha=$1 ctx=$2 want=$3 secs=$4 every=${5:-5} st deadline last=""
	deadline=$(($(now_s) + secs))
	while :; do
		st=$(state_of "$sha" "$ctx")
		if [ "$st" != "$last" ]; then
			log "${sha:0:12} $ctx: $st"
			last=$st
		fi
		if printf '%s' "$st" | grep -Eq "^($want) "; then
			printf '%s\n' "$st"
			return 0
		fi
		if [ "$(now_s)" -ge "$deadline" ]; then
			log "${sha:0:12} $ctx: no ($want) within ${secs}s (last: $st)"
			return 1
		fi
		sleep "$every"
	done
}

# kept_log SHA JOB: the check log the broker kept.
kept_log_path() { printf '%s/checks/%s-%s-%s.log' "$STATE" "${REPO//\//_}" "${1:0:12}" "$(printf '%s' "$2" | sed 's/[^A-Za-z0-9._-]\{1,\}/_/g')"; }
kept_log() { rcat "$(kept_log_path "$1" "$2")" 2>/dev/null; }

# ---------------------------------------------------------------- API outage

# block_api: api.github.com unreachable for the daemon (new and open
# connections). Linux: /etc/hosts plus iptables resets on every address it
# had. Windows: lib.ps1 (hosts file, firewall rule, open connections reset).
block_api() {
	if [ "$OS" = linux ]; then
		local ips ip pid keep
		ips=$(getent ahostsv4 api.github.com | awk '{print $1}' | sort -u)
		pid=$(daemon_pid)
		ips="$ips $(sudo ss -tnpH state established "( dport = :443 )" 2>/dev/null | grep "pid=$pid," | awk '{print $4}' | sed 's/:443$//' | sort -u)"
		sudo cp /etc/hosts "$W/hosts.bak"
		echo "127.0.0.1 api.github.com" | sudo tee -a /etc/hosts >/dev/null
		: >"$W/blocked-ips"
		# git push goes to github.com, which must stay reachable.
		keep=" $(getent ahostsv4 github.com | awk '{print $1}' | sort -u | tr '\n' ' ') "
		for ip in $(printf '%s\n' $ips | sort -u); do
			case $keep in *" $ip "*) continue ;; esac
			sudo iptables -I OUTPUT -d "$ip" -p tcp -j REJECT --reject-with tcp-reset
			echo "$ip" >>"$W/blocked-ips"
		done
		log "blocked api.github.com: $(tr '\n' ' ' <"$W/blocked-ips")"
	else
		ps1 Block-Api
	fi
	touch "$W/api-blocked"
}

unblock_api() {
	[ -e "$W/api-blocked" ] || return 0
	if [ "$OS" = linux ]; then
		local ip
		sudo cp "$W/hosts.bak" /etc/hosts
		while read -r ip; do sudo iptables -D OUTPUT -d "$ip" -p tcp -j REJECT --reject-with tcp-reset || true; done <"$W/blocked-ips"
	else
		ps1 Unblock-Api
	fi
	rm -f "$W/api-blocked"
	log "unblocked api.github.com"
}
unblock_api_quiet() { unblock_api >/dev/null 2>&1 || true; }

# ---------------------------------------------------------------- stats, diagnostics

# pct P VALUES...: nearest-rank percentile.
pct() {
	local p=$1
	shift
	printf '%s\n' "$@" | sort -n | awk -v p="$p" '{ v[NR] = $1 } END { if (NR == 0) { print "null"; exit } r = int((p / 100) * NR + 0.999999); if (r < 1) r = 1; if (r > NR) r = NR; print v[r] }'
}

diag() {
	echo "::group::diagnostics ($SCN, $OS)"
	echo "== status"
	tm_status || true
	echo "== daemon log"
	tm_logs 400 || true
	if [ "$OS" = linux ]; then
		echo "== units"
		systemctl list-units --all --no-pager 'tentmaker*' || true
		echo "== runner processes"
		ps -u tentmaker-runner -o pid,etime,args 2>/dev/null || true
	fi
	echo "== kept check logs"
	if [ "$OS" = linux ]; then
		sudo sh -c "ls -la $STATE/checks 2>/dev/null; tail -n 60 $STATE/checks/*.log 2>/dev/null" || true
	else
		ls -la "$STATE/checks" 2>/dev/null || true
		tail -n 60 "$STATE"/checks/*.log 2>/dev/null || true
	fi
	echo "== release server"
	tail -n 30 "$W/release-server.log" 2>/dev/null || true
	echo "::endgroup::"
}

# ---------------------------------------------------------------- the usual start

# scn_up SETUP_ARGS...: the release server with this build, the installer,
# setup with SETUP_ARGS, connect. Records install_s, setup_s and the budget.
scn_up() {
	local t
	serve_release
	publish "${SCN_VERSION:-0.0.1-scn.1}"
	t=$(now_ms)
	tm_install
	measure install_s "$(secs_since "$t")"
	t=$(now_ms)
	tm_setup "$@"
	measure setup_s "$(secs_since "$t")"
	# The daemon checks for updates on its own; against the real releases it
	# would replace the build under test with the latest published one.
	set_daemon_env TENTMAKER_UPDATE_API="http://127.0.0.1:$PORT"
	tm_connect
	measure_s version "$(timeout 30 tentmaker version --json | jqr -r .version)"
	measure free_disk_gib "$(free_gib)"
	measure_j budget "$(tm_status | jqr -c '{profile: .decision.profile, slots: .decision.slots, limit: .decision.limit, admit: .decision.admit, why: .why}')"
}

# final_ok STATE_LINE: the line from wait_state is a success.
is_success() { case $1 in success\ *) return 0 ;; *) return 1 ;; esac; }
# st_epoch / st_desc: fields of a wait_state line.
st_epoch() { printf '%s' "$1" | cut -d' ' -f2; }
st_desc() { printf '%s' "$1" | cut -d' ' -f3-; }

# rate_left: the token's remaining core API calls (this call costs none).
rate_left() {
	printf 'header = "Authorization: Bearer %s"\n' "$GH_TOKEN" |
		curl -sS --max-time 20 -K - -H "Accept: application/vnd.github+json" https://api.github.com/rate_limit |
		jqr '.resources.core.remaining // null' 2>/dev/null || echo null
}

# free_gib: free disk (GiB, rounded down) where the state lives.
free_gib() {
	if [ "$OS" = linux ]; then
		df -B1 --output=avail /var/lib | tail -1 | awk '{ printf "%d", $1 / 1073741824 }'
	else
		ps1 Free-GiB
	fi
}

# fill_disk LEAVE_GIB: leave about LEAVE_GIB free where the state lives.
fill_disk() {
	if [ "$OS" = linux ]; then
		local free
		free=$(df -B1 --output=avail /var/lib | tail -1 | tr -d ' ')
		sudo fallocate -l $((free - $1 * 1073741824)) /var/lib/tm-scn-fill
	else
		ps1 Fill-Disk "$1" >/dev/null
	fi
	touch "$W/disk-filled"
}
unfill_disk() {
	[ -e "$W/disk-filled" ] || return 0
	if [ "$OS" = linux ]; then sudo rm -f /var/lib/tm-scn-fill; else ps1 Unfill-Disk >/dev/null; fi
	rm -f "$W/disk-filled"
}

# wait_new_daemon OLD_PID SECONDS: the service manager started a new daemon
# and its control API answers.
wait_new_daemon() {
	local old=$1 deadline=$(($(now_s) + $2)) p
	while [ "$(now_s)" -lt "$deadline" ]; do
		p=$(daemon_pid 2>/dev/null || echo 0)
		if [ -n "$p" ] && [ "$p" != 0 ] && [ "$p" != "$old" ] && tm_status 2>/dev/null | jqr -e '.service == "running"' >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	return 1
}
