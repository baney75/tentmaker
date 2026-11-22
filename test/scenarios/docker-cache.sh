#!/usr/bin/env bash
# docker-cache: a repo's container store keeps docker build warm.
#
# Builds test/scenarios/docker/Dockerfile (two stages, base images pinned by
# digest) twice, in two check jobs of one repo on one device: the first on
# an empty store, the second after the first job ended and the device reset.
# The second must be at least 3x faster, pull nothing and reuse every layer.
# The build context is the same in both (only .tm-scn/ differs).
# shellcheck source=test/scenarios/lib.sh
. "$(dirname "$0")/lib.sh"
scn_begin docker-cache
[ "$OS" = linux ] || die "docker-cache runs on Linux only"
git_init
CTX=tentmaker/docker
BR=$(branch main)

scn_up
tm_repo_add "$BR"

# round SHA NAME: wait for the check, then print its TM-DOCKER line.
round() {
	local sha=$1 name=$2 st line
	st=$(wait_state "$sha" "$CTX" "success|failure|error" 900) || die "$name: no final status within 900 s"
	measure_s "${name}_status" "$st"
	line=$(kept_log "$sha" docker | grep -m1 '^TM-DOCKER ' || true)
	if ! is_success "$st" || [ -z "$line" ]; then
		kept_log "$sha" docker | tail -n 80 >&2 || true
		die "$name build failed ($st)"
	fi
	log "$name: $line"
	printf '%s\n' "$line"
}
field() { sed -n "s/.*[[:space:]]$1=\([0-9]*\).*/\1/p" <<<"$2"; }

cold_c=$(mk_commit "$BASE" docker.yml cold)
git_push "$cold_c:refs/heads/$BR"
cold=$(round "$cold_c" cold)

warm_c=$(mk_commit "$cold_c" docker.yml warm)
git_push "$warm_c:refs/heads/$BR"
warm=$(round "$warm_c" warm)

cold_ms=$(field build_ms "$cold")
warm_ms=$(field build_ms "$warm")
measure cold_build_ms "$cold_ms"
measure warm_build_ms "$warm_ms"
measure warm_cached_steps "$(field cached_steps "$warm")"
measure warm_pulls "$(field pulls "$warm")"
speedup=$(awk -v c="$cold_ms" -v w="$warm_ms" 'BEGIN { printf "%.1f", (w > 0 ? c / w : 0) }')
measure speedup "$speedup"
log "cold ${cold_ms} ms, warm ${warm_ms} ms, ${speedup}x"
check "the warm build is at least 3x faster (cold ${cold_ms} ms, warm ${warm_ms} ms, ${speedup}x)" test "$((warm_ms * 3))" -le "$cold_ms"
check "the warm build pulls nothing ($warm)" test "$(field pulls "$warm")" = 0
check "the warm build reuses its layers ($warm)" test "$(field cached_steps "$warm")" -ge 4
logs=$(tm_logs)
check "the device bound the repo's container store" grep -q 'container store bound' <<<"$logs"
