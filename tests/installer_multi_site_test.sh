#!/bin/sh

# Contract tests for install.sh's multi-site flags: what K3s is installed
# with, and what the installer refuses to do to a K3s that is already
# running. The script is sourced only after its final entrypoint is removed,
# and every command that would touch the machine is a stub, so these tests
# cannot provision the machine that runs them.

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
	awk '/^main "\$@"$/ { found=1; exit } { print } END { if (!found) exit 1 }' \
		"$1" > "$2"
}

INSTALL_LIB="$TMP_ROOT/install-lib.sh"
sourceable "$ROOT/install.sh" "$INSTALL_LIB"

# fresh_install runs install_k3s on a machine with no K3s, with the given
# flags, and records what the K3s installer was handed. ROUTE is what
# `ip route get` answers.
fresh_install() {
	fresh_log=$1
	shift
	: > "$fresh_log"
	ROUTE=${ROUTE:-} sh -c '
		. "$1"
		LOG=$2
		shift 2
		parse_flags "$@"
		need_cmd() { [ "$1" = ip ]; }
		ip() { printf "%s\n" "${ROUTE:-}"; }
		require_wireguard() { printf "wireguard-checked\n" >> "$LOG"; }
		curl() { printf "%s\n" installer; }
		sh() { cat >/dev/null; printf "exec=%s\n" "${INSTALL_K3S_EXEC:-}" >> "$LOG"; }
		k3s() { return 0; }
		sleep() { :; }
		install_k3s
	' sh "$INSTALL_LIB" "$fresh_log" "$@"
}

# running_install runs install_k3s where K3s is already up, started from the
# given unit file.
running_install() {
	running_log=$1
	running_unit=$2
	shift 2
	: > "$running_log"
	sh -c '
		. "$1"
		LOG=$2
		K3S_UNIT_FILE=$3
		K3S_CONFIG_FILE=/nonexistent
		shift 3
		parse_flags "$@"
		need_cmd() { return 0; }
		systemctl() { return 0; }
		curl() { printf "curl\n" >> "$LOG"; }
		sh() { cat >/dev/null; printf "reinstalled\n" >> "$LOG"; }
		k3s() { return 0; }
		sleep() { :; }
		install_k3s
	' sh "$INSTALL_LIB" "$running_log" "$running_unit" "$@"
}

test_flags_are_checked_before_anything_runs() {
	for bad in \
		'--public-ip 203.0.113.7' \
		'--multi-site --public-ip 203.0.113.7;id' \
		'--multi-site --public-ip 203.0.113.007' \
		'--multi-site --public-ip 256.1.1.1' \
		'--multi-site --public-ip node.example.com' \
		'--site DAR-A' \
		'--site dar;id' \
		'--site -dar' \
		'--multi-site --skip-k3s' \
		'--site dar-a --skip-k3s'; do
		# Word-split on purpose: each case is a list of flags.
		# shellcheck disable=SC2086
		if sh -c '. "$1"; shift; parse_flags "$@"' sh "$INSTALL_LIB" $bad 2>/dev/null; then
			not_ok "flags are checked: '$bad' was accepted"
			return
		fi
	done
	for good in \
		'--multi-site' \
		'--multi-site --public-ip 203.0.113.7' \
		'--multi-site --public-ip 2001:db8::7' \
		'--site dar-a' \
		'--multi-site --site eu.west_1'; do
		# shellcheck disable=SC2086
		if ! sh -c '. "$1"; shift; parse_flags "$@"' sh "$INSTALL_LIB" $good; then
			not_ok "flags are checked: '$good' was refused"
			return
		fi
	done
	ok 'site and public address flags are checked before anything runs'
}

test_one_place_installs_k3s_as_before() {
	log="$TMP_ROOT/plain.log"
	fresh_install "$log" >/dev/null
	if [ "$(cat "$log")" = "exec=" ]; then
		ok 'an install in one place hands K3s nothing new'
	else
		not_ok 'an install in one place hands K3s nothing new'
		cat "$log" >&2
	fi
}

test_multi_site_installs_wireguard() {
	log="$TMP_ROOT/multi.log"
	fresh_install "$log" --multi-site --public-ip 203.0.113.7 --site dar-a >/dev/null
	got=$(cat "$log")
	if assert_contains "$got" 'exec=server ' &&
		assert_contains "$got" '--flannel-backend=wireguard-native' &&
		assert_contains "$got" '--flannel-external-ip' &&
		assert_contains "$got" '--node-external-ip=203.0.113.7' &&
		assert_contains "$got" '--tls-san=203.0.113.7' &&
		assert_contains "$got" '--node-label=topology.kubernetes.io/zone=dar-a' &&
		assert_contains "$got" 'wireguard-checked'; then
		ok '--multi-site installs K3s over WireGuard at its public address'
	else
		not_ok '--multi-site installs K3s over WireGuard at its public address'
	fi
}

test_public_address_comes_from_the_default_route() {
	log="$TMP_ROOT/detect.log"
	ROUTE='1.1.1.1 via 203.0.113.1 dev eth0 src 203.0.113.9 uid 0' \
		fresh_install "$log" --multi-site >/dev/null
	if assert_contains "$(cat "$log")" '--node-external-ip=203.0.113.9'; then
		ok 'with no --public-ip, the default route interface address is used'
	else
		not_ok 'with no --public-ip, the default route interface address is used'
	fi
}

test_a_private_address_is_not_guessed() {
	log="$TMP_ROOT/private.log"
	err="$TMP_ROOT/private.err"
	if ROUTE='1.1.1.1 via 10.0.0.1 dev ens3 src 10.0.0.5 uid 0' \
		fresh_install "$log" --multi-site >/dev/null 2>"$err"; then
		not_ok 'a private default-route address is refused, not used'
		return
	fi
	if assert_contains "$(cat "$err")" '10.0.0.5' &&
		assert_contains "$(cat "$err")" '--public-ip' &&
		! assert_contains "$(cat "$log")" 'exec=' 2>/dev/null; then
		ok 'a private default-route address is refused, not used'
	else
		not_ok 'a private default-route address is refused, not used'
	fi
}

test_running_vxlan_is_not_changed() {
	unit="$TMP_ROOT/k3s-vxlan.service"
	printf '%s\n' '[Service]' "ExecStart=/usr/local/bin/k3s \\" "    server \\" > "$unit"
	log="$TMP_ROOT/vxlan.log"
	err="$TMP_ROOT/vxlan.err"
	if running_install "$log" "$unit" --multi-site >/dev/null 2>"$err"; then
		not_ok 'a running K3s on VXLAN is refused, not reconfigured'
		return
	fi
	msg=$(cat "$err")
	if assert_contains "$msg" "'vxlan'" &&
		assert_contains "$msg" 'restarting K3s on every node' &&
		! assert_contains "$(cat "$log")" 'reinstalled' 2>/dev/null &&
		! assert_contains "$(cat "$log")" 'curl' 2>/dev/null; then
		ok 'a running K3s on VXLAN is refused, not reconfigured'
	else
		not_ok 'a running K3s on VXLAN is refused, not reconfigured'
	fi
}

test_running_wireguard_is_left_alone() {
	# The shape get.k3s.io writes: one quoted argument per line, here with the
	# flag and its value as two arguments.
	unit="$TMP_ROOT/k3s-wireguard.service"
	printf '%s\n' '[Service]' "ExecStart=/usr/local/bin/k3s \\" \
		"    server \\" "	'--flannel-backend' \\" "	'wireguard-native' \\" \
		"	'--node-external-ip=203.0.113.7' \\" > "$unit"
	log="$TMP_ROOT/wireguard.log"
	if running_install "$log" "$unit" --multi-site >/dev/null 2>&1 &&
		! assert_contains "$(cat "$log")" 'reinstalled' 2>/dev/null; then
		ok 'a re-run on a multi-site server changes nothing'
	else
		not_ok 'a re-run on a multi-site server changes nothing'
	fi
}

test_backend_is_read_from_either_place() {
	config="$TMP_ROOT/config.yaml"
	printf '%s\n' 'write-kubeconfig-mode: "0644"' 'flannel-backend: "wireguard-native"' > "$config"
	unit="$TMP_ROOT/k3s-eq.service"
	printf '%s\n' "ExecStart=/usr/local/bin/k3s \\" "	'server' \\" \
		"	'--flannel-backend=host-gw' \\" > "$unit"
	from_config=$(sh -c '. "$1"; K3S_UNIT_FILE=/nonexistent; K3S_CONFIG_FILE=$2; k3s_flannel_backend' \
		sh "$INSTALL_LIB" "$config")
	from_unit=$(sh -c '. "$1"; K3S_UNIT_FILE=$2; K3S_CONFIG_FILE=/nonexistent; k3s_flannel_backend' \
		sh "$INSTALL_LIB" "$unit")
	neither=$(sh -c '. "$1"; K3S_UNIT_FILE=/nonexistent; K3S_CONFIG_FILE=/nonexistent; k3s_flannel_backend' \
		sh "$INSTALL_LIB")
	if [ "$from_config" = "wireguard-native" ] && [ "$from_unit" = "host-gw" ] &&
		[ "$neither" = "vxlan" ]; then
		ok 'the running backend is read from the unit or the config file'
	else
		not_ok "the running backend is read from the unit or the config file: config=$from_config unit=$from_unit neither=$neither"
	fi
}

printf '1..8\n'
test_flags_are_checked_before_anything_runs
test_one_place_installs_k3s_as_before
test_multi_site_installs_wireguard
test_public_address_comes_from_the_default_route
test_a_private_address_is_not_guessed
test_running_vxlan_is_not_changed
test_running_wireguard_is_left_alone
test_backend_is_read_from_either_place

if [ "$fail" -ne 0 ]; then
	printf '%s test(s) failed\n' "$fail" >&2
	exit 1
fi
