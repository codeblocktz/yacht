package engine

import (
	"context"

	"github.com/codeblocktz/yacht/internal/cluster"
)

// DNSSettings is how the install publishes hostnames, as Admin › DNS shows
// it: the name customers' own domains CNAME to, and ExternalDNS's ownership
// prefix.
type DNSSettings = cluster.DNS

// DNS is the install's DNS settings. An install that has never saved them has
// an empty CNAME target and the default prefix, not an error.
func (e *Engine) DNS(ctx context.Context) (DNSSettings, error) {
	return cluster.New(e.Pool, e.Keeper, e.Log).DNS(ctx)
}

// SetDNS stores the install's DNS settings, as Admin › DNS does: for a
// wrapper that knows the name customers' domains should point at and fills
// it in rather than leave a first-day step for an operator to miss. The
// form's checks apply and nothing is written when nothing differs. A changed
// target re-applies the apps that route by CNAME; it restarts none of them.
func (e *Engine) SetDNS(ctx context.Context, cnameTarget, txtPrefix string) error {
	return cluster.New(e.Pool, e.Keeper, e.Log).SetDNS(ctx, cnameTarget, txtPrefix)
}
