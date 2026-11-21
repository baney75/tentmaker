#!/usr/bin/env bash
# failures: each way a job goes wrong ends honestly.
#   fails     a failing step: failure, "failed at step boom", log kept, the
#             next step never runs
#   tolerant  continue-on-error: success
#   slow      timeout-minutes: 1 with sleep 90: killed, final status within
#             120 s of starting (60 s timeout plus 60 s grace), no process left
#   db        services: never run, never pending, the reason in the log
# shellcheck source=test/scenarios/lib.sh
. "$(dirname "$0")/lib.sh"
scn_begin failures
git_init
BR=$(branch main)

scn_up
c=$(mk_commit "$BASE" failures.yml all)
tm_repo_add "$BR"
git_push "$c:refs/heads/$BR"
T0=$(now_s)

st=$(wait_state "$c" tentmaker/fails "success|failure|error" 420) || die "fails: no final status"
measure_s fails_status "$st"
check "fails ends in failure ($st)" test "${st%% *}" = failure
check "fails says failed at step boom ($st)" grep -q "failed at step boom" <<<"$st"
log_fails=$(kept_log "$c" fails || true)
check "fails has a kept log" test -n "$log_fails"
check "fails' kept log records the failure" grep -q '^state: failure' <<<"$log_fails"
check "fails' kept log shows exit 3" grep -q 'the step failed (exit 3)' <<<"$log_fails"
check "the step after boom never ran" bash -c '! grep -q "this step must not run" <<<"$1"' _ "$log_fails"

st=$(wait_state "$c" tentmaker/tolerant "success|failure|error" 420) || die "tolerant: no final status"
measure_s tolerant_status "$st"
check "tolerant (continue-on-error) succeeds ($st)" is_success "$st"
check "tolerant's log says continue-on-error let it go on" grep -q "continue-on-error lets the job go on" <<<"$(kept_log "$c" tolerant)"

st=$(wait_state "$c" tentmaker/slow "success|failure|error" 420) || die "slow: no final status"
measure_s slow_status "$st"
p=$(first_epoch "$c" tentmaker/slow pending)
f=$(st_epoch "$st")
if [ -n "$p" ]; then
	measure slow_pending_to_final_s $((f - p))
	check "slow ends within 120 s of starting (took $((f - p))s)" test $((f - p)) -le 120
else
	soft_fail "slow never posted pending"
fi
check "slow ends in failure ($st)" test "${st%% *}" = failure
check "slow says it timed out ($st)" grep -q "timed out at step nap" <<<"$st"
sleep 15
if [ "$OS" = linux ]; then
	# The runner user's own service manager, its session bus (setup waits
	# for that bus) and podman's socket service are not job processes.
	left=$(ps -u tentmaker-runner -o pid=,args= 2>/dev/null | grep -Ev 'systemd --user|\(sd-pam\)|dbus-daemon --session|dbus-broker|podman system service|catatonit' || true)
	units=$(systemctl list-units --no-legend --state=active 'tentmaker-job*' 2>/dev/null || true)
else
	left=$(ps1 Runner-Procs)
	units=""
fi
measure_s leftover_processes "$left"
check "no process left for the runner user after the timeout ($left)" test -z "$left"
check "no job unit left active ($units)" test -z "$units"

# db: planned out. By now the other three are done, so the broker has seen
# this commit many times.
st=$(state_of "$c" tentmaker/db)
measure_s db_status "$st"
check "db (services:) never got a status ($st)" test "$st" = none
skip=$(tm_logs 5000 | grep 'msg="check job skipped"' | grep 'job=db' | head -1 || true)
measure_s db_skip_log "$skip"
check "the log says why db was skipped ($skip)" grep -q services <<<"$skip"
measure total_checks_s $(($(now_s) - T0))
