package web

import (
	"strings"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/domain"
)

// Turning a domain's state into something a person can act on.
//
// The state machine answers "where is this domain"; this answers "what should
// the reader do about it", which is a different question and the one the old UI
// never addressed. A single amber "not verified" was true of a name that did
// not exist, a name still propagating, and a name pointing at somebody else's
// platform — and offered the same advice to all three.

// domainStatus is the word and colour beside a domain's name.
//
// A dot and a word, matching the rest of the dashboard rather than inventing a
// second vocabulary for this page.
func domainStatus(c domain.Custom) (label, class string) {
	switch c.State {
	case domain.StateRouted:
		return "live", "status-ok"
	case domain.StateVerified:
		// Proven but not yet in the router. Brief, and worth its own word: a
		// person refreshing during it should not be told it is live.
		return "routing", "status-info status-live"
	case domain.StatePending:
		return "checking", "status-info status-live"
	case domain.StateAwaitingDNS:
		return "waiting for DNS", "status-warn"
	case domain.StateMisdirected:
		return "points elsewhere", "status-err"
	case domain.StateDrifted:
		return "needs attention", "status-err"
	}
	return "unknown", "status-neutral"
}

// domainSteps is the progression shown under a domain.
//
// Four steps, always all four, because the shape of what is coming is itself
// information: somebody who has just added a domain can see that a certificate
// is going to be a question before they get there.
func domainSteps(c domain.Custom, httpsOnly bool) []Step {
	return []Step{
		{
			Label: "Domain added",
			State: StepDone,
			At:    c.CreatedAt,
		},
		dnsStep(c),
		routingStep(c),
		certificateStep(c, httpsOnly),
	}
}

// dnsStep is the one that carries the diagnosis.
func dnsStep(c domain.Custom) Step {
	step := Step{Label: "DNS points here", At: c.LastCheckedAt}

	// A lookup that could not be made is not a verdict about the domain, and
	// must not be drawn as one. Said before anything else, because every other
	// line on the page is stale while it is true.
	if c.LastError != "" && !c.State.Routable() {
		step.State = StepActive
		step.Detail = c.LastError + ". Nothing is wrong with the domain as far as this can tell — the check will run again."
		return step
	}

	switch c.State {
	case domain.StatePending:
		step.State = StepActive
		step.Detail = "Looking it up now."

	case domain.StateAwaitingDNS:
		step.State = StepActive
		step.Detail = "No record found yet. Changes at a DNS provider usually take a few minutes to spread, and this keeps checking on its own."

	case domain.StateMisdirected:
		step.State = StepErr
		step.Detail = "This name resolves, but not here — it " + c.Observed +
			". Change the record to point at " + c.Target + "."

	case domain.StateDrifted:
		step.State = StepErr
		if c.Observed != "" {
			step.Detail = "This was live and no longer points here — it " + c.Observed +
				". Traffic has stopped being routed to the app."
		} else {
			step.Detail = "This was live and no longer resolves. Traffic has stopped being routed to the app."
		}

	case domain.StateVerified, domain.StateRouted:
		step.State = StepDone
		step.Detail = "Resolves to " + c.Target + "."
	}
	return step
}

func routingStep(c domain.Custom) Step {
	step := Step{Label: "Routed to the app"}
	switch c.State {
	case domain.StateRouted:
		step.State = StepDone
		step.Detail = "Requests for this name reach the app."
		step.At = c.VerifiedAt
	case domain.StateVerified:
		// Proven, and the apply has not finished or has not succeeded. The
		// checker retries either way, which is why this is not an error.
		step.State = StepActive
		step.Detail = "Adding it to the router."
	default:
		step.State = StepWait
		step.Detail = "Nothing is routed until the record resolves."
	}
	return step
}

// certificateStep tells the truth about HTTPS on a brought domain.
//
// There is no per-domain certificate here at all: the install's one certificate
// is a wildcard for the platform domain, and a custom domain can never be under
// it. That used to be silent — the domain showed a green "routed" and the
// browser showed a warning — which is the worst division of labour available.
func certificateStep(c domain.Custom, httpsOnly bool) Step {
	step := Step{Label: "Certificate"}

	if c.State != domain.StateRouted {
		step.State = StepWait
		step.Detail = "Checked once the name resolves."
		return step
	}

	if httpsOnly {
		// Enforce HTTPS is on, so plain HTTP is not served at all and there is
		// no certificate that matches this name. Every visitor sees a warning.
		step.State = StepErr
		step.Detail = "No certificate covers this name, and this app is served over HTTPS only — " +
			"browsers will refuse the connection. Put a certificate for this name in front of the cluster, " +
			"or turn off Enforce HTTPS to serve it over plain HTTP."
		return step
	}

	step.State = StepWait
	step.Detail = "Served over plain HTTP. No certificate covers this name — " +
		"the install's certificate only covers its own platform domain."
	return step
}

// domainRecord is the record to create, in the three fields every provider asks
// for.
//
// The name is the part before the registrable domain, which is what a provider's
// form wants — pasting the whole hostname into it is the most common way this
// goes wrong, and produces shop.example.com.example.com.
//
// Two labels is treated as an apex. It is a heuristic: shop.co.uk is two labels
// and not an apex anybody owns, and knowing better needs the public suffix list.
// The popover beside this says which is which, which is cheaper than being
// wrong quietly.
func domainRecord(host, target string) DNSRecord {
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	if len(labels) <= 2 {
		// An apex cannot carry a CNAME. Providers call the flattened
		// equivalent ALIAS, ANAME, or "CNAME flattening" depending on who they
		// are; verification accepts any of them because it compares addresses.
		return DNSRecord{Type: "ALIAS", Name: "@", Value: target}
	}
	return DNSRecord{
		Type:  "CNAME",
		Name:  strings.Join(labels[:len(labels)-2], "."),
		Value: target,
	}
}

// domainsSettled reports whether every domain has reached a state that will not
// change on its own.
//
// What the polling fragment uses to stop asking. A page left open on a settled
// list should not keep a request every three seconds going all afternoon.
func domainsSettled(n app.Networking) bool {
	for _, c := range n.Custom {
		if !c.State.Settled() {
			return false
		}
	}
	return true
}

// domainNeedsAttention marks the states worth finding in a long list.
func domainNeedsAttention(c domain.Custom) bool {
	return c.State == domain.StateMisdirected || c.State == domain.StateDrifted
}
