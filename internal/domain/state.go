package domain

import "time"

// How far a custom domain has got toward serving traffic.
//
// This replaced a boolean, and the reason is worth stating: "not verified" was
// the same value for a name that does not exist, a name still propagating, and
// a name pointing at somebody else's load balancer. Those need three different
// things done about them, and the UI could not tell them apart because the
// database never recorded which one it was.
type State string

const (
	// StatePending is a claim nothing has looked at yet. It lasts seconds —
	// the checker picks it up on its next pass — but it is a real state rather
	// than an absence, because "we have not looked" and "we looked and found
	// nothing" are different answers to the only question being asked.
	StatePending State = "pending"

	// StateAwaitingDNS means the name does not resolve at all. Usually the
	// record has not been created, or has not propagated.
	StateAwaitingDNS State = "awaiting_dns"

	// StateMisdirected means the name resolves, but not here. The most
	// actionable state there is, and the one the old boolean could not express:
	// the row's observed column holds what it actually points at.
	StateMisdirected State = "misdirected"

	// StateVerified means DNS points here and nothing has applied it yet.
	// A brief step, and separate from routed on purpose — a domain proven but
	// absent from the Ingress is a real situation, and one that used to be
	// invisible.
	StateVerified State = "verified"

	// StateRouted means proven and carried by the Ingress. The finished state.
	StateRouted State = "routed"

	// StateDrifted means this was routed and a later check found it no longer
	// points here. Nothing could reach this state before, because nothing ever
	// re-checked and nothing ever cleared the boolean: a domain whose DNS was
	// deleted stayed "verified" and stayed in the Ingress forever.
	StateDrifted State = "drifted"
)

// Settled reports whether a state needs no further attention.
//
// Only routed is settled. Drifted is not: it is a finished check with a result
// somebody has to act on.
func (s State) Settled() bool { return s == StateRouted }

// Routable reports whether this state may carry traffic.
//
// The database answers the same question in SQL — verified is generated from
// exactly this set — and the Ingress is built from that query rather than from
// this method. Both exist because the gate belongs in SQL where no caller can
// forget it, and the UI still needs to explain the answer.
func (s State) Routable() bool { return s == StateVerified || s == StateRouted }

// Classify decides a domain's state from what a lookup saw.
//
// A pure function of the observation and where the domain already was, so the
// whole state machine is testable without a database, a network or a clock.
//
// previous matters in exactly one way: falling out of routed is drift, and drift
// is worth distinguishing from never having worked. Somebody whose live site
// just stopped resolving needs a different sentence from somebody who has not
// created the record yet.
func Classify(obs Observation, previous State) State {
	// A resolver that could not answer is not evidence about the domain. Same
	// rule the build reconciler follows for an unreachable API server: an
	// outage here must not turn every custom domain on the install into a
	// failure. The caller records the error and tries again later.
	if obs.Err != nil {
		if previous == "" {
			return StatePending
		}
		return previous
	}

	switch {
	case obs.PointsHere:
		// Already routed stays routed. Returning verified would ask the caller
		// to re-apply an Ingress that already carries the host, on every check,
		// forever.
		if previous == StateRouted {
			return StateRouted
		}
		return StateVerified

	case !obs.Resolves:
		if previous == StateRouted {
			return StateDrifted
		}
		return StateAwaitingDNS

	default:
		if previous == StateRouted {
			return StateDrifted
		}
		return StateMisdirected
	}
}

// Backoff intervals. Someone who has just created a record is watching the
// page; a domain that has been live for a month is not, and asking DNS about it
// every ten seconds forever is a cost with no reader.
const (
	checkEager  = 10 * time.Second
	checkSoon   = 30 * time.Second
	checkSteady = time.Minute
	checkSlow   = 5 * time.Minute
	checkIdle   = 15 * time.Minute

	// driftRecheck is how often a domain that has fallen over is retried. Faster
	// than idle because somebody is probably fixing it right now, and slower
	// than eager because it was working for a while and may be an outage rather
	// than a mistake.
	driftRecheck = 5 * time.Minute

	// settledRecheck is how often a live domain is re-proven. This is the drift
	// detector, and its cost is one lookup per domain per interval — so it is
	// set by what a stale route costs rather than by how fast anybody needs to
	// know.
	settledRecheck = 6 * time.Hour
)

// NextCheck says how long to wait before looking again.
//
// attempts is how many consecutive checks have now run without the domain
// settling; it resets when the state changes or somebody asks for a check.
func NextCheck(state State, attempts int) time.Duration {
	switch state {
	case StateRouted:
		return settledRecheck
	case StateDrifted:
		return driftRecheck
	}

	switch {
	case attempts <= 3:
		return checkEager
	case attempts <= 6:
		return checkSoon
	case attempts <= 12:
		return checkSteady
	case attempts <= 24:
		return checkSlow
	}
	return checkIdle
}
