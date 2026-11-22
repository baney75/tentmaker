#!/usr/bin/env bash
# docker-build.sh: the Docker workload. Builds test/bench/Dockerfile (a
# pinned two-stage build of tentmaker itself) from the repo root, runs the
# image once and removes it.
#
# GitHub's runners build with docker and BuildKit. Under Tentmaker the job
# builds with its own rootless podman CLI (buildah): the docker CLI there
# goes through the runner's podman socket, which a checks-mode job could not
# reach in the first bench runs. Neither side keeps a layer cache between
# jobs; this number is the baseline for one.
set -euo pipefail
tag=localhost/tm-bench:${GITHUB_SHA:-local}
engine=docker
if [ "${TENTMAKER:-}" = 1 ]; then
	engine=podman
fi
"$engine" --version
"$engine" build --pull -f test/bench/Dockerfile -t "$tag" .
"$engine" run --rm "$tag" version
"$engine" image rm "$tag" >/dev/null
