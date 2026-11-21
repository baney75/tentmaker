#!/usr/bin/env bash
# Tests for the TS_AUTHKEY expiry guard in team.sh. No network, no tailnet:
# a fake tailscale and a fake sudo on PATH record every call, and TEAM_NOW
# stands in for the clock.
#
#   bash .github/team/team_test.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
team=$here/team.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/bin" "$tmp/ws"
cat >"$tmp/bin/tailscale" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_CALLS"
if [ "${1:-}" = status ]; then printf '{"BackendState":"%s"}\n' "$FAKE_STATE"; fi
EOF
cat >"$tmp/bin/sudo" <<'EOF'
#!/usr/bin/env bash
exec "$@"
EOF
chmod +x "$tmp/bin/tailscale" "$tmp/bin/sudo"

expires=$(sed -n 's/^KEY_EXPIRES=//p' "$team")
[ -n "$expires" ] || { echo "FAIL: team.sh has no KEY_EXPIRES constant"; exit 1; }
exp=$(date -u -d "$expires" +%s)
fails=0

# run NOW STATE COMMAND: runs team.sh COMMAND; sets rc, out and calls.
run() {
	: >"$tmp/calls"
	rc=0
	out=$(env PATH="$tmp/bin:$PATH" FAKE_CALLS="$tmp/calls" FAKE_STATE="$2" TEAM_NOW="$1" \
		REPO=owner/repo TEAM_SHA=0000000 RUN_KEY=1.1 POOL=ci-1 HOST_PREFIX=tm-1 BR_PREFIX=tm-team-1-1 \
		RUNNER_TEMP="$tmp" GITHUB_WORKSPACE="$tmp/ws" GITHUB_STEP_SUMMARY=/dev/null \
		bash "$team" "$3" 2>&1) || rc=$?
	calls=$(cat "$tmp/calls")
}

check() { # NAME CONDITION...
	local name=$1
	shift
	if "$@"; then
		echo "ok   $name"
	else
		echo "FAIL $name"
		printf '  rc=%s\n  out=%s\n  tailscale calls=%s\n' "$rc" "$out" "${calls:-none}"
		fails=$((fails + 1))
	fi
}
has() { printf '%s' "$out" | grep -qF "$1"; }
hasnt() { ! has "$1"; }

run "$exp" Running key-check
check "key-check fails at the expiry time" [ "$rc" -ne 0 ]
check "key-check says renew TS_AUTHKEY at the expiry time" has "renew TS_AUTHKEY"
check "key-check says how to renew" has "reusable and ephemeral"
check "key-check names the secret and the constant" has "TS_AUTHKEY repo secret"
check "key-check never calls tailscale" [ -z "$calls" ]

run $((exp + 86400 * 30)) Running key-check
check "key-check fails a month after the expiry" [ "$rc" -ne 0 ]
check "key-check says renew TS_AUTHKEY a month after the expiry" has "renew TS_AUTHKEY"

run $((exp - 60)) Running key-check
check "key-check passes a minute before the expiry" [ "$rc" -eq 0 ]
check "key-check warns a minute before the expiry" has "::warning::TS_AUTHKEY expires"

run $((exp - 86400 * 9)) Running key-check
check "key-check passes nine days before the expiry" [ "$rc" -eq 0 ]
check "key-check is quiet nine days before the expiry" hasnt "renew TS_AUTHKEY"

run $((exp - 86400 * 9)) NeedsLogin login-hint
check "login-hint fails on a refused key" [ "$rc" -ne 0 ]
check "login-hint says renew TS_AUTHKEY on a refused key" has "renew TS_AUTHKEY"

run $((exp - 86400 * 9)) NeedsMachineAuth login-hint
check "login-hint says renew TS_AUTHKEY when the node needs approval" has "renew TS_AUTHKEY"

run $((exp - 86400 * 9)) Stopped login-hint
check "login-hint fails on other errors" [ "$rc" -ne 0 ]
check "login-hint does not blame the key on other errors" hasnt "renew TS_AUTHKEY"

run "$exp" Stopped login-hint
check "login-hint says renew TS_AUTHKEY after the expiry" has "renew TS_AUTHKEY"

# The date lives in one place: the constant.
d=${expires%%T*}
n=$(grep -rhoF "$d" "$here" "$here/../workflows" | wc -l)
check "the expiry date is written once ($d)" [ "$n" -eq 1 ]

if [ "$fails" -ne 0 ]; then
	echo "$fails failed"
	exit 1
fi
echo "all passed"
