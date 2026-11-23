#!/bin/sh
# Tentmaker installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --channel dev
#   curl -fsSL .../install.sh | sh -s -- --yes -- --memory 4GiB --boost-memory 12GiB --repo owner/name
#   curl -fsSL .../install.sh | sh -s -- --yes -- --repo-here --pr   (from a checkout)
#
# Options:
#   --channel stable|dev   release channel (default stable; env TENTMAKER_CHANNEL)
#   --version X.Y.Z        a specific stable release (env TENTMAKER_VERSION)
#   --prefix DIR           install directory (default /usr/local/bin, or
#                          ~/.local/bin when that isn't writable without root)
#   --no-setup             only install the binary
#   --yes                  run `sudo tentmaker setup --yes` after installing,
#                          with every argument after `--` passed through
#   --json                 for agents: print exactly one JSON line on stdout,
#                          setup's result with --yes, else what was installed
#                          and the next step; a failure is {"ok":false,...}
#                          (also on when the setup arguments hold --json)
#   -h, --help             show this help
#
# The installer never prompts. It downloads the release archive and
# SHA256SUMS, refuses to install unless the sha256 matches, checks that the
# binary runs, then moves it into place atomically. When stdin is not a
# terminal (curl | sh), sudo runs with -n so it fails instead of waiting for
# a password; when there is a terminal on /dev/tty, sudo first asks for the
# password there (sudo -v), so a person can run the piped one-liner.
#
# Testing hooks: TENTMAKER_DOWNLOAD_BASE replaces
# https://github.com/baney75/tentmaker/releases (a URL or a local directory
# with the same layout); TENTMAKER_SUDO replaces the sudo command.
#
# Everything runs inside main, called on the last line, so a partial
# download from a pipe runs nothing.

set -eu

TM_REPO_URL="https://github.com/baney75/tentmaker"

say() { printf 'tentmaker-install: %s\n' "$*" >&2; }

# tm_json: print one JSON line on stdout. tm_setup_ran: setup printed it.
tm_json=0
tm_setup_ran=0

# json_str TEXT: TEXT as a JSON string.
json_str() {
	printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr '\n\t\r' '   ')"
}

# die MESSAGE [FIX]. The JSON line goes to fd 3, main's stdout, so a die
# inside $(...) still reaches the caller.
die() {
	printf 'tentmaker-install: error: %s\n' "$1" >&2
	tm_fix=
	if [ $# -gt 1 ] && [ -n "$2" ]; then
		printf 'tentmaker-install: fix: %s\n' "$2" >&2
		tm_fix=$2
	fi
	if [ "$tm_json" = 1 ] && [ "$tm_setup_ran" = 0 ]; then
		printf '{"ok":false,"error":%s,"fix":%s}\n' "$(json_str "$1")" "$(json_str "$tm_fix")" >&3
	fi
	exit 1
}

usage() {
	cat <<USAGE
Install Tentmaker on Linux or macOS.

usage: install.sh [--channel stable|dev] [--version X.Y.Z] [--prefix DIR]
                  [--no-setup] [--yes] [-- SETUP ARGS...]

  --channel stable|dev   release channel (default stable)
  --version X.Y.Z        a specific stable release
  --prefix DIR           install directory (default /usr/local/bin, or
                         ~/.local/bin without root)
  --no-setup             only install the binary
  --yes                  run 'sudo tentmaker setup --yes' with SETUP ARGS
  --json                 print exactly one JSON line on stdout (for agents)
  -h, --help             show this help

From a checkout of your repo, set up this machine and open a pull request
that moves CI onto it:
  curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh |
    sh -s -- --yes -- --repo-here --pr

Example for agents (one JSON line on stdout, with "ok" and "next"):
  curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh |
    sh -s -- --yes --json -- --repo-here --pr

Docs: $TM_REPO_URL
USAGE
}

have() { command -v "$1" >/dev/null 2>&1; }

detect_os() {
	case $(uname -s) in
		Linux) echo linux ;;
		Darwin) echo darwin ;;
		MINGW* | MSYS* | CYGWIN* | Windows_NT)
			die "this is the Linux and macOS installer" \
				"on Windows run in PowerShell: irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex" ;;
		*) die "unsupported OS $(uname -s)" "Tentmaker runs on Linux, macOS and Windows" ;;
	esac
}

detect_arch() {
	tm_arch=$(uname -m)
	# A shell under Rosetta reports x86_64 on Apple silicon; install the native build.
	if [ "$tm_arch" = x86_64 ] && [ "$(uname -s)" = Darwin ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
		tm_arch=arm64
	fi
	case $tm_arch in
		x86_64 | amd64) echo amd64 ;;
		aarch64 | arm64 | armv8*) echo arm64 ;;
		*) die "unsupported CPU $tm_arch" "Tentmaker ships amd64 and arm64 builds" ;;
	esac
}

# fetch URL_OR_PATH DEST
fetch() {
	case $1 in
		/*)
			[ -f "$1" ] || die "not found: $1" "check TENTMAKER_DOWNLOAD_BASE"
			cp "$1" "$2"
			return
			;;
	esac
	if have curl; then
		curl -fsSL --proto '=https,http' --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 300 -o "$2" "$1" ||
			die "download failed: $1" "check your network, or that the release exists at $TM_REPO_URL/releases"
	elif have wget; then
		wget -q --tries=3 --timeout=30 -O "$2" "$1" ||
			die "download failed: $1" "check your network, or that the release exists at $TM_REPO_URL/releases"
	else
		die "neither curl nor wget is installed" "install curl (apt install curl, dnf install curl, brew install curl) and rerun"
	fi
}

# sha256_of FILE
sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "can't verify the download: neither sha256sum nor shasum is installed" \
			"install coreutils (sha256sum) or perl (shasum) and rerun; Tentmaker never installs unverified"
	fi
}

# path_has DIR: is DIR on PATH?
path_has() {
	case ":$PATH:" in
		*":$1:"*) return 0 ;;
	esac
	return 1
}

# quote_args ARGS...: print the arguments shell-quoted, for display.
quote_args() {
	for tm_a in "$@"; do
		case $tm_a in
			*[!A-Za-z0-9_./:=@%+-]* | '') printf " '%s'" "$(printf '%s' "$tm_a" | sed "s/'/'\\\\''/g")" ;;
			*) printf ' %s' "$tm_a" ;;
		esac
	done
}

# run_setup CMD...: run setup, setting tm_rc. With --json, setup's line is
# captured and forwarded, and only a line that came back counts as its result.
run_setup() {
	tm_rc=0
	if [ "$tm_json" = 1 ]; then
		tm_out=$("$@") || tm_rc=$?
		if [ -n "$tm_out" ]; then
			tm_setup_ran=1
			printf '%s\n' "$tm_out"
		fi
	else
		"$@" || tm_rc=$?
	fi
}

main() {
	exec 3>&1
	channel=${TENTMAKER_CHANNEL:-stable}
	version=${TENTMAKER_VERSION:-}
	prefix=""
	run_setup=0
	no_setup=0
	while [ $# -gt 0 ]; do
		case $1 in
			--channel)
				[ $# -ge 2 ] || die "--channel needs a value" "use --channel stable or --channel dev"
				channel=$2
				shift 2
				;;
			--channel=*) channel=${1#*=}; shift ;;
			--version)
				[ $# -ge 2 ] || die "--version needs a value" "use --version 1.2.3"
				version=$2
				shift 2
				;;
			--version=*) version=${1#*=}; shift ;;
			--prefix)
				[ $# -ge 2 ] || die "--prefix needs a directory" "use --prefix \$HOME/.local/bin"
				prefix=$2
				shift 2
				;;
			--prefix=*) prefix=${1#*=}; shift ;;
			--no-setup) no_setup=1; shift ;;
			--yes | -y) run_setup=1; shift ;;
			--json) tm_json=1; shift ;;
			-h | --help) usage; exit 0 ;;
			--) shift; break ;;
			*) die "unknown option $1" "run with --help; arguments for setup go after --, like: sh -s -- --yes -- --memory 4GiB" ;;
		esac
	done
	# "$@" now holds the arguments for `tentmaker setup`.
	tm_has_json=0
	for tm_a in "$@"; do
		case $tm_a in --json) tm_has_json=1 ;; esac
	done
	if [ "$tm_has_json" = 1 ]; then
		tm_json=1
	elif [ "$tm_json" = 1 ]; then
		set -- --json "$@"
	fi

	case $channel in
		stable | dev) ;;
		*) die "unknown channel '$channel'" "use --channel stable or --channel dev (there are no other channels)" ;;
	esac
	if [ -n "$version" ]; then
		version=${version#v}
		[ "$channel" = stable ] || die "--version picks a stable release, but --channel is $channel" "drop --channel dev, or drop --version for the newest dev build"
		printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
			die "'$version' is not a release version" "use --version X.Y.Z, like --version 0.1.0"
	fi

	os=$(detect_os)
	arch=$(detect_arch)
	asset="tentmaker_${os}_${arch}.tar.gz"
	base=${TENTMAKER_DOWNLOAD_BASE:-$TM_REPO_URL/releases}
	base=${base%/}
	if [ -n "$version" ]; then
		dl="$base/download/v$version"
		what="v$version"
	elif [ "$channel" = dev ]; then
		dl="$base/download/dev"
		what="the dev channel"
	else
		dl="$base/latest/download"
		what="the latest stable release"
	fi

	# Pick the install directory.
	is_root=0
	[ "$(id -u)" = 0 ] && is_root=1
	if [ -z "$prefix" ]; then
		prefix=/usr/local/bin
		if [ "$is_root" = 0 ] && ! { [ -d "$prefix" ] && [ -w "$prefix" ]; }; then
			[ -n "${HOME:-}" ] || die "can't write $prefix and HOME is not set" "rerun as root, or pass --prefix DIR"
			prefix="$HOME/.local/bin"
			say "$prefix (no root, so not /usr/local/bin; run the installer with sudo for a system-wide install)"
		fi
	fi
	mkdir -p "$prefix" 2>/dev/null || die "can't create $prefix" "rerun with sudo, or pass --prefix to a directory you own"
	[ -w "$prefix" ] || die "can't write to $prefix" "rerun with sudo (curl -fsSL ... | sudo sh), or pass --prefix \$HOME/.local/bin"
	have tar || die "tar is not installed" "install tar and rerun"
	have gzip || die "gzip is not installed" "install gzip and rerun"

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/tentmaker-install.XXXXXX") || die "can't create a temp directory" "set TMPDIR to a writable directory"
	staged=""
	# shellcheck disable=SC2064 # expand now: these paths are fixed
	trap "rm -rf '$tmp'" EXIT
	trap 'exit 130' INT TERM

	say "downloading $asset from $what"
	fetch "$dl/SHA256SUMS" "$tmp/SHA256SUMS"
	fetch "$dl/$asset" "$tmp/$asset"

	want=$(awk -v n="$asset" '$2 == n || $2 == "*" n { print tolower($1); exit }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "SHA256SUMS has no entry for $asset" "the release may be incomplete; retry later or report it at $TM_REPO_URL/issues"
	got=$(sha256_of "$tmp/$asset")
	if [ "$got" != "$want" ]; then
		die "checksum mismatch for $asset: got $got, SHA256SUMS says $want; refusing to install" \
			"retry; if it repeats, report it at $TM_REPO_URL/issues (the download was changed or corrupted)"
	fi
	say "verified sha256 $got"

	mkdir "$tmp/x"
	tar -xzf "$tmp/$asset" -C "$tmp/x" tentmaker 2>/dev/null ||
		die "$asset has no tentmaker binary" "report it at $TM_REPO_URL/issues"
	if [ ! -f "$tmp/x/tentmaker" ] || [ -L "$tmp/x/tentmaker" ]; then
		die "$asset has no tentmaker binary" "report it at $TM_REPO_URL/issues"
	fi

	# Stage next to the target so the final mv is an atomic rename, and check
	# the binary runs before it replaces anything.
	staged="$prefix/.tentmaker.new.$$"
	# shellcheck disable=SC2064
	trap "rm -rf '$tmp'; rm -f '$staged'" EXIT
	cp "$tmp/x/tentmaker" "$staged"
	chmod 0755 "$staged"
	if ! installed_version=$("$staged" version 2>&1); then
		die "the downloaded binary doesn't run here: $installed_version" \
			"check that $prefix is not on a noexec filesystem, or pass --prefix to another directory"
	fi
	mv -f "$staged" "$prefix/tentmaker"
	bin="$prefix/tentmaker"
	say "installed $installed_version to $bin"
	path_has "$prefix" || say "$prefix is not on your PATH; add it, or run $bin by its full path"

	# Setup needs root. Use the full path: sudo's secure_path often lacks
	# /usr/local/bin and never has ~/.local/bin.
	if [ "$is_root" = 1 ]; then
		setup_cmd="$bin setup"
	else
		setup_cmd="sudo $bin setup"
	fi
	if [ "$run_setup" = 1 ] && [ "$no_setup" = 0 ]; then
		say "running: $setup_cmd --yes$(quote_args "$@")"
		# TENTMAKER_JSON_ONELINE: setup's JSON stays one line on a pty.
		if [ "$is_root" = 1 ]; then
			run_setup env TENTMAKER_JSON_ONELINE=1 "$bin" setup --yes "$@"
		else
			sudo_cmd=${TENTMAKER_SUDO:-sudo}
			have "$sudo_cmd" || die "setup needs root and sudo is not installed" "run as root: $bin setup --yes$(quote_args "$@")"
			# sudo resets the environment; keep the ssh-agent socket so
			# setup --pr can push over an SSH origin.
			agent=
			if [ -n "${SSH_AUTH_SOCK:-}" ]; then agent="SSH_AUTH_SOCK=$SSH_AUTH_SOCK"; fi
			# sudo_setup [-n] ARGS...: setup through sudo, with the agent
			# socket. Not ${agent:+env "$agent"}: zsh keeps that one word.
			sudo_setup() {
				tm_n=
				if [ "${1:-}" = -n ]; then tm_n=-n; shift; fi
				"$sudo_cmd" ${tm_n:+"$tm_n"} env TENTMAKER_JSON_ONELINE=1 ${agent:+"$agent"} "$bin" setup --yes "$@"
			}
			if [ -t 0 ]; then
				run_setup sudo_setup "$@"
			else
				# No terminal on stdin (curl | sh). A person at a terminal
				# types the sudo password on /dev/tty; an agent with no
				# terminal skips this and sudo -n never waits.
				# The probe runs in a subshell: a failed redirection on the
				# special built-in : would end a POSIX shell (dash) here.
				if (: </dev/tty) 2>/dev/null; then
					"$sudo_cmd" -v </dev/tty ||
						die "sudo did not accept the password" "run it yourself: $setup_cmd --yes$(quote_args "$@")"
				fi
				# sudo -n fails with no output when it needs a password.
				run_setup sudo_setup -n "$@"
			fi
		fi
		[ "$tm_rc" = 0 ] ||
			die "setup did not run or failed (exit $tm_rc); sudo may need a password" \
				"fix the error above, then run: $setup_cmd --yes$(quote_args "$@")"
		say "done"
	else
		say "next step: $setup_cmd$(quote_args "$@")"
		say "  (an agent can run: $setup_cmd --yes --json --memory 4GiB --boost-memory 12GiB --repo owner/name)"
		if [ "$tm_json" = 1 ]; then
			printf '{"ok":true,"installed":%s,"version":%s,"next":%s}\n' "$(json_str "$bin")" "$(json_str "$installed_version")" \
				"$(json_str "$setup_cmd --yes$(quote_args "$@")")"
		fi
	fi
}

main "$@"
