#!/bin/sh
#
# Yacht upgrader.
#
#   curl -sSL https://codeblocktz.github.io/yacht/upgrade.sh | sudo sh
#
# Replaces the binary and restarts the service. It does not touch K3s, Postgres,
# the environment file, or the unit — an upgrade that re-provisions the machine
# is an install, and this is deliberately the smaller, duller operation.
#
# If the new version does not come up healthy, the previous binary is put back
# and the service restarted on it, so a bad release costs a restart rather than
# an outage.
#
# Flags (when piped, pass them after `sh -s --`):
#   --version vX.Y.Z   upgrade (or downgrade) to a specific release
#   --help             print this and exit

set -eu

REPO="codeblocktz/yacht"
INSTALL_DIR="/usr/local/bin"
BIN="${INSTALL_DIR}/yacht"
PREV="${INSTALL_DIR}/yacht.prev"
ENV_FILE="/etc/yacht/yacht.env"

VERSION=""

say()  { printf '  %s\n' "$*"; }
step() { printf '\n\033[1m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1; }

# Spelled out rather than read back from $0, which is "sh" when the script
# arrives on stdin — the exact case --help most needs to work in.
usage() {
	cat <<-EOF
		Yacht upgrader.

		  curl -sSL https://codeblocktz.github.io/yacht/upgrade.sh | sudo sh

		Replaces the binary and restarts the service. It does not touch K3s,
		Postgres, the environment file, or the unit. If the new version does not
		come up healthy the previous binary is put back, so a bad release costs
		a restart rather than an outage.

		Flags (when piped, pass them after 'sh -s --'):
		  --version vX.Y.Z    upgrade or downgrade to a specific release
		  --help              print this and exit
	EOF
}

parse_flags() {
	while [ $# -gt 0 ]; do
		case "$1" in
			--version) VERSION="${2:-}"; shift 2 ;;
			--help|-h) usage; exit 0 ;;
			*) die "unknown flag '$1'" ;;
		esac
	done
}

require_installed() {
	[ "$(id -u)" = "0" ] || die "must run as root — pipe to \`sudo sh\` rather than \`sh\`"
	[ -x "$BIN" ] || die "no yacht binary at ${BIN} — run the installer first"
	[ -f "$ENV_FILE" ] || die "no config at ${ENV_FILE} — run the installer first"
	systemctl list-unit-files yacht.service >/dev/null 2>&1 \
		|| die "yacht.service is not installed — run the installer first"
	for c in curl tar sha256sum systemctl; do
		need_cmd "$c" || die "$c is required"
	done

	case "$(uname -m)" in
		x86_64|amd64)  ARCH="amd64" ;;
		aarch64|arm64) ARCH="arm64" ;;
		*) die "only amd64 and arm64 are supported, found '$(uname -m)'" ;;
	esac
}

# port reads the listen port out of the config rather than assuming 8080, so an
# install that moved it is still health-checked on the port it actually serves.
port() {
	p=$(sed -n 's/^YACHT_ADDR=.*:\([0-9]\{1,\}\)$/\1/p' "$ENV_FILE" | head -n1)
	[ -n "$p" ] || p="8080"
	printf '%s' "$p"
}

# current_version asks the running service rather than the binary on disk. The
# binary takes no flags, and /healthz reports the version that is actually
# serving — which is the one an upgrade is moving away from.
#
# Not -f: /healthz answers 503 when the cluster is unreachable, and that
# response still carries the version.
current_version() {
	v=$(curl -sS --max-time 5 "http://127.0.0.1:$(port)/healthz" 2>/dev/null \
		| sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
	[ -n "$v" ] || v="unknown"
	printf '%s' "$v"
}

resolve_version() {
	[ -n "$VERSION" ] && return 0
	VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		| head -n1) || true
	[ -n "$VERSION" ] || die "could not resolve the latest release — pass --version vX.Y.Z"
}

fetch() {
	num="${VERSION#v}"
	tarball="yacht_${num}_linux_${ARCH}.tar.gz"
	base="https://github.com/${REPO}/releases/download/${VERSION}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	curl -fsSL -o "${tmp}/${tarball}" "${base}/${tarball}" \
		|| die "download failed: ${base}/${tarball}"
	curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" \
		|| die "download failed: ${base}/checksums.txt"

	want=$(sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]]\{1,\}\*\{0,1\}${tarball}\$/\1/p" \
		"${tmp}/checksums.txt" | head -n1)
	[ -n "$want" ] || die "no checksum published for ${tarball}"
	got=$(sha256sum "${tmp}/${tarball}" | cut -d' ' -f1)
	[ "$want" = "$got" ] || die "checksum mismatch for ${tarball}: expected ${want}, got ${got}"

	tar -xzf "${tmp}/${tarball}" -C "$tmp" || die "could not unpack ${tarball}"
	[ -f "${tmp}/yacht" ] || die "${tarball} does not contain a yacht binary"
	STAGED="${tmp}/yacht"
	TMPDIR_USED="$tmp"
	say "checksum ok"
}

healthy() {
	p=$(port)
	i=0
	while [ "$i" -lt 45 ]; do
		code=$(curl -fsS -o /dev/null -w '%{http_code}' \
			"http://127.0.0.1:${p}/healthz" 2>/dev/null || printf '000')
		[ "$code" = "200" ] && return 0
		systemctl is-active --quiet yacht || return 1
		i=$((i + 1))
		sleep 2
	done
	return 1
}

rollback() {
	printf '\033[33m==>\033[0m New version unhealthy — rolling back\n' >&2
	mv -f "$PREV" "$BIN"
	systemctl restart yacht
	if healthy; then
		printf '  restored %s\n' "$(current_version)" >&2
	else
		printf '  \033[31mrollback did not come up either\033[0m — check journalctl -u yacht\n' >&2
	fi
	exit 1
}

main() {
	parse_flags "$@"
	require_installed

	from=$(current_version)
	resolve_version

	step "Upgrading from ${from} to ${VERSION}"
	if [ "$from" = "$VERSION" ]; then
		say "already on ${VERSION} — reinstalling the same build"
	fi

	fetch

	# The outgoing binary is kept, not overwritten. It is the only copy that is
	# known to have worked on this machine.
	cp -f "$BIN" "$PREV"
	install -m 0755 "$STAGED" "${BIN}.new"
	mv -f "${BIN}.new" "$BIN"
	rm -rf "$TMPDIR_USED"
	trap - EXIT INT TERM

	step "Restarting"
	systemctl restart yacht

	if healthy; then
		rm -f "$PREV"
		step "Done"
		say "yacht is now ${VERSION}"
		say "journalctl -u yacht -f"
		printf '\n'
	else
		rollback
	fi
}

main "$@"
