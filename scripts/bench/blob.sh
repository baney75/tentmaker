#!/usr/bin/env bash
# blob.sh: the cache workload's 256 MiB blob (.github/workflows/bench-cache.yml).
#
# On GitHub's runners actions/cache/restore and actions/cache/save move
# ~/bench-blob; these commands only check it. Under Tentmaker (TENTMAKER=1)
# the cache actions are dropped and the blob goes to and from the repo cache
# Tentmaker mounts at $XDG_CACHE_HOME, which is the colocated cache this
# workload measures.
#
#   blob.sh restore   Tentmaker: copy the cached blob into ~/bench-blob, if there is one
#   blob.sh hit       print the blob's size if ~/bench-blob holds a blob, else 0,
#                     and record cache_hit for the report
#   blob.sh verify    check a restored blob against its sha256 (outside the timed restore)
#   blob.sh make      write a new random blob and its sha256 when there is none
#   blob.sh save      Tentmaker: copy a new blob into the repo cache, synced to disk
set -euo pipefail
size=$((256 * 1024 * 1024))
dir=$HOME/bench-blob
state=${RUNNER_TEMP:?RUNNER_TEMP is required}/bench-blob-hit
tm() { [ "${TENTMAKER:-}" = 1 ]; }
store() {
	[ -n "${XDG_CACHE_HOME:-}" ] || { echo "blob.sh: XDG_CACHE_HOME is not set; Tentmaker gave this job no repo cache" >&2; exit 1; }
	printf '%s/bench-blob' "$XDG_CACHE_HOME"
}
case ${1:-} in
	restore)
		tm || exit 0
		s=$(store)
		if [ -s "$s/blob.bin" ] && [ -s "$s/blob.sha256" ]; then
			mkdir -p "$dir"
			dd if="$s/blob.bin" of="$dir/blob.bin" bs=4M status=none
			cp "$s/blob.sha256" "$dir/blob.sha256"
		fi
		;;
	hit)
		if [ -s "$dir/blob.bin" ] && [ -s "$dir/blob.sha256" ]; then
			echo 1 >"$state"
			"$(dirname "$0")/phase.sh" meta cache_hit 1
			stat -c %s "$dir/blob.bin"
		else
			echo 0 >"$state"
			"$(dirname "$0")/phase.sh" meta cache_hit 0
			echo 0
		fi
		;;
	verify)
		[ "$(cat "$state")" = 1 ] || exit 0
		(cd "$dir" && sha256sum -c --quiet blob.sha256) || { echo "blob.sh: the restored blob does not match its sha256" >&2; exit 1; }
		;;
	make)
		[ "$(cat "$state")" = 1 ] && exit 0
		mkdir -p "$dir"
		head -c "$size" /dev/urandom >"$dir/blob.bin"
		(cd "$dir" && sha256sum blob.bin >blob.sha256)
		;;
	save)
		[ "$(cat "$state")" = 1 ] && { echo 0; exit 0; }
		if tm; then
			s=$(store)
			mkdir -p "$s"
			dd if="$dir/blob.bin" of="$s/blob.bin" bs=4M conv=fsync status=none
			cp "$dir/blob.sha256" "$s/blob.sha256"
		fi
		stat -c %s "$dir/blob.bin"
		;;
	*)
		echo "usage: blob.sh restore|hit|verify|make|save" >&2
		exit 2
		;;
esac
