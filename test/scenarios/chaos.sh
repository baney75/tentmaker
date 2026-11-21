#!/usr/bin/env bash
# chaos: the daemon and its world break while jobs run or wait.
#   a  kill -9 the daemon mid-job: the service manager restarts it and the
#      check gets a final status within 3 minutes of the kill
#   b  restart the service while 2 jobs run: both get final statuses
#   c  api.github.com unreachable for 90 s while commits wait: status and
#      doctor show the outage, the daemon doesn't crash, and every commit
#      gets its status after
#   d  free disk below min_free_disk: no new admits and status says why;
#      freeing it resumes
# shellcheck source=test/scenarios/lib.sh
. "$(dirname "$0")/lib.sh"
scn_begin chaos
git_init

scn_up --memory 4GiB --boost-memory 4GiB --cpus 4 --boost-cpus 4
BA=$(branch a)
BB=$(branch b)
BC1=$(branch c1)
BC2=$(branch c2)
BD=$(branch d)
tm_repo_add "$BA" "$BB" "$BC1" "$BC2" "$BD"

# a: kill -9 mid-job.
c=$(mk_commit "$BASE" long.yml "kill")
git_push "$c:refs/heads/$BA"
wait_state "$c" tentmaker/long "pending" 180 2 >/dev/null || die "a: the job never started"
sleep 15
old=$(daemon_pid)
r0=$(daemon_restarts)
tk=$(now_s)
if [ "$OS" = linux ]; then sudo systemctl kill -s KILL tentmaker; else ps1 Kill-Daemon >/dev/null; fi
log "a: killed daemon $old"
if wait_new_daemon "$old" 120; then
	measure a_restart_s $(($(now_s) - tk))
	measure_s a_restarts "$r0 -> $(daemon_restarts)"
else
	die "a: the service did not restart within 120 s"
fi
if st=$(wait_state "$c" tentmaker/long "success|failure|error" $((180 - ($(now_s) - tk))) 3); then
	measure_s a_status "$st"
	measure a_kill_to_final_s $(($(st_epoch "$st") - tk))
	check "a: a final status within 180 s of the kill" test $(($(st_epoch "$st") - tk)) -le 180
else
	soft_fail "a: no final status within 180 s of the kill"
fi

# b: restart with 2 jobs running.
c=$(mk_commit "$BASE" pair.yml "restart")
git_push "$c:refs/heads/$BB"
wait_state "$c" tentmaker/one "pending" 180 2 >/dev/null || soft_fail "b: one never started"
wait_state "$c" tentmaker/two "pending" 60 2 >/dev/null || soft_fail "b: two never started"
sleep 10
running=$(tm_status | jqr '(.worker.running // []) | length')
measure b_running_at_restart "$running"
old=$(daemon_pid)
tr0=$(now_s)
if [ "$OS" = linux ]; then sudo systemctl restart tentmaker; else ps1 Restart-Daemon >/dev/null; fi
wait_new_daemon "$old" 120 || die "b: the service did not come back within 120 s"
measure b_restart_s $(($(now_s) - tr0))
for j in one two; do
	if st=$(wait_state "$c" "tentmaker/$j" "success|failure|error" $((180 - ($(now_s) - tr0))) 3); then
		measure_s "b_${j}_status" "$st"
		measure "b_${j}_restart_to_final_s" $(($(st_epoch "$st") - tr0))
	else
		soft_fail "b: $j has no final status within 180 s of the restart"
	fi
done

# c: api.github.com unreachable for 90 s while two commits wait.
c1=$(mk_commit "$BASE" quick.yml "outage 1")
c2=$(mk_commit "$BASE" quick.yml "outage 2")
pid=$(daemon_pid)
r0=$(daemon_restarts)
block_api
tb=$(now_s)
git_push "$c1:refs/heads/$BC1" "$c2:refs/heads/$BC2"
: >"$W/outage.jsonl"
doctor_done=""
while [ $(($(now_s) - tb)) -lt 90 ]; do
	tm_status 2>/dev/null | jqr -c '{t: (now | floor), repos: [.broker.repos[]? | {healthy, last_error}], errors: ((.broker.last_errors // []) | length), problems: [.problems[]?.what]}' >>"$W/outage.jsonl" 2>/dev/null || echo '{"status":"unreadable"}' >>"$W/outage.jsonl"
	if [ -z "$doctor_done" ] && [ $(($(now_s) - tb)) -ge 45 ]; then
		timeout 90 tentmaker doctor --json 2>/dev/null | tr -d '\r' >"$W/doctor-outage.json" || true
		doctor_done=1
	fi
	sleep 5
done
unblock_api
tu=$(now_s)
measure c_blocked_s $((tu - tb))
seen=$(jqr -s '[.[] | select((.repos | any(.healthy == false or ((.last_error // "") != ""))) or .errors > 0)] | length' "$W/outage.jsonl")
measure c_samples "$(wc -l <"$W/outage.jsonl" | tr -d ' ')"
measure c_samples_showing_outage "$seen"
measure_s c_status_sample "$(jqr -s -c '[.[] | select(.repos | any(.healthy == false))][0] // .[-1]' "$W/outage.jsonl")"
check "c: status showed the outage while it lasted ($seen samples)" test "$seen" -gt 0
dfail=$(jqr -c '[.checks[]? | select(.status != "ok" and .check == "forge") | .what]' "$W/doctor-outage.json" 2>/dev/null || echo '[]')
measure_j c_doctor_forge "$dfail"
check "c: doctor showed the outage ($dfail)" test "$dfail" != "[]"
check "c: the daemon did not restart during the outage (pid $pid -> $(daemon_pid), restarts $r0 -> $(daemon_restarts))" test "$(daemon_pid)" = "$pid"
for x in "$c1" "$c2"; do
	if st=$(wait_state "$x" tentmaker/hello "success|failure|error" 240 3); then
		check "c: ${x:0:12} succeeds after the outage ($st)" is_success "$st"
		p=$(first_epoch "$x" tentmaker/hello pending)
		[ -n "$p" ] && measure "c_unblock_to_pending_s_${x:0:7}" $((p - tu))
		measure "c_unblock_to_final_s_${x:0:7}" $(($(st_epoch "$st") - tu))
	else
		soft_fail "c: ${x:0:12} has no final status within 240 s of the unblock"
	fi
done

# d: the disk guard.
free=$(free_gib)
measure d_free_before_gib "$free"
if [ "$free" -lt 12 ]; then
	soft_fail "d: only ${free} GiB free; can't leave 10 GiB to test the 20 GiB guard"
else
	fill_disk 10
	measure d_free_filled_gib "$(free_gib)"
	sleep 8
	s=$(tm_status)
	admit=$(printf '%s' "$s" | jqr '.decision.admit')
	why=$(printf '%s' "$s" | jqr -r '[.why, (.decision.reasons // [])[], (.problems // [])[].what] | join("; ")')
	measure_s d_status_why "$why"
	check "d: no new admits with the disk full (admit $admit)" test "$admit" = false
	check "d: status says free disk is low ($why)" grep -qi "free disk" <<<"$why"
	c=$(mk_commit "$BASE" quick.yml "disk")
	git_push "$c:refs/heads/$BD"
	sleep 45
	st=$(state_of "$c" tentmaker/hello)
	measure_s d_status_while_full "$st"
	check "d: no job started with the disk full ($st)" test "$st" = none
	unfill_disk
	tf=$(now_s)
	if st=$(wait_state "$c" tentmaker/hello "success|failure|error" 180 3); then
		check "d: the job runs once the disk is freed ($st)" is_success "$st"
		p=$(first_epoch "$c" tentmaker/hello pending)
		[ -n "$p" ] && measure d_free_to_pending_s $((p - tf))
	else
		soft_fail "d: no final status within 180 s of freeing the disk"
	fi
fi
measure rate_left "$(rate_left)"
