#!/usr/bin/env bash
# Helpers and scenario steps for .github/workflows/team.yml.
#
#   bash .github/team/team.sh COMMAND [ARGS...]
#
# Jobs coordinate through commit statuses on the workflow's head sha, with
# contexts tm-team/<run id>.<attempt>.<signal>. Reads use ETags, so polling
# a status that did not change costs no API rate limit. The GitHub token is
# read from the masked GH_TOKEN env and reaches curl on stdin, never argv.
#
# Env: REPO, TEAM_SHA, RUN_KEY, POOL, HOST_PREFIX, BR_PREFIX, GH_TOKEN.
set -euo pipefail

: "${REPO:?}" "${TEAM_SHA:?}" "${RUN_KEY:?}" "${POOL:?}" "${HOST_PREFIX:?}" "${BR_PREFIX:?}"

TEAM_TMP=${RUNNER_TEMP:-/tmp}/team
mkdir -p "$TEAM_TMP"
METRICS=${GITHUB_WORKSPACE:-$PWD}/metrics
mkdir -p "$METRICS"
CHECKS=/var/lib/tentmaker/checks
BRANCHES="s1 s2 s3 s4 s5 s6 w1 t1 t2 t3 t4 t5 t6 r1 r2"
TEMPLATE=.github/workflows/tentmaker-team.yml
TAG=tag:tentmaker-ci

ACL_HINT='Add to the tailnet policy file (either form) so the CI nodes reach each other on the mesh port:
  "grants": [{"src": ["tag:tentmaker-ci"], "dst": ["tag:tentmaker-ci"], "ip": ["tcp:7433"]}]
or
  "acls": [{"action": "accept", "src": ["tag:tentmaker-ci"], "dst": ["tag:tentmaker-ci:7433"]}]'
SSH_HINT='To let devices add reach the CI nodes, add to the tailnet policy file:
  "grants": [{"src": ["tag:tentmaker-ci"], "dst": ["tag:tentmaker-ci"], "ip": ["tcp:22"]}],
  "ssh": [{"action": "accept", "src": ["tag:tentmaker-ci"], "dst": ["tag:tentmaker-ci"], "users": ["root"]}]'

case "$(uname -s)" in
MINGW* | MSYS* | CYGWIN*) WINDOWS=1 ;;
*) WINDOWS=0 ;;
esac
if [ "$WINDOWS" = 1 ]; then
	export PATH="$PATH:/c/Program Files/Tailscale:/c/Program Files/Tentmaker"
fi

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

# SCENARIO names the scenario a failure belongs to, so die records it.
SCENARIO=""
die() {
	local msg="$*"
	printf '::error::%s\n' "${msg//$'\n'/%0A}"
	if [ -n "$SCENARIO" ] && [ ! -s "$METRICS/team-$SCENARIO.json" ]; then
		jq -n --arg s "$SCENARIO" --arg e "$msg" '{scenario: $s, result: "fail", error: $e}' >"$METRICS/team-$SCENARIO.json"
	fi
	exit 1
}

host() { printf '%s-%s' "$HOST_PREFIX" "$1"; }
br() { printf '%s-%s' "$BR_PREFIX" "$1"; }
now() { date -u +%s; }
epoch() { date -u -d "$1" +%s; }
jqr() { jq "$@" | tr -d '\r'; }

# tm runs the tentmaker CLI (as root on Linux) with a timeout.
tm() {
	if [ "$WINDOWS" = 1 ]; then
		timeout 180 tentmaker "$@"
	else
		sudo timeout 180 tentmaker "$@"
	fi
}

ts() {
	if [ "$WINDOWS" = 1 ]; then
		timeout 60 tailscale "$@"
	else
		sudo timeout 60 tailscale "$@"
	fi
}

# ---------------------------------------------------------------- GitHub API

# api_get PATH OUT: GET with an ETag cache. A 304 costs no rate limit.
api_get() {
	local path=$1 out=$2 key etag code
	local -a cond=()
	key=$(printf '%s' "$path" | tr -c 'A-Za-z0-9' '_')
	etag=$(cat "$TEAM_TMP/$key.etag" 2>/dev/null || true)
	if [ -n "$etag" ] && [ -s "$TEAM_TMP/$key.body" ]; then
		cond=(-H "If-None-Match: $etag")
	fi
	code=$(printf 'header = "Authorization: Bearer %s"\n' "$GH_TOKEN" |
		curl -sS --max-time 20 -K - -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
			"${cond[@]}" -D "$TEAM_TMP/$key.hdr" -o "$TEAM_TMP/$key.new" -w '%{http_code}' \
			"https://api.github.com/$path") || code=000
	case $code in
	200)
		mv "$TEAM_TMP/$key.new" "$TEAM_TMP/$key.body"
		tr -d '\r' <"$TEAM_TMP/$key.hdr" | awk 'tolower($1) == "etag:" { $1 = ""; sub(/^ /, ""); print }' >"$TEAM_TMP/$key.etag"
		;;
	304) ;;
	*)
		log "GET $path: HTTP $code"
		return 1
		;;
	esac
	cp "$TEAM_TMP/$key.body" "$out"
}

# combined SHA OUT: every status on SHA (latest per context), up to 300.
combined() {
	local sha=$1 out=$2 p
	: >"$out.all"
	for p in 1 2 3; do
		api_get "repos/$REPO/commits/$sha/status?per_page=100&page=$p" "$out.p$p" || return 1
		jq -c '.statuses[]' "$out.p$p" >>"$out.all"
		[ "$(jq '.statuses | length' "$out.p$p" | tr -d '\r')" -ge 100 ] || break
	done
	jq -s '.' "$out.all" >"$out"
}

# history SHA OUT: every status ever posted on SHA, newest first.
history() {
	timeout 30 gh api --paginate "repos/$REPO/commits/$1/statuses?per_page=100" | jq -s 'add // []' >"$2"
}

# ---------------------------------------------------------------- signals

sig_ctx() { printf 'tm-team/%s.%s' "$RUN_KEY" "$1"; }

# sig_post NAME STATE DESC
sig_post() {
	local i desc=${3:0:139}
	for i in 1 2 3 4 5; do
		if timeout 30 gh api -X POST "repos/$REPO/statuses/$TEAM_SHA" -f state="$2" -f context="$(sig_ctx "$1")" \
			-f description="$desc" >/dev/null; then
			log "posted $(sig_ctx "$1") = $2 ($desc)"
			return 0
		fi
		sleep $((i * 3))
	done
	log "could not post $(sig_ctx "$1")"
	return 1
}

# sig_get NAME: "state<TAB>description<TAB>created_at", or nothing.
sig_get() {
	combined "$TEAM_SHA" "$TEAM_TMP/signals.json" || return 1
	jqr -r --arg c "$(sig_ctx "$1")" '.[] | select(.context == $c) | [.state, .description, .created_at] | @tsv' "$TEAM_TMP/signals.json"
}

# check_abort fails when another job posted abort-<role>.
check_abort() {
	local a
	[ -s "$TEAM_TMP/signals.json" ] || return 0
	a=$(jqr -r --arg p "$(sig_ctx abort-)" '[.[] | select(.context | startswith($p)) | "\(.context): \(.description)"] | join("; ")' "$TEAM_TMP/signals.json")
	if [ -n "$a" ]; then
		die "another job failed: $a"
	fi
}

# sig_wait NAME TIMEOUT_S: waits for success and prints the signal line.
sig_wait() {
	local name=$1 deadline=$((SECONDS + $2)) got
	log "waiting for $(sig_ctx "$name") (up to $2 s)"
	while :; do
		got=$(sig_get "$name" || true)
		check_abort
		case $got in
		success*)
			printf '%s\n' "$got"
			return 0
			;;
		failure* | error*) die "$(sig_ctx "$name") is $got" ;;
		esac
		[ "$SECONDS" -lt "$deadline" ] || die "timed out after $2 s waiting for $(sig_ctx "$name")"
		sleep 5
	done
}

# abort ROLE WHY: tell the other jobs to stop waiting.
abort() { sig_post "abort-$1" failure "${2:-job failed}" || true; }

# ---------------------------------------------------------------- tailnet

# login_hint: why `tailscale up` failed, with the owner's fix when it is the key.
login_hint() {
	local state=""
	if command -v tailscale >/dev/null 2>&1; then
		state=$(ts status --json 2>/dev/null | jqr -r '.BackendState // ""' || true)
	fi
	log "tailscale backend state: ${state:-unknown}"
	if [ "$state" = NeedsLogin ] || [ "$state" = NeedsMachineAuth ] || [ "$(date -u +%Y%m%d)" -gt 20261201 ]; then
		die "TS_AUTHKEY expired or invalid: ask the owner for a new reusable, ephemeral key tagged tag:tentmaker-ci"
	fi
	die "tailscale up failed (backend state ${state:-unknown}); see the Tailscale step's log"
}

peer_ip() {
	ts status --json | jqr -r --arg h "$1" \
		'[(.Peer // {})[] | select((.HostName | ascii_downcase) == $h or (.DNSName | ascii_downcase | startswith($h + "."))) | .TailscaleIPs[]? | select(test("^[0-9.]+$"))] | first // ""'
}

# probe ROLE...: the hello endpoint of each role's device answers over the
# tailnet. When it can't be reached while tailscale ping works, the tailnet
# ACL (or that device's firewall) drops tcp/7433.
probe() {
	local r h ip code deadline
	for r in "$@"; do
		h=$(host "$r")
		deadline=$((SECONDS + 90))
		ip=""
		while [ -z "$ip" ]; do
			ip=$(peer_ip "$h")
			[ -n "$ip" ] || [ "$SECONDS" -lt "$deadline" ] || die "$h is not on the tailnet (tailscale status lists no such peer)"
			[ -n "$ip" ] || sleep 3
		done
		while :; do
			code=$(curl -s -o "$TEAM_TMP/hello-$r.json" -w '%{http_code}' --noproxy '*' --max-time 6 "http://$ip:7433/v1/hello") || true
			[ "$code" = 200 ] && break
			[ "$SECONDS" -lt "$deadline" ] || break
			sleep 3
		done
		case $code in
		200)
			log "hello $h ($ip): $(tr -d '\r\n' <"$TEAM_TMP/hello-$r.json")"
			jqr -e --arg p "$POOL" '.pool == $p' "$TEAM_TMP/hello-$r.json" >/dev/null || die "$h is in another pool: $(cat "$TEAM_TMP/hello-$r.json")"
			;;
		403) die "$h ($ip) refused this device's hello (403): mesh.members does not admit tagged devices there" ;;
		*)
			if ts ping -c 3 --timeout 5s "$ip" >"$TEAM_TMP/ping-$r.txt" 2>&1; then
				cat "$TEAM_TMP/ping-$r.txt" >&2
				die "hello to $h ($ip:7433) failed (curl HTTP '$code') while tailscale ping works: the tailnet policy (or $h's firewall) blocks tcp/7433 between tag:tentmaker-ci nodes. $ACL_HINT"
			fi
			cat "$TEAM_TMP/ping-$r.txt" >&2
			die "hello to $h ($ip:7433) failed (curl HTTP '$code') and tailscale ping fails too: no tailnet path to $h"
			;;
		esac
	done
}

# wait_mesh: the daemon is serving and its mesh is up.
wait_mesh() {
	local deadline=$((SECONDS + 120)) st
	while :; do
		st=$(tm status --json 2>/dev/null || true)
		if printf '%s' "$st" | jqr -e '.service == "running" and .mesh.up == true' >/dev/null 2>&1; then
			log "service running, mesh up as $(printf '%s' "$st" | jqr -r '.mesh.self // "?"')"
			return 0
		fi
		[ "$SECONDS" -lt "$deadline" ] || die "the daemon's mesh is not up after 120 s: $(printf '%s' "$st" | jqr -c '{service, mesh, problems}' 2>/dev/null || printf '%s' "$st")"
		sleep 3
	done
}

# ---------------------------------------------------------------- scenario 1: pool

# pool_check ROLE: this device's `tentmaker pool --json` lists a, b and c,
# each answering with platforms, within 2 minutes of the last setup.
pool_check() {
	local me=$1 since t0 lines ok r d plats
	SCENARIO=pool
	since=0
	for r in a b c; do
		lines=$(sig_wait "ready-$r" 1800)
		t0=$(epoch "$(printf '%s' "$lines" | cut -f3)")
		[ "$t0" -gt "$since" ] && since=$t0
	done
	log "last setup finished at $(date -u -d "@$since" +%T)"
	probe $(for r in a b c; do [ "$r" = "$me" ] || printf '%s ' "$r"; done)
	local deadline=$((since + 120)) why=""
	while :; do
		tm pool --json >"$TEAM_TMP/pool.json" 2>"$TEAM_TMP/pool.err" || true
		ok=1
		why=""
		for r in a b c; do
			d=$(host "$r")
			plats=$(jqr -r --arg d "$d" '[.devices[]? | select(.device == $d and ((.error // "") == "")) | .status.worker.executors[]?.platforms[]?] | unique | join(",")' "$TEAM_TMP/pool.json" 2>/dev/null || true)
			if [ -z "$plats" ]; then
				ok=0
				why="$why $d"
			fi
		done
		if [ "$ok" = 1 ]; then
			break
		fi
		if [ "$(now)" -gt "$deadline" ]; then
			cat "$TEAM_TMP/pool.json" "$TEAM_TMP/pool.err" >&2 || true
			sig_post "pool-$me" failure "missing or no platforms:$why after $(($(now) - since)) s" || true
			die "pool on $(host "$me"): missing or without platforms:$why, $(($(now) - since)) s after the last setup"
		fi
		sleep 5
	done
	local took=$(($(now) - since))
	jqr '.' "$TEAM_TMP/pool.json" >&2
	local summary
	summary=$(jqr -r '[.devices[] | "\(.device | split("-") | last)=\([.status.worker.executors[]?.platforms[]?] | unique | join("+"))\(if .broker then "(broker)" else "" end)"] | join(" ")' "$TEAM_TMP/pool.json")
	# Linux on a and b, Windows on c; only a is a broker.
	jqr -e --arg a "$(host a)" --arg b "$(host b)" --arg c "$(host c)" '
		def plats($d): [.devices[] | select(.device == $d) | .status.worker.executors[]?.platforms[]?];
		def broker($d): [.devices[] | select(.device == $d) | .broker] | first;
		(plats($a) | any(startswith("linux/"))) and (plats($b) | any(startswith("linux/"))) and (plats($c) | any(startswith("windows/")))
		and broker($a) == true and broker($b) == false and broker($c) == false' "$TEAM_TMP/pool.json" >/dev/null ||
		{ sig_post "pool-$me" failure "wrong platforms or broker flags: $summary" || true; die "pool on $(host "$me"): wrong platforms or broker flags: $summary"; }
	sig_post "pool-$me" success "t=${took}s $summary"
	cp "$TEAM_TMP/pool.json" "$METRICS/pool-$me.raw.json"
	log "pool on $(host "$me") is complete after ${took} s: $summary"
}

# ---------------------------------------------------------------- broker A: branches and checks

gpush() {
	local d=$1
	shift
	# The helper reads the token from the masked env; argv carries only its name.
	timeout 120 git -C "$d" -c credential.helper= \
		-c 'credential.helper=!f() { test "$1" = get || exit 0; echo username=x-access-token; echo "password=$GH_TOKEN"; }; f' \
		push -q -f "https://github.com/$REPO.git" "$@"
}

gcommit() {
	git -C "$1" add -A
	git -C "$1" -c user.name=tentmaker-team -c user.email=tentmaker-team@users.noreply.github.com commit -q -m "$2"
}

# placeholders: every throwaway branch exists from the start at a commit with
# no workflow, so the brokers' branch polls are answered with a cacheable
# 200 (a missing branch is an uncached 404 on every poll).
placeholders() {
	local d r refs=()
	d=$(mktemp -d)
	git -C "$d" init -q
	printf 'Throwaway branch for team.yml run %s. Deleted at the end of the run.\n' "$RUN_KEY" >"$d/README.md"
	gcommit "$d" "team $RUN_KEY placeholder"
	for r in $BRANCHES; do refs+=("HEAD:refs/heads/$(br "$r")"); done
	gpush "$d" "${refs[@]}"
	log "placeholder $(git -C "$d" rev-parse HEAD) pushed to ${#refs[@]} branches"
}

# push_branch NAME LINUX_SLEEP WINDOWS_SLEEP: a commit with only the team
# workflow on the throwaway branch NAME; prints its sha.
push_branch() {
	local d
	d=$(mktemp -d)
	git -C "$d" init -q
	mkdir -p "$d/.github/workflows"
	sed -e "s/^  TM_MARK: .*/  TM_MARK: \"$RUN_KEY-$1-$(date +%s%N)\"/" \
		-e "s/^  TM_LINUX_SLEEP: .*/  TM_LINUX_SLEEP: \"$2\"/" \
		-e "s/^  TM_WINDOWS_SLEEP: .*/  TM_WINDOWS_SLEEP: \"$3\"/" \
		"$TEMPLATE" >"$d/$TEMPLATE"
	grep -q "TM_LINUX_SLEEP: \"$2\"" "$d/$TEMPLATE" && grep -q "TM_WINDOWS_SLEEP: \"$3\"" "$d/$TEMPLATE" || die "could not set the sleeps in $TEMPLATE"
	printf 'Throwaway branch for team.yml run %s. Deleted at the end of the run.\n' "$RUN_KEY" >"$d/README.md"
	gcommit "$d" "team $RUN_KEY $1 linux=${2}s windows=${3}s"
	gpush "$d" "HEAD:refs/heads/$(br "$1")"
	git -C "$d" rev-parse HEAD
	log "pushed $(br "$1") linux=${2}s windows=${3}s"
}

# ctx_line SHA CONTEXT: "state<TAB>description<TAB>created_at" or nothing.
ctx_line() {
	combined "$1" "$TEAM_TMP/st-$1.json" || return 1
	jqr -r --arg c "$2" '.[] | select(.context == $c) | [.state, .description, .created_at] | @tsv' "$TEAM_TMP/st-$1.json"
}

# wait_ctx SHA CONTEXT STATE_REGEX TIMEOUT: prints the matching line.
wait_ctx() {
	local deadline=$((SECONDS + $4)) got
	while :; do
		got=$(ctx_line "$1" "$2" || true)
		if [ -n "$got" ] && printf '%s' "$got" | cut -f1 | grep -Eq "^($3)$"; then
			printf '%s\n' "$got"
			return 0
		fi
		check_abort
		[ "$SECONDS" -lt "$deadline" ] || die "$2 on ${1:0:12} is '${got:-none}' after $4 s; wanted $3"
		sleep 5
	done
}

# wait_done TIMEOUT SHA...: every listed sha has a final tentmaker/linux and
# tentmaker/windows status.
wait_done() {
	local t=$1 sha c
	shift
	local deadline=$((SECONDS + t))
	for sha in "$@"; do
		for c in tentmaker/linux tentmaker/windows; do
			wait_ctx "$sha" "$c" 'success|failure|error' $((deadline - SECONDS > 0 ? deadline - SECONDS : 1)) >/dev/null
		done
	done
}

check_device() { # SHA JOB: the device the broker's kept check log names
	sudo sed -n 's/^device: //p' "$CHECKS/${REPO//\//_}-${1:0:12}-$2.log" 2>/dev/null | head -1
}

# audit SCENARIO BRANCH SHA: one JSON line per context on SHA with how many
# pending and final statuses it got, the final one, and the device.
audit() {
	local sc=$1 b=$2 sha=$3 c job dev
	history "$sha" "$TEAM_TMP/hist-$sha.json"
	for c in tentmaker/linux tentmaker/windows; do
		job=${c#tentmaker/}
		dev=$(check_device "$sha" "$job")
		jq -c --arg b "$b" --arg sha "$sha" --arg c "$c" --arg dev "$dev" '
			[.[] | select(.context == $c)] as $all
			| ($all | map(select(.state == "pending"))) as $p
			| ($all | map(select(.state != "pending"))) as $f
			| {branch: $b, sha: $sha, context: $c, device_from_log: $dev,
			   pending: ($p | length), final: ($f | length),
			   pending_desc: ($p | map(.description)), final_states: ($f | map(.state)),
			   final_desc: ($f | map(.description) | first),
			   pending_at: ($p | map(.created_at) | last), final_at: ($f | map(.created_at) | first),
			   seconds: (if ($p | length) > 0 and ($f | length) > 0 then (($f | map(.created_at) | first | fromdate) - ($p | map(.created_at) | last | fromdate)) else null end)}' \
			"$TEAM_TMP/hist-$sha.json"
	done
}

# assert_once FILE: every audited context got exactly one pending and one
# final status, and the final one is success.
assert_once() {
	local bad
	bad=$(jq -s -c '[.[] | select(.pending != 1 or .final != 1 or .final_states != ["success"])]' "$1")
	[ "$bad" = "[]" ] || die "checks that did not run exactly once with one success: $bad"
}

write_metrics() { # SCENARIO JSON
	printf '%s\n' "$2" | jq '.' >"$METRICS/team-$1.json"
	log "metrics/team-$1.json: $(jq -c '.' "$METRICS/team-$1.json" | head -c 600)"
}

# ---------------------------------------------------------------- broker A: scenarios

a_pool() {
	SCENARIO=pool
	pool_check a
	local b c
	b=$(sig_wait pool-b 300 | cut -f2)
	c=$(sig_wait pool-c 300 | cut -f2)
	write_metrics pool "$(jq -n --arg a "$(sig_get pool-a | cut -f2)" --arg b "$b" --arg c "$c" \
		--slurpfile raw "$METRICS/pool-a.raw.json" '{scenario: "pool", result: "pass", limit_s: 120,
		views: {a: $a, b: $b, c: $c}, devices_seen_by_a: [$raw[0].devices[] | {device, broker, version, error,
		platforms: ([.status.worker.executors[]?.platforms[]?] | unique)}]}')"
}

a_spread() {
	SCENARIO=spread
	tm status --json | jqr -e '.paused == true' >/dev/null || die "A's worker should be paused for the spread"
	local r sha shas=() t0
	t0=$(now)
	for r in s1 s2 s3 s4 s5 s6; do
		sha=$(push_branch "$r" 5 5)
		shas+=("$sha")
		printf '%s %s\n' "$r" "$sha" >>"$TEAM_TMP/spread.shas"
	done
	wait_done 900 "${shas[@]}"
	local took=$(($(now) - t0))
	: >"$TEAM_TMP/spread.audit"
	local i=0
	for r in s1 s2 s3 s4 s5 s6; do
		audit spread "$(br "$r")" "${shas[$i]}" >>"$TEAM_TMP/spread.audit"
		i=$((i + 1))
	done
	cat "$TEAM_TMP/spread.audit" >&2
	assert_once "$TEAM_TMP/spread.audit"
	local wrong
	wrong=$(jq -s -c --arg b "$(host b)" --arg c "$(host c)" '[.[] | select((.context == "tentmaker/linux" and .device_from_log != $b) or (.context == "tentmaker/windows" and .device_from_log != $c))]' "$TEAM_TMP/spread.audit")
	[ "$wrong" = "[]" ] || die "checks on the wrong device (Linux must run on B, Windows on C): $wrong"
	write_metrics spread "$(jq -s --argjson took "$took" '{scenario: "spread", result: "pass", checks: length, wall_s: $took,
		linux_on: ([.[] | select(.context == "tentmaker/linux") | .device_from_log] | unique),
		windows_on: ([.[] | select(.context == "tentmaker/windows") | .device_from_log] | unique),
		max_check_s: ([.[].seconds] | max), checks_detail: .}' "$TEAM_TMP/spread.audit")"
}

a_worker_loss() {
	SCENARIO=worker-loss
	local sha1 sha2 line down_at err_at took
	sha1=$(push_branch w1 120 1)
	line=$(wait_ctx "$sha1" tentmaker/linux pending 300)
	printf '%s' "$line" | cut -f2 | grep -qx "running on $(host b)" || die "w1 linux should run on B: $line"
	sleep 15
	sig_post kill-b success "stop the daemon and tailscale down"
	line=$(sig_wait b-down 180)
	down_at=$(epoch "$(printf '%s' "$line" | cut -f3)")
	line=$(wait_ctx "$sha1" tentmaker/linux 'error|failure|success' 300)
	err_at=$(epoch "$(printf '%s' "$line" | cut -f3)")
	took=$((err_at - down_at))
	log "w1 linux after B went down: $line ($took s)"
	[ "$(printf '%s' "$line" | cut -f1)" = error ] || die "w1 linux should end in error when B goes offline: $line"
	printf '%s' "$line" | cut -f2 | grep -q 'device went offline' || die "w1 linux error should say device went offline: $line"
	[ "$took" -le 180 ] || die "the offline error took $took s after B went down; the limit is 180 s"

	tm resume --json >/dev/null || die "resume A's worker failed"
	sha2=$(push_branch w1 5 1)
	wait_done 600 "$sha2"
	audit worker-loss "$(br w1)" "$sha2" >"$TEAM_TMP/loss.audit"
	cat "$TEAM_TMP/loss.audit" >&2
	assert_once "$TEAM_TMP/loss.audit"
	local rerun_dev
	rerun_dev=$(jq -r 'select(.context == "tentmaker/linux") | .device_from_log' "$TEAM_TMP/loss.audit")
	[ -n "$rerun_dev" ] && [ "$rerun_dev" != "$(host b)" ] || die "the re-push should run on another worker, ran on '$rerun_dev'"

	sig_post b-return success "B may come back"
	sig_wait ready-b2 600 >/dev/null
	# B's old job ends after it comes back and reports a result for a check
	# the broker already failed; the broker must ignore it.
	sleep 45
	audit worker-loss "$(br w1)" "$sha1" >"$TEAM_TMP/loss-old.audit"
	cat "$TEAM_TMP/loss-old.audit" >&2
	local old
	old=$(jq -c 'select(.context == "tentmaker/linux") | {pending, final, final_states}' "$TEAM_TMP/loss-old.audit")
	[ "$old" = '{"pending":1,"final":1,"final_states":["error"]}' ] || die "the lost check on the first w1 commit should have one pending and one error, got $old"
	write_metrics worker-loss "$(jq -n --argjson took "$took" --arg dev "$rerun_dev" --slurpfile old "$TEAM_TMP/loss-old.audit" \
		--slurpfile re "$TEAM_TMP/loss.audit" '{scenario: "worker-loss", result: "pass", limit_s: 180,
		offline_error_after_s: $took, rerun_device: $dev, lost: $old, rerun: $re}')"
}

a_two_brokers() {
	SCENARIO=two-brokers
	sig_post start-d success "join as a second broker"
	sig_wait ready-d 900 >/dev/null
	local deadline=$((SECONDS + 150))
	until tm pool --json 2>/dev/null | jqr -e --arg d "$(host d)" '[.devices[] | select(.device == $d and .broker and ((.error // "") == ""))] | length == 1' >/dev/null; do
		[ "$SECONDS" -lt "$deadline" ] || die "A's pool never showed $(host d) as a broker"
		sleep 5
	done
	# Every worker rediscovers its brokers within 30 s.
	sleep 40
	local r sha shas=() t0
	t0=$(now)
	for r in t1 t2 t3 t4 t5 t6; do
		sha=$(push_branch "$r" 10 10)
		shas+=("$sha")
	done
	wait_done 900 "${shas[@]}"
	local took=$(($(now) - t0))
	: >"$TEAM_TMP/two.audit"
	local i=0
	for r in t1 t2 t3 t4 t5 t6; do
		audit two-brokers "$(br "$r")" "${shas[$i]}" >>"$TEAM_TMP/two.audit"
		i=$((i + 1))
	done
	cat "$TEAM_TMP/two.audit" >&2
	assert_once "$TEAM_TMP/two.audit"
	local on_a
	on_a=$(jq -s '[.[] | select(.device_from_log != "")] | length' "$TEAM_TMP/two.audit")
	sig_post t-done success "two-broker checks are done"
	local dline on_d
	dline=$(sig_wait d-stopped 300 | cut -f2)
	on_d=$(printf '%s' "$dline" | sed -n 's/.*granted=\([0-9]*\).*/\1/p')
	[ -n "$on_d" ] || die "D did not report how many checks it granted: $dline"
	[ $((on_a + on_d)) -eq 12 ] || die "A granted $on_a and D granted $on_d checks; together they must be exactly 12"
	write_metrics two-brokers "$(jq -s --argjson took "$took" --argjson a "$on_a" --argjson d "$on_d" '{scenario: "two-brokers", result: "pass",
		checks: length, wall_s: $took, granted_by_a: $a, granted_by_d: $d,
		ran_on: ([.[].pending_desc[0]] | group_by(.) | map({(.[0]): length}) | add), checks_detail: .}' "$TEAM_TMP/two.audit")"
}

a_broker_restart() {
	SCENARIO=broker-restart
	tm pause --json team broker-restart >/dev/null || die "pause A's worker failed"
	local s1 s2 l1 l2 restart_at took
	s1=$(push_branch r1 60 1)
	s2=$(push_branch r2 60 1)
	l1=$(wait_ctx "$s1" tentmaker/linux pending 300)
	l2=$(wait_ctx "$s2" tentmaker/linux pending 300)
	for l in "$l1" "$l2"; do
		printf '%s' "$l" | cut -f2 | grep -qx "running on $(host b)" || die "r1 and r2 linux should run on B: $l"
	done
	# Let both jobs fetch their source and get going first.
	sleep 15
	restart_at=$(now)
	sudo timeout 60 systemctl restart tentmaker || die "restart A's service failed"
	wait_mesh
	took=$(($(now) - restart_at))
	log "A restarted in $took s"
	tm status --json | jqr -e '.paused == true' >/dev/null || die "A's pause should survive the restart"
	wait_done 600 "$s1" "$s2"
	audit broker-restart "$(br r1)" "$s1" >"$TEAM_TMP/restart.audit"
	audit broker-restart "$(br r2)" "$s2" >>"$TEAM_TMP/restart.audit"
	cat "$TEAM_TMP/restart.audit" >&2
	assert_once "$TEAM_TMP/restart.audit"
	local early
	early=$(jq -s -c --argjson t "$restart_at" --arg b "$(host b)" '[.[] | select(.context == "tentmaker/linux") | select((.final_at | fromdate) <= $t or (.pending_at | fromdate) > $t or .device_from_log != $b)]' "$TEAM_TMP/restart.audit")
	[ "$early" = "[]" ] || die "B's checks should be granted before the restart, run on B and post their results after it: $early"
	write_metrics broker-restart "$(jq -s --argjson t "$restart_at" --argjson took "$took" '{scenario: "broker-restart", result: "pass",
		restart_at: ($t | todate), restart_s: $took,
		linux_results_after_restart_s: [.[] | select(.context == "tentmaker/linux") | ((.final_at | fromdate) - $t)],
		checks_detail: .}' "$TEAM_TMP/restart.audit")"
}

a_devices_add() {
	SCENARIO=devices-add
	local e rc=0 out reason
	e=$(host e)
	if ! sig_get ready-e | grep -q '^success'; then
		write_metrics devices-add "$(jq -n '{scenario: "devices-add", result: "skipped", reason: "job E never joined the tailnet"}')"
		return 0
	fi
	out=$(sudo timeout 900 tentmaker devices add --json --memory 2GiB --boost-memory 4GiB "$e" 2>&1) || rc=$?
	printf '%s\n' "$out" >&2
	if [ "$rc" = 0 ]; then
		write_metrics devices-add "$(jq -n --arg o "$out" '{scenario: "devices-add", result: "pass", output: $o}')"
		return 0
	fi
	if printf '%s' "$out" | grep -Eqi 'policy does not permit|not permitted|access denied|denied by|tailnet policy'; then
		reason="the tailnet policy denies Tailscale SSH between tag:tentmaker-ci nodes"
		write_metrics devices-add "$(jq -n --arg r "$reason" --arg h "$SSH_HINT" --arg o "$out" --argjson rc "$rc" \
			'{scenario: "devices-add", result: "skipped", reason: $r, acl_fix: $h, exit: $rc, output: $o}')"
		printf '::warning::devices add skipped: %s. %s\n' "$reason" "${SSH_HINT//$'\n'/%0A}"
		return 0
	fi
	reason=$(printf '%s' "$out" | jq -r '.error // empty' 2>/dev/null | head -c 400 || true)
	[ -n "$reason" ] || reason=$(printf '%s' "$out" | tail -n 5 | head -c 400)
	write_metrics devices-add "$(jq -n --arg r "$reason" --arg o "$out" --argjson rc "$rc" \
		'{scenario: "devices-add", result: "skipped", reason: $r, exit: $rc, output: $o}')"
	printf '::warning::devices add did not finish: %s\n' "${reason//$'\n'/%0A}"
}

summary() {
	local f
	{
		echo "## Tentmaker team test ($RUN_KEY)"
		echo
		echo "| scenario | result | numbers |"
		echo "| --- | --- | --- |"
		for s in pool spread worker-loss two-brokers broker-restart devices-add; do
			f=$METRICS/team-$s.json
			if [ -s "$f" ]; then
				jq -r '"| \(.scenario) | \(.result) | \(del(.scenario, .result, .checks_detail, .devices_seen_by_a, .output, .lost, .rerun, .acl_fix) | tostring | gsub("\\|"; "/") | .[0:400]) |"' "$f"
			else
				echo "| $s | not run | |"
			fi
		done
	} >>"${GITHUB_STEP_SUMMARY:-/dev/stderr}"
}

# ---------------------------------------------------------------- workers

b_loss() {
	sig_wait kill-b 3000 >/dev/null
	sudo timeout 60 systemctl stop tentmaker
	ts down
	sig_post b-down success "daemon stopped, tailscale down"
	sig_wait b-return 900 >/dev/null
	if ! ts up --hostname="$(host b)" --accept-routes --advertise-tags="$TAG"; then
		ts up --reset --hostname="$(host b)" --accept-routes --advertise-tags="$TAG" || login_hint
	fi
	sudo timeout 60 systemctl start tentmaker
	wait_mesh
	sig_post ready-b2 success "B is back"
}

d_stop() {
	sig_wait t-done 2400 >/dev/null
	local n
	n=$(sudo sh -c "ls -1 '$CHECKS' 2>/dev/null | grep -c '\\.log\$'" || true)
	n=${n:-0}
	sudo timeout 60 systemctl stop tentmaker
	sig_post d-stopped success "granted=$n"
}

# ---------------------------------------------------------------- dispatch

cmd=${1:?command}
shift
case $cmd in
sig-post) sig_post "$@" ;;
sig-wait) sig_wait "$@" ;;
abort) abort "$@" ;;
login-hint) login_hint ;;
wait-mesh) wait_mesh ;;
ready)
	wait_mesh
	sig_post "ready-$1" success "$(host "$1") is set up"
	;;
pool) pool_check "$1" ;;
placeholders) placeholders ;;
a-pool) a_pool ;;
a-spread) a_spread ;;
a-worker-loss) a_worker_loss ;;
a-two-brokers) a_two_brokers ;;
a-broker-restart) a_broker_restart ;;
a-devices-add) a_devices_add ;;
summary) summary ;;
b-loss) b_loss ;;
d-stop) d_stop ;;
branches) for r in $BRANCHES; do printf '%s\n' "$(br "$r")"; done ;;
*) die "unknown command $cmd" ;;
esac
