#!/usr/bin/env bash
# hit.sh DIR: did this job start with the cache an earlier run of the same
# workload left? DIR is a folder the cache holds (GOCACHE, npm's cache).
#
# It records cache_hit 1 when DIR holds the mark a passing earlier run left,
# else 0 (phase.sh meta), then leaves the mark. A workload runs it after its
# work, just before phase.sh report, so only a run that passed leaves a mark.
# The mark goes wherever the cache goes: actions/cache saves it with DIR on
# GitHub's runners, and DIR is in Tentmaker's repo cache under Tentmaker. A
# fresh VM or an emptied repo cache has no mark, so a warm run that silently
# ran cold says so.
set -euo pipefail
d=${1:?usage: hit.sh DIR}
m=$d/.tentmaker-bench-mark
here=$(dirname "$0")
if [ -e "$m" ]; then
	"$here/phase.sh" meta cache_hit 1
else
	"$here/phase.sh" meta cache_hit 0
fi
mkdir -p "$d"
: >"$m"
