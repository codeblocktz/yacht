package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Which resolver answers, and why it is worth choosing.
//
// The Resolver interface has always carried a note that a caller "may substitute
// a resolver that does not use the host's own cache — a freshly created record
// is exactly the case where a cached negative answer is wrong". Nothing ever
// did, so verification asked the host resolver, which had usually just cached
// the NXDOMAIN from the check made moments before the record was created.
//
// The effect is specific and maddening: somebody creates the record, the
// dashboard says the name does not resolve, and it keeps saying so for the
// negative TTL — commonly five to fifteen minutes — with no way to tell that
// from a record that was typed wrong.

// DirectResolver asks one nameserver directly, over its own connection.
//
// Not a cache-busting trick. Go's resolver keeps no cache of its own; what it
// consults is the operating system's, via whatever /etc/resolv.conf points at.
// Dialling a chosen server bypasses that path, so the answer comes from a
// resolver this process picked rather than from whatever the host decided to
// remember.
type DirectResolver struct {
	// Server is host:port, for example "1.1.1.1:53".
	Server string

	// Timeout bounds a single lookup. Zero means the package default.
	Timeout time.Duration
}

// NewDirectResolver builds a resolver that queries one server.
//
// The server is a deliberate choice by the operator rather than a default,
// because it is a decision about where this install's DNS questions go. An
// air-gapped install must not silently start depending on a public resolver,
// which is why nothing here is switched on unless it is configured.
func NewDirectResolver(server string) DirectResolver {
	return DirectResolver{Server: withDefaultPort(server), Timeout: lookupTimeout}
}

func (d DirectResolver) resolver() *net.Resolver {
	server, timeout := d.Server, d.Timeout
	if timeout <= 0 {
		timeout = lookupTimeout
	}
	return resolverAt(server, timeout)
}

func (d DirectResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	return d.resolver().LookupCNAME(ctx, host)
}

func (d DirectResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return d.resolver().LookupHost(ctx, host)
}

// Describe names the resolver that answered, for the page to show.
//
// Worth surfacing because "this name does not resolve" is a claim about a
// specific resolver's opinion, and somebody comparing it against their own dig
// output needs to know the two asked the same question of the same server.
func (d DirectResolver) Describe() string { return d.Server }

// Describe on the standard resolver names the thing it actually consults.
func (n NetResolver) Describe() string { return "this machine's resolver" }

// AuthoritativeResolver asks the nameservers that own the name.
//
// The default, and the only arrangement that actually answers the question this
// package asks. Every recursive resolver caches negative answers — RFC 2308,
// for the zone's SOA minimum, commonly five minutes to an hour — and the check
// Yacht makes moments before somebody creates a record is exactly what puts one
// there. Pointing at a public resolver instead does not remove a cache from the
// path, it swaps one for another, and then fills that one too.
//
// A zone's own nameservers hold no cache. A record created two seconds ago is
// visible two seconds later.
//
// Finding them is one ordinary lookup: walk up the labels until something
// answers with NS records. That lookup goes through the system resolver on
// purpose — NS records are positive answers with long lifetimes, which is
// precisely what a cache is good at.
type AuthoritativeResolver struct {
	// Fallback answers when no authoritative server can be reached at all —
	// an install whose egress does not allow arbitrary port 53, or a name
	// whose delegation cannot be found.
	//
	// Not used when an authoritative server answers and the answer is "no".
	// Falling back there would reintroduce the cached negative this exists to
	// avoid, which is the whole point.
	Fallback Resolver

	Timeout time.Duration
}

func (a AuthoritativeResolver) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return lookupTimeout
}

func (a AuthoritativeResolver) fallback() Resolver {
	if a.Fallback != nil {
		return a.Fallback
	}
	return NetResolver{}
}

// The lookups below bound themselves with a context, and that is not belt and
// braces — it is the only thing that bounds them.
//
// net.Dialer.Timeout limits establishing a connection, and a UDP "dial" is not
// a connection: it succeeds instantly and the wait happens on the read. Go's
// resolver then retries across every server in resolv.conf before giving up, so
// a nameserver that silently drops packets costs forty seconds rather than the
// few this is configured for. Measured, not assumed.

func (a AuthoritativeResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	res, err := a.serversFor(ctx, host)
	if err != nil {
		return a.fallback().LookupCNAME(ctx, host)
	}

	ask, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()

	name, err := res.LookupCNAME(ask, host)
	if err != nil && !answered(err) {
		return a.fallback().LookupCNAME(ctx, host)
	}
	return name, err
}

func (a AuthoritativeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	res, err := a.serversFor(ctx, host)
	if err != nil {
		return a.fallback().LookupHost(ctx, host)
	}

	ask, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()

	addrs, err := res.LookupHost(ask, host)
	if err != nil && !answered(err) {
		return a.fallback().LookupHost(ctx, host)
	}
	return addrs, err
}

func (a AuthoritativeResolver) Describe() string { return "the domain's own nameservers" }

// answered reports whether an error is a real answer rather than a failure to
// get one.
//
// NXDOMAIN from an authoritative server is the truth, and the most important
// truth this package handles: it is what "the record has not been created yet"
// looks like. Treating it as a failure and retrying through a cache would undo
// everything above.
func answered(err error) bool {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return false
	}
	return dnsErr.IsNotFound
}

// serversFor finds a resolver pointed at the nameservers for a name's zone.
//
// Walks up the labels because a name is rarely its own zone: shop.example.com
// is almost always a record inside example.com rather than a delegation of its
// own. The first level that answers with NS records is the zone that holds the
// record, which is the server whose answer is authoritative for it.
func (a AuthoritativeResolver) serversFor(ctx context.Context, host string) (*net.Resolver, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()

	labels := strings.Split(normalize(host), ".")
	// Stops before the last label: querying the root for a TLD's nameservers
	// answers a question nobody asked and costs a round trip to find out.
	for i := 0; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")

		nss, err := net.DefaultResolver.LookupNS(ctx, zone)
		if err != nil || len(nss) == 0 {
			continue
		}
		for _, ns := range nss {
			// The nameserver's own address, through the system resolver. This
			// is a positive answer with a long lifetime, so a cache here is
			// working as intended rather than in the way.
			addrs, err := net.DefaultResolver.LookupHost(ctx, ns.Host)
			if err != nil || len(addrs) == 0 {
				continue
			}
			return resolverAt(net.JoinHostPort(addrs[0], "53"), a.timeout()), nil
		}
	}
	return nil, fmt.Errorf("domain: no authoritative nameserver found for %q", host)
}

// resolverAt builds a resolver that talks to one server and nothing else.
func resolverAt(server string, timeout time.Duration) *net.Resolver {
	return &net.Resolver{
		// PreferGo keeps the lookup inside Go's own client, which is what
		// makes Dial reachable at all: the cgo resolver ignores it.
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: timeout}
			return dialer.DialContext(ctx, network, server)
		},
	}
}

// Describer is implemented by resolvers that can say who they are. Optional, so
// a substituted resolver in a test or a wrapping application does not have to
// implement it.
type Describer interface {
	Describe() string
}

// ResolverName describes a resolver, falling back to something honest.
func ResolverName(res Resolver) string {
	if d, ok := res.(Describer); ok {
		return d.Describe()
	}
	return "the configured resolver"
}

// withDefaultPort accepts "1.1.1.1" as readily as "1.1.1.1:53".
//
// Nobody types the port, and a missing one otherwise fails at dial time with an
// error about an address rather than about configuration.
func withDefaultPort(server string) string {
	if server == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, "53")
}
