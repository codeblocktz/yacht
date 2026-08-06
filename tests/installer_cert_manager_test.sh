#!/bin/sh

# Focused contract tests for the cluster dependency managed by install.sh and
# upgrade.sh. The shipped scripts are sourced only after their final entrypoint
# is removed, so these tests cannot provision the machine that runs them.

set -eu

ROOT=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
TMP_ROOT=$(mktemp -d)
trap 'rm -rf "$TMP_ROOT"' EXIT INT TERM

pass=0
fail=0

ok() {
	pass=$((pass + 1))
	printf 'ok %s - %s\n' "$pass" "$1"
}

not_ok() {
	fail=$((fail + 1))
	printf 'not ok %s - %s\n' "$((pass + fail))" "$1" >&2
}

assert_contains() {
	case "$1" in
		*"$2"*) return 0 ;;
	esac
	printf 'missing expected text: %s\n--- output ---\n%s\n' "$2" "$1" >&2
	return 1
}

sourceable() {
	src=$1
	dst=$2
	awk '/^main "\$@"$/ { found=1; exit } { print } END { if (!found) exit 1 }' \
		"$src" > "$dst"
}

INSTALL_LIB="$TMP_ROOT/install-lib.sh"
UPGRADE_LIB="$TMP_ROOT/upgrade-lib.sh"
sourceable "$ROOT/install.sh" "$INSTALL_LIB"
sourceable "$ROOT/upgrade.sh" "$UPGRADE_LIB"

test_production_gate() {
	err="$TMP_ROOT/production.err"
	if sh -c '. "$1"; parse_flags --acme-environment production' sh "$INSTALL_LIB" \
		2>"$err"; then
		not_ok 'production ACME without an email is rejected'
		return
	fi
	if assert_contains "$(cat "$err")" '--acme-email is required'; then
		ok 'production ACME without an email is rejected'
	else
		not_ok 'production ACME without an email is rejected'
	fi
}

test_unknown_acme_environment() {
	err="$TMP_ROOT/acme-environment.err"
	if sh -c '. "$1"; parse_flags --acme-environment https://example.test/directory' \
		sh "$INSTALL_LIB" 2>"$err"; then
		not_ok 'arbitrary ACME endpoints are rejected'
		return
	fi
	if assert_contains "$(cat "$err")" "must be 'staging' or 'production'"; then
		ok 'arbitrary ACME endpoints are rejected'
	else
		not_ok 'arbitrary ACME endpoints are rejected'
	fi
}

test_issuer_rendering() {
	staging=$(sh -c '. "$1"; render_cluster_issuer' sh "$INSTALL_LIB")
	production=$(sh -c \
		'. "$1"; ACME_ENVIRONMENT=production; ACME_EMAIL=ops@example.test; render_cluster_issuer' \
		sh "$INSTALL_LIB")

	if assert_contains "$staging" 'https://acme-staging-v02.api.letsencrypt.org/directory' &&
		assert_contains "$staging" 'name: yacht-acme' &&
		! assert_contains "$staging" 'email:' 2>/dev/null &&
		assert_contains "$production" 'https://acme-v02.api.letsencrypt.org/directory' &&
		assert_contains "$production" 'email: ops@example.test' &&
		assert_contains "$production" 'ingressClassName: traefik'; then
		ok 'ClusterIssuer rendering is staging-safe and production-explicit'
	else
		not_ok 'ClusterIssuer rendering is staging-safe and production-explicit'
	fi
}

test_ready_install_is_idempotent() {
	log="$TMP_ROOT/idempotent.log"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.33.4+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|$CERT_MANAGER_VERSION" ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") printf "%s" "$ACME_STAGING_SERVER" ;;
				"get clusterissuer yacht-acme"|"get ingressclass traefik") return 0 ;;
				*) return 0 ;;
			esac
		}
		fetch_cert_manager_manifest() { printf "downloaded\n" >> "$LOG"; }
		reconcile_cert_manager
	' sh "$INSTALL_LIB" "$log")

	commands=$(cat "$log")
	if ! assert_contains "$commands" 'get deployment -A' ||
		assert_contains "$commands" 'downloaded' 2>/dev/null ||
		assert_contains "$commands" 'apply -f' 2>/dev/null; then
		not_ok 'a ready pinned install does not download or reapply resources'
		return
	fi
	if assert_contains "$out" "cert-manager ${CERT_MANAGER_VERSION:-v1.21.1} in cert-manager is ready"; then
		ok 'a ready pinned install does not download or reapply resources'
	else
		not_ok 'a ready pinned install does not download or reapply resources'
	fi
}

test_partial_install_has_diagnostics() {
	err="$TMP_ROOT/partial.err"
	if sh -c '
		. "$1"
		CERT_MANAGER_NAMESPACE=cert-manager
		kube() {
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.33.4+k3s1\"}}" ;;
				"wait --for=condition=Established crd/certificates.cert-manager.io"*) return 0 ;;
				"-n cert-manager rollout status deployment/cert-manager "*) return 0 ;;
				"-n cert-manager rollout status deployment/cert-manager-cainjector "*) return 0 ;;
				"-n cert-manager rollout status deployment/cert-manager-webhook "*) return 1 ;;
				"-n cert-manager get pods"*) printf "%s\n" "cert-manager-webhook-0 0/1 CrashLoopBackOff" >&2; return 0 ;;
				"get apiservice v1.webhook.cert-manager.io"*) printf "%s" False ;;
				*) return 0 ;;
			esac
		}
		wait_cert_manager_ready
	' sh "$INSTALL_LIB" 2>"$err"; then
		not_ok 'a partial cert-manager install reports the failed component'
		return
	fi
	diagnostics=$(cat "$err")
	if assert_contains "$diagnostics" 'cert-manager-webhook' &&
		assert_contains "$diagnostics" 'kubectl -n cert-manager get pods' &&
		assert_contains "$diagnostics" 'did not become ready'; then
		ok 'a partial cert-manager install reports the failed component'
	else
		not_ok 'a partial cert-manager install reports the failed component'
	fi
}

test_skip_is_visible() {
	out=$(sh -c '. "$1"; SKIP_CERT_MANAGER=yes; reconcile_cert_manager' sh "$INSTALL_LIB" 2>&1)
	if assert_contains "$out" 'custom-domain certificates are unavailable' &&
		assert_contains "$out" '--skip-cert-manager'; then
		ok 'skipping cert-manager is visible and actionable'
	else
		not_ok 'skipping cert-manager is visible and actionable'
	fi
}

test_skip_k3s_never_installs() {
	log="$TMP_ROOT/skip-k3s.log"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		SKIP_K3S=yes
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.33.4+k3s1\"}}" ;;
				*) return 1 ;;
			esac
		}
		reconcile_cert_manager
	' sh "$INSTALL_LIB" "$log" 2>&1)
	commands=$(cat "$log")
	if assert_contains "$out" 'operator-managed cluster' &&
		assert_contains "$out" 'custom-domain certificates are unavailable' &&
		! assert_contains "$commands" 'apply' 2>/dev/null; then
		ok '--skip-k3s reports an absent dependency without changing the cluster'
	else
		not_ok '--skip-k3s reports an absent dependency without changing the cluster'
	fi
}

test_incompatible_existing_version_is_not_changed() {
	log="$TMP_ROOT/incompatible.log"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.33.4+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.19.7" ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
	' sh "$INSTALL_LIB" "$log" 2>&1)
	commands=$(cat "$log")
	if assert_contains "$out" 'outside Yacht' &&
		assert_contains "$out" 'will not upgrade or downgrade' &&
		! assert_contains "$commands" 'apply' 2>/dev/null; then
		ok 'an incompatible existing cert-manager is diagnosed, never changed'
	else
		not_ok 'an incompatible existing cert-manager is diagnosed, never changed'
	fi
}

test_fresh_managed_install_uses_pinned_manifest() {
	log="$TMP_ROOT/fresh.log"
	issuer_state="$TMP_ROOT/fresh-issuer"
	manifest_dir="$TMP_ROOT/fresh-manifest"
	mkdir -p "$manifest_dir"
	: > "$manifest_dir/cert-manager.yaml"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		ISSUER_STATE=$3
		FAKE_MANIFEST_DIR=$4
		need_cmd() { return 0; }
		fetch_cert_manager_manifest() {
			CERT_MANAGER_MANIFEST_DIR=$FAKE_MANIFEST_DIR
			CERT_MANAGER_MANIFEST_FILE=$FAKE_MANIFEST_DIR/cert-manager.yaml
		}
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.33.4+k3s1\"}}" ;;
				"get deployment -A "*|"get crd certificates.cert-manager.io") return 1 ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get ingressclass traefik") return 0 ;;
				"get clusterissuer yacht-acme") [ -f "$ISSUER_STATE" ] ;;
				"apply -f -") cat >/dev/null; : > "$ISSUER_STATE"; return 0 ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") printf "%s" "$ACME_STAGING_SERVER" ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
		printf "available=%s\n" "$CERT_MANAGER_AVAILABLE"
	' sh "$INSTALL_LIB" "$log" "$issuer_state" "$manifest_dir" 2>&1)
	commands=$(cat "$log")
	if assert_contains "$commands" "apply -f $manifest_dir/cert-manager.yaml" &&
		assert_contains "$commands" 'apply -f -' &&
		assert_contains "$out" "cert-manager v1.21.1 in cert-manager is ready" &&
		assert_contains "$out" 'available=yes'; then
		ok 'a fresh managed cluster installs the pinned manifest and issuer'
	else
		not_ok 'a fresh managed cluster installs the pinned manifest and issuer'
	fi
}

test_upgrade_check_is_non_mutating() {
	log="$TMP_ROOT/upgrade.log"
	env_file="$TMP_ROOT/upgrade.env"
	kubeconfig="$TMP_ROOT/kubeconfig"
	: > "$log"
	: > "$kubeconfig"
	printf 'YACHT_KUBECONFIG=%s\n' "$kubeconfig" > "$env_file"
	out=$(sh -c '
		. "$1"
		LOG=$2
		ENV_FILE=$3
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.33.4+k3s1\"}}" ;;
				"get deployment -A "*) return 1 ;;
				*) return 0 ;;
			esac
		}
		check_cert_manager
	' sh "$UPGRADE_LIB" "$log" "$env_file" 2>&1)

	commands=$(cat "$log")
	if assert_contains "$out" 'custom-domain certificates are unavailable' &&
		assert_contains "$out" 're-run install.sh' &&
		! assert_contains "$commands" 'apply' 2>/dev/null; then
		ok 'upgrade reports an absent dependency without mutating the cluster'
	else
		not_ok 'upgrade reports an absent dependency without mutating the cluster'
	fi
}

test_missing_flag_values_have_yacht_diagnostics() {
	for flag in --version --port --database-url --acme-environment --acme-email; do
		err="$TMP_ROOT/missing-$(printf '%s' "$flag" | tr -d -).err"
		if sh -c '. "$1"; parse_flags "$2"' sh "$INSTALL_LIB" "$flag" 2>"$err"; then
			not_ok "missing value for $flag has a Yacht diagnostic"
			return
		fi
		if ! assert_contains "$(cat "$err")" "error:" ||
			! assert_contains "$(cat "$err")" "$flag requires a value"; then
			not_ok "missing value for $flag has a Yacht diagnostic"
			return
		fi
	done
	err="$TMP_ROOT/missing-upgrade-version.err"
	if sh -c '. "$1"; parse_flags --version' sh "$UPGRADE_LIB" 2>"$err"; then
		not_ok 'missing two-argument flag values have Yacht diagnostics'
		return
	fi
	if assert_contains "$(cat "$err")" 'error:' &&
		assert_contains "$(cat "$err")" '--version requires a value'; then
		ok 'missing two-argument flag values have Yacht diagnostics'
	else
		not_ok 'missing two-argument flag values have Yacht diagnostics'
	fi
}

test_server_version_requires_server_object_and_api_success() {
	server=$(sh -c '
		. "$1"
		kube() { printf "%s\n" "{\"clientVersion\":{\"gitVersion\":\"v1.36.9\"},\"serverVersion\":{\"gitVersion\":\"v1.34.7+k3s1\"}}"; }
		kubernetes_version
	' sh "$INSTALL_LIB")
	if [ "$server" != 'v1.34.7+k3s1' ]; then
		not_ok 'Kubernetes compatibility uses serverVersion from a successful API call'
		return
	fi
	if sh -c '
		. "$1"
		kube() { printf "%s\n" "{\"clientVersion\":{\"gitVersion\":\"v1.36.9\"}}"; }
		kubernetes_version
	' sh "$INSTALL_LIB" >/dev/null 2>&1; then
		not_ok 'Kubernetes compatibility uses serverVersion from a successful API call'
		return
	fi
	if sh -c '
		. "$1"
		kube() { return 1; }
		kubernetes_version
	' sh "$INSTALL_LIB" >/dev/null 2>&1; then
		not_ok 'Kubernetes compatibility uses serverVersion from a successful API call'
		return
	fi
	ok 'Kubernetes compatibility uses serverVersion from a successful API call'
}

test_install_upgrade_compatibility_constants_agree() {
	install_values=$(sh -c '. "$1"; printf "%s|%s|%s" "$CERT_MANAGER_COMPATIBLE_MINOR" "$KUBERNETES_COMPATIBLE_MIN_MINOR" "$KUBERNETES_COMPATIBLE_MAX_MINOR"' sh "$INSTALL_LIB")
	upgrade_values=$(sh -c '. "$1"; printf "%s|%s|%s" "$CERT_MANAGER_COMPATIBLE_MINOR" "$KUBERNETES_COMPATIBLE_MIN_MINOR" "$KUBERNETES_COMPATIBLE_MAX_MINOR"' sh "$UPGRADE_LIB")
	if [ "$install_values" = "$upgrade_values" ] && [ "$install_values" = 'v1.21|33|36' ]; then
		ok 'install and upgrade compatibility constants agree'
	else
		not_ok 'install and upgrade compatibility constants agree'
	fi
}

test_fresh_k3s_is_pinned_to_reviewed_minor() {
	log="$TMP_ROOT/k3s-channel.log"
	: > "$log"
	sh -c '
		. "$1"
		LOG=$2
		need_cmd() { return 1; }
		curl() { printf "%s\n" installer; }
		sh() { cat >/dev/null; printf "channel=%s\n" "${INSTALL_K3S_CHANNEL:-}" >> "$LOG"; }
		k3s() { return 0; }
		sleep() { :; }
		install_k3s
	' sh "$INSTALL_LIB" "$log" >/dev/null
	if assert_contains "$(cat "$log")" 'channel=v1.36'; then
		ok 'fresh K3s installation is pinned to the reviewed v1.36 minor'
	else
		not_ok 'fresh K3s installation is pinned to the reviewed v1.36 minor'
	fi
}

test_manifest_fetch_uses_pinned_url_and_checksum() {
	fixture="$TMP_ROOT/pinned-manifest.yaml"
	printf '%s\n' 'apiVersion: v1' 'kind: List' 'items: []' > "$fixture"
	want=$(sha256sum "$fixture" | cut -d' ' -f1)
	log="$TMP_ROOT/pinned-fetch.log"
	: > "$log"
	sh -c '
		. "$1"
		FIXTURE=$2
		LOG=$3
		CERT_MANAGER_MANIFEST_SHA256=$4
		curl() {
			printf "%s\n" "$*" >> "$LOG"
			while [ "$#" -gt 0 ]; do
				case "$1" in -o) out=$2; shift 2 ;; *) shift ;; esac
			done
			cp "$FIXTURE" "$out"
		}
		fetch_cert_manager_manifest
		cmp "$FIXTURE" "$CERT_MANAGER_MANIFEST_FILE"
		rm -rf "$CERT_MANAGER_MANIFEST_DIR"
		trap - EXIT INT TERM
	' sh "$INSTALL_LIB" "$fixture" "$log" "$want"
	if assert_contains "$(cat "$log")" 'https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml'; then
		ok 'controlled manifest bytes pass only through the exact pinned URL and checksum'
	else
		not_ok 'controlled manifest bytes pass only through the exact pinned URL and checksum'
	fi
}

test_manifest_mismatch_degrades_before_apply() {
	fixture="$TMP_ROOT/tampered-manifest.yaml"
	printf '%s\n' 'tampered: true' > "$fixture"
	log="$TMP_ROOT/tampered.log"
	: > "$log"
	if out=$(sh -c '
		. "$1"
		FIXTURE=$2
		LOG=$3
		need_cmd() { return 0; }
		curl() {
			while [ "$#" -gt 0 ]; do
				case "$1" in -o) out=$2; shift 2 ;; *) shift ;; esac
			done
			cp "$FIXTURE" "$out"
		}
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) return 1 ;;
				"get crd certificates.cert-manager.io") return 1 ;;
				*) return 1 ;;
			esac
		}
		reconcile_cert_manager
		printf "available=%s\n" "$CERT_MANAGER_AVAILABLE"
	' sh "$INSTALL_LIB" "$fixture" "$log" 2>&1); then
		status=0
	else
		status=$?
	fi
	commands=$(cat "$log")
	if [ "$status" -eq 0 ] && assert_contains "$out" 'checksum mismatch' &&
		assert_contains "$out" 'available=no' &&
		! assert_contains "$commands" 'apply' 2>/dev/null; then
		ok 'manifest checksum mismatch degrades custom TLS before any apply'
	else
		not_ok 'manifest checksum mismatch degrades custom TLS before any apply'
	fi
}

test_nondefault_cert_manager_namespace_is_used() {
	log="$TMP_ROOT/nondefault-namespace.log"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-services|v1.21.1" ;;
				"-n cert-services get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get ingressclass traefik") return 0 ;;
				"get clusterissuer yacht-acme") return 0 ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") printf "%s" "$ACME_STAGING_SERVER" ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
	' sh "$INSTALL_LIB" "$log" 2>&1)
	if assert_contains "$(cat "$log")" '-n cert-services rollout status deployment/cert-manager-webhook' &&
		assert_contains "$out" 'cert-services'; then
		ok 'an existing ready cert-manager in a nondefault namespace is detected'
	else
		not_ok 'an existing ready cert-manager in a nondefault namespace is detected'
	fi
}

test_skip_k3s_ready_issuer_with_flags_is_read_only() {
	log="$TMP_ROOT/skip-ready.log"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		SKIP_K3S=yes
		ACME_ENVIRONMENT=production
		ACME_ENVIRONMENT_SET=yes
		ACME_EMAIL=ops@example.test
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.21.1" ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get ingressclass traefik"|"get clusterissuer yacht-acme") return 0 ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") printf "%s" "$ACME_STAGING_SERVER" ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
	' sh "$INSTALL_LIB" "$log" 2>&1)
	commands=$(cat "$log")
	if assert_contains "$out" 'operator-managed cluster' &&
		assert_contains "$out" 'currently uses staging' &&
		assert_contains "$out" 'requested production' &&
		! assert_contains "$commands" 'apply' 2>/dev/null; then
		ok '--skip-k3s remains read-only with an existing issuer and ACME flags'
	else
		not_ok '--skip-k3s remains read-only with an existing issuer and ACME flags'
	fi
}

test_missing_traefik_degrades_without_blocking() {
	log="$TMP_ROOT/missing-traefik.log"
	: > "$log"
	if out=$(sh -c '
		. "$1"
		LOG=$2
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.21.1" ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get ingressclass traefik") return 1 ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
		printf "available=%s\n" "$CERT_MANAGER_AVAILABLE"
	' sh "$INSTALL_LIB" "$log" 2>&1); then status=0; else status=$?; fi
	if [ "$status" -eq 0 ] && assert_contains "$out" 'IngressClass/traefik is absent' &&
		assert_contains "$out" 'available=no'; then
		ok 'missing Traefik makes custom TLS unavailable without blocking Yacht'
	else
		not_ok 'missing Traefik makes custom TLS unavailable without blocking Yacht'
	fi
}

test_preexisting_readiness_failure_has_no_delete_guidance() {
	if out=$(sh -c '
		. "$1"
		need_cmd() { return 0; }
		kube() {
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.21.1" ;;
				"wait --for=condition=Established "*) return 1 ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
		printf "available=%s\n" "$CERT_MANAGER_AVAILABLE"
	' sh "$INSTALL_LIB" 2>&1); then status=0; else status=$?; fi
	if [ "$status" -eq 0 ] && assert_contains "$out" 'available=no' &&
		! assert_contains "$out" 'kubectl delete -f' 2>/dev/null; then
		ok 'pre-existing dependency failure degrades without destructive guidance'
	else
		not_ok 'pre-existing dependency failure degrades without destructive guidance'
	fi
}

test_preexisting_issuer_failure_has_no_delete_guidance() {
	if out=$(sh -c '
		. "$1"
		need_cmd() { return 0; }
		kube() {
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.21.1" ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get ingressclass traefik"|"get clusterissuer yacht-acme") return 0 ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") printf "%s" "$ACME_STAGING_SERVER" ;;
				"wait --for=condition=Ready clusterissuer/yacht-acme "*) return 1 ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
		printf "available=%s\n" "$CERT_MANAGER_AVAILABLE"
	' sh "$INSTALL_LIB" 2>&1); then status=0; else status=$?; fi
	if [ "$status" -eq 0 ] && assert_contains "$out" 'ClusterIssuer yacht-acme readiness failed' &&
		assert_contains "$out" 'available=no' &&
		! assert_contains "$out" 'kubectl delete -f' 2>/dev/null; then
		ok 'pre-existing issuer readiness failure has no dependency deletion guidance'
	else
		not_ok 'pre-existing issuer readiness failure has no dependency deletion guidance'
	fi
}

test_fresh_partial_has_scoped_delete_guidance_and_continues() {
	manifest_dir="$TMP_ROOT/partial-manifest"
	mkdir -p "$manifest_dir"
	: > "$manifest_dir/cert-manager.yaml"
	if out=$(sh -c '
		. "$1"
		FAKE_MANIFEST_DIR=$2
		need_cmd() { return 0; }
		fetch_cert_manager_manifest() { CERT_MANAGER_MANIFEST_DIR=$FAKE_MANIFEST_DIR; CERT_MANAGER_MANIFEST_FILE=$FAKE_MANIFEST_DIR/cert-manager.yaml; }
		kube() {
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*|"get crd certificates.cert-manager.io") return 1 ;;
				"apply -f "*) return 0 ;;
				"wait --for=condition=Established "*) return 1 ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
		printf "available=%s\n" "$CERT_MANAGER_AVAILABLE"
	' sh "$INSTALL_LIB" "$manifest_dir" 2>&1); then status=0; else status=$?; fi
	if [ "$status" -eq 0 ] && assert_contains "$out" 'kubectl delete -f' &&
		assert_contains "$out" 'available=no'; then
		ok 'only a dependency freshly applied by this run gets scoped deletion guidance'
	else
		not_ok 'only a dependency freshly applied by this run gets scoped deletion guidance'
	fi
}

test_environment_change_with_references_is_refused() {
	log="$TMP_ROOT/env-referenced.log"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		ACME_ENVIRONMENT=production
		ACME_ENVIRONMENT_SET=yes
		ACME_EMAIL=ops@example.test
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.21.1" ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get ingressclass traefik"|"get clusterissuer yacht-acme") return 0 ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") printf "%s" "$ACME_STAGING_SERVER" ;;
				"get certificates.cert-manager.io -A "*) printf "%s\n" "ClusterIssuer|yacht-acme" ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
	' sh "$INSTALL_LIB" "$log" 2>&1)
	if assert_contains "$out" 'controlled migration' &&
		assert_contains "$out" '1 Certificate' &&
		! assert_contains "$(cat "$log")" 'apply -f -' 2>/dev/null; then
		ok 'ACME environment mutation is refused while Certificates reference the issuer'
	else
		not_ok 'ACME environment mutation is refused while Certificates reference the issuer'
	fi
}

test_environment_change_without_references_is_applied() {
	log="$TMP_ROOT/env-unreferenced.log"
	server_state="$TMP_ROOT/issuer-server"
	printf '%s' 'https://acme-staging-v02.api.letsencrypt.org/directory' > "$server_state"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		SERVER_STATE=$3
		ACME_ENVIRONMENT=production
		ACME_ENVIRONMENT_SET=yes
		ACME_EMAIL=ops@example.test
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.21.1" ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get ingressclass traefik"|"get clusterissuer yacht-acme") return 0 ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") cat "$SERVER_STATE" ;;
				"get certificates.cert-manager.io -A "*) return 0 ;;
				"apply -f -") cat >/dev/null; printf "%s" "$ACME_PRODUCTION_SERVER" > "$SERVER_STATE" ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
	' sh "$INSTALL_LIB" "$log" "$server_state" 2>&1)
	if assert_contains "$(cat "$log")" 'apply -f -' &&
		assert_contains "$out" 'production' &&
		[ "$(cat "$server_state")" = 'https://acme-v02.api.letsencrypt.org/directory' ]; then
		ok 'ACME environment mutation proceeds only with zero referencing Certificates'
	else
		not_ok 'ACME environment mutation proceeds only with zero referencing Certificates'
	fi
}

test_dependency_degradation_does_not_stop_yacht_install() {
	log="$TMP_ROOT/main-continues.log"
	: > "$log"
	sh -c '
		. "$1"
		LOG=$2
		parse_flags() { :; }
		require_root() { :; }
		require_platform() { ARCH=amd64; }
		require_tools() { :; }
		uname() { case "$1" in -s) printf Linux ;; *) printf x86_64 ;; esac; }
		resolve_version() { VERSION=v0.0.0-test; }
		install_k3s() { printf "%s\n" k3s >> "$LOG"; }
		reconcile_cert_manager() { CERT_MANAGER_AVAILABLE=no; printf "%s\n" dependency-degraded >> "$LOG"; return 0; }
		install_binary() { printf "%s\n" yacht-binary >> "$LOG"; }
		install_postgres() { printf "%s\n" postgres >> "$LOG"; }
		configure() { printf "%s\n" configure >> "$LOG"; }
		install_unit() { printf "%s\n" unit >> "$LOG"; }
		wait_healthy() { printf "%s\n" healthy >> "$LOG"; }
		summary() { printf "%s\n" summary >> "$LOG"; }
		main
	' sh "$INSTALL_LIB" "$log" >/dev/null
	sequence=$(tr '\n' ' ' < "$log")
	if assert_contains "$sequence" 'dependency-degraded yacht-binary postgres configure unit healthy summary'; then
		ok 'dependency degradation does not stop Yacht installation'
	else
		not_ok 'dependency degradation does not stop Yacht installation'
	fi
}

test_upgrade_reports_live_issuer_environment_read_only() {
	log="$TMP_ROOT/upgrade-live.log"
	env_file="$TMP_ROOT/upgrade-live.env"
	kubeconfig="$TMP_ROOT/upgrade-live.kubeconfig"
	: > "$log"
	: > "$kubeconfig"
	printf 'YACHT_KUBECONFIG=%s\n' "$kubeconfig" > "$env_file"
	out=$(sh -c '
		. "$1"
		LOG=$2
		ENV_FILE=$3
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"clientVersion\":{\"gitVersion\":\"v1.99.0\"},\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-services|v1.21.1" ;;
				"-n cert-services get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"get ingressclass traefik"|"get clusterissuer yacht-acme") return 0 ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") printf "%s" "https://acme-v02.api.letsencrypt.org/directory" ;;
				*) return 0 ;;
			esac
		}
		check_cert_manager
	' sh "$UPGRADE_LIB" "$log" "$env_file" 2>&1)
	if assert_contains "$out" 'cert-services' && assert_contains "$out" 'production' &&
		! assert_contains "$(cat "$log")" 'apply' 2>/dev/null; then
		ok 'upgrade reports the live issuer environment and remains read-only'
	else
		not_ok 'upgrade reports the live issuer environment and remains read-only'
	fi
}

test_shipped_supply_chain_constants_are_exact() {
	values=$(sh -c '. "$1"; printf "%s|%s|%s" "$CERT_MANAGER_VERSION" "$CERT_MANAGER_MANIFEST_URL" "$CERT_MANAGER_MANIFEST_SHA256"' sh "$INSTALL_LIB")
	want='v1.21.1|https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml|5f6a499b8c1857d57f560f536e0dcc830914b45c420899fe7ad0692c8624e408'
	if [ "$values" = "$want" ]; then
		ok 'shipped cert-manager version, official URL, and manifest SHA are exact'
	else
		not_ok 'shipped cert-manager version, official URL, and manifest SHA are exact'
	fi
}

test_install_upgrade_acme_mappings_agree() {
	install_values=$(sh -c '. "$1"; printf "%s|%s|%s|%s" "$ACME_STAGING_SERVER" "$ACME_PRODUCTION_SERVER" "$(acme_environment_for_server "$ACME_STAGING_SERVER")" "$(acme_environment_for_server "$ACME_PRODUCTION_SERVER")"' sh "$INSTALL_LIB")
	upgrade_values=$(sh -c '. "$1"; printf "%s|%s|%s|%s" "$ACME_STAGING_SERVER" "$ACME_PRODUCTION_SERVER" "$(acme_environment_for_server "$ACME_STAGING_SERVER")" "$(acme_environment_for_server "$ACME_PRODUCTION_SERVER")"' sh "$UPGRADE_LIB")
	want='https://acme-staging-v02.api.letsencrypt.org/directory|https://acme-v02.api.letsencrypt.org/directory|staging|production'
	if [ "$install_values" = "$want" ] && [ "$upgrade_values" = "$want" ]; then
		ok 'install and upgrade ACME URL mappings are identical'
	else
		not_ok 'install and upgrade ACME URL mappings are identical'
	fi
}

test_environment_change_reference_query_failure_is_read_only() {
	log="$TMP_ROOT/env-query-failure.log"
	: > "$log"
	out=$(sh -c '
		. "$1"
		LOG=$2
		ACME_ENVIRONMENT=production
		ACME_ENVIRONMENT_SET=yes
		ACME_EMAIL=ops@example.test
		need_cmd() { return 0; }
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.21.1" ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"apply --dry-run=server -f -") cat >/dev/null; return 0 ;;
				"get ingressclass traefik"|"get clusterissuer yacht-acme") return 0 ;;
				"get clusterissuer yacht-acme -o jsonpath={.spec.acme.server}") printf "%s" "$ACME_STAGING_SERVER" ;;
				"get certificates.cert-manager.io -A "*) return 1 ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
		printf "available=%s\n" "$CERT_MANAGER_AVAILABLE"
	' sh "$INSTALL_LIB" "$log" 2>&1)
	commands=$(cat "$log")
	if assert_contains "$out" 'could not prove whether Certificates reference' &&
		assert_contains "$out" 'ClusterIssuer yacht-acme is ready (staging)' &&
		assert_contains "$out" 'available=yes' &&
		! assert_contains "$commands" 'apply -f -' 2>/dev/null; then
		ok 'Certificate reference query failure refuses environment mutation and reports live issuer'
	else
		not_ok 'Certificate reference query failure refuses environment mutation and reports live issuer'
	fi
}

test_webhook_dry_run_is_managed_only_and_degrades_safely() {
	managed_log="$TMP_ROOT/dry-run-managed.log"
	skip_log="$TMP_ROOT/dry-run-skip.log"
	: > "$managed_log"
	: > "$skip_log"
	sh -c '
		. "$1"
		LOG=$2
		CERT_MANAGER_NAMESPACE=cert-manager
		SKIP_K3S=no
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"apply --dry-run=server -f -") cat >/dev/null; return 0 ;;
				*) return 0 ;;
			esac
		}
		wait_cert_manager_ready
	' sh "$INSTALL_LIB" "$managed_log"
	sh -c '
		. "$1"
		LOG=$2
		CERT_MANAGER_NAMESPACE=cert-manager
		SKIP_K3S=yes
		kube() {
			printf "%s\n" "$*" >> "$LOG"
			case "$*" in
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				*) return 0 ;;
			esac
		}
		wait_cert_manager_ready
	' sh "$INSTALL_LIB" "$skip_log"
	if degraded=$(sh -c '
		. "$1"
		need_cmd() { return 0; }
		kube() {
			case "$*" in
				"version -o json") printf "%s\n" "{\"serverVersion\":{\"gitVersion\":\"v1.36.3+k3s1\"}}" ;;
				"get deployment -A "*) printf "%s\n" "cert-manager|v1.21.1" ;;
				"-n cert-manager get endpoints cert-manager-webhook "*) printf "%s" 10.0.0.8 ;;
				"get validatingwebhookconfiguration cert-manager-webhook "*) printf "%s" Y2E= ;;
				"apply --dry-run=server -f -") cat >/dev/null; return 1 ;;
				*) return 0 ;;
			esac
		}
		reconcile_cert_manager
		printf "available=%s\n" "$CERT_MANAGER_AVAILABLE"
	' sh "$INSTALL_LIB" 2>&1); then status=0; else status=$?; fi
	if assert_contains "$(cat "$managed_log")" 'apply --dry-run=server -f -' &&
		! assert_contains "$(cat "$skip_log")" 'apply' 2>/dev/null &&
		[ "$status" -eq 0 ] && assert_contains "$degraded" 'server-side dry-run' &&
		assert_contains "$degraded" 'available=no'; then
		ok 'webhook dry-run occurs only on managed K3s and failure degrades safely'
	else
		not_ok 'webhook dry-run occurs only on managed K3s and failure degrades safely'
	fi
}

printf '1..30\n'
test_production_gate
test_unknown_acme_environment
test_issuer_rendering
test_ready_install_is_idempotent
test_partial_install_has_diagnostics
test_skip_is_visible
test_skip_k3s_never_installs
test_incompatible_existing_version_is_not_changed
test_fresh_managed_install_uses_pinned_manifest
test_upgrade_check_is_non_mutating
test_missing_flag_values_have_yacht_diagnostics
test_server_version_requires_server_object_and_api_success
test_install_upgrade_compatibility_constants_agree
test_fresh_k3s_is_pinned_to_reviewed_minor
test_manifest_fetch_uses_pinned_url_and_checksum
test_manifest_mismatch_degrades_before_apply
test_nondefault_cert_manager_namespace_is_used
test_skip_k3s_ready_issuer_with_flags_is_read_only
test_missing_traefik_degrades_without_blocking
test_preexisting_readiness_failure_has_no_delete_guidance
test_preexisting_issuer_failure_has_no_delete_guidance
test_fresh_partial_has_scoped_delete_guidance_and_continues
test_environment_change_with_references_is_refused
test_environment_change_without_references_is_applied
test_dependency_degradation_does_not_stop_yacht_install
test_upgrade_reports_live_issuer_environment_read_only
test_shipped_supply_chain_constants_are_exact
test_install_upgrade_acme_mappings_agree
test_environment_change_reference_query_failure_is_read_only
test_webhook_dry_run_is_managed_only_and_degrades_safely

if [ "$fail" -ne 0 ]; then
	printf '%s test(s) failed\n' "$fail" >&2
	exit 1
fi
