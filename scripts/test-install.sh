#!/bin/sh
# Test scripts/install.sh against fake releases, with no network and no root.
#
#   sh scripts/test-install.sh
#
# Builds fake release trees (a shell script stands in for the tentmaker
# binary), points TENTMAKER_DOWNLOAD_BASE at them as local paths and over
# `python3 -m http.server` on a random port, and runs the installer under
# every POSIX shell present (sh, dash, bash, zsh). TENTMAKER_SUDO points at a
# fake sudo that records its arguments. Runs shellcheck when installed.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
installer="$here/install.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/tentmaker-test-install.XXXXXX")
server_pid=""
cleanup() {
	if [ -n "$server_pid" ]; then kill "$server_pid" 2>/dev/null || true; fi
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0
fail=0
ok() { pass=$((pass + 1)); printf 'ok   %s\n' "$*"; }
bad() { fail=$((fail + 1)); printf 'FAIL %s\n' "$*"; }

sha() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

# release DIR VERSION: one release's assets for linux and darwin, both archs.
release() {
	mkdir -p "$1"
	stage="$work/stage-$2"
	rm -rf "$stage"
	mkdir -p "$stage"
	cat >"$stage/tentmaker" <<EOF
#!/bin/sh
case "\$1" in
version) echo "tentmaker $2" ;;
setup) shift; printf '%s\n' "\$@" >"\$TM_SETUP_LOG" ;;
*) exit 2 ;;
esac
EOF
	chmod 0755 "$stage/tentmaker"
	echo "MIT" >"$stage/LICENSE"
	for os in linux darwin; do
		for arch in amd64 arm64; do
			(cd "$stage" && COPYFILE_DISABLE=1 tar -czf "$1/tentmaker_${os}_${arch}.tar.gz" tentmaker LICENSE)
		done
	done
	(cd "$1" && sha tentmaker_*.tar.gz >SHA256SUMS)
}

# tree ROOT: a fake GitHub releases layout.
tree() {
	release "$1/latest/download" 9.9.9
	release "$1/download/v1.0.0" 1.0.0
	release "$1/download/dev" 10.0.0-dev.202611210400.abc1234
}

good="$work/srv/good"
tree "$good"
# tampered: same SHA256SUMS, archives changed after signing.
tampered="$work/srv/tampered"
tree "$tampered"
for f in "$tampered"/latest/download/*.tar.gz "$tampered"/download/*/*.tar.gz; do
	printf 'x' >>"$f"
done
# missing: SHA256SUMS without our archives.
missing="$work/srv/missing"
tree "$missing"
for d in "$missing/latest/download" "$missing/download/dev" "$missing/download/v1.0.0"; do
	echo "0000000000000000000000000000000000000000000000000000000000000000  other.tar.gz" >"$d/SHA256SUMS"
done

fakesudo="$work/fakesudo"
cat >"$fakesudo" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >"$TM_SUDO_LOG"
if [ "$1" = -v ]; then exit 0; fi
if [ "$1" = -n ]; then shift; fi
exec "$@"
EOF
chmod 0755 "$fakesudo"

n=0
# run SHELL BASE ARGS...: run the installer with stdin closed (no TTY). Sets
# out (combined output) and rc. Uses a fresh prefix in $prefix.
run() {
	sh_=$1
	base=$2
	shift 2
	n=$((n + 1))
	prefix="$work/p$n/bin"
	export TM_SETUP_LOG="$work/setup$n.log" TM_SUDO_LOG="$work/sudo$n.log"
	rc=0
	out=$(SSH_AUTH_SOCK=${TM_AGENT:-} TENTMAKER_DOWNLOAD_BASE="$base" TENTMAKER_SUDO="$fakesudo" ${TM_WRAP:-} "$sh_" "$installer" --prefix "$prefix" "$@" </dev/null 2>&1) || rc=$?
}

# run_json SHELL BASE ARGS...: like run, but out is stdout alone (stderr is
# dropped), for the --json agent mode.
run_json() {
	sh_=$1
	base=$2
	shift 2
	n=$((n + 1))
	prefix="$work/p$n/bin"
	export TM_SETUP_LOG="$work/setup$n.log" TM_SUDO_LOG="$work/sudo$n.log"
	rc=0
	out=$(SSH_AUTH_SOCK=${TM_AGENT:-} TENTMAKER_DOWNLOAD_BASE="$base" TENTMAKER_SUDO="$fakesudo" "$sh_" "$installer" --prefix "$prefix" "$@" </dev/null 2>/dev/null) || rc=$?
}

# one_line TEXT: TEXT is exactly one line.
one_line() { [ -n "$1" ] && [ "$(printf '%s\n' "$1" | wc -l | tr -d ' ')" = 1 ]; }

have_setsid() { command -v setsid >/dev/null 2>&1 && setsid -w true </dev/null >/dev/null 2>&1; }

installed() { "$prefix/tentmaker" version 2>/dev/null || echo "(not installed)"; }

check_shell() {
	s=$1

	run "$s" "$good" --no-setup
	if [ $rc = 0 ] && [ "$(installed)" = "tentmaker 9.9.9" ]; then ok "$s: stable install"; else bad "$s: stable install rc=$rc $(installed): $out"; fi
	leftovers=$(find "$prefix" -name '.tentmaker.new.*' | wc -l | tr -d ' ')
	if [ "$leftovers" = 0 ]; then ok "$s: no staged files left"; else bad "$s: staged files left in $prefix"; fi
	case $out in *"next step: sudo $prefix/tentmaker setup"* | *"next step: $prefix/tentmaker setup"*) ok "$s: prints the next step" ;; *) bad "$s: no next step: $out" ;; esac

	run "$s" "$good" --version v1.0.0 --no-setup
	if [ $rc = 0 ] && [ "$(installed)" = "tentmaker 1.0.0" ]; then ok "$s: --version"; else bad "$s: --version rc=$rc $(installed): $out"; fi
	# Upgrade in place over an existing binary.
	TENTMAKER_DOWNLOAD_BASE="$good" "$s" "$installer" --prefix "$prefix" --no-setup </dev/null >/dev/null 2>&1 || true
	if [ "$(installed)" = "tentmaker 9.9.9" ]; then ok "$s: upgrade over an existing install"; else bad "$s: upgrade: $(installed)"; fi

	run "$s" "$good" --channel dev --yes -- --memory 4GiB --repo owner/name "two words"
	expected=$(printf '%s\n' --yes --memory 4GiB --repo owner/name "two words")
	if [ $rc = 0 ] && [ "$(installed)" = "tentmaker 10.0.0-dev.202611210400.abc1234" ] &&
		[ "$(cat "$TM_SETUP_LOG" 2>/dev/null)" = "$expected" ]; then
		ok "$s: dev channel with --yes and setup passthrough"
	else
		bad "$s: dev --yes rc=$rc $(installed) setup=[$(cat "$TM_SETUP_LOG" 2>/dev/null)]: $out"
	fi
	if [ "$(id -u)" != 0 ]; then
		case $(cat "$TM_SUDO_LOG" 2>/dev/null) in
			"-n $prefix/tentmaker setup --yes"*) ok "$s: sudo -n with no TTY" ;;
			*) bad "$s: sudo called as [$(cat "$TM_SUDO_LOG" 2>/dev/null)]" ;;
		esac
		# No controlling terminal at all (an agent or a CI job): the
		# /dev/tty probe must not end the script before setup runs.
		if have_setsid; then
			TM_WRAP="setsid -w" run "$s" "$good" --yes -- --repo owner/name
			if [ $rc = 0 ] && [ -s "$TM_SETUP_LOG" ]; then ok "$s: setup runs with no controlling terminal"; else bad "$s: no ctty rc=$rc setup=[$(cat "$TM_SETUP_LOG" 2>/dev/null)]: $out"; fi
		fi
		# The log joins sudo's arguments with spaces, so setup must also have
		# run: zsh once passed "env SSH_AUTH_SOCK=..." to sudo as one word.
		TM_AGENT=/tmp/agent.sock run "$s" "$good" --yes -- --repo owner/name
		case $rc:$(cat "$TM_SUDO_LOG" 2>/dev/null) in
			"0:-n env SSH_AUTH_SOCK=/tmp/agent.sock $prefix/tentmaker setup --yes"*)
				if [ -s "$TM_SETUP_LOG" ]; then ok "$s: keeps the ssh-agent socket through sudo"; else bad "$s: agent socket: setup did not run: $out"; fi ;;
			*) bad "$s: agent socket not passed: rc=$rc, sudo called as [$(cat "$TM_SUDO_LOG" 2>/dev/null)]: $out" ;;
		esac
	fi

	run "$s" "$good" --yes --no-setup
	if [ $rc = 0 ] && [ ! -f "$TM_SETUP_LOG" ]; then ok "$s: --no-setup skips setup"; else bad "$s: --no-setup ran setup: $out"; fi

	run "$s" "$tampered" --no-setup
	if [ $rc != 0 ] && [ ! -e "$prefix/tentmaker" ]; then
		case $out in *"checksum mismatch"*"fix:"*) ok "$s: sha mismatch refuses" ;; *) bad "$s: mismatch message: $out" ;; esac
	else
		bad "$s: tampered archive installed (rc=$rc): $out"
	fi

	run "$s" "$missing" --no-setup
	if [ $rc != 0 ] && [ ! -e "$prefix/tentmaker" ]; then ok "$s: missing checksum refuses"; else bad "$s: installed without a checksum: $out"; fi

	run "$s" "$good" --channel beta
	case "$rc:$out" in 0:*) bad "$s: accepted channel beta" ;; *"unknown channel"*) ok "$s: rejects unknown channel" ;; *) bad "$s: channel beta: $out" ;; esac

	run "$s" "$good" --channel dev --version 1.0.0
	if [ $rc != 0 ]; then ok "$s: rejects --version with dev"; else bad "$s: accepted --version with dev"; fi

	run "$s" "$good" --frobnicate
	if [ $rc != 0 ]; then ok "$s: rejects unknown flag"; else bad "$s: accepted unknown flag"; fi

	# --json: one JSON line on stdout, whatever happens.
	run_json "$s" "$good" --json --no-setup
	case "$rc:$out" in
		0:'{"ok":true,"installed":"'*'","next":"'*'setup --yes --json"}') if one_line "$out"; then ok "$s: --json prints the next step"; else bad "$s: --json lines: $out"; fi ;;
		*) bad "$s: --json --no-setup rc=$rc: $out" ;;
	esac
	run_json "$s" "$tampered" --json --no-setup
	case "$rc:$out" in
		0:*) bad "$s: --json tampered exited 0" ;;
		*'{"ok":false,"error":"checksum mismatch'*'"fix":"retry;'*) if one_line "$out"; then ok "$s: --json failure is one JSON line"; else bad "$s: --json failure lines: $out"; fi ;;
		*) bad "$s: --json tampered: $out" ;;
	esac
	if command -v python3 >/dev/null 2>&1; then
		if printf '%s' "$out" | python3 -c 'import json,sys; v=json.load(sys.stdin); sys.exit(0 if v["ok"] is False and v["fix"] else 1)'; then
			ok "$s: --json failure parses"
		else
			bad "$s: --json failure does not parse: $out"
		fi
	fi
	run_json "$s" "$good" --yes --json -- --repo owner/name
	if [ $rc = 0 ] && [ "$(cat "$TM_SETUP_LOG" 2>/dev/null)" = "$(printf '%s\n' --yes --json --repo owner/name)" ]; then
		ok "$s: --json passes --json to setup"
	else
		bad "$s: --json setup=[$(cat "$TM_SETUP_LOG" 2>/dev/null)] rc=$rc: $out"
	fi
	run_json "$s" "$good" --yes -- --json --repo owner/name
	if [ $rc = 0 ] && [ "$(cat "$TM_SETUP_LOG" 2>/dev/null)" = "$(printf '%s\n' --yes --json --repo owner/name)" ] && [ -z "$out" ]; then
		ok "$s: setup's own --json is not doubled"
	else
		bad "$s: setup --json setup=[$(cat "$TM_SETUP_LOG" 2>/dev/null)] rc=$rc: $out"
	fi

	# Piped, like curl | sh.
	n=$((n + 1))
	prefix="$work/p$n/bin"
	rc=0
	out=$(TENTMAKER_DOWNLOAD_BASE="$good" "$s" -s -- --prefix "$prefix" --no-setup <"$installer" 2>&1) || rc=$?
	if [ $rc = 0 ] && [ "$(installed)" = "tentmaker 9.9.9" ]; then ok "$s: piped install"; else bad "$s: piped rc=$rc: $out"; fi
}

shells=""
for s in sh dash bash zsh; do
	if command -v "$s" >/dev/null 2>&1; then shells="$shells $s"; fi
done
for s in $shells; do
	check_shell "$s"
done

# Over HTTP, like the real thing.
if command -v python3 >/dev/null 2>&1 && command -v curl >/dev/null 2>&1; then
	port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
	python3 -m http.server --bind 127.0.0.1 --directory "$work/srv" "$port" >/dev/null 2>&1 &
	server_pid=$!
	i=0
	until curl -fsS --max-time 2 -o /dev/null "http://127.0.0.1:$port/good/latest/download/SHA256SUMS" 2>/dev/null; do
		i=$((i + 1))
		if [ $i -ge 50 ]; then bad "http server did not start"; break; fi
		sleep 0.2
	done
	run sh "http://127.0.0.1:$port/good" --no-setup
	if [ $rc = 0 ] && [ "$(installed)" = "tentmaker 9.9.9" ]; then ok "http: stable install"; else bad "http: install rc=$rc: $out"; fi
	run sh "http://127.0.0.1:$port/good" --channel dev --no-setup
	if [ $rc = 0 ] && [ "$(installed)" = "tentmaker 10.0.0-dev.202611210400.abc1234" ]; then ok "http: dev install"; else bad "http: dev rc=$rc: $out"; fi
	run sh "http://127.0.0.1:$port/tampered" --no-setup
	if [ $rc != 0 ] && [ ! -e "$prefix/tentmaker" ]; then ok "http: sha mismatch refuses"; else bad "http: tampered installed: $out"; fi
	run sh "http://127.0.0.1:$port/nothing-here" --no-setup
	if [ $rc != 0 ]; then ok "http: 404 fails cleanly"; else bad "http: 404 accepted"; fi
else
	printf 'skip http tests: need python3 and curl\n'
fi

if command -v shellcheck >/dev/null 2>&1; then
	if shellcheck "$here"/*.sh; then ok "shellcheck scripts/*.sh"; else bad "shellcheck scripts/*.sh"; fi
else
	printf 'skip shellcheck: not installed\n'
fi

printf '\n%d passed, %d failed (shells:%s)\n' "$pass" "$fail" "$shells"
[ "$fail" = 0 ]
