package orchestrator

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// IsolationMode is how far one team's workloads are kept from everything
// else on the cluster's network.
type IsolationMode string

const (
	// IsolationTeam refuses every connection into a team's workloads that
	// comes from another team's, or from a build. Its own apps, and the
	// platform — the ingress controller, the engine's waker — still reach
	// them. On a cluster shared by teams, a database one of them deploys
	// must not be reachable by the rest.
	//
	// What an unset mode means, though no longer what a new install is
	// given: the installer writes strict into a new install's settings. An
	// install made before that has no line for it and has run on team all
	// along, and an upgrade must not tighten its network unasked — a build
	// pushing to a registry on the LAN would stop.
	IsolationTeam IsolationMode = "team"

	// IsolationStrict is IsolationTeam, and limits what a team's workloads
	// and builds may connect out to as well: DNS, the team's own apps,
	// public addresses, and the ingress controller's entrypoints, which serve
	// the cluster's own public addresses. Private addresses — the nodes' network, the
	// platform's own database, cloud metadata, anything else on the LAN — are
	// refused unless named in Isolation.EgressAllow. What the installer
	// writes for a new install.
	IsolationStrict IsolationMode = "strict"

	// IsolationOff writes no network policy, and removes any written before.
	IsolationOff IsolationMode = "off"
)

// Isolation is the network isolation the orchestrator enforces.
type Isolation struct {
	Mode IsolationMode
	// EgressAllow are private ranges strict isolation still lets workloads
	// and builds reach: an image registry on the LAN, say.
	EgressAllow []netip.Prefix
}

// OrDefault is i with an unset mode taken as IsolationTeam: isolation is
// what a cluster gets unless somebody turns it off, and team is what an
// install that never chose has been running on.
func (i Isolation) OrDefault() Isolation {
	if i.Mode == "" {
		i.Mode = IsolationTeam
	}
	return i
}

// ParseIsolation reads a mode and a comma-separated list of CIDRs, as the
// engine's configuration gives them.
func ParseIsolation(mode, allow string) (Isolation, error) {
	var i Isolation
	switch m := IsolationMode(strings.ToLower(strings.TrimSpace(mode))); m {
	case "":
		i.Mode = IsolationTeam
	case IsolationTeam, IsolationStrict, IsolationOff:
		i.Mode = m
	default:
		return Isolation{}, fmt.Errorf("network isolation %q is not team, strict or off", mode)
	}
	for _, raw := range strings.Split(allow, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return Isolation{}, fmt.Errorf("egress allow %q is not a CIDR range, such as 10.0.0.0/24: %w", raw, err)
		}
		i.EgressAllow = append(i.EgressAllow, p.Masked())
	}
	return i, nil
}

// NetworkIsolator is an orchestrator that enforces network isolation. It
// brings every namespace it manages into line with its isolation at startup,
// so workloads deployed before isolation existed, or before its mode
// changed, are covered without waiting for their next deploy; and changes it
// while running, for an operator setting it from a page rather than the
// configuration.
type NetworkIsolator interface {
	EnsureIsolation(ctx context.Context) error
	Isolation() Isolation
	SetIsolation(ctx context.Context, i Isolation) error
}
