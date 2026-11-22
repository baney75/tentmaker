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
# Podman's network namespace is one per device, shared by every job's
# containers (docs/DESIGN.md, linux executor): its holder, its pasta uplink
# and the unit holding podman's pause process run as the runner from setup
# on, so they are not left over from a job. They are told apart by their
# unit's cgroup, never by name: a job's leak can run `sleep infinity` too.
DEVICE_UNITS="tentmaker-podnet.service tentmaker-podnet-uplink.service tentmaker-podns.service"
# device_unit PID: the per-device unit PID runs in, or nothing.
device_unit() {
	local cg u
	cg=$(sed -n 's/^0:://p' "/proc/$1/cgroup" 2>/dev/null) || return 0
	for u in $DEVICE_UNITS; do
		case "$cg/" in */"$u"/*) printf '%s' "$u" ;; esac
	done
}
# runner_leftovers: the runner's processes outside the per-device units. The
# runner user's own service manager, its session bus (setup waits for that
# bus) and podman's socket service are not job processes either.
runner_leftovers() {
	local pid args u
	ps -u tentmaker-runner -o pid=,args= 2>/dev/null |
		grep -Ev 'systemd --user|\(sd-pam\)|dbus-daemon --session|dbus-broker|podman system service|catatonit' |
		while read -r pid args; do
			u=$(device_unit "$pid")
			if [ -n "$u" ]; then
				printf '%s %s (%s)\n' "$pid" "$args" "$u" >>"$W/device-procs"
				continue
			fi
			printf '%7s %s\n' "$pid" "$args"
		done
}
if [ "$OS" = linux ]; then
	: >"$W/device-procs"
	left=$(runner_leftovers)
	measure_s device_processes "$(cat "$W/device-procs")"
	units=$(systemctl list-units --no-legend --state=active 'tentmaker-job*' 2>/dev/null || true)
else
	left=$(ps1 Runner-Procs)
	units=""
fi
measure_s leftover_processes "$left"
check "no process left for the runner user after the timeout ($left)" test -z "$left"
check "no job unit left active ($units)" test -z "$units"
if [ "$OS" = linux ]; then
	# The exclusion must not hide a real leak: a runner process with the
	# holder's very argv, in the same slice, but in a unit of its own.
	sudo systemd-run --quiet --uid=tentmaker-runner --unit=tm-scn-leak.service --slice=tentmaker.slice sleep infinity
	sleep 1
	: >"$W/device-procs"
	planted=$(runner_leftovers)
	sudo systemctl stop tm-scn-leak.service || true
	check "a planted leak in the same slice is still reported ($planted)" grep -q 'sleep infinity' <<<"$planted"
fi

# db: planned out. By now the other three are done, so the broker has seen
# this commit many times.
st=$(state_of "$c" tentmaker/db)
measure_s db_status "$st"
check "db (services:) never got a status ($st)" test "$st" = none
skip=$(tm_logs 5000 | grep 'msg="check job skipped"' | grep 'job=db' | head -1 || true)
measure_s db_skip_log "$skip"
check "the log says why db was skipped ($skip)" grep -q services <<<"$skip"
measure total_checks_s $(($(now_s) - T0))
