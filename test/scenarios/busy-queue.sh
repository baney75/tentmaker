#!/usr/bin/env bash
# busy-queue: eight branches pushed at once to a device with exactly two
# slots. Each commit gets exactly one run and one final status (success),
# never more than two run at once, and the queue drains within a bound.
#
# The bound: 4 waves (8 jobs, 2 slots) of a 30 s job plus up to 20 s of
# start and report each, plus up to 35 s for the broker to see the pushes
# (its slowest poll is 30 s): 4 * 50 + 35 = 235 s. The assertion is 300 s.
# shellcheck source=test/scenarios/lib.sh
. "$(dirname "$0")/lib.sh"
scn_begin busy-queue
git_init
CTX=tentmaker/work
N=8
BOUND=300

# 4 GiB and 4 CPUs at 2 GiB and 2 CPUs a job: exactly 2 slots, plugged in or not.
scn_up --memory 4GiB --boost-memory 4GiB --cpus 4 --boost-cpus 4
slots=$(tm_status | jqr '.worker.slots.total // .decision.slots')
measure slots "$slots"
[ "$slots" = 2 ] || die "the budget gives $slots slots, not 2: $(tm_status | jqr -c '.decision')"

shas=()
refs=()
brs=()
for i in $(seq 1 $N); do
	c=$(mk_commit "$BASE" work.yml "branch $i")
	shas+=("$c")
	brs+=("$(branch "b$i")")
	refs+=("$c:refs/heads/$(branch "b$i")")
done
tm_repo_add "${brs[@]}"
sleep 10

# Every 2 s: how many jobs run.
(
	while :; do
		tm_status 2>/dev/null | jqr -c '{t: now | floor, used: (.worker.slots.used // null), running: ((.worker.running // []) | length)}' 2>/dev/null || true
		sleep 2
	done
) >"$W/samples.jsonl" &
SAMPLER=$!
SCN_BG+=("$SAMPLER")

T0=$(now_s)
git_push "${refs[@]}"
log "pushed $N branches at once"

# =() so the array is set: under set -u an empty, unset one is an error.
declare -A final=()
deadline=$((T0 + BOUND + 120))
while [ "${#final[@]}" -lt "$N" ] && [ "$(now_s)" -lt "$deadline" ]; do
	for c in "${shas[@]}"; do
		[ -n "${final[$c]:-}" ] && continue
		st=$(state_of "$c" "$CTX")
		case $st in success\ * | failure\ * | error\ *)
			final[$c]=$st
			log "${c:0:12}: $st (${#final[@]}/$N)"
			;;
		esac
	done
	sleep 3
done
kill "$SAMPLER" 2>/dev/null || true
wait "$SAMPLER" 2>/dev/null || true
last=0
for c in "${shas[@]}"; do
	if [ -n "${final[$c]:-}" ]; then
		e=$(st_epoch "${final[$c]}")
		[ "$e" -gt "$last" ] && last=$e
	fi
done
wall=$((last - T0))
measure finished "${#final[@]}"
measure drain_s "$wall"
check "all $N commits got a final status" test "${#final[@]}" -eq "$N"
check "the queue drained within ${BOUND}s (took ${wall}s)" test "$wall" -le "$BOUND"

tm_logs 5000 >"$W/daemon.log"
waits=()
for c in "${shas[@]}"; do
	st=${final[$c]:-none}
	check "${c:0:12} succeeded ($st)" is_success "$st"
	pend=$(count_states "$c" "$CTX" pending)
	grants=$(grep 'msg="check granted"' "$W/daemon.log" | grep -c "sha=$c" || true)
	check "${c:0:12} ran once (pending statuses $pend, grants $grants)" test "$pend" = 1 -a "$grants" = 1
	p=$(first_epoch "$c" "$CTX" pending)
	[ -n "$p" ] && waits+=($((p - T0)))
done
measure_j queue_wait_s "$(printf '%s\n' "${waits[@]}" | jqr -sc .)"

nsamples=$(wc -l <"$W/samples.jsonl" | tr -d ' ')
maxrun=$(jqr -s '[.[].running] | max // 0' "$W/samples.jsonl")
maxused=$(jqr -s '[.[].used | select(. != null)] | max // 0' "$W/samples.jsonl")
measure samples "$nsamples"
measure max_running "$maxrun"
measure max_slots_used "$maxused"
check "never more than 2 running (max running $maxrun, max used $maxused)" test "$maxrun" -le 2 -a "$maxused" -le 2
check "both slots were used (max running $maxrun)" test "$maxrun" -eq 2
measure rate_left "$(rate_left)"
