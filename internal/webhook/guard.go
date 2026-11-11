package webhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/codeblocktz/yacht/internal/egress"
)

// errBlockedAddress is a webhook whose host resolves somewhere the engine
// will not send to.
var errBlockedAddress = errors.New("that address is private to this network, and webhooks are only sent to public ones")

// blocked is the shared rule for what a tenant-named connection may not
// reach (see package egress). A webhook URL is somebody's input, and the
// engine is the one process that can reach the cluster's own network; without
// this, a project's webhook would be a way to make it call there.
func blocked(addr netip.Addr) bool { return egress.Blocked(addr) }

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
