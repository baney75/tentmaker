#!/usr/bin/env bash
# dogfood (Linux): this repo's own CI (ci.yml) through Tentmaker, next to
# GitHub's hosted runners on the same commit. "test (ubuntu-latest)" and
# "cross-build" must pass on both; the durations are recorded.
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

c=$(mk_commit "$BASE" "$SCN_ROOT/.github/workflows/ci.yml" "ci")
measure_s sha "$c"
tm_repo_add "$BR"
git_push "$c:refs/heads/$BR"
tp=$(now_s)

# The same commit on GitHub's runners.
timeout 60 gh workflow run ci.yml --repo "$REPO" --ref "$BR" || soft_fail "could not dispatch ci.yml on $BR"

declare -A tmst
for j in "${JOBS[@]}"; do
	if st=$(wait_state "$c" "tentmaker/$j" "success|failure|error" 1800 10); then
		tmst[$j]=$st
	else
		tmst[$j]="none"
		soft_fail "tentmaker/$j: no final status within 30 minutes"
	fi
done

# GitHub's run of the same commit.
run=""
for _ in $(seq 1 60); do
	run=$(timeout 30 gh api "repos/$REPO/actions/workflows/ci.yml/runs?head_sha=$c&per_page=5" --jq '.workflow_runs[0].id // empty' || true)
	[ -n "$run" ] && break
	sleep 10
done
[ -n "$run" ] || soft_fail "no GitHub run of ci.yml on $c"
gh_jobs='[]'
if [ -n "$run" ]; then
	for _ in $(seq 1 120); do
		gh_jobs=$(timeout 30 gh api "repos/$REPO/actions/runs/$run/jobs?per_page=50" --jq '[.jobs[] | {name, status, conclusion, started_at, completed_at}]' || echo '[]')
		pending=$(printf '%s' "$gh_jobs" | jqr --argjson want "$(printf '%s\n' "${JOBS[@]}" | jqr -R . | jqr -s .)" '[.[] | select(.name as $n | $want | index($n)) | select(.status != "completed")] | length')
		[ "$pending" = 0 ] && [ "$gh_jobs" != '[]' ] && break
		sleep 15
	done
	measure_s github_run "https://github.com/$REPO/actions/runs/$run"
fi

for j in "${JOBS[@]}"; do
	key=$(printf '%s' "$j" | tr -c 'A-Za-z0-9' '_' | sed 's/_*$//')
	st=${tmst[$j]}
	measure_s "tentmaker_${key}_status" "$st"
	check "tentmaker/$j passes under Tentmaker ($st)" is_success "$st"
	p=$(first_epoch "$c" "tentmaker/$j" pending)
	if is_success "$st" && [ -n "$p" ]; then
		measure "tentmaker_${key}_s" $(($(st_epoch "$st") - p))
		measure "tentmaker_${key}_push_to_final_s" $(($(st_epoch "$st") - tp))
	fi
	g=$(printf '%s' "$gh_jobs" | jqr -c --arg n "$j" '[.[] | select(.name == $n)][0]')
	gc=$(printf '%s' "$g" | jqr -r '.conclusion // "none"')
	measure_s "github_${key}_conclusion" "$gc"
	check "$j passes on GitHub's runner ($gc)" test "$gc" = success
	if [ "$gc" = success ]; then
		measure "github_${key}_s" "$(printf '%s' "$g" | jqr '((.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601))')"
	fi
done

# The other Linux jobs in ci.yml, recorded only.
for j in shellcheck "install.sh (ubuntu-latest)"; do
	key=$(printf '%s' "$j" | tr -c 'A-Za-z0-9' '_' | sed 's/_*$//')
	measure_s "tentmaker_${key}_status" "$(state_of "$c" "tentmaker/$j")"
done
measure rate_left "$(rate_left)"
