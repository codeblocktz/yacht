#!/bin/sh
#
# Yacht installer.
#
#   curl -sSL https://codeblocktz.github.io/yacht/install.sh | sudo sh
#
# Provisions K3s, Postgres, and Yacht itself as a systemd unit on a fresh
# Debian or Ubuntu box. Safe to re-run: it replaces the binary and the unit and
# leaves every generated secret alone.
#
# Flags (when piped, pass them after `sh -s --`):
#   --version vX.Y.Z   install a specific release rather than the latest
#   --port N           listen port, default 8080
#   --rotate-token     issue a new dashboard token instead of keeping the old one
#   --skip-k3s         do not install K3s; use the kubeconfig already present
#   --database-url URL use an existing Postgres instead of installing one
#   --help             print this and exit

set -eu

REPO="codeblocktz/yacht"
INSTALL_DIR="/usr/local/bin"
CONF_DIR="/etc/yacht"
ENV_FILE="${CONF_DIR}/yacht.env"
K3S_KUBECONFIG="/etc/rancher/k3s/k3s.yaml"
KUBECONFIG_DST="${CONF_DIR}/kubeconfig"
UNIT_FILE="/etc/systemd/system/yacht.service"
SVC_USER="yacht"

VERSION=""
PORT="8080"
ROTATE_TOKEN="no"
SKIP_K3S="no"
DATABASE_URL=""

# ---------------------------------------------------------------- output ----

say()  { printf '  %s\n' "$*"; }
step() { printf '\n\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ----------------------------------------------------------------- checks ---

# require_root refuses rather than re-executing under sudo. The script arrives
# on stdin, so there is nothing to hand a second interpreter — re-reading it
# would mean a second download, and a second download is a second thing that
# could differ from the one that was audited.
require_root() {
	[ "$(id -u)" = "0" ] || die "must run as root — pipe to \`sudo sh\` rather than \`sh\`"
}

require_platform() {
	[ -r /etc/os-release ] || die "cannot read /etc/os-release — unsupported system"
	# shellcheck disable=SC1091
	. /etc/os-release
	case "${ID:-}:${ID_LIKE:-}" in
		debian*|ubuntu*|*:*debian*|*:*ubuntu*) ;;
		*) die "only Debian and Ubuntu are supported, found '${ID:-unknown}'" ;;
	esac

	case "$(uname -m)" in
		x86_64|amd64)  ARCH="amd64" ;;
		aarch64|arm64) ARCH="arm64" ;;
		*) die "only amd64 and arm64 are supported, found '$(uname -m)'" ;;
	esac

	[ -d /run/systemd/system ] || die "systemd is required and is not running"
}

need_cmd() { command -v "$1" >/dev/null 2>&1; }

require_tools() {
	need_cmd curl || die "curl is required"
	for c in tar sha256sum systemctl; do
		need_cmd "$c" || die "$c is required"
	done
}

# ------------------------------------------------------------------ flags ---

# Spelled out rather than read back from $0, which is "sh" when the script
# arrives on stdin — the exact case --help most needs to work in.
usage() {
	cat <<-EOF
		Yacht installer.

		  curl -sSL https://codeblocktz.github.io/yacht/install.sh | sudo sh

		Provisions K3s, Postgres, and Yacht as a systemd unit on Debian or
		Ubuntu. Safe to re-run: it replaces the binary and the unit and leaves
		every generated secret alone.

		Flags (when piped, pass them after 'sh -s --'):
		  --version vX.Y.Z    install a specific release rather than the latest
		  --port N            listen port, default 8080
		  --rotate-token      issue a new dashboard token instead of keeping it
		  --skip-k3s          use the kubeconfig already present
		  --database-url URL  use an existing Postgres instead of installing one
		  --help              print this and exit
	EOF
}

parse_flags() {
	while [ $# -gt 0 ]; do
		case "$1" in
			--version)      VERSION="${2:-}"; shift 2 ;;
			--port)         PORT="${2:-}"; shift 2 ;;
			--database-url) DATABASE_URL="${2:-}"; shift 2 ;;
			--rotate-token) ROTATE_TOKEN="yes"; shift ;;
			--skip-k3s)     SKIP_K3S="yes"; shift ;;
			--help|-h)      usage; exit 0 ;;
			*) die "unknown flag '$1'" ;;
		esac
	done
	case "$PORT" in
		''|*[!0-9]*) die "--port must be a number, got '$PORT'" ;;
	esac
}

# ----------------------------------------------------------------- secrets --

# env_get reads a value out of the existing environment file.
#
# Re-running the installer must never mint a new YACHT_SECRET_KEY: it seals
# every stored secret, and losing it loses them with no recovery path. Same for
# the database password, which is the only copy anybody has.
#
# The file below is rewritten from scratch on every run, so anything not read
# back here is dropped. That is why YACHT_SECRET_KEY_PREVIOUS is carried over
# too: an operator part-way through replacing a key has the old one in there by
# hand, and dropping it would take every value not yet rewritten with it.
env_get() {
	[ -f "$ENV_FILE" ] || return 1
	v=$(sed -n "s/^$1=//p" "$ENV_FILE" | head -n1)
	[ -n "$v" ] || return 1
	printf '%s' "$v"
}

rand_hex() { od -An -tx1 -N"${1:-24}" /dev/urandom | tr -d ' \n'; }
rand_b64() { head -c "${1:-32}" /dev/urandom | base64 | tr -d '\n'; }

# ---------------------------------------------------------------- release ---

resolve_version() {
	[ -n "$VERSION" ] && return 0
	step "Resolving the latest release"
	VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		| head -n1) || true
	[ -n "$VERSION" ] || die "could not resolve the latest release — pass --version vX.Y.Z"
	say "$VERSION"
}

install_binary() {
	step "Installing yacht ${VERSION} (${ARCH})"

	num="${VERSION#v}"
	tarball="yacht_${num}_linux_${ARCH}.tar.gz"
	base="https://github.com/${REPO}/releases/download/${VERSION}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	curl -fsSL -o "${tmp}/${tarball}" "${base}/${tarball}" \
		|| die "download failed: ${base}/${tarball}"
	curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" \
		|| die "download failed: ${base}/checksums.txt"

	# Verified before it is unpacked, not after it is installed. A checksum
	# checked once the binary is already in place is a report, not a gate.
	want=$(sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]]\{1,\}\*\{0,1\}${tarball}\$/\1/p" \
		"${tmp}/checksums.txt" | head -n1)
	[ -n "$want" ] || die "no checksum published for ${tarball}"
	got=$(sha256sum "${tmp}/${tarball}" | cut -d' ' -f1)
	[ "$want" = "$got" ] || die "checksum mismatch for ${tarball}: expected ${want}, got ${got}"
	say "checksum ok"

	tar -xzf "${tmp}/${tarball}" -C "$tmp" || die "could not unpack ${tarball}"
	[ -f "${tmp}/yacht" ] || die "${tarball} does not contain a yacht binary"

	install -m 0755 "${tmp}/yacht" "${INSTALL_DIR}/yacht.new"
	mv -f "${INSTALL_DIR}/yacht.new" "${INSTALL_DIR}/yacht"
	say "${INSTALL_DIR}/yacht"

	rm -rf "$tmp"
	trap - EXIT INT TERM
}

# -------------------------------------------------------------------- k3s ---

install_k3s() {
	if [ "$SKIP_K3S" = "yes" ]; then
		step "Skipping K3s (--skip-k3s)"
		[ -r "$K3S_KUBECONFIG" ] || die "--skip-k3s given but ${K3S_KUBECONFIG} is not readable"
		return 0
	fi

	if need_cmd k3s && systemctl is-active --quiet k3s; then
		step "K3s is already running"
	else
		step "Installing K3s"
		curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_SELINUX_RPM=true sh - \
			|| die "K3s install failed"
	fi

	step "Waiting for the node to be ready"
	i=0
	while [ "$i" -lt 60 ]; do
		if k3s kubectl wait --for=condition=Ready node --all --timeout=10s >/dev/null 2>&1; then
			say "ready"
			return 0
		fi
		i=$((i + 1))
		sleep 5
	done
	die "the K3s node did not become ready — check \`journalctl -u k3s\`"
}

# --------------------------------------------------------------- postgres ---

install_postgres() {
	if [ -n "$DATABASE_URL" ]; then
		step "Using the Postgres given on the command line"
		return 0
	fi

	# An existing DSN is reused verbatim. The role's password lives nowhere
	# else, so regenerating one would orphan the database the install is on.
	if existing=$(env_get YACHT_DATABASE_URL); then
		step "Keeping the existing database"
		DATABASE_URL="$existing"
		return 0
	fi

	step "Installing Postgres"
	if ! need_cmd psql; then
		DEBIAN_FRONTEND=noninteractive apt-get update -qq
		DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql \
			|| die "could not install postgresql"
	else
		say "already present"
	fi
	systemctl enable --now postgresql >/dev/null 2>&1 || true

	pw=$(rand_hex 24)
	role_exists=$(su - postgres -c \
		"psql -tAc \"SELECT 1 FROM pg_roles WHERE rolname='yacht'\"" 2>/dev/null || true)
	if [ "$role_exists" = "1" ]; then
		su - postgres -c "psql -qc \"ALTER ROLE yacht WITH LOGIN PASSWORD '${pw}'\"" >/dev/null \
			|| die "could not reset the yacht role password"
	else
		su - postgres -c "psql -qc \"CREATE ROLE yacht WITH LOGIN PASSWORD '${pw}'\"" >/dev/null \
			|| die "could not create the yacht role"
	fi

	db_exists=$(su - postgres -c \
		"psql -tAc \"SELECT 1 FROM pg_database WHERE datname='yacht'\"" 2>/dev/null || true)
	if [ "$db_exists" != "1" ]; then
		su - postgres -c "createdb -O yacht yacht" || die "could not create the yacht database"
	fi

	DATABASE_URL="postgres://yacht:${pw}@127.0.0.1:5432/yacht?sslmode=disable"
	say "database ready"
}

# ------------------------------------------------------------------ config --

# render_env prints the environment file the service reads.
#
# Split out from configure so the half that decides what survives a re-run can
# be exercised without provisioning a machine — it reads the old file and
# prints the new one, and touches nothing else. Rendering twice must give the
# same file: anything written here but not read back through env_get is a value
# the next run drops, and that is exactly how a retired key goes missing.
render_env() {
	secret_key=$(env_get YACHT_SECRET_KEY || rand_b64 32)
	secret_key_previous=$(env_get YACHT_SECRET_KEY_PREVIOUS || printf '')
	if [ "$ROTATE_TOKEN" = "yes" ]; then
		auth_token=$(rand_hex 24)
	else
		auth_token=$(env_get YACHT_AUTH_TOKEN || rand_hex 24)
	fi
	owner_email=$(env_get YACHT_OWNER_EMAIL || printf '')
	max_concurrent_builds=$(env_get YACHT_MAX_CONCURRENT_BUILDS || printf '2')

	cat <<-EOF
		# Written by the Yacht installer. Re-running preserves every value here
		# except the port and the binary; edit freely and restart the service.
		#
		# YACHT_SECRET_KEY seals stored secrets. There is no recovery path if it
		# is lost, so back this file up before you need it.
		#
		# To replace it: put the new key in YACHT_SECRET_KEY, move the old one to
		# YACHT_SECRET_KEY_PREVIOUS, restart. Both are kept across re-runs. Every
		# stored value keeps opening, and each moves to the new key the next time
		# it is written. Empty the previous key only once nothing still needs it.
		YACHT_DATABASE_URL=${DATABASE_URL}
		YACHT_KUBECONFIG=${KUBECONFIG_DST}
		YACHT_ADDR=:${PORT}
		YACHT_AUTH_TOKEN=${auth_token}
		YACHT_SECRET_KEY=${secret_key}
		YACHT_SECRET_KEY_PREVIOUS=${secret_key_previous}
		YACHT_OWNER_EMAIL=${owner_email}
		YACHT_MAX_CONCURRENT_BUILDS=${max_concurrent_builds}

		# Set YACHT_BASE_URL to a public https URL to turn magic-link sign-in on,
		# and YACHT_APP_DOMAIN to the domain apps get a hostname under.
		#YACHT_BASE_URL=
		#YACHT_APP_DOMAIN=
	EOF
}

configure() {
	step "Writing ${ENV_FILE}"

	id -u "$SVC_USER" >/dev/null 2>&1 || \
		useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"

	mkdir -p "$CONF_DIR"
	chmod 0750 "$CONF_DIR"
	chown root:"$SVC_USER" "$CONF_DIR"

	# The k3s kubeconfig is root-only, and widening it would hand cluster-admin
	# to every local user. Copying gives the service its own 0600 copy instead.
	[ -r "$K3S_KUBECONFIG" ] || die "${K3S_KUBECONFIG} is not readable"
	install -o "$SVC_USER" -g "$SVC_USER" -m 0600 "$K3S_KUBECONFIG" "$KUBECONFIG_DST"

	# Redirected rather than piped, so the auth_token render_env settled on is
	# still set for summary() to print.
	umask 077
	render_env > "${ENV_FILE}.new"
	chown root:"$SVC_USER" "${ENV_FILE}.new"
	chmod 0640 "${ENV_FILE}.new"
	mv -f "${ENV_FILE}.new" "$ENV_FILE"

	AUTH_TOKEN="$auth_token"
	say "secrets preserved across re-runs"
}

install_unit() {
	step "Installing the systemd unit"
	cat > "$UNIT_FILE" <<-EOF
		[Unit]
		Description=Yacht — a self-hosted PaaS for Kubernetes
		Documentation=https://github.com/${REPO}
		After=network-online.target postgresql.service k3s.service
		Wants=network-online.target

		[Service]
		Type=simple
		User=${SVC_USER}
		Group=${SVC_USER}
		EnvironmentFile=${ENV_FILE}
		ExecStart=${INSTALL_DIR}/yacht
		Restart=always
		RestartSec=5

		NoNewPrivileges=yes
		PrivateTmp=yes
		ProtectSystem=full
		ProtectHome=yes
		ProtectControlGroups=yes
		ProtectKernelTunables=yes
		RestrictSUIDSGID=yes

		[Install]
		WantedBy=multi-user.target
	EOF
	systemctl daemon-reload
	systemctl enable yacht >/dev/null 2>&1
	systemctl restart yacht
	say "yacht.service started"
}

# ------------------------------------------------------------------ verify ---

wait_healthy() {
	step "Waiting for the dashboard"
	i=0
	while [ "$i" -lt 60 ]; do
		code=$(curl -fsS -o /dev/null -w '%{http_code}' \
			"http://127.0.0.1:${PORT}/healthz" 2>/dev/null || printf '000')
		if [ "$code" = "200" ]; then
			say "healthy"
			return 0
		fi
		# 503 is the documented answer when the process is up but the cluster is
		# not reachable yet, so it is worth continuing to wait on.
		if ! systemctl is-active --quiet yacht; then
			printf '\n'
			journalctl -u yacht -n 40 --no-pager >&2 || true
			die "yacht exited — see the log above"
		fi
		i=$((i + 1))
		sleep 2
	done
	printf '\n'
	journalctl -u yacht -n 40 --no-pager >&2 || true
	die "the dashboard did not become healthy within 2 minutes"
}

public_addr() {
	ip=$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)
	[ -n "$ip" ] || ip=$(hostname -I 2>/dev/null | awk '{print $1}')
	[ -n "$ip" ] || ip="<server-ip>"
	printf '%s' "$ip"
}

summary() {
	addr=$(public_addr)
	cat <<-EOF

		  Yacht ${VERSION} is running.

		    URL     http://${addr}:${PORT}
		    Token   ${AUTH_TOKEN}

		  Paste the token when the dashboard asks for it.

		    Config    ${ENV_FILE}
		    Service   systemctl status yacht
		    Logs      journalctl -u yacht -f

		  This dashboard is served over plain HTTP, so the token crosses the
		  network in the clear. Put it behind a domain and TLS before you rely
		  on it, or reach it over an SSH tunnel in the meantime:

		    ssh -L ${PORT}:127.0.0.1:${PORT} root@${addr}

		  Back up ${ENV_FILE}. YACHT_SECRET_KEY seals your stored secrets and
		  cannot be regenerated.

	EOF
}

# -------------------------------------------------------------------- main ---

main() {
	parse_flags "$@"
	require_root
	require_platform
	require_tools

	printf '\n\033[1mYacht installer\033[0m — %s/%s\n' "$(uname -s)" "$ARCH"

	resolve_version
	install_binary
	install_k3s
	install_postgres
	configure
	install_unit
	wait_healthy
	summary
}

# Called on the last line so a truncated download runs nothing. Everything
# above is a definition; a connection that drops midway leaves a shell that has
# learned some functions and provisioned no part of a machine.
main "$@"
