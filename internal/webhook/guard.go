package webhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// errBlockedAddress is a webhook whose host resolves somewhere the engine
// will not send to.
var errBlockedAddress = errors.New("that address is private to this network, and webhooks are only sent to public ones")

// blockedPrefixes are the ranges a webhook may not reach: this machine, the
// private networks the cluster's pods, services and API sit on, link-local —
// which is where a cloud's metadata service answers — and carrier-grade NAT.
// A webhook URL is somebody's input, and the engine is the one process that
// can reach all of those; without this, a project's webhook would be a way
// to make it call them.
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

func blocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// guardedClient sends webhooks. The check is at connect time, on the address
// actually dialled, so a name that resolves somewhere public when it is saved
// and somewhere private later is still refused; and redirects are not
// followed, so a public receiver cannot hand the request on to a private one.
func guardedClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errBlockedAddress
			}
			addr, err := netip.ParseAddr(host)
			if err != nil || blocked(addr) {
				return errBlockedAddress
			}
			return nil
		}
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
