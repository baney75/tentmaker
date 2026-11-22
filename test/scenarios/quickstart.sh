#!/usr/bin/env bash
# quickstart: a new user's first green check, then how long a push waits
# for its pending status.
#
# Targets: install start to the first tentmaker/hello success under 3
# minutes (the build is not counted: a user downloads it); push to pending
# p95 under 40 s over 5 pushes made after the poller has gone idle. Idle,
# the broker polls every 30 s (docs/DESIGN.md), and reading the workflow,
# the grant and the status post add a few seconds, so 30 s is the poll
# alone; 40 s still fails a slower poll.
# shellcheck source=test/scenarios/lib.sh
. "$(dirname "$0")/lib.sh"
scn_begin quickstart
git_init
CTX=tentmaker/hello
BR=$(branch main)

# The commit is on GitHub before the user installs anything.
c=$(mk_commit "$BASE" quick.yml first)
git_push "$c:refs/heads/$BR"

T0=$(now_s)
scn_up
tm_repo_add "$BR"
measure ready_s $(($(now_s) - T0))
if st=$(wait_state "$c" "$CTX" "success|failure|error" 300); then
	first=$(($(st_epoch "$st") - T0))
	measure first_green_s "$first"
	check "the first check succeeds ($st)" is_success "$st"
	check "first green within 180 s of the install starting (took ${first}s)" test "$first" -le 180
else
	die "no final $CTX on the first commit within 300 s"
fi

# Pushes after the poller has backed off to its slowest interval.
lat=()
prev=$c
for i in 1 2 3 4 5; do
	sleep 70
	c=$(mk_commit "$prev" quick.yml "push $i")
	prev=$c
	git_push "$c:refs/heads/$BR"
	tp=$(now_s)
	if st=$(wait_state "$c" "$CTX" "pending|success|failure|error" 120 2); then
		p=$(first_epoch "$c" "$CTX" pending)
		[ -n "$p" ] || p=$(st_epoch "$st")
		lat+=($((p - tp)))
		log "push $i: pending after $((p - tp))s"
	else
		soft_fail "push $i: no status within 120 s"
		lat+=(120)
	fi
	if st=$(wait_state "$c" "$CTX" "success|failure|error" 180); then
		check "push $i succeeds ($st)" is_success "$st"
	else
		soft_fail "push $i: no final status within 180 s"
	fi
done
measure_j push_to_pending_s "$(printf '%s\n' "${lat[@]}" | jqr -sc .)"
p50=$(pct 50 "${lat[@]}")
p95=$(pct 95 "${lat[@]}")
measure push_to_pending_p50_s "$p50"
measure push_to_pending_p95_s "$p95"
check "push to pending p95 under 40 s (p95 ${p95}s)" test "$p95" -lt 40
measure rate_left "$(rate_left)"
