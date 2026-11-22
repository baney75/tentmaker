#!/usr/bin/env bash
# phase.sh: timing marks for the bench workloads (.github/workflows/bench-*.yml).
# The same steps run on GitHub's hosted runners and under Tentmaker, so the
# marks are taken the same way on both: the job's own clock, to the
# nanosecond, kept in $RUNNER_TEMP across steps.
#
#   phase.sh begin NAME          mark the start of a phase
#   phase.sh end NAME [BYTES]    mark its end (BYTES: data the phase moved)
#   phase.sh run NAME CMD...     begin NAME, run CMD, end NAME
#   phase.sh meta KEY VALUE      record a fact about the job (cache_hit 1, go go1.25.0)
#   phase.sh report              print every mark as a BENCH_PHASE line and every
#                                fact as a BENCH_META line
#
# The report step is the job's last, so its lines are in the log tail
# Tentmaker keeps. scripts/bench reads them back out of both logs.
set -euo pipefail
f=${RUNNER_TEMP:?RUNNER_TEMP is required}/bench-phases
meta=$RUNNER_TEMP/bench-meta
now() { date +%s.%N; }
case ${1:-} in
	begin) printf '%s begin %s\n' "${2:?name}" "$(now)" >>"$f" ;;
	end) printf '%s end %s %s\n' "${2:?name}" "$(now)" "${3:-0}" >>"$f" ;;
	run)
		name=${2:?name}
		shift 2
		printf '%s begin %s\n' "$name" "$(now)" >>"$f"
		"$@"
		printf '%s end %s 0\n' "$name" "$(now)" >>"$f"
		;;
	meta)
		key=${2:?key}
		val=${3:-}
		case $val in
			'' | *[[:space:]]*)
				echo "phase.sh: meta $key needs a value with no spaces, got '$val'" >&2
				exit 2
				;;
		esac
		printf '%s %s\n' "$key" "$val" >>"$meta"
		;;
	report)
		[ -s "$f" ] || { echo "phase.sh: no marks in $f" >&2; exit 1; }
		sed 's/^/BENCH_PHASE /' "$f"
		if [ -s "$meta" ]; then sed 's/^/BENCH_META /' "$meta"; fi
		;;
	*)
		echo "usage: phase.sh begin|end|run|meta|report ..." >&2
		exit 2
		;;
esac
