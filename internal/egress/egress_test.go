package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// The ranges that matter inside a cluster: loopback, the pod and service
// networks, the metadata service on link-local, and the IPv6 equivalents —
// including an IPv4 address dressed as IPv6, which is the same address.
func TestPrivateAddressesAreBlockedAndPublicOnesAreNot(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.43.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "::1", "::", "fc00::1", "fe80::1",
		"::ffff:10.0.0.1",
	} {
		if !Blocked(netip.MustParseAddr(raw)) {
			t.Errorf("%s is not blocked", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "140.82.112.3", "2606:4700:4700::1111"} {
		if Blocked(netip.MustParseAddr(raw)) {
			t.Errorf("public %s is blocked", raw)
		}
	}
}

// The guard is in the dial, so the one exemption is by the authority a URL
// names: the install's own registry, spelled as configured. Every other
// connection to a private address — the same server under another name
// included — is refused without a request reaching it.
func TestATransportDialsOnlyPublicAddressesAndWhatItIsTold(t *testing.T) {
	hits := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits <- r.Host
	}))
	t.Cleanup(srv.Close)
	own := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := strings.Cut(own, ":")

	transport, err := Transport(nil, own)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}

	resp, err := client.Get(srv.URL + "/v2/")
	if err != nil {
		t.Fatalf("the allowed authority was refused: %v", err)
	}
	resp.Body.Close()
	<-hits

	_, err = client.Get("http://localhost:" + port + "/v2/")
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("another name for a private address gave %v, want ErrBlocked", err)
	}
	select {
	case h := <-hits:
		t.Errorf("a blocked request reached the server as %s", h)
	default:
	}
}

func TestAHostIsCheckedByEveryAddressItHas(t *testing.T) {
	ctx := context.Background()
	for _, host := range []string{"127.0.0.1", "[::1]", "10.0.0.1", "localhost"} {
		if err := CheckHost(ctx, host); !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckHost(%s) = %v, want ErrBlocked", host, err)
		}
	}
	if err := CheckHost(ctx, "8.8.8.8"); err != nil {
		t.Errorf("CheckHost(8.8.8.8) = %v, want nil", err)
	}
}

type notATransport struct{}

func (notATransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

// A round tripper whose dial cannot be reached cannot be guarded, and is
// refused rather than used unguarded.
func TestAnUnguardableTransportIsRefused(t *testing.T) {
	if _, err := Transport(notATransport{}); err == nil {
		t.Error("a transport whose dial cannot be guarded was accepted")
	}
}

// The IPv6 spellings of a private address: a zone, which netip's prefix match
// ignores and so would otherwise pass every range; and the forms that carry
// an IPv4 address inside them — NAT64, 6to4, Teredo, IPv4-compatible — which
// a gateway on the path can turn back into a private IPv4 one. Site-local is
// deprecated and private.
func TestEveryIPv6SpellingOfAPrivateAddressIsBlocked(t *testing.T) {
	for _, raw := range []string{
		"::1%zz", "fe80::1%eth0", "2606:4700:4700::1111%x",
		"64:ff9b::a00:1", "64:ff9b:1::a00:1", "2002:a00:1::1", "2001:0:a00:1::1",
		"::a00:1", "::7f00:1", "fec0::1",
	} {
		if !Blocked(netip.MustParseAddr(raw)) {
			t.Errorf("%s is not blocked", raw)
		}
	}
	if err := CheckHost(context.Background(), "[::1%zz]:443"); !errors.Is(err, ErrBlocked) {
		t.Errorf("CheckHost of a zoned literal = %v, want ErrBlocked", err)
	}
}

// A zoned loopback literal in a URL is refused in the dial, without reaching
// what listens there.
func TestAZonedLoopbackURLIsNotDialled(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	transport, err := Transport(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&http.Client{Transport: transport}).Get("http://[::1%25zz]:" + port + "/")
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("a zoned loopback URL gave %v, want ErrBlocked", err)
	}
	select {
	case <-accepted:
		t.Error("the zoned loopback address was connected to")
	default:
	}
}

// The operator's lists: a name allowed is reached whatever it resolves to;
// an address or range allowed is reached unless it is also refused; a range
// refused is refused although it is public by address.
func TestTheOperatorsPolicyAllowsAndRefuses(t *testing.T) {
	p, err := ParsePolicy(
		[]string{"registry.lan:5000", "git.lan", "10.20.0.0/16", "100.100.1.1"},
		[]string{"10.20.30.0/24", "45.33.32.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]bool{
		"10.20.1.1": false, "10.20.30.1": true, "100.100.1.1": false, "100.100.1.2": true,
		"45.33.32.9": true, "8.8.8.8": false, "127.0.0.1": true, "fe80::1%x": true,
	} {
		if got := p.Blocked(netip.MustParseAddr(raw)); got != want {
			t.Errorf("Blocked(%s) = %v, want %v", raw, got, want)
		}
	}
	ctx := context.Background()
	for host, ok := range map[string]bool{
		"registry.lan:5000": true, "registry.lan:6000": false, "git.lan:22": true,
		"git.lan": true, "10.20.1.1:443": true, "127.0.0.1:22": false,
	} {
		// Refused is ErrBlocked, or a name that does not resolve here.
		if err := p.CheckHost(ctx, host); ok != (err == nil) {
			t.Errorf("CheckHost(%s) = %v, want allowed=%v", host, err, ok)
		}
	}
	for _, bad := range []string{"https://x", "a b", "fe80::1%eth0", "user@host"} {
		if _, err := ParsePolicy([]string{bad}, nil); err == nil {
			t.Errorf("allow %q was accepted", bad)
		}
	}
	if _, err := ParsePolicy(nil, []string{"registry.lan"}); err == nil {
		t.Error("a name was accepted as a range to refuse")
	}
}

// One transport per base and allow-set, kept, so connections are reused.
func TestATransportIsBuiltOncePerAllowSet(t *testing.T) {
	a, _ := Transport(nil, "b:1", "a:1")
	b, _ := Transport(nil, "A:1", "b:1")
	c, _ := Transport(nil)
	if a != b {
		t.Error("the same allow-set built two transports")
	}
	if a == c {
		t.Error("different allow-sets share a transport")
	}
}

// Through an operator's proxy the dial is to the proxy, so the destination
// is checked by name before the request is handed over — each redirect hop
// too — and a request for the proxy itself, sent around it, is refused.
func TestAProxiedRequestIsCheckedByItsDestination(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "http://10.0.0.1/inside", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("proxied"))
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == proxyURL.Host {
			return nil, nil // NO_PROXY for itself
		}
		return proxyURL, nil
	}
	transport, err := Transport(base)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}

	resp, err := client.Get("http://203.0.114.7/public")
	if err != nil {
		t.Fatalf("a public destination through the proxy: %v", err)
	}
	resp.Body.Close()

	for _, target := range []string{"http://10.0.0.1/inside", "http://203.0.114.7/hop", proxy.URL + "/direct"} {
		if _, err := client.Get(target); !errors.Is(err, ErrBlocked) {
			t.Errorf("GET %s gave %v, want ErrBlocked", target, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range seen {
		if strings.Contains(s, "10.0.0.1") || strings.Contains(s, "/direct") {
			t.Errorf("the proxy was asked for %s", s)
		}
	}
}

// The documentation, benchmarking, discard and translation ranges are not
// where a registry or a webhook receiver is.
func TestTheReservedRangesAreBlocked(t *testing.T) {
	for _, raw := range []string{
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "192.88.99.1",
		"2001:db8::1", "100::1", "::ffff:0:a00:1", "2001:1ff::1",
	} {
		if !Blocked(netip.MustParseAddr(raw)) {
			t.Errorf("%s is not blocked", raw)
		}
	}
}

// What the operator writes is read strictly: a wildcard, a bracketed host
// without a port, a port out of range or a name with characters no name has
// is refused at start, saying why, rather than allowing nothing — or
// something else. An IPv4 range written as IPv6 is the IPv4 range; an
// address:port allowed is still refused when deny covers the address; and a
// trailing dot or another case is the same name.
func TestTheOperatorsListsAreReadStrictly(t *testing.T) {
	for _, bad := range []string{
		"*.lan", "[fd00::1]", "registry.lan:0", "registry.lan:70000", "registry.lan:x",
		"reg istry.lan", "réseau!.lan", "::ffff:10.0.0.0/90",
	} {
		if _, err := ParsePolicy([]string{bad}, nil); err == nil {
			t.Errorf("allow %q was accepted", bad)
		}
	}
	p, err := ParsePolicy(
		[]string{"10.9.9.9:5000", "[fd00::5]:5000", "Registry.LAN.:5000"},
		[]string{"::ffff:10.9.0.0/112"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Blocked(netip.MustParseAddr("10.9.1.1")) {
		t.Error("an IPv4-mapped range in deny did not refuse the IPv4 address")
	}
	ctx := context.Background()
	if err := p.CheckHost(ctx, "10.9.9.9:5000"); !errors.Is(err, ErrBlocked) {
		t.Errorf("an address:port allowed but denied gave %v, want ErrBlocked", err)
	}
	if err := p.CheckHost(ctx, "[fd00::5]:5000"); err != nil {
		t.Errorf("an IPv6 address:port allowed gave %v", err)
	}
	if err := p.CheckHost(ctx, "[fd00::5]:5001"); !errors.Is(err, ErrBlocked) {
		t.Errorf("another port of an address:port allowed gave %v", err)
	}
	for _, spelled := range []string{"registry.lan:5000", "REGISTRY.lan.:5000"} {
		if err := p.CheckHost(ctx, spelled); err != nil {
			t.Errorf("CheckHost(%s) = %v, want the allowed name", spelled, err)
		}
	}
}

// net/http maps a host to ASCII before dialling it, so a fullwidth
// "１２７.0.0.1" dials 127.0.0.1. Every comparison is made on that form: the
// proxy's own address by another spelling is the proxy's own address, and
// is refused.
func TestAnotherSpellingOfTheProxyIsTheProxy(t *testing.T) {
	var hits sync.Map
	proxy := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Store(r.URL.String(), true)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = func(r *http.Request) (*url.URL, error) {
		if !strings.HasPrefix(r.URL.Host, "203.0.114.") {
			return nil, nil // NO_PROXY for everything but the public test host
		}
		return proxyURL, nil
	}
	transport, err := Transport(base)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}
	// Warm the proxy's address into what the transport knows.
	if resp, err := client.Get("http://203.0.114.7/"); err == nil {
		resp.Body.Close()
	}
	for _, target := range []string{
		"http://１２７.0.0.1:" + proxyURL.Port() + "/direct",
		"http://127.0.0.1.:" + proxyURL.Port() + "/direct",
	} {
		if _, err := client.Get(target); !errors.Is(err, ErrBlocked) {
			t.Errorf("GET %s gave %v, want ErrBlocked", target, err)
		}
	}
	hits.Range(func(k, _ any) bool {
		if strings.Contains(k.(string), "/direct") {
			t.Errorf("the proxy was reached directly for %s", k)
		}
		return true
	})
}
