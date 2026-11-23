#!/usr/bin/env bash
# bench (Linux): the Tentmaker side of .github/workflows/bench.yml. It runs
# one workload, .github/workflows/bench-$BENCH_WORKLOAD.yml, through
# Tentmaker on this VM: BENCH_REPS times (default 3), a cold run on a new
# branch, then a warm run of the same tree on the same branch. Between pairs
# it empties Tentmaker's repo cache and podman's images, so every cold run
# starts empty.
#
# The workload file goes in verbatim as .tm-scn/wf.yml. For each run it
# keeps, under bench-raw/tm/<workload>/<rep>-<round>/, the commit's statuses
# (when the job was granted and when it ended), the check log Tentmaker kept
# (the job's phase marks), and meta.json (the push time). scripts/bench turns
# those into the tables, and api.json (the daemon's API call counts at the
# end) into the API calls table; this script only records. A job that doesn't pass
# is recorded and reported as a failure, except on the workloads the gate
# leaves out (UNGATED, which scripts/bench tests against its own list): there
# it is recorded with a warning, so only the gate decides the bench run.
#
# Go and Node come from the documented tools folder, copied from this VM's
# own toolchains, as on a device where they are installed.
# shellcheck source=test/scenarios/lib.sh
. "$(dirname "$0")/lib.sh"
WL=${BENCH_WORKLOAD:?BENCH_WORKLOAD is required}
REPS=${BENCH_REPS:-3}
scn_begin "bench-$WL"
[ "$OS" = linux ] || die "bench runs on Linux only"
git_init
WF=$SCN_ROOT/.github/workflows/bench-$WL.yml
[ -f "$WF" ] || die "no workload file $WF"
CTX="tentmaker/$WL"
UNGATED="docker"
OUT=$SCN_ROOT/bench-raw/tm/$WL
mkdir -p "$OUT"

# The dogfood scenario's device: 8 GiB and 4 CPUs, the default job size.
scn_up --memory 8GiB --boost-memory 8GiB --cpus 4 --boost-cpus 4
# The repo's shared API limit left as the reps start; under a quarter of the
# limit the daemon paces its polls (issue #39). rate_left at the end is below.
measure "rate_left_start" "$(rate_left)"

# stage_tool TOOL DIR VERSION: DIR as <state>/tools/TOOL-VERSION.
stage_tool() {
	sudo rm -rf "$STATE/tools/$1-$3"
	sudo cp -a "$2" "$STATE/tools/$1-$3"
	sudo chown -R root:root "$STATE/tools/$1-$3"
	sudo chmod -R a+rX,go-w "$STATE/tools/$1-$3"
	measure_s "${1}_version" "$3"
}
stage_tool go "$(go env GOROOT)" "$(go env GOVERSION | sed 's/^go//')"
if [ "$WL" = node ]; then
	nd=$(find /opt/hostedtoolcache/node -mindepth 2 -maxdepth 2 -name x64 -path '*/22.*' | sort -V | tail -1)
	[ -n "$nd" ] || die "no Node 22 in this VM's tool cache"
	stage_tool node "$nd" "$(basename "$(dirname "$nd")")"
fi

jqr -n --arg w "$WL" --arg os "${ImageOS:-}" --arg iv "${ImageVersion:-}" \
	--argjson cpus "$(nproc)" --arg model "$(lscpu | sed -n 's/^Model name: *//p' | head -1)" \
	--argjson mem "$(awk '/^MemTotal:/ { printf "%.1f", $2 / 1048576 }' /proc/meminfo)" \
	--arg v "$(timeout 30 tentmaker version --json | jqr -r .version)" \
	--argjson budget "$(tm_status | jqr -c '.decision // {}')" \
	'{workload: $w, image_os: $os, image_version: $iv, cpus: $cpus, cpu_model: $model, memory_gib: $mem, version: $v, budget: $budget}' \
	>"$OUT/device.json"

BRS=()
for rep in $(seq 1 "$REPS"); do BRS+=("$(branch "r$rep")"); done
tm_repo_add "${BRS[@]}"

soft_note() { echo "::warning title=bench $WL::$*"; }
ungated() { case " $UNGATED " in *" $WL "*) return 0 ;; *) return 1 ;; esac; }

# sample ROUND REP COMMIT BRANCH: push, wait for the final status, keep the record.
sample() {
	local round=$1 rep=$2 c=$3 br=$4 d tp st p
	d=$OUT/$rep-$round
	mkdir -p "$d"
	git_push "$c:refs/heads/$br"
	tp=$(now_s)
	if st=$(wait_state "$c" "$CTX" "success|failure|error" 2400 3); then
		if is_success "$st"; then
			log "ok: $CTX passes ($round, rep $rep: $st)"
		elif ungated; then
			soft_note "$CTX did not pass ($round, rep $rep: $st); recorded, and the gate leaves $WL out"
		else
			soft_fail "$CTX passes ($round, rep $rep: $st)"
		fi
	elif ungated; then
		soft_note "$CTX: no final status within 40 minutes ($round, rep $rep, $c); recorded, and the gate leaves $WL out"
	else
		soft_fail "$CTX: no final status within 40 minutes ($round, rep $rep, $c)"
	fi
	statuses "$c" >"$d/statuses.json" 2>/dev/null || echo '[]' >"$d/statuses.json"
	kept_log "$c" "$WL" >"$d/log.txt" || true
	jqr -n --arg w "$WL" --argjson rep "$rep" --arg round "$round" --arg sha "$c" --arg ctx "$CTX" --argjson tp "$tp" \
		'{workload: $w, rep: $rep, round: $round, sha: $sha, context: $ctx, push_epoch: $tp}' >"$d/meta.json"
	measure_s "status_${round}_$rep" "${st:-none}"
	# A slow pickup is a finding: keep the daemon's log for it.
	p=$(first_epoch "$c" "$CTX" pending)
	if [ -z "$p" ] || [ "$((p - tp))" -gt 10 ]; then
		soft_note "$CTX ($round, rep $rep) was picked up $((${p:-$tp} - tp)) s after the push; daemon log kept"
		tm_logs 3000 >"$d/daemon.log"
	fi
}

# reset_caches: the next cold run starts with no repo cache, and no job
# slots or podman images (the daemon's reset at start drops both).
#
# podman's namespace unit outlives the daemon and keeps the repo's container
# store bound, so it stops before the store goes, as in the daemon's own
# reset. Deleting the store under it made its podman spin about 24 s in
# storage's lock code and die of a stack overflow when the daemon's reset
# stopped it, and that wait was the next cold run's start latency (issue #39).
reset_caches() {
	sleep 5 # the ended job's cleanup
	timeout 120 sudo systemctl stop tentmaker
	timeout 60 sudo systemctl stop tentmaker-podns.service 2>/dev/null || true
	sudo find /var/cache/tentmaker/repos -mindepth 1 -delete 2>/dev/null || true
	timeout 120 sudo systemctl start tentmaker
	wait_service_up 60 || die "the service did not come back after the cache reset"
}

for rep in $(seq 1 "$REPS"); do
	br=${BRS[$((rep - 1))]}
	c=$(mk_commit "$BASE" "$WF" "rep $rep cold")
	sample cold "$rep" "$c" "$br"
	measure "repo_cache_bytes_$rep" "$(sudo du -sb /var/cache/tentmaker/repos 2>/dev/null | cut -f1 || echo 0)"
	c2=$(mk_commit "$c" "$WF" "rep $rep warm")
	sample warm "$rep" "$c2" "$br"
	if [ "$rep" -lt "$REPS" ]; then reset_caches; fi
done
measure "rate_left" "$(rate_left)"
# The daemon's own API call counts over the run (scripts/bench: API calls).
tm_status | jqr --arg at "$(date -u +%FT%TZ)" '(.broker.api.github // {}) + {at: $at}' >"$OUT/api.json" ||
	echo '{}' >"$OUT/api.json"
