package webhook

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/egress"
)

// A zoned loopback literal is loopback by another spelling, and is refused in
// the dial like the plain one; an address the operator allowed is reached.
func TestTheWebhookGuardRefusesZonesAndHonoursTheOperator(t *testing.T) {
	if ln, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		accepted := make(chan struct{}, 1)
		go func() {
			if c, err := ln.Accept(); err == nil {
				accepted <- struct{}{}
				c.Close()
			}
		}()
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		_, err := guardedClient(false, nil).Post("http://[::1%25zz]:"+port+"/", "text/plain", nil)
		if err == nil || !strings.Contains(err.Error(), errBlockedAddress.Error()) {
			t.Errorf("a zoned loopback receiver gave %v, want it refused as private", err)
		}
		ln.Close()
		select {
		case <-accepted:
			t.Error("the zoned loopback receiver was connected to")
		default:
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	if _, err := guardedClient(false, nil).Get(srv.URL); err == nil {
		t.Error("a loopback receiver was reached without the operator allowing it")
	}
	policy, err := egress.ParsePolicy([]string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := guardedClient(false, policy).Get(srv.URL)
	if err != nil {
		t.Fatalf("a receiver at an address the operator allowed: %v", err)
	}
	resp.Body.Close()
}
