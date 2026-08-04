package domain

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// NXDOMAIN from a zone's own nameserver is the truth, and the most important
// truth this package handles: it is what "the record has not been created yet"
// looks like. Retrying it through a cache would undo the whole point.
func TestAnAuthoritativeNoIsNotRetried(t *testing.T) {
	notFound := &net.DNSError{Err: "no such host", IsNotFound: true}
	if !answered(notFound) {
		t.Error("an authoritative NXDOMAIN is treated as a failure to get an answer")
	}

	for _, err := range []error{
		&net.DNSError{Err: "i/o timeout", IsTimeout: true},
		&net.DNSError{Err: "connection refused"},
		errors.New("something else entirely"),
	} {
		if answered(err) {
			t.Errorf("%v is treated as an authoritative answer", err)
		}
	}
}

// With nothing configured the resolver still has one, so a caller never has to
// decide whether it is safe to use.
func TestTheFallbackDefaultsToTheSystemResolver(t *testing.T) {
	if _, ok := (AuthoritativeResolver{}).fallback().(NetResolver); !ok {
		t.Error("no fallback by default")
	}
	if got := (AuthoritativeResolver{}).timeout(); got != lookupTimeout {
		t.Errorf("timeout = %v, want the package default", got)
	}
}

// Reads real DNS. Opt-in for the reason the database tests are, and a skip
// reads as a pass — set YACHT_LIVE_DNS when changing anything here.
func TestAuthoritativeResolutionAgainstRealDNS(t *testing.T) {
	if os.Getenv("YACHT_LIVE_DNS") == "" {
		t.Skip("set YACHT_LIVE_DNS to read real DNS")
	}
	a := AuthoritativeResolver{Fallback: NetResolver{}}
	ctx := context.Background()

	// A subdomain is rarely its own zone, so the walk has to climb to find the
	// delegation that actually holds the record.
	if _, err := a.serversFor(ctx, "www.github.com"); err != nil {
		t.Fatalf("serversFor: %v", err)
	}

	addrs, err := a.LookupHost(ctx, "github.com")
	if err != nil || len(addrs) == 0 {
		t.Fatalf("LookupHost = %v, %v", addrs, err)
	}

	cname, err := a.LookupCNAME(ctx, "www.github.com")
	if err != nil || cname == "" {
		t.Fatalf("LookupCNAME = %q, %v", cname, err)
	}

	// A name that does not exist inside a zone that does. This is the case the
	// whole design is for: the answer must be a real no, from the zone itself.
	_, err = a.LookupHost(ctx, "definitely-not-here-9f3a.github.com")
	if err == nil {
		t.Fatal("a name that does not exist resolved")
	}
	if !answered(err) {
		t.Errorf("err = %v, want an authoritative no rather than a lookup failure", err)
	}
}

// The custom Dial has to be the thing that decides which server is asked. If it
// were ignored, every lookup would quietly go back through the system resolver
// and its cache — which would leave this whole file doing nothing.
func TestTheChosenServerIsReallyTheOneAsked(t *testing.T) {
	if os.Getenv("YACHT_LIVE_DNS") == "" {
		t.Skip("set YACHT_LIVE_DNS to read real DNS")
	}
	// TEST-NET-1: guaranteed not to be a working resolver.
	r := resolverAt("192.0.2.1:53", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := r.LookupHost(ctx, "github.com"); err == nil {
		t.Fatal("an unroutable server answered — Dial is being ignored and the " +
			"system resolver is answering instead")
	}
}

// A nameserver that drops packets must cost the configured timeout and no more.
//
// Measured rather than assumed. net.Dialer.Timeout bounds establishing a
// connection, and a UDP "dial" is not a connection — it succeeds instantly and
// the wait lands on the read, after which Go retries across every server in
// resolv.conf. This took forty seconds before the lookups were given a context.
func TestAnUnreachableNameserverIsBounded(t *testing.T) {
	if os.Getenv("YACHT_LIVE_DNS") == "" {
		t.Skip("set YACHT_LIVE_DNS to read real DNS")
	}
	const bound = 2 * time.Second

	r := resolverAt("192.0.2.1:53", bound)
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()

	start := time.Now()
	_, err := r.LookupHost(ctx, "github.com")
	took := time.Since(start)

	if err == nil {
		t.Fatal("an unroutable server answered")
	}
	if took > bound+2*time.Second {
		t.Errorf("took %v, want it bounded near %v", took.Round(time.Millisecond), bound)
	}
	if answered(err) {
		t.Error("a network failure was classified as an authoritative answer")
	}
}
