#!/usr/bin/env bash
# upgrade: safe to update, safe to roll back, and a broken release undoes
# itself.
#   A -> B with `tentmaker update` (dev channel, the fake release), jobs pass
#   `tentmaker rollback` returns to A, jobs pass
#   C (built with -tags tentmaker_badstart: version --json works, the daemon
#   exits at start) is picked up by auto-update, and the daemon is back on A
#   within 3 minutes with C recorded as bad
#
# Test hooks (docs/DESIGN.md): TENTMAKER_UPDATE_API points update at the
# fake release; TENTMAKER_UPDATE_EVERY shortens the auto-update interval.
# shellcheck source=test/scenarios/lib.sh
. "$(dirname "$0")/lib.sh"
scn_begin upgrade
git_init
CTX=tentmaker/hello
BR=$(branch main)
A=0.0.1-scn.1
B=0.0.1-scn.2
C=0.0.1-scn.3
API=http://127.0.0.1:$PORT

SCN_VERSION=$A scn_up --channel dev
tm_repo_add "$BR"

# update and rollback change the binary the service runs. That is the one on
# PATH only when only root can change it there: on the hosted Linux runner
# /usr/local/bin is writable by every user, so setup copies it to
# /usr/local/libexec/tentmaker and says so (docs/DESIGN.md).
SVC_BIN=$(jqr -r '.binary // empty' "$W/setup.json")
[ -n "$SVC_BIN" ] || die "setup --json names no service binary"
[ "$OS" = windows ] && SVC_BIN=$(cygpath -u "$SVC_BIN")
measure_s service_binary "$SVC_BIN"
svc_version() { tm_status 2>/dev/null | jqr -r '.version // empty'; }
bin_version() { timeout 30 "$SVC_BIN" version --json 2>/dev/null | jqr -r '.version // empty'; }
# job MARK: one commit, which must succeed.
n=0
job() {
	n=$((n + 1))
	local c st
	c=$(mk_commit "$BASE" quick.yml "$1 $n")
	git_push "$c:refs/heads/$BR"
	if st=$(wait_state "$c" "$CTX" "success|failure|error" 300); then
		check "a job on $1 succeeds ($st)" is_success "$st"
	else
		soft_fail "a job on $1: no final status within 300 s"
	fi
}

check "A is running ($(svc_version))" test "$(svc_version)" = "$A"
job "$A"

# A -> B.
publish "$B"
t=$(now_ms)
out=$(tm_env TENTMAKER_UPDATE_API="$API" update --channel dev --json 2>&1 | tr -d '\r') || soft_fail "update exited non-zero: $out"
measure update_s "$(secs_since "$t")"
measure_j update_output "$(printf '%s' "$out" | jqr -c . 2>/dev/null || jqr -n --arg s "$out" '$s')"
wait_service_up 60 || soft_fail "the service is not up after the update"
check "B is installed ($(bin_version))" test "$(bin_version)" = "$B"
check "B is running ($(svc_version))" test "$(svc_version)" = "$B"
job "$B"

# B -> A.
t=$(now_ms)
out=$(tm rollback --json 2>&1 | tr -d '\r') || soft_fail "rollback exited non-zero: $out"
measure rollback_s "$(secs_since "$t")"
measure_s rollback_output "$out"
wait_service_up 60 || soft_fail "the service is not up after the rollback"
check "rollback put A back ($(bin_version))" test "$(bin_version)" = "$A"
check "A is running again ($(svc_version))" test "$(svc_version)" = "$A"
job "after rollback"

# A -> C (bad) by auto-update, which must undo itself.
publish "$C" tentmaker_badstart
measure_s bad_build "$C (tags tentmaker_badstart)"
set_daemon_env TENTMAKER_UPDATE_API="$API" TENTMAKER_UPDATE_EVERY=10s
t0=$(now_s)
saw_c=""
tc=""
back=""
while [ $(($(now_s) - t0)) -lt 420 ]; do
	bv=$(bin_version)
	sv=$(svc_version)
	if [ "$bv" = "$C" ] && [ -z "$saw_c" ]; then
		saw_c=1
		tc=$(now_s)
		log "auto-update installed $C after $((tc - t0))s"
	fi
	if [ -n "$saw_c" ] && [ "$bv" = "$A" ] && [ "$sv" = "$A" ]; then
		back=$(now_s)
		break
	fi
	sleep 2
done
check "auto-update picked up C" test -n "$saw_c"
if [ -n "$saw_c" ]; then
	measure auto_update_after_s $((tc - t0))
fi
if [ -n "$back" ]; then
	measure bad_to_rolled_back_s $((back - tc))
	check "back on A within 180 s of C going in (took $((back - tc))s)" test $((back - tc)) -le 180
else
	soft_fail "the daemon is not back on A (binary $(bin_version), service $(svc_version))"
fi
bad=$(rcat "$STATE/update-bad.json" 2>/dev/null | tr -d '\r' || true)
measure_s update_bad "$bad"
check "C is recorded as bad ($bad)" grep -q "$C" <<<"$bad"
# A must not take C again.
sleep 40
check "A stays (binary $(bin_version), service $(svc_version))" test "$(bin_version)" = "$A" -a "$(svc_version)" = "$A"
job "A after the bad update"
