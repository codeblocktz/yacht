package egress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
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
