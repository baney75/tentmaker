#!/usr/bin/env bash
# dogfood (Linux): this repo's own CI (ci.yml) through Tentmaker, next to
# GitHub's hosted runners on the same commit. "test (ubuntu-latest)" and
# "cross-build" must pass on both; the durations are recorded.
#
# Two rounds: a cold one, then a warm one on a new commit of the same tree
# on the same branch (Tentmaker's repo cache and GitHub's setup-go cache
# are both warm by then). The ratchet: the warm Tentmaker test job takes no
# longer than GitHub's warm one, and the median pickup (push to the first
# pending status, first-wave jobs only, since 2 slots make the rest queue)
# is 5 s or less.
#
# Go comes from the documented tools folder: setup-go steps find
# /var/lib/tentmaker/tools/go-<version>.
#
# The commit is a throwaway one with ci.yml copied to .tm-scn/wf.yml, so
# the broker never plans ci.yml on main's head or on open PRs. GitHub runs
# the real ci.yml on it through workflow_dispatch.
# shellcheck source=test/scenarios/lib.sh
. "$(dirname "$0")/lib.sh"
scn_begin dogfood
[ "$OS" = linux ] || die "dogfood runs on Linux only"
git_init
BR=$(branch ci)
JOBS=("test (ubuntu-latest)" "cross-build")

# 8 GiB and 4 CPUs: 2 slots of the default 2 GiB, 2 CPU job.
scn_up --memory 8GiB --boost-memory 8GiB --cpus 4 --boost-cpus 4
gover=$(go env GOVERSION | sed 's/^go//')
gor=$(go env GOROOT)
sudo cp -a "$gor" "$STATE/tools/go-$gover"
sudo chown -R root:root "$STATE/tools/go-$gover"
sudo chmod -R a+rX,go-w "$STATE/tools/go-$gover"
measure_s go_version "$gover"

# Every Linux job of ci.yml; the first two get the device's 2 slots.
ALL=("test (ubuntu-latest)" "cross-build" "shellcheck" "install.sh (ubuntu-latest)")
PICKUPS=()
keyof() { printf '%s' "$1" | tr -c 'A-Za-z0-9' '_' | sed 's/_*$//'; }

# round SUFFIX COMMIT: push COMMIT, run ci.yml on GitHub on it, wait for
# both, record the numbers with SUFFIX ("" for the cold round, "_warm").
# Sets TM_TEST_S and GH_TEST_S (empty when the job didn't pass).
round() {
	local sfx=$1 c=$2 tp st j key p run="" gh_jobs='[]' pending g gc pend=()
	declare -A tmst=()
	TM_TEST_S="" GH_TEST_S=""
	measure_s "sha$sfx" "$c"
	git_push "$c:refs/heads/$BR"
	tp=$(now_s)
	timeout 60 gh workflow run ci.yml --repo "$REPO" --ref "$BR" || soft_fail "could not dispatch ci.yml on $BR ($c)"

	for j in "${ALL[@]}"; do
		if st=$(wait_state "$c" "tentmaker/$j" "success|failure|error" 1800 5); then
			tmst[$j]=$st
		else
			tmst[$j]="none"
			case $j in "test (ubuntu-latest)" | cross-build) soft_fail "tentmaker/$j: no final status within 30 minutes ($c)" ;; esac
		fi
	done

	# Pickup: push to the first pending status of each of the first two jobs.
	for j in "${ALL[@]}"; do
		p=$(first_epoch "$c" "tentmaker/$j" pending)
		[ -n "$p" ] && pend+=("$((p - tp))")
	done
	if [ "${#pend[@]}" -ge 2 ]; then
		mapfile -t pend < <(printf '%s\n' "${pend[@]}" | sort -n)
		measure "pickup${sfx}_s" "${pend[0]}"
		measure "pickup${sfx}_second_s" "${pend[1]}"
		PICKUPS+=("${pend[0]}" "${pend[1]}")
	else
		soft_fail "fewer than two pending statuses on $c"
	fi

	for _ in $(seq 1 60); do
		run=$(timeout 30 gh api "repos/$REPO/actions/workflows/ci.yml/runs?head_sha=$c&per_page=5" --jq '.workflow_runs[0].id // empty' || true)
		[ -n "$run" ] && break
		sleep 10
	done
	[ -n "$run" ] || soft_fail "no GitHub run of ci.yml on $c"
	if [ -n "$run" ]; then
		for _ in $(seq 1 120); do
			gh_jobs=$(timeout 30 gh api "repos/$REPO/actions/runs/$run/jobs?per_page=50" --jq '[.jobs[] | {name, status, conclusion, started_at, completed_at}]' || echo '[]')
			pending=$(printf '%s' "$gh_jobs" | jqr --argjson want "$(printf '%s\n' "${JOBS[@]}" | jqr -R . | jqr -s .)" '[.[] | select(.name as $n | $want | index($n)) | select(.status != "completed")] | length')
			[ "$pending" = 0 ] && [ "$gh_jobs" != '[]' ] && break
			sleep 15
		done
		measure_s "github_run$sfx" "https://github.com/$REPO/actions/runs/$run"
	fi

	for j in "${JOBS[@]}"; do
		key=$(keyof "$j")
		st=${tmst[$j]}
		measure_s "tentmaker_${key}${sfx}_status" "$st"
		check "tentmaker/$j passes under Tentmaker ($st, $c)" is_success "$st"
		p=$(first_epoch "$c" "tentmaker/$j" pending)
		if is_success "$st" && [ -n "$p" ]; then
			measure "tentmaker_${key}${sfx}_s" $(($(st_epoch "$st") - p))
			measure "tentmaker_${key}${sfx}_push_to_final_s" $(($(st_epoch "$st") - tp))
			[ "$j" = "test (ubuntu-latest)" ] && TM_TEST_S=$(($(st_epoch "$st") - p))
		fi
		g=$(printf '%s' "$gh_jobs" | jqr -c --arg n "$j" '[.[] | select(.name == $n)][0]')
		gc=$(printf '%s' "$g" | jqr -r '.conclusion // "none"')
		measure_s "github_${key}${sfx}_conclusion" "$gc"
		check "$j passes on GitHub's runner ($gc, $c)" test "$gc" = success
		if [ "$gc" = success ]; then
			measure "github_${key}${sfx}_s" "$(printf '%s' "$g" | jqr '((.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601))')"
			[ "$j" = "test (ubuntu-latest)" ] && GH_TEST_S=$(printf '%s' "$g" | jqr '((.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601))')
		fi
	done
	# The other Linux jobs in ci.yml, recorded only.
	for j in shellcheck "install.sh (ubuntu-latest)"; do
		measure_s "tentmaker_$(keyof "$j")${sfx}_status" "${tmst[$j]}"
	done
	measure "rate_left${sfx}" "$(rate_left)"
}

tm_repo_add "$BR"
c=$(mk_commit "$BASE" "$SCN_ROOT/.github/workflows/ci.yml" "ci")
round "" "$c"
measure repo_cache_bytes_cold "$(sudo du -sb /var/cache/tentmaker/repos 2>/dev/null | cut -f1 || echo 0)"

# The same tree again (only the marker differs), on the same branch.
c2=$(mk_commit "$c" "$SCN_ROOT/.github/workflows/ci.yml" "ci warm")
round _warm "$c2"
measure repo_cache_bytes_warm "$(sudo du -sb /var/cache/tentmaker/repos 2>/dev/null | cut -f1 || echo 0)"

# The ratchet.
if [ -n "$TM_TEST_S" ] && [ -n "$GH_TEST_S" ]; then
	check "warm Tentmaker test (${TM_TEST_S}s) is no slower than warm GitHub-hosted test (${GH_TEST_S}s)" test "$TM_TEST_S" -le "$GH_TEST_S"
else
	soft_fail "no warm test time to compare (tentmaker '${TM_TEST_S}', github '${GH_TEST_S}')"
fi
if [ "${#PICKUPS[@]}" -gt 0 ]; then
	med=$(printf '%s\n' "${PICKUPS[@]}" | sort -n | awk '{ v[NR] = $1 } END { if (NR % 2) print v[(NR + 1) / 2]; else print (v[NR / 2] + v[NR / 2 + 1]) / 2 }')
	measure pickup_median_s "$med"
	check "median pickup (${med}s over ${#PICKUPS[@]} first-wave jobs) is 5 s or less" awk -v m="$med" 'BEGIN { exit !(m <= 5) }'
else
	soft_fail "no pickup measured"
fi
