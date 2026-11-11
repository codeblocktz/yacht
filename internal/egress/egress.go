// Package egress keeps the engine's own requests off the network it sits on
// when somebody else chose where they go.
//
// The engine is the one process that can reach everything: the cluster's API,
// every Service, the node's metadata endpoint. A webhook URL, a repository URL
// and an image reference are all a tenant's input, and each makes the engine
// open a connection. Without a check, each is a way to aim the engine at an
// address the tenant cannot reach, and to learn from the answer — or from how
// long it took — what is there.
//
// The check is on the address actually dialled, inside the dial, so a name
// that resolves somewhere public when it is looked at and somewhere private
// when it is connected to is still refused, and so is every redirect hop.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked is a connection refused because its address is private to this
// network.
var ErrBlocked = errors.New("that address is private to this network, and only public ones are reached from here")

// blockedPrefixes are the ranges a tenant-named connection may not reach: this
// machine, the private networks the cluster's pods, services and API sit on,
// link-local — which is where a cloud's metadata service answers — carrier-
// grade NAT, and the reserved and multicast ranges nothing legitimate is at.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// Blocked reports whether an address is one a tenant-named connection may not
// reach. An IPv4 address written as IPv6 is judged as the IPv4 one it is.
func Blocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Control is a net.Dialer Control that refuses a blocked address. It runs
// after the name has been resolved, on the address about to be connected to.
func Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrBlocked
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || Blocked(addr) {
		return ErrBlocked
	}
	return nil
}

// Transport returns a copy of base that dials public addresses only, except
// the authorities in allow — host:port, as a URL names them — which are
// dialled as they are. That is the hole an install's own internal registry
// needs, and nothing wider.
//
// Proxying is turned off: through a proxy the dial is to the proxy, and the
// destination it is asked for is never seen here to be checked.
func Transport(base http.RoundTripper, allow ...string) (*http.Transport, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	t, ok := base.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("egress: cannot guard dialing through %T", base)
	}
	allowed := make(map[string]bool, len(allow))
	for _, a := range allow {
		allowed[strings.ToLower(a)] = true
	}
	plain := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: Control}

	clone := t.Clone()
	clone.Proxy = nil
	// Any dialer the base carried would bypass the guard, TLS ones included.
	clone.Dial, clone.DialTLS, clone.DialTLSContext = nil, nil, nil
	clone.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if allowed[strings.ToLower(address)] {
			return plain.DialContext(ctx, network, address)
		}
		return guarded.DialContext(ctx, network, address)
	}
	return clone, nil
}

// CheckHost resolves a host and refuses it when any address it has is
// blocked. For a connection whose dial cannot be guarded — an SSH transport
// that dials for itself — this is the best that can be done, and it is weaker:
// a name can resolve differently between this lookup and the dial.
func CheckHost(ctx context.Context, host string) error {
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if Blocked(addr) {
			return ErrBlocked
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if Blocked(a) {
			return ErrBlocked
		}
	}
	return nil
}
