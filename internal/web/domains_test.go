package web

import (
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/domain"
)

// The name field is the part before the person's own domain.
//
// Pasting the whole hostname into a provider's Name field is the single most
// common way this goes wrong, and it produces shop.example.com.example.com —
// which then does not resolve, for a reason nothing on the page explains.
func TestDomainRecord(t *testing.T) {
	for _, tc := range []struct {
		host string
		want DNSRecord
	}{
		{
			host: "shop.example.com",
			want: DNSRecord{Type: "CNAME", Name: "shop", Value: "edge.yacht.test"},
		},
		{
			host: "a.b.example.com",
			want: DNSRecord{Type: "CNAME", Name: "a.b", Value: "edge.yacht.test"},
		},
		{
			// An apex cannot carry a CNAME. Providers call the flattened
			// equivalent ALIAS, ANAME or CNAME flattening; verification accepts
			// any of them because it compares addresses.
			host: "example.com",
			want: DNSRecord{Type: "ALIAS", Name: "@", Value: "edge.yacht.test"},
		},
		{
			host: "example.com.",
			want: DNSRecord{Type: "ALIAS", Name: "@", Value: "edge.yacht.test"},
		},
	} {
		t.Run(tc.host, func(t *testing.T) {
			got := domainRecord(tc.host, "edge.yacht.test")
			if got != tc.want {
				t.Errorf("domainRecord(%q) = %+v, want %+v", tc.host, got, tc.want)
			}
		})
	}
}

// A lookup that could not be made is not a verdict about the domain, and must
// not be drawn as one. Said before anything else, because every other line on
// the page is stale while it is true.
func TestAFailedLookupIsNotShownAsTheDomainBeingWrong(t *testing.T) {
	step := dnsStep(domain.Custom{
		State:     domain.StateAwaitingDNS,
		LastError: "domain: the configured target \"edge.missing.test\" does not resolve",
	})

	if step.State == StepErr {
		t.Error("a resolver failure is drawn as the domain being wrong")
	}
	if !strings.Contains(step.Detail, "Nothing is wrong with the domain") {
		t.Errorf("detail = %q, want it to say the domain is not the problem", step.Detail)
	}
}

// Enforcing HTTPS on a name with no certificate is the case that actually
// breaks for visitors, so it is the one drawn as an error.
func TestTheCertificateStepIsHarshestWhenHTTPSIsEnforced(t *testing.T) {
	live := domain.Custom{State: domain.StateRouted}

	enforced := certificateStep(live, true)
	if enforced.State != StepErr {
		t.Errorf("state = %q, want an error when HTTPS is enforced with no certificate", enforced.State)
	}
	if !strings.Contains(enforced.Detail, "browsers will refuse") {
		t.Errorf("detail = %q, want it to say what a visitor sees", enforced.Detail)
	}

	plain := certificateStep(live, false)
	if plain.State == StepErr {
		t.Error("plain HTTP works, so it should not be drawn as an error")
	}
}

// Nothing about a certificate is claimed before the name even resolves.
func TestTheCertificateStepWaitsUntilTheDomainIsLive(t *testing.T) {
	step := certificateStep(domain.Custom{State: domain.StateAwaitingDNS}, true)
	if step.State != StepWait {
		t.Errorf("state = %q, want it to wait until there is something to serve", step.State)
	}
}

// The poll stops only when every domain has settled. One still moving keeps the
// whole list watched, because they are rendered together.
func TestDomainsSettled(t *testing.T) {
	routed := domain.Custom{State: domain.StateRouted}
	waiting := domain.Custom{State: domain.StateAwaitingDNS}

	if !domainsSettled(app.Networking{Custom: []domain.Custom{routed, routed}}) {
		t.Error("a list of live domains is not settled")
	}
	if domainsSettled(app.Networking{Custom: []domain.Custom{routed, waiting}}) {
		t.Error("a list with one domain still moving is settled")
	}
	if !domainsSettled(app.Networking{}) {
		t.Error("an empty list is not settled")
	}

	// Drifted is a finished check whose result somebody has to act on, not a
	// resting state — it keeps being watched so a repair shows up on its own.
	drifted := domain.Custom{State: domain.StateDrifted}
	if domainsSettled(app.Networking{Custom: []domain.Custom{drifted}}) {
		t.Error("a drifted domain is treated as settled")
	}
}

// The progression is always all four steps, so somebody who has just added a
// domain can see that a certificate is going to be a question before they get
// there.
func TestDomainStepsAlwaysShowTheWholeJourney(t *testing.T) {
	steps := domainSteps(domain.Custom{
		State: domain.StatePending, CreatedAt: time.Now(),
	}, false)

	if len(steps) != 4 {
		t.Fatalf("steps = %d, want 4", len(steps))
	}
	if steps[0].State != StepDone {
		t.Error("the claim itself is not marked done")
	}
	for _, s := range steps[1:] {
		if s.State == StepDone {
			t.Errorf("%q is done on a domain nothing has checked yet", s.Label)
		}
	}
}
