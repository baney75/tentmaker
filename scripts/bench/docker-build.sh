#!/usr/bin/env bash
# docker-build.sh: the Docker workload. Builds test/bench/Dockerfile (a
# pinned two-stage build of tentmaker itself) from the repo root.
#
# GitHub's runners build with BuildKit. Under Tentmaker the docker CLI talks
# to rootless podman, which has no BuildKit endpoint, so the classic builder
# API is used (podman builds it with buildah). Tentmaker keeps no layer cache
# between jobs yet; this number is the baseline for that work.
set -euo pipefail
if [ "${TENTMAKER:-}" = 1 ]; then
	export DOCKER_BUILDKIT=0
fi
tag=tm-bench:${GITHUB_SHA:-local}
docker build --pull -f test/bench/Dockerfile -t "$tag" .
docker run --rm "$tag" version
docker image rm "$tag" >/dev/null
