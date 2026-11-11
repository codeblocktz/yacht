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
// The exceptions are spelled out where they are made: a request sent through
// the operator's HTTP proxy, and an SSH host, are checked by resolving the
// name first, which a name that answers differently a moment later gets past.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrBlocked is a connection refused because its address is private to this
// network.
var ErrBlocked = errors.New("that address is private to this network, and only public ones are reached from here")

// blockedPrefixes are the ranges a tenant-named connection may not reach: this
// machine, the private networks the cluster's pods, services and API sit on,
// link-local — which is where a cloud's metadata service answers — carrier-
// grade NAT, the reserved and multicast ranges nothing legitimate is at, and
// the IPv6 forms that carry an IPv4 address inside them — NAT64, 6to4,
// Teredo, IPv4-compatible — any of which can be a private IPv4 address by
// another name.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"224.0.0.0/4", "240.0.0.0/4",
		"::/96", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
		"64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// Policy is the operator's say over what the engine may reach on a tenant's
// behalf: addresses to allow despite the rule above — a registry on the LAN or
// a tailnet — and ranges to refuse besides it, such as a pod or service
// network outside the private ranges. The nil Policy is the rule alone.
type Policy struct {
	allowHosts map[string]bool // host:port, or host for every port; lower case
	allowNets  []netip.Prefix
	denyNets   []netip.Prefix

	transports sync.Map // transportKey → *http.Transport
}

type transportKey struct {
	base  *http.Transport
	allow string
}

// none is the nil Policy's state, so a transport built without one is cached
// as well.
var none = &Policy{}

func (p *Policy) or() *Policy {
	if p == nil {
		return none
	}
	return p
}

// ParsePolicy reads the operator's lists. allow holds host, host:port, IP
// addresses and CIDRs; deny holds IP addresses and CIDRs. A host allowed by
// name is dialled as it resolves, without a check, because the operator named
// it; an address or range allowed is still refused when deny also covers it.
func ParsePolicy(allow, deny []string) (*Policy, error) {
	p := &Policy{allowHosts: map[string]bool{}}
	for _, raw := range allow {
		s := strings.ToLower(strings.TrimSpace(raw))
		if s == "" {
			continue
		}
		if pfx, ok := parseNet(s); ok {
			p.allowNets = append(p.allowNets, pfx)
			continue
		}
		if strings.ContainsAny(s, "/%@ ") || strings.Contains(s, "://") {
			return nil, fmt.Errorf("egress: %q is not a host, host:port, address or CIDR", raw)
		}
		p.allowHosts[s] = true
	}
	for _, raw := range deny {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		pfx, ok := parseNet(s)
		if !ok {
			return nil, fmt.Errorf("egress: %q is not an address or CIDR", raw)
		}
		p.denyNets = append(p.denyNets, pfx)
	}
	return p, nil
}

// parseNet reads an address as a one-address range, or a CIDR.
func parseNet(s string) (netip.Prefix, bool) {
	if pfx, err := netip.ParsePrefix(s); err == nil {
		return pfx.Masked(), true
	}
	if addr, err := netip.ParseAddr(s); err == nil && addr.Zone() == "" {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), true
	}
	return netip.Prefix{}, false
}

// Blocked reports whether an address may not be reached.
//
// An address with a zone — fe80::1%eth0, ::1%x — is refused outright: a zone
// is never part of a public address, and netip's prefix match is false for
// one, so it would otherwise walk past every range. An IPv4 address written
// as IPv6 is judged as the IPv4 address it is.
func (p *Policy) Blocked(addr netip.Addr) bool {
	p = p.or()
	if !addr.IsValid() || addr.Zone() != "" {
		return true
	}
	addr = addr.Unmap()
	for _, n := range p.denyNets {
		if n.Contains(addr) {
			return true
		}
	}
	for _, n := range p.allowNets {
		if n.Contains(addr) {
			return false
		}
	}
	for _, n := range blockedPrefixes {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// Blocked is the rule without an operator's policy.
func Blocked(addr netip.Addr) bool { return none.Blocked(addr) }

// allowedName reports whether the operator allowed a host, or host:port, by
// name.
func (p *Policy) allowedName(authority string) bool {
	p = p.or()
	if len(p.allowHosts) == 0 {
		return false
	}
	authority = strings.ToLower(authority)
	if p.allowHosts[authority] {
		return true
	}
	host, _, err := net.SplitHostPort(authority)
	return err == nil && p.allowHosts[host]
}

// Control is a net.Dialer Control that refuses a blocked address. It runs
// after the name has been resolved, on the address about to be connected to.
func (p *Policy) Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrBlocked
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || p.Blocked(addr) {
		return ErrBlocked
	}
	return nil
}

// Control is the rule's Control without an operator's policy.
func Control(network, address string, c syscall.RawConn) error {
	return none.Control(network, address, c)
}

// Transport returns a transport derived from base that dials public
// addresses only, except the authorities in allow — host:port, as a URL names
// them — and those the policy allows. The caller's allow is the hole an
// install's own internal registry needs, and nothing wider.
//
// One transport per base and allow, kept: a transport per request would
// throw its connections away each time.
//
// An HTTP proxy configured on base is kept. Through a proxy the dial is to
// the proxy, which the operator chose and which may well be private; the
// destination is checked instead, by resolving its name before the request
// goes to the proxy — weaker than the dial check, because a name can resolve
// differently by the time the proxy connects to it. Every redirect hop is a
// request of its own, and checked the same way. A request whose destination
// is the proxy itself, sent around it, is refused.
func (p *Policy) Transport(base http.RoundTripper, allow ...string) (*http.Transport, error) {
	p = p.or()
	if base == nil {
		base = http.DefaultTransport
	}
	t, ok := base.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("egress: cannot guard dialing through %T", base)
	}
	allowed := make([]string, 0, len(allow))
	for _, a := range allow {
		allowed = append(allowed, strings.ToLower(a))
	}
	slices.Sort(allowed)
	key := transportKey{base: t, allow: strings.Join(allowed, ",")}
	if got, ok := p.transports.Load(key); ok {
		return got.(*http.Transport), nil
	}

	plain := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: p.Control}
	exempt := func(authority string) bool {
		authority = strings.ToLower(authority)
		return slices.Contains(allowed, authority) || p.allowedName(authority)
	}

	var proxies sync.Map // authority of a proxy the base sent a request to
	baseProxy := t.Proxy
	if baseProxy != nil {
		// Known before any request, so one that targets a proxy directly is
		// refused from the first.
		for _, scheme := range []string{"https", "http"} {
			probe := &http.Request{URL: &url.URL{Scheme: scheme, Host: "egress-probe.invalid"}}
			if u, err := baseProxy(probe); err == nil && u != nil {
				proxies.Store(authorityOf(u), true)
			}
		}
	}

	clone := t.Clone()
	// Any dialer the base carried would bypass the guard, TLS ones included.
	clone.Dial, clone.DialTLS, clone.DialTLSContext = nil, nil, nil
	clone.Proxy = func(req *http.Request) (*url.URL, error) {
		var proxy *url.URL
		if baseProxy != nil {
			u, err := baseProxy(req)
			if err != nil {
				return nil, err
			}
			proxy = u
		}
		target := authorityOf(req.URL)
		if exempt(target) {
			return proxy, nil
		}
		if proxy == nil {
			if _, isProxy := proxies.Load(target); isProxy {
				return nil, ErrBlocked
			}
			return nil, nil
		}
		if err := p.CheckHost(req.Context(), target); err != nil {
			return nil, err
		}
		proxies.Store(authorityOf(proxy), true)
		return proxy, nil
	}
	clone.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if exempt(address) {
			return plain.DialContext(ctx, network, address)
		}
		if _, isProxy := proxies.Load(strings.ToLower(address)); isProxy {
			return plain.DialContext(ctx, network, address)
		}
		return guarded.DialContext(ctx, network, address)
	}
	got, _ := p.transports.LoadOrStore(key, clone)
	return got.(*http.Transport), nil
}

// Transport is the rule's Transport without an operator's policy.
func Transport(base http.RoundTripper, allow ...string) (*http.Transport, error) {
	return none.Transport(base, allow...)
}

// authorityOf is host:port, lower case, with the scheme's port made explicit
// — what a transport dials for the URL.
func authorityOf(u *url.URL) string {
	host, port := u.Hostname(), u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		case "socks5", "socks5h":
			port = "1080"
		}
	}
	return strings.ToLower(net.JoinHostPort(host, port))
}

// CheckHost resolves a host — host or host:port — and refuses it when any
// address it has is blocked, unless the operator allowed the name. For a
// connection whose dial cannot be guarded — one through a proxy, or an SSH
// transport that dials for itself — this is the best that can be done, and
// it is weaker: a name can resolve differently between this lookup and the
// dial.
func (p *Policy) CheckHost(ctx context.Context, hostport string) error {
	hostport = strings.ToLower(strings.TrimSpace(hostport))
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
		if p.allowedName(hostport) {
			return nil
		}
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" || strings.Contains(host, "%") {
		return ErrBlocked
	}
	if p.allowedName(host) {
		return nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if p.Blocked(addr) {
			return ErrBlocked
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if p.Blocked(a) {
			return ErrBlocked
		}
	}
	return nil
}

// CheckHost is the rule's CheckHost without an operator's policy.
func CheckHost(ctx context.Context, hostport string) error { return none.CheckHost(ctx, hostport) }
