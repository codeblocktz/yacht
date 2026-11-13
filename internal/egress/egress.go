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
// The exception is spelled out where it is made: a request sent through the
// operator's HTTP proxy is checked by resolving the name first, which a name
// that answers differently a moment later gets past.
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/idna"
)

// ErrBlocked is a connection refused because its address is private to this
// network.
var ErrBlocked = errors.New("that address is private to this network, and only public ones are reached from here")

// blockedPrefixes are the ranges a tenant-named connection may not reach: this
// machine, the private networks the cluster's pods, services and API sit on,
// link-local — which is where a cloud's metadata service answers — carrier-
// grade NAT, the documentation, benchmarking, reserved and multicast ranges
// nothing legitimate is at, and the IPv6 forms that carry an IPv4 address
// inside them — NAT64, 6to4, Teredo, IPv4-compatible, IPv4-translated — any
// of which can be a private IPv4 address by another name.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "::ffff:0:0:0/96", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
		"64:ff9b::/96", "64:ff9b:1::/48", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
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
	allowNames     map[string]bool // host:port, or host for every port
	allowAddrPorts map[netip.AddrPort]bool
	allowNets      []netip.Prefix
	denyNets       []netip.Prefix

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

// ParsePolicy reads the operator's lists.
//
// allow holds host:port — the form to prefer, since it opens one service
// rather than every port on a machine — host, address:port, addresses and
// CIDRs. deny holds addresses and CIDRs. A host allowed by name is dialled as
// it resolves, without a check, because the operator named it; an address,
// address:port or range allowed is still refused when deny covers it.
func ParsePolicy(allow, deny []string) (*Policy, error) {
	p := &Policy{allowNames: map[string]bool{}, allowAddrPorts: map[netip.AddrPort]bool{}}
	for _, raw := range allow {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if err := p.addAllow(s); err != nil {
			return nil, fmt.Errorf("egress: allow %q: %w", raw, err)
		}
	}
	for _, raw := range deny {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		pfx, err := parseNet(s)
		if err != nil {
			return nil, fmt.Errorf("egress: deny %q: %w", raw, err)
		}
		p.denyNets = append(p.denyNets, pfx)
	}
	return p, nil
}

func (p *Policy) addAllow(s string) error {
	switch {
	case strings.Contains(s, "://"):
		return errors.New("a host or address, not a URL")
	case strings.Contains(s, "*"):
		return errors.New("wildcards are not supported; name each host")
	case strings.ContainsAny(s, "@% \t"):
		return errors.New("not a host, host:port, address or CIDR")
	}
	if strings.Contains(s, "/") {
		pfx, err := parseNet(s)
		if err != nil {
			return err
		}
		p.allowNets = append(p.allowNets, pfx)
		return nil
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		pfx, err := parseNet(addr.String())
		if err != nil {
			return err
		}
		p.allowNets = append(p.allowNets, pfx)
		return nil
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q is not a port", port)
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if addr.Zone() != "" {
				return errors.New("an address with a zone is never reached")
			}
			p.allowAddrPorts[netip.AddrPortFrom(addr.Unmap(), uint16(n))] = true
			return nil
		}
		name, ok := canonicalHost(host)
		if !ok || !validName(name) {
			return fmt.Errorf("%q is not a host name", host)
		}
		p.allowNames[net.JoinHostPort(name, port)] = true
		return nil
	}
	if strings.ContainsAny(s, "[]") {
		return errors.New("brackets go only around an IPv6 address with a port, as in [fd00::1]:5000")
	}
	name, ok := canonicalHost(s)
	if !ok || !validName(name) {
		return fmt.Errorf("%q is not a host name", s)
	}
	p.allowNames[name] = true
	return nil
}

// parseNet reads an address as a one-address range, or a CIDR. An IPv4 range
// written as IPv6 (::ffff:10.0.0.0/104) is read as the IPv4 range it is,
// because addresses are judged after the same unmapping.
func parseNet(s string) (netip.Prefix, error) {
	if pfx, err := netip.ParsePrefix(s); err == nil {
		if pfx.Addr().Is4In6() {
			if pfx.Bits() < 96 {
				return netip.Prefix{}, errors.New("an IPv4-mapped range must be /96 or longer")
			}
			pfx = netip.PrefixFrom(pfx.Addr().Unmap(), pfx.Bits()-96)
		}
		return pfx.Masked(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, errors.New("not an address or CIDR")
	}
	if addr.Zone() != "" {
		return netip.Prefix{}, errors.New("an address with a zone is never reached")
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// canonicalHost is a host as it is dialled and compared: mapped to ASCII the
// way net/http maps it before dialling — so a fullwidth "１２７.0.0.1" is the
// 127.0.0.1 it reaches — in lower case, without brackets or a trailing dot.
// false is a host that cannot be one.
func canonicalHost(h string) (string, bool) {
	h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
	if h == "" {
		return "", false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return strings.ToLower(h), true
	}
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			a, err := idna.Lookup.ToASCII(h)
			if err != nil {
				return "", false
			}
			h = strings.TrimSuffix(a, ".")
			break
		}
	}
	return strings.ToLower(h), true
}

// canonicalAuthority is host:port with the host canonical.
func canonicalAuthority(authority string) (string, bool) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return "", false
	}
	host, ok := canonicalHost(host)
	if !ok {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

// validName is a DNS name's characters: letters, digits, hyphens, dots, and
// the underscores some service names carry.
func validName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
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
	if p.denied(addr) {
		return true
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

func (p *Policy) denied(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, n := range p.denyNets {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// Blocked is the rule without an operator's policy.
func Blocked(addr netip.Addr) bool { return none.Blocked(addr) }

// allowedAuthority reports whether the operator allowed an authority —
// host:port, or a bare host — by name, or as an address:port that deny does
// not cover. A range allowed is not answered here: that is judged on the
// address dialled, in Blocked.
func (p *Policy) allowedAuthority(authority string) bool {
	p = p.or()
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = authority, ""
	}
	host, ok := canonicalHost(host)
	if !ok {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || addr.Zone() != "" {
			return false
		}
		addr = addr.Unmap()
		return p.allowAddrPorts[netip.AddrPortFrom(addr, uint16(n))] && !p.denied(addr)
	}
	if port != "" && p.allowNames[net.JoinHostPort(host, port)] {
		return true
	}
	return p.allowNames[host]
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
//
// Every host is compared in the form it is dialled in (see canonicalHost), so
// another spelling of the same address is the same address.
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
		if c, ok := canonicalAuthority(a); ok {
			allowed = append(allowed, c)
		}
	}
	slices.Sort(allowed)
	key := transportKey{base: t, allow: strings.Join(allowed, ",")}
	if got, ok := p.transports.Load(key); ok {
		return got.(*http.Transport), nil
	}

	plain := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: p.Control}
	exempt := func(authority string) bool {
		return slices.Contains(allowed, authority) || p.allowedAuthority(authority)
	}

	var proxies sync.Map // canonical authority of a proxy the base sends to
	baseProxy := t.Proxy
	if baseProxy != nil {
		// Known before any request, so one that targets a proxy directly is
		// refused from the first.
		for _, scheme := range []string{"https", "http"} {
			probe := &http.Request{URL: &url.URL{Scheme: scheme, Host: "egress-probe.invalid"}}
			if u, err := baseProxy(probe); err == nil && u != nil {
				if a, ok := authorityOf(u); ok {
					proxies.Store(a, true)
				}
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
		target, ok := authorityOf(req.URL)
		if !ok {
			return nil, ErrBlocked
		}
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
		if a, ok := authorityOf(proxy); ok {
			proxies.Store(a, true)
		}
		return proxy, nil
	}
	clone.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		authority, ok := canonicalAuthority(address)
		if !ok {
			return nil, ErrBlocked
		}
		if exempt(authority) {
			return plain.DialContext(ctx, network, address)
		}
		if _, isProxy := proxies.Load(authority); isProxy {
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

// authorityOf is the canonical host:port a transport dials for a URL, with
// the scheme's port made explicit.
func authorityOf(u *url.URL) (string, bool) {
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
	return canonicalAuthority(net.JoinHostPort(host, port))
}

// CheckHost resolves a host — host or host:port — and refuses it when any
// address it has is blocked, unless the operator allowed it. For a
// connection whose dial cannot be guarded — one through a proxy — this is the
// best that can be done, and it is weaker: a name can resolve differently
// between this lookup and the dial.
func (p *Policy) CheckHost(ctx context.Context, hostport string) error {
	hostport = strings.TrimSpace(hostport)
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host, ok := canonicalHost(host)
	if !ok || strings.Contains(host, "%") {
		return ErrBlocked
	}
	if p.allowedAuthority(hostport) {
		return nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if p.Blocked(addr) {
			return ErrBlocked
		}
		return nil
	}
	if !validName(host) {
		return ErrBlocked
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
