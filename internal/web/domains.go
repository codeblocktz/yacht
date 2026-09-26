package web

import (
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
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
func domainSteps(c domain.Custom, n app.Networking) []Step {
	return []Step{
		{
			Label: "Domain added",
			State: StepDone,
			At:    c.CreatedAt,
		},
		dnsStep(c),
		routingStep(c),
		certificateStep(c, n),
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

// certPhase is where HTTPS stands for one custom domain.
//
// Decided once and read by both the certificate step and the chip beside the
// domain's name, so the header cannot say "HTTPS" while the step below it is red.
type certPhase int

const (
	certNotRouted certPhase = iota // nothing is served yet, so nothing to say
	certNoIssuer                   // plain HTTP: the install has no issuer
	certUnknown                    // requested; what became of it could not be read
	certRequested                  // requested, and still within the grace period
	certStuck                      // requested, and long enough that it should be here
	certUntrusted                  // issued by an authority browsers reject
	certExpired                    // issued, and renewal has been failing
	certTrusted                    // served over HTTPS a browser accepts
)

// issuanceGrace is how long a certificate is simply "being requested" before
// the page starts asking why not. An HTTP-01 challenge normally completes in
// well under a minute; ten covers a slow issuer without leaving somebody
// watching a spinner that will never finish.
const issuanceGrace = 10 * time.Minute

func certPhaseOf(c domain.Custom, n app.Networking) (certPhase, orchestrator.Certificate) {
	if c.State != domain.StateRouted {
		return certNotRouted, orchestrator.Certificate{}
	}
	if !n.Issuing {
		return certNoIssuer, orchestrator.Certificate{}
	}
	cert, known := n.Certs[c.Host]
	switch {
	case !known:
		return certUnknown, cert
	case cert.Issued && time.Now().After(cert.NotAfter):
		return certExpired, cert
	case cert.Issued && !cert.Trusted:
		return certUntrusted, cert
	case cert.Issued:
		return certTrusted, cert
	case time.Since(c.VerifiedAt) > issuanceGrace:
		return certStuck, cert
	}
	return certRequested, cert
}

// domainTransport is the chip beside a domain's name: how a visitor reaches it.
//
// Only for a live domain. Before that the status word already says what is
// happening, and a chip claiming "HTTP" for a name nothing is served on yet
// would be a promise.
func domainTransport(c domain.Custom, n app.Networking) (label, class, iconName string, ok bool) {
	phase, _ := certPhaseOf(c, n)
	switch phase {
	case certTrusted:
		return "HTTPS", "status-ok", "lock", true
	case certRequested, certUnknown:
		return "HTTPS pending", "status-info", "lock-open", true
	case certUntrusted, certExpired, certStuck:
		return "certificate problem", "status-err", "lock-open", true
	case certNoIssuer:
		if n.HTTPSOnly {
			return "no certificate", "status-err", "lock-open", true
		}
		return "HTTP only", "status-neutral", "lock-open", true
	}
	return "", "", "", false
}

// describeCertificate is the command that answers "why" for a certificate, run
// against the app's own namespace so it can be pasted as it is.
func describeCertificate(c domain.Custom, n app.Networking) string {
	return "kubectl -n " + n.Namespace + " describe certificate " + orchestrator.CertSecretName(c.Host)
}

// productionInstall is the installer re-run that switches issuance to Let's
// Encrypt production. The address is a placeholder because it is the
// operator's, and production refuses to issue without one.
const productionInstall = "curl -sSL https://codeblocktz.github.io/yacht/install.sh | sudo sh -s -- " +
	"--acme-environment production --acme-email you@example.com"

// certificateStep tells the truth about HTTPS on a brought domain.
//
// The install's own certificate is a wildcard for the platform domain, and a
// custom domain can never be under it. Where the install has an issuer, each
// custom domain is given a certificate of its own and this reports what became
// of it; where it has none, it says the name is served over plain HTTP. That
// used to be silent — the domain showed a green "routed" and the browser showed
// a warning — which is the worst division of labour available.
//
// The detail says what is true in a sentence or two. What to do about it goes
// in Commands, where it can be copied, rather than inside the prose.
func certificateStep(c domain.Custom, n app.Networking) Step {
	step := Step{Label: "Certificate"}
	phase, cert := certPhaseOf(c, n)

	switch phase {
	case certNotRouted:
		step.State = StepWait
		step.Detail = "Checked once the name resolves."

	case certNoIssuer:
		if n.HTTPSOnly {
			// Enforce HTTPS is on, so plain HTTP is not served at all and there
			// is no certificate that matches this name.
			step.State = StepErr
			step.Detail = "No certificate covers this name, and this app is served over HTTPS only — " +
				"browsers will refuse the connection. Turn off Enforce HTTPS to serve it over plain HTTP, " +
				"or give the install a certificate issuer."
			return step
		}
		step.State = StepWait
		step.Detail = "Served over plain HTTP. No certificate covers this name — " +
			"the install's certificate only covers its own platform domain."

	case certUnknown:
		// Not an error about the domain, and not drawn as one.
		step.State = StepActive
		step.Detail = "A certificate has been requested. Its status could not be read just now."

	case certRequested:
		step.State = StepActive
		step.Detail = "Requesting one from Let's Encrypt. This usually takes under a minute"
		if n.HTTPSOnly {
			step.Detail += " — until then, browsers warn visitors."
		} else {
			step.Detail += "; plain HTTP works meanwhile."
		}

	case certStuck:
		step.State = StepErr
		step.Detail = "Still none " + strings.TrimSuffix(relativeTime(c.VerifiedAt), " ago") +
			" after this name went live. Let's Encrypt has to reach it on port 80, " +
			"without being redirected to HTTPS first."
		step.Commands = []string{describeCertificate(c, n)}

	case certExpired:
		step.State = StepErr
		step.Detail = "Expired " + relativeTime(cert.NotAfter) + " and not renewed. " +
			"Renewal needs this name to keep reaching the cluster on port 80."
		step.Commands = []string{describeCertificate(c, n)}

	case certUntrusted:
		// What the installer's default, Let's Encrypt staging, produces. To a
		// visitor it is a warning page, so it is never drawn as done.
		step.State = StepWait
		if n.HTTPSOnly {
			step.State = StepErr
		}
		step.Detail = "Issued, but by an authority browsers don't trust — usually Let's Encrypt staging, " +
			"the installer's default. To switch to trusted certificates, re-run the installer for production, " +
			"then delete this one so it is issued again now rather than at renewal."
		step.Commands = []string{
			productionInstall,
			"kubectl -n " + n.Namespace + " delete secret " + orchestrator.CertSecretName(c.Host),
		}

	case certTrusted:
		step.State = StepDone
		step.Detail = "Valid until " + cert.NotAfter.Local().Format("2 Jan 2006") +
			" — " + untilTime(cert.NotAfter) + ". Renews automatically."
	}
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
		// A live name still waiting on its certificate keeps the list
		// watched, so the tick appears when it is issued rather than on the
		// next manual refresh.
		if n.Issuing && c.State == domain.StateRouted && !n.Certs[c.Host].Issued {
			return false
		}
	}
	return true
}

// deploymentsInFlight reports whether anything on this page can still change.
//
// What stops the deployments panel polling. A build is the long case — the
// deployment stays running until its Job ends — so this is also what keeps the
// page alive across the half hour that a slow build takes.
func deploymentsInFlight(d AppDetailData) bool {
	for _, dep := range d.Deployments {
		if dep.Status == app.DeployRunning || dep.Status == "pending" {
			return true
		}
	}
	return false
}

// domainNeedsAttention marks the states worth finding in a long list.
func domainNeedsAttention(c domain.Custom) bool {
	return c.State == domain.StateMisdirected || c.State == domain.StateDrifted
}
