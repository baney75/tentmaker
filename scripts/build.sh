#!/bin/sh
# Build Tentmaker release archives for every supported OS and arch.
#
#   scripts/build.sh VERSION [OUTDIR]
#
# VERSION is X.Y.Z or X.Y.Z-dev.<stamp>.<sha7> (no leading v). OUTDIR
# defaults to dist/. Writes tentmaker_<os>_<arch>.tar.gz (.zip on Windows),
# each holding the binary, LICENSE and README.md, plus SHA256SUMS.
# Needs only the Go toolchain, tar, gzip and zip.
set -eu

main() {
	if [ $# -lt 1 ] || [ -z "$1" ]; then
		echo "usage: scripts/build.sh VERSION [OUTDIR]" >&2
		exit 2
	fi
	version=$1
	case $version in
		v*) echo "build.sh: give the version without a leading v (got $version)" >&2; exit 2 ;;
	esac
	if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
		echo "build.sh: $version is not a version like 1.2.3 or 1.2.3-dev.202611210400.abc1234" >&2
		exit 2
	fi

	root=$(cd "$(dirname "$0")/.." && pwd)
	out=${2:-$root/dist}
	mkdir -p "$out"
	out=$(cd "$out" && pwd)
	for tool in go tar gzip zip; do
		command -v "$tool" >/dev/null 2>&1 || { echo "build.sh: $tool is required" >&2; exit 1; }
	done

	stage=$(mktemp -d "${TMPDIR:-/tmp}/tentmaker-build.XXXXXX")
	trap 'rm -rf "$stage"' EXIT INT TERM

	cd "$root"
	ldflags="-s -w -X github.com/baney75/tentmaker/internal/cli.Version=$version"
	rm -f "$out"/tentmaker_*.tar.gz "$out"/tentmaker_*.zip "$out/SHA256SUMS"
	for os in linux darwin windows; do
		for arch in amd64 arm64; do
			name=tentmaker_${os}_${arch}
			dir=$stage/$name
			mkdir -p "$dir"
			bin=tentmaker
			[ "$os" = windows ] && bin=tentmaker.exe
			echo "building $name" >&2
			CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$dir/$bin" ./cmd/tentmaker
			cp LICENSE "$dir/"
			files="$bin LICENSE"
			if [ -f README.md ]; then
				cp README.md "$dir/"
				files="$files README.md"
			fi
			# Files sit at the archive's top level, owned by root.
			# shellcheck disable=SC2086 # $files is a fixed list of plain names
			if [ "$os" = windows ]; then
				(cd "$dir" && zip -q -X "$out/$name.zip" $files)
			else
				(cd "$dir" && tar_create "$out/$name.tar.gz" $files)
			fi
		done
	done

	cd "$out"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum tentmaker_*.tar.gz tentmaker_*.zip > SHA256SUMS
	else
		shasum -a 256 tentmaker_*.tar.gz tentmaker_*.zip > SHA256SUMS
	fi
	echo "wrote $(wc -l < SHA256SUMS | tr -d ' ') archives and SHA256SUMS to $out" >&2
}

# tar_create OUT FILES...: a gzip tar with numeric root ownership. GNU and
# BSD tar spell the owner flags differently.
tar_create() {
	dst=$1
	shift
	# COPYFILE_DISABLE stops macOS tar from adding ._ metadata files.
	if tar --version 2>/dev/null | grep -q 'GNU tar'; then
		tar --owner=0 --group=0 --numeric-owner -cf - "$@" | gzip -9n > "$dst"
	else
		COPYFILE_DISABLE=1 tar --uid 0 --gid 0 --uname root --gname root -cf - "$@" | gzip -9n > "$dst"
	fi
}

main "$@"
