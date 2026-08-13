#!/bin/sh
#
# Yacht installer.
#
#   curl -sSL https://codeblocktz.github.io/yacht/install.sh | sudo sh
#
# Provisions K3s, cert-manager, Postgres, and Yacht itself as a systemd unit on
# a fresh Debian or Ubuntu box. Safe to re-run: it replaces the binary and the
# unit, leaves every generated secret alone, and never changes an existing
# cert-manager version.
#
# Flags (when piped, pass them after `sh -s --`):
#   --version vX.Y.Z   install a specific release rather than the latest
#   --port N           listen port, default 8080
#   --rotate-token     issue a new dashboard token instead of keeping the old one
#   --skip-k3s         do not install K3s; use the kubeconfig already present
#   --skip-cert-manager run without custom-domain certificate issuance
#   --acme-environment staging|production (staging by default)
#   --acme-email EMAIL required when production issuance is selected
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

# v1.21 is a current supported cert-manager release and is tested upstream on
# Kubernetes 1.33–1.36. Yacht pins the patch and the official static manifest's
# digest so a re-run cannot silently install different cluster-admin code.
CERT_MANAGER_VERSION="v1.21.1"
CERT_MANAGER_COMPATIBLE_MINOR="v1.21"
CERT_MANAGER_MANIFEST_SHA256="5f6a499b8c1857d57f560f536e0dcc830914b45c420899fe7ad0692c8624e408"
CERT_MANAGER_MANIFEST_URL="https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
CERT_MANAGER_ISSUER="yacht-acme"
KUBERNETES_COMPATIBLE_MIN_MINOR="33"
KUBERNETES_COMPATIBLE_MAX_MINOR="36"
K3S_INSTALL_CHANNEL="v1.36"
ACME_STAGING_SERVER="https://acme-staging-v02.api.letsencrypt.org/directory"
ACME_PRODUCTION_SERVER="https://acme-v02.api.letsencrypt.org/directory"

VERSION=""
PORT="8080"
ROTATE_TOKEN="no"
SKIP_K3S="no"
SKIP_CERT_MANAGER="no"
ACME_ENVIRONMENT="staging"
ACME_ENVIRONMENT_SET="no"
ACME_EMAIL=""
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

		Provisions K3s, cert-manager, Postgres, and Yacht as a systemd unit on
		Debian or Ubuntu. Safe to re-run: it replaces the binary and the unit,
		leaves generated secrets alone, and never changes an existing
		cert-manager version.

		Flags (when piped, pass them after 'sh -s --'):
		  --version vX.Y.Z    install a specific release rather than the latest
		  --port N            listen port, default 8080
		  --rotate-token      issue a new dashboard token instead of keeping it
		  --skip-k3s          use the kubeconfig already present
		  --skip-cert-manager run Yacht without custom-domain certificate issuance
		  --acme-environment  staging or production (default: staging)
		  --acme-email EMAIL  required with --acme-environment production
		  --database-url URL  use an existing Postgres instead of installing one
		  --help              print this and exit

		HTTP-01 certificate issuance requires public port 80 to reach the cluster.
		A cluster-wide HTTP-to-HTTPS redirect prevents ACME challenges from working.
	EOF
}

valid_acme_email() {
	case "$1" in
		*@*.*) ;;
		*) return 1 ;;
	esac
	case "$1" in
		*[!A-Za-z0-9._%+@-]*) return 1 ;;
	esac
	acme_local=${1%%@*}
	acme_domain=${1#*@}
	[ -n "$acme_local" ] && [ -n "$acme_domain" ] &&
		[ "$acme_domain" = "${acme_domain#*@}" ]
}

require_flag_value() {
	flag_name=$1
	flag_value=${2-}
	case "$flag_value" in
		''|--*) die "${flag_name} requires a value" ;;
	esac
}

parse_flags() {
	while [ $# -gt 0 ]; do
		case "$1" in
			--version)      require_flag_value "$1" "${2-}"; VERSION=$2; shift 2 ;;
			--port)         require_flag_value "$1" "${2-}"; PORT=$2; shift 2 ;;
			--database-url) require_flag_value "$1" "${2-}"; DATABASE_URL=$2; shift 2 ;;
			--rotate-token) ROTATE_TOKEN="yes"; shift ;;
			--skip-k3s)     SKIP_K3S="yes"; shift ;;
			--skip-cert-manager) SKIP_CERT_MANAGER="yes"; shift ;;
			--acme-environment)
				require_flag_value "$1" "${2-}"
				ACME_ENVIRONMENT=$2
				ACME_ENVIRONMENT_SET="yes"
				shift 2
				;;
			--acme-email) require_flag_value "$1" "${2-}"; ACME_EMAIL=$2; shift 2 ;;
			--help|-h)      usage; exit 0 ;;
			*) die "unknown flag '$1'" ;;
		esac
	done
	case "$PORT" in
		''|*[!0-9]*) die "--port must be a number, got '$PORT'" ;;
	esac
	case "$ACME_ENVIRONMENT" in
		staging|production) ;;
		*) die "--acme-environment must be 'staging' or 'production', got '$ACME_ENVIRONMENT'" ;;
	esac
	if [ -n "$ACME_EMAIL" ] && ! valid_acme_email "$ACME_EMAIL"; then
		die "--acme-email must be a single email address safe to write to the ClusterIssuer"
	fi
	if [ "$ACME_ENVIRONMENT" = "production" ] && [ -z "$ACME_EMAIL" ]; then
		die "--acme-email is required with --acme-environment production"
	fi
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
		curl -sfL https://get.k3s.io \
			| INSTALL_K3S_CHANNEL="$K3S_INSTALL_CHANNEL" \
				INSTALL_K3S_SKIP_SELINUX_RPM=true sh - \
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

# --------------------------------------------------------- cert-manager -----

kube() {
	if need_cmd k3s; then
		k3s kubectl --kubeconfig "$K3S_KUBECONFIG" "$@"
	else
		kubectl --kubeconfig "$K3S_KUBECONFIG" "$@"
	fi
}

kubernetes_version() {
	kubernetes_json=$(kube version -o json 2>/dev/null) || return 1
	kubernetes_server_version=$(printf '%s' "$kubernetes_json" | tr '\n' ' ' \
		| sed -n 's/.*"serverVersion"[[:space:]]*:[[:space:]]*{[^}]*"gitVersion"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
	[ -n "$kubernetes_server_version" ] || return 1
	printf '%s' "$kubernetes_server_version"
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

cert_manager_is_compatible() {
	case "$1" in
		"${CERT_MANAGER_COMPATIBLE_MINOR}".*) return 0 ;;
		*) return 1 ;;
	esac
}

cert_manager_footprint_exists() {
	if kube get crd certificates.cert-manager.io >/dev/null 2>&1; then
		return 0
	fi
	cm_footprint=$(kube get deployment -A -l app.kubernetes.io/instance=cert-manager \
		-o 'jsonpath={.items[0].metadata.name}' 2>/dev/null) || return 1
	[ -n "$cm_footprint" ]
}

render_cluster_issuer() {
	case "$ACME_ENVIRONMENT" in
		production) acme_server=$ACME_PRODUCTION_SERVER ;;
		*) acme_server=$ACME_STAGING_SERVER ;;
	esac
	cat <<-EOF
		apiVersion: cert-manager.io/v1
		kind: ClusterIssuer
		metadata:
		  name: ${CERT_MANAGER_ISSUER}
		spec:
		  acme:
	EOF
	if [ -n "$ACME_EMAIL" ]; then
		printf '    email: %s\n' "$ACME_EMAIL"
	fi
	cat <<-EOF
		    server: ${acme_server}
		    privateKeySecretRef:
		      name: yacht-acme-account
		    solvers:
		      - http01:
		          ingress:
		            ingressClassName: traefik
	EOF
}

cert_manager_diagnostics() {
	cm_diag_namespace=${CERT_MANAGER_NAMESPACE:-cert-manager}
	warn "inspect with: kubectl -n ${cm_diag_namespace} get pods; kubectl -n ${cm_diag_namespace} describe deployment cert-manager-webhook"
	kube -n "$cm_diag_namespace" get pods >&2 2>/dev/null || true
	kube -n "$cm_diag_namespace" get events --sort-by=.lastTimestamp >&2 2>/dev/null || true
}

wait_cert_manager_ready() {
	if ! kube wait --for=condition=Established \
		crd/certificates.cert-manager.io \
		crd/clusterissuers.cert-manager.io --timeout=120s >/dev/null 2>&1; then
		cert_manager_diagnostics
		return 1
	fi
	for cm_deployment in cert-manager cert-manager-cainjector cert-manager-webhook; do
		if ! kube -n "$CERT_MANAGER_NAMESPACE" rollout status "deployment/${cm_deployment}" \
			--timeout=120s >/dev/null 2>&1; then
			warn "cert-manager component ${cm_deployment} did not become ready"
			cert_manager_diagnostics
			return 1
		fi
	done
	cm_webhook_endpoint=$(kube -n "$CERT_MANAGER_NAMESPACE" get endpoints cert-manager-webhook \
		-o 'jsonpath={.subsets[0].addresses[0].ip}' 2>/dev/null) || true
	cm_webhook_ca=$(kube get validatingwebhookconfiguration cert-manager-webhook \
		-o 'jsonpath={.webhooks[0].clientConfig.caBundle}' 2>/dev/null) || true
	if [ -z "$cm_webhook_endpoint" ] || [ -z "$cm_webhook_ca" ]; then
		warn "cert-manager-webhook has no ready endpoint or injected CA bundle"
		cert_manager_diagnostics
		return 1
	fi
	# On a Yacht-managed cluster, prove admission answers before calling the
	# dependency ready. Operator-managed clusters stay strictly read-only and
	# use endpoint/CA observation only.
	if [ "$SKIP_K3S" = "no" ] &&
		! render_cluster_issuer | kube apply --dry-run=server -f - >/dev/null 2>&1; then
		warn "cert-manager-webhook rejected or did not answer a server-side dry-run"
		cert_manager_diagnostics
		return 1
	fi
}

fetch_cert_manager_manifest() {
	CERT_MANAGER_MANIFEST_DIR=$(mktemp -d)
	CERT_MANAGER_MANIFEST_FILE="${CERT_MANAGER_MANIFEST_DIR}/cert-manager.yaml"
	trap 'rm -rf "$CERT_MANAGER_MANIFEST_DIR"' EXIT INT TERM
	if ! curl -fsSL -o "$CERT_MANAGER_MANIFEST_FILE" "$CERT_MANAGER_MANIFEST_URL"; then
		warn "could not download pinned cert-manager manifest: ${CERT_MANAGER_MANIFEST_URL}"
		return 1
	fi
	cm_got=$(sha256sum "$CERT_MANAGER_MANIFEST_FILE" | cut -d' ' -f1)
	if [ "$cm_got" != "$CERT_MANAGER_MANIFEST_SHA256" ]; then
		warn "cert-manager manifest checksum mismatch: expected ${CERT_MANAGER_MANIFEST_SHA256}, got ${cm_got}"
		return 1
	fi
}

cert_manager_unavailable() {
	CERT_MANAGER_AVAILABLE="no"
	warn "custom-domain certificates are unavailable: $*"
	warn "Yacht will still run; platform wildcard TLS is unchanged"
}

cleanup_cert_manager_manifest() {
	if [ -n "${CERT_MANAGER_MANIFEST_DIR:-}" ]; then
		rm -rf "$CERT_MANAGER_MANIFEST_DIR"
	fi
	trap - EXIT INT TERM
}

fresh_dependency_cleanup_guidance() {
	if [ "${CERT_MANAGER_APPLIED_THIS_RUN:-no}" = "yes" ]; then
		warn "this exact run found no prior cert-manager, applied the pinned manifest, and left that fresh install partial"
		warn "after preserving diagnostics, roll back only this run's fresh partial install with: kubectl delete -f ${CERT_MANAGER_MANIFEST_URL}"
	fi
}

cert_manager_degraded() {
	cert_manager_unavailable "$*"
	fresh_dependency_cleanup_guidance
	return 0
}

requested_acme_server() {
	case "$ACME_ENVIRONMENT" in
		production) printf '%s' "$ACME_PRODUCTION_SERVER" ;;
		*) printf '%s' "$ACME_STAGING_SERVER" ;;
	esac
}

acme_environment_for_server() {
	case "$1" in
		"$ACME_STAGING_SERVER") printf '%s' staging ;;
		"$ACME_PRODUCTION_SERVER") printf '%s' production ;;
		*) printf '%s' custom ;;
	esac
}

cluster_issuer_server() {
	issuer_server=$(kube get clusterissuer "$CERT_MANAGER_ISSUER" \
		-o 'jsonpath={.spec.acme.server}' 2>/dev/null) || return 1
	[ -n "$issuer_server" ] || return 1
	printf '%s' "$issuer_server"
}

cluster_issuer_reference_count() {
	issuer_refs=$(kube get certificates.cert-manager.io -A \
		-o 'jsonpath={range .items[*]}{.spec.issuerRef.kind}{"|"}{.spec.issuerRef.name}{"\n"}{end}' \
		2>/dev/null) || return 1
	printf '%s\n' "$issuer_refs" | awk -F'|' -v issuer="$CERT_MANAGER_ISSUER" \
		'$1 == "ClusterIssuer" && $2 == issuer { count++ } END { print count + 0 }'
}

ensure_traefik_ingress_class() {
	if kube get ingressclass traefik >/dev/null 2>&1; then
		return 0
	fi
	cert_manager_unavailable "IngressClass/traefik is absent; HTTP-01 cannot route challenges — enable K3s Traefik or create a Traefik IngressClass, then re-run"
	return 1
}

wait_cluster_issuer_ready() {
	if kube wait --for=condition=Ready "clusterissuer/${CERT_MANAGER_ISSUER}" \
		--timeout=120s >/dev/null 2>&1; then
		return 0
	fi
	warn "ClusterIssuer ${CERT_MANAGER_ISSUER} is not Ready"
	warn "inspect with: kubectl describe clusterissuer ${CERT_MANAGER_ISSUER}"
	return 1
}

ensure_cluster_issuer() {
	requested_server=$(requested_acme_server)
	if kube get clusterissuer "$CERT_MANAGER_ISSUER" >/dev/null 2>&1; then
		live_server=$(cluster_issuer_server || printf '')
		if [ -z "$live_server" ]; then
			cert_manager_degraded "ClusterIssuer ${CERT_MANAGER_ISSUER} has no readable ACME server"
			return 0
		fi
		live_environment=$(acme_environment_for_server "$live_server")

		if [ "$SKIP_K3S" = "yes" ]; then
			if [ "$ACME_ENVIRONMENT_SET" = "yes" ] && [ "$requested_server" != "$live_server" ]; then
				cert_manager_unavailable "operator-managed cluster issuer currently uses ${live_environment}; requested ${ACME_ENVIRONMENT} was not applied because --skip-k3s is strictly read-only"
				warn "remediation: run 'kubectl get certificates -A', migrate any references to a separately named issuer, update ClusterIssuer/${CERT_MANAGER_ISSUER} with the cluster owner's tooling, then re-run without ACME mutation flags"
				return 0
			fi
			say "ClusterIssuer ${CERT_MANAGER_ISSUER} uses ${live_environment}; operator-managed configuration left unchanged"
		elif [ "$ACME_ENVIRONMENT_SET" = "yes" ]; then
			if [ "$requested_server" != "$live_server" ]; then
				ref_count=$(cluster_issuer_reference_count || printf 'unknown')
				if [ "$ref_count" = "unknown" ]; then
					warn "could not prove whether Certificates reference ClusterIssuer/${CERT_MANAGER_ISSUER}; refusing ACME environment mutation"
					warn "perform a controlled migration after: kubectl get certificates -A"
				elif [ "$ref_count" -gt 0 ]; then
					warn "refusing ACME environment mutation: ${ref_count} Certificate resource(s) reference ClusterIssuer/${CERT_MANAGER_ISSUER}"
					warn "perform a controlled migration/reissuance to a separately named issuer before changing environments"
				else
					step "Updating ClusterIssuer ${CERT_MANAGER_ISSUER} from ${live_environment} to ${ACME_ENVIRONMENT}"
					if ! render_cluster_issuer | kube apply -f - >/dev/null; then
						cert_manager_degraded "could not update ClusterIssuer ${CERT_MANAGER_ISSUER}; existing certificates were not claimed to have changed environments"
						return 0
					fi
					live_server=$requested_server
					live_environment=$ACME_ENVIRONMENT
				fi
			else
				step "Updating ClusterIssuer ${CERT_MANAGER_ISSUER} (${live_environment}) by explicit request"
				if ! render_cluster_issuer | kube apply -f - >/dev/null; then
					cert_manager_degraded "could not update ClusterIssuer ${CERT_MANAGER_ISSUER}"
					return 0
				fi
			fi
		elif [ -n "$ACME_EMAIL" ]; then
			warn "--acme-email alone does not rewrite an existing issuer; also select --acme-environment staging or production"
		fi
	else
		if [ "$SKIP_K3S" = "yes" ]; then
			cert_manager_unavailable "ClusterIssuer ${CERT_MANAGER_ISSUER} is absent in the operator-managed cluster; create it with the cluster owner's tooling, then re-run"
			return 0
		fi
		step "Creating ClusterIssuer ${CERT_MANAGER_ISSUER} (${ACME_ENVIRONMENT})"
		if ! render_cluster_issuer | kube apply -f - >/dev/null; then
			cert_manager_degraded "could not create ClusterIssuer ${CERT_MANAGER_ISSUER}"
			return 0
		fi
		live_server=$requested_server
		live_environment=$ACME_ENVIRONMENT
	fi

	if ! wait_cluster_issuer_ready; then
		cert_manager_degraded "ClusterIssuer ${CERT_MANAGER_ISSUER} readiness failed; Yacht installation continues"
		return 0
	fi
	# Re-read status after any mutation so the summary never claims an
	# environment the live issuer does not actually use.
	live_server=$(cluster_issuer_server || printf '')
	if [ -z "$live_server" ]; then
		cert_manager_degraded "ClusterIssuer ${CERT_MANAGER_ISSUER} became unreadable after readiness"
		return 0
	fi
	CERT_MANAGER_ACME_ENVIRONMENT=$(acme_environment_for_server "$live_server")
	CERT_MANAGER_AVAILABLE="yes"
	say "ClusterIssuer ${CERT_MANAGER_ISSUER} is ready (${CERT_MANAGER_ACME_ENVIRONMENT})"
}

reconcile_cert_manager() {
	CERT_MANAGER_AVAILABLE="no"
	CERT_MANAGER_APPLIED_THIS_RUN="no"
	CERT_MANAGER_NAMESPACE=""
	if [ "$SKIP_CERT_MANAGER" = "yes" ]; then
		step "Skipping cert-manager (--skip-cert-manager)"
		cert_manager_unavailable "the installer was explicitly told to skip cert-manager"
		return 0
	fi
	if ! need_cmd k3s && ! need_cmd kubectl; then
		if [ "$SKIP_K3S" = "yes" ]; then
			cert_manager_unavailable "kubectl is required to inspect the operator-managed cluster"
			return 0
		fi
		cert_manager_unavailable "kubectl is required to install or inspect cert-manager"
		return 0
	fi

	cm_kubernetes_version=$(kubernetes_version || printf '')
	if ! kubernetes_is_compatible "$cm_kubernetes_version"; then
		cert_manager_unavailable "a successful Kubernetes API serverVersion in the supported 1.${KUBERNETES_COMPATIBLE_MIN_MINOR}–1.${KUBERNETES_COMPATIBLE_MAX_MINOR} range is required, found '${cm_kubernetes_version:-unknown}'"
		return 0
	fi

	cm_existing_details=$(cert_manager_details || printf '')
	if [ -n "$cm_existing_details" ]; then
		CERT_MANAGER_NAMESPACE=${cm_existing_details%%|*}
		cm_existing_version=${cm_existing_details#*|}
		if ! cert_manager_is_compatible "$cm_existing_version"; then
			cert_manager_unavailable "existing cert-manager ${cm_existing_version:-unknown} in namespace ${CERT_MANAGER_NAMESPACE} is outside Yacht's compatible ${CERT_MANAGER_COMPATIBLE_MINOR} release line; Yacht will not upgrade or downgrade it"
			warn "upgrade cert-manager using its official instructions, then re-run this installer"
			return 0
		fi
		step "Checking existing cert-manager ${cm_existing_version} in ${CERT_MANAGER_NAMESPACE}"
		if ! wait_cert_manager_ready; then
			cert_manager_degraded "the pre-existing cert-manager installation is partial or unhealthy; Yacht did not mutate it and installation continues"
			return 0
		fi
		say "cert-manager ${cm_existing_version} in ${CERT_MANAGER_NAMESPACE} is ready"
	else
		if cert_manager_footprint_exists; then
			cert_manager_unavailable "a pre-existing partial cert-manager installation exists but its controller namespace/version cannot be identified; Yacht will not modify it"
			cert_manager_diagnostics
			return 0
		fi
		if [ "$SKIP_K3S" = "yes" ]; then
			cert_manager_unavailable "cert-manager is absent from the operator-managed cluster; install compatible ${CERT_MANAGER_COMPATIBLE_MINOR} and ClusterIssuer ${CERT_MANAGER_ISSUER} with the cluster owner's tooling"
			return 0
		fi

		step "Installing cert-manager ${CERT_MANAGER_VERSION}"
		if ! fetch_cert_manager_manifest; then
			cleanup_cert_manager_manifest
			cert_manager_unavailable "pinned cert-manager manifest download or checksum validation failed; no cluster apply was attempted"
			return 0
		fi
		CERT_MANAGER_APPLIED_THIS_RUN="yes"
		if ! kube apply -f "$CERT_MANAGER_MANIFEST_FILE" >/dev/null; then
			cleanup_cert_manager_manifest
			cert_manager_diagnostics
			cert_manager_degraded "applying cert-manager ${CERT_MANAGER_VERSION} failed; Yacht installation continues"
			return 0
		fi
		cleanup_cert_manager_manifest
		CERT_MANAGER_NAMESPACE="cert-manager"
		if ! wait_cert_manager_ready; then
			cert_manager_degraded "fresh cert-manager ${CERT_MANAGER_VERSION} did not become ready; Yacht installation continues"
			return 0
		fi
		cm_existing_version=$CERT_MANAGER_VERSION
		say "cert-manager ${CERT_MANAGER_VERSION} in ${CERT_MANAGER_NAMESPACE} is ready"
	fi

	if ! ensure_traefik_ingress_class; then
		return 0
	fi
	ensure_cluster_issuer
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
	if [ "${CERT_MANAGER_AVAILABLE:-no}" = "yes" ]; then
		say "Custom TLS cert-manager ${cm_existing_version:-$CERT_MANAGER_VERSION}, issuer ${CERT_MANAGER_ISSUER} (${CERT_MANAGER_ACME_ENVIRONMENT})"
		say "HTTP-01 needs public port 80; do not redirect all HTTP cluster-wide"
	else
		warn "custom-domain certificates are unavailable; platform wildcard TLS is unchanged"
	fi
}

# -------------------------------------------------------------------- main ---

main() {
	parse_flags "$@"
	require_root
	require_platform
	require_tools

	printf '\n\033[1mYacht installer\033[0m — %s/%s\n' "$(uname -s)" "$ARCH"

	resolve_version
	install_k3s
	reconcile_cert_manager
	# Dependency degradation is recorded before Yacht is replaced, but never
	# blocks the control plane or changes existing platform-wildcard TLS.
	install_binary
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
