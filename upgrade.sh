#!/bin/sh
#
# Yacht upgrader.
#
#   curl -sSL https://codeblocktz.github.io/yacht/upgrade.sh | sudo sh
#
# Replaces the binary and restarts the service. It performs a read-only
# cert-manager compatibility/readiness check, but does not touch K3s,
# cert-manager, Postgres, the environment file, or the unit — an upgrade that
# re-provisions the machine is an install, and this is deliberately the
# smaller, duller operation.
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

# The upgrader accepts the release line installed by install.sh but deliberately
# does not carry an installable patch version or manifest of its own.
CERT_MANAGER_COMPATIBLE_MINOR="v1.21"
CERT_MANAGER_ISSUER="yacht-acme"
KUBERNETES_COMPATIBLE_MIN_MINOR="33"
KUBERNETES_COMPATIBLE_MAX_MINOR="36"
ACME_STAGING_SERVER="https://acme-staging-v02.api.letsencrypt.org/directory"
ACME_PRODUCTION_SERVER="https://acme-v02.api.letsencrypt.org/directory"

VERSION=""

say()  { printf '  %s\n' "$*"; }
step() { printf '\n\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1; }

# Spelled out rather than read back from $0, which is "sh" when the script
# arrives on stdin — the exact case --help most needs to work in.
usage() {
	cat <<-EOF
		Yacht upgrader.

		  curl -sSL https://codeblocktz.github.io/yacht/upgrade.sh | sudo sh

		Replaces the binary and restarts the service. It reports cert-manager
		readiness without changing cluster resources, and does not touch K3s,
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
			--version)
				case "${2-}" in ''|--*) die "--version requires a value" ;; esac
				VERSION=$2
				shift 2
				;;
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

# --------------------------------------------------------- cert-manager -----

env_get() {
	v=$(sed -n "s/^$1=//p" "$ENV_FILE" | head -n1)
	[ -n "$v" ] || return 1
	printf '%s' "$v"
}

kube() {
	if need_cmd k3s; then
		k3s kubectl --kubeconfig "$CERT_MANAGER_KUBECONFIG" "$@"
	else
		kubectl --kubeconfig "$CERT_MANAGER_KUBECONFIG" "$@"
	fi
}

kubernetes_version() {
	kubernetes_json=$(kube version -o json 2>/dev/null) || return 1
	kubernetes_server_version=$(printf '%s' "$kubernetes_json" | tr '\n' ' ' \
		| sed -n 's/.*"serverVersion"[[:space:]]*:[[:space:]]*{[^}]*"gitVersion"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
	[ -n "$kubernetes_server_version" ] || return 1
	printf '%s' "$kubernetes_server_version"
}

cert_manager_details() {
	cm_details=$(kube get deployment -A -l app.kubernetes.io/name=cert-manager \
		-o 'jsonpath={range .items[*]}{.metadata.namespace}{"|"}{.metadata.labels.app\.kubernetes\.io/version}{"\n"}{end}' \
		2>/dev/null) || return 1
	cm_details=$(printf '%s\n' "$cm_details" | sed '/^$/d')
	[ -n "$cm_details" ] || return 1
	[ "$(printf '%s\n' "$cm_details" | wc -l | tr -d ' ')" = "1" ] || return 1
	case "$cm_details" in *'|'*) ;; *) return 1 ;; esac
	printf '%s' "$cm_details"
}

kubernetes_is_compatible() {
	case "$1" in
		v1.*.*) kubernetes_minor=${1#v1.}; kubernetes_minor=${kubernetes_minor%%.*} ;;
		*) return 1 ;;
	esac
	case "$kubernetes_minor" in *[!0-9]*|'') return 1 ;; esac
	[ "$kubernetes_minor" -ge "$KUBERNETES_COMPATIBLE_MIN_MINOR" ] &&
		[ "$kubernetes_minor" -le "$KUBERNETES_COMPATIBLE_MAX_MINOR" ]
}

acme_environment_for_server() {
	case "$1" in
		"$ACME_STAGING_SERVER") printf '%s' staging ;;
		"$ACME_PRODUCTION_SERVER") printf '%s' production ;;
		*) printf '%s' custom ;;
	esac
}

check_cert_manager() {
	CERT_MANAGER_AVAILABLE="no"
	CERT_MANAGER_KUBECONFIG=$(env_get YACHT_KUBECONFIG || printf '')
	if [ -z "$CERT_MANAGER_KUBECONFIG" ] || [ ! -r "$CERT_MANAGER_KUBECONFIG" ]; then
		warn "custom-domain certificates are unavailable: YACHT_KUBECONFIG is not readable; Yacht upgrade will continue"
		return 0
	fi
	if ! need_cmd k3s && ! need_cmd kubectl; then
		warn "custom-domain certificates are unavailable: kubectl is not installed; Yacht upgrade will continue"
		return 0
	fi

	cm_kubernetes_version=$(kubernetes_version || printf '')
	if ! kubernetes_is_compatible "$cm_kubernetes_version"; then
		warn "custom-domain certificates are unavailable: a successful Kubernetes API serverVersion in the supported 1.${KUBERNETES_COMPATIBLE_MIN_MINOR}–1.${KUBERNETES_COMPATIBLE_MAX_MINOR} range is required, found '${cm_kubernetes_version:-unknown}'"
		warn "the Yacht binary upgrade will continue and platform wildcard TLS is unchanged"
		return 0
	fi

	cm_existing_details=$(cert_manager_details || printf '')
	if [ -z "$cm_existing_details" ]; then
		warn "custom-domain certificates are unavailable: cert-manager is absent"
		warn "the upgrader will not install it; re-run install.sh on a Yacht-managed K3s cluster, or install compatible ${CERT_MANAGER_COMPATIBLE_MINOR} and ClusterIssuer ${CERT_MANAGER_ISSUER} yourself"
		return 0
	fi
	CERT_MANAGER_NAMESPACE=${cm_existing_details%%|*}
	cm_existing_version=${cm_existing_details#*|}
	case "$cm_existing_version" in
		"${CERT_MANAGER_COMPATIBLE_MINOR}".*) ;;
		*)
			warn "custom-domain certificates are unavailable: existing cert-manager ${cm_existing_version:-unknown} in namespace ${CERT_MANAGER_NAMESPACE} is outside the compatible ${CERT_MANAGER_COMPATIBLE_MINOR} line"
			warn "the upgrader will not change it; follow cert-manager's official upgrade instructions, then re-run"
			return 0
			;;
	esac

	for cm_deployment in cert-manager cert-manager-cainjector cert-manager-webhook; do
		if ! kube -n "$CERT_MANAGER_NAMESPACE" rollout status "deployment/${cm_deployment}" \
			--timeout=15s >/dev/null 2>&1; then
			warn "custom-domain certificates are unavailable: ${cm_deployment} is not ready"
			warn "inspect with: kubectl -n ${CERT_MANAGER_NAMESPACE} get pods; Yacht upgrade will continue"
			return 0
		fi
	done
	cm_webhook_endpoint=$(kube -n "$CERT_MANAGER_NAMESPACE" get endpoints cert-manager-webhook \
		-o 'jsonpath={.subsets[0].addresses[0].ip}' 2>/dev/null) || true
	cm_webhook_ca=$(kube get validatingwebhookconfiguration cert-manager-webhook \
		-o 'jsonpath={.webhooks[0].clientConfig.caBundle}' 2>/dev/null) || true
	if [ -z "$cm_webhook_endpoint" ] || [ -z "$cm_webhook_ca" ]; then
		warn "custom-domain certificates are unavailable: cert-manager-webhook has no ready endpoint or injected CA bundle"
		warn "inspect namespace ${CERT_MANAGER_NAMESPACE}; Yacht upgrade will continue"
		return 0
	fi
	if ! kube get ingressclass traefik >/dev/null 2>&1; then
		warn "custom-domain certificates are unavailable: IngressClass/traefik is absent, so HTTP-01 cannot route challenges"
		warn "enable K3s Traefik or create a Traefik IngressClass; Yacht upgrade will continue"
		return 0
	fi
	if ! kube get clusterissuer "$CERT_MANAGER_ISSUER" >/dev/null 2>&1; then
		warn "custom-domain certificates are unavailable: ClusterIssuer ${CERT_MANAGER_ISSUER} is absent"
		warn "the upgrader is read-only; create the issuer or re-run install.sh on Yacht-managed K3s"
		return 0
	fi
	if ! kube wait --for=condition=Ready "clusterissuer/${CERT_MANAGER_ISSUER}" \
		--timeout=15s >/dev/null 2>&1; then
		warn "custom-domain certificates are unavailable: ClusterIssuer ${CERT_MANAGER_ISSUER} is not Ready"
		warn "inspect with: kubectl describe clusterissuer ${CERT_MANAGER_ISSUER}; Yacht upgrade will continue"
		return 0
	fi
	cm_issuer_server=$(kube get clusterissuer "$CERT_MANAGER_ISSUER" \
		-o 'jsonpath={.spec.acme.server}' 2>/dev/null) || true
	if [ -z "$cm_issuer_server" ]; then
		warn "custom-domain certificates are unavailable: ClusterIssuer ${CERT_MANAGER_ISSUER} has no readable ACME server"
		return 0
	fi
	CERT_MANAGER_AVAILABLE="yes"
	CERT_MANAGER_ACME_ENVIRONMENT=$(acme_environment_for_server "$cm_issuer_server")
	say "cert-manager ${cm_existing_version} in ${CERT_MANAGER_NAMESPACE} and ClusterIssuer ${CERT_MANAGER_ISSUER} are ready (${CERT_MANAGER_ACME_ENVIRONMENT}, left unchanged)"
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
	check_cert_manager

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
		if [ "${CERT_MANAGER_AVAILABLE:-no}" = "yes" ]; then
			say "custom-domain certificate dependency preserved and ready (${CERT_MANAGER_ACME_ENVIRONMENT})"
		else
			warn "custom-domain certificates remain unavailable; platform wildcard TLS was not changed"
		fi
		say "journalctl -u yacht -f"
		printf '\n'
	else
		rollback
	fi
}

main "$@"
