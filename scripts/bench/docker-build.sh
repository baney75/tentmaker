#!/usr/bin/env bash
# docker-build.sh: the Docker workload. Builds test/bench/Dockerfile (a
# pinned two-stage build of tentmaker itself) from the repo root, runs the
# image once and removes it.
#
# GitHub's runners build with docker and BuildKit, and start every job with
# an empty layer cache. Under Tentmaker the job builds with its own rootless
# podman CLI (buildah): the docker CLI there goes through the runner's podman
# socket, which a checks-mode job could not reach in the first bench runs.
# Tentmaker keeps the repo's podman store (its container store) between
# jobs, bound to podman only when a job starts on an idle device; a job that
# starts while another runs builds in podman's default store instead.
#
# Under Tentmaker it records, before the build (phase.sh meta):
#
#   store_bound 1   the job got the repo's container store: the executor
#                   sets CONTAINERS_STORAGE_CONF only for a verified bind
#   cache_hit 1     the store is bound and holds the mark a passing earlier
#                   run left (the image $mark), so the build can reuse its
#                   layers
#
# and after a passing run it tags the image as $mark, which stays in the
# store. A bound store that an earlier run never filled is a miss, as an
# emptied Go cache is for hit.sh. The hosted runner keeps no layer cache, so
# it records neither.
#
# Both sides pull the digest-pinned base images (test/bench/Dockerfile) from
# the first mirror that serves them all, and record it as "registry". ECR
# Public alone refused GitHub's runners with "toomanyrequests: Data limit
# exceeded" (issue #39). The pulls are part of the timed build on both sides;
# the hosted runner still starts with no image or layer cache.
set -euo pipefail
tag=localhost/tm-bench:${GITHUB_SHA:-local}
mark=localhost/tm-bench-mark:latest
here=$(dirname "$0")
engine=docker
if [ "${TENTMAKER:-}" = 1 ]; then
	engine=podman
fi
"$engine" --version
if [ "$engine" = podman ]; then
	bound=0 hit=0
	if [ -n "${CONTAINERS_STORAGE_CONF:-}" ] && [ -f "$CONTAINERS_STORAGE_CONF" ]; then
		bound=1
		if podman image exists "$mark"; then hit=1; fi
	fi
	"$here/phase.sh" meta store_bound "$bound"
	"$here/phase.sh" meta cache_hit "$hit"
fi
bases=$(sed -n 's|^FROM \${BASE}/\([^ ]*\).*|\1|p' test/bench/Dockerfile)
[ -n "$bases" ] || { echo "docker-build.sh: no FROM \${BASE}/ line in test/bench/Dockerfile" >&2; exit 1; }
# Bound each pull where timeout exists (the Linux bench hosts); macOS, where
# only this script's tests run it against a fake engine, has none.
bounded() { if command -v timeout >/dev/null 2>&1; then timeout "$@"; else shift; "$@"; fi; }
base=
for b in mirror.gcr.io/library public.ecr.aws/docker/library; do
	ok=1
	for img in $bases; do
		if ! bounded 300 "$engine" pull -q "$b/$img" >/dev/null; then
			echo "docker-build.sh: could not pull $b/$img; trying the next mirror" >&2
			ok=0
			break
		fi
	done
	if [ "$ok" = 1 ]; then
		base=$b
		break
	fi
done
[ -n "$base" ] || { echo "docker-build.sh: no mirror served the base images" >&2; exit 1; }
"$here/phase.sh" meta registry "${base%%/*}"
# The base images are pulled by digest above, so the build needs no --pull.
"$engine" build --build-arg "BASE=$base" -f test/bench/Dockerfile -t "$tag" .
"$engine" run --rm "$tag" version
if [ "$engine" = podman ]; then
	podman tag "$tag" "$mark"
fi
"$engine" image rm "$tag" >/dev/null
