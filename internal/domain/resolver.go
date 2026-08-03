package domain

import (
	"context"
	"net"
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
	return &net.Resolver{
		// PreferGo keeps the lookup inside Go's own DNS client, which is what
		// makes Dial reachable at all: the cgo resolver ignores it and goes
		// back to the system path this exists to avoid.
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: timeout}
			return dialer.DialContext(ctx, network, server)
		},
	}
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
