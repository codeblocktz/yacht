package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

type fakeNets struct {
	net     app.Networking
	added   []string
	removed []uuid.UUID
	err     error
}

func (f *fakeNets) Networking(context.Context, string, string) (app.Networking, error) {
	return f.net, nil
}

func (f *fakeNets) SetNetworking(_ context.Context, _, _ string, https, cname bool) error {
	f.net.HTTPSOnly, f.net.CNAMEOnly = https, cname
	return f.err
}

func (f *fakeNets) AddDomain(_ context.Context, _, _, host string) error {
	if f.err != nil {
		return f.err
	}
	f.added = append(f.added, host)
	return nil
}

func (f *fakeNets) VerifyDomain(context.Context, string, string, uuid.UUID) error { return f.err }

func (f *fakeNets) RemoveDomain(_ context.Context, _, _ string, id uuid.UUID) error {
	f.removed = append(f.removed, id)
	return f.err
}

func (f *fakeNets) ResolverName() string { return "this machine's resolver" }

func netServer(t *testing.T, n Nets) http.Handler {
	t.Helper()
	const team = "net-team"
	s, err := New(Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            newFakeApps(sampleApp(team, "web")),
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: account.RoleAdmin},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		Nets:            n,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

// The controls have to be in the tab somebody opens to find them.
//
// This shipped as a page of its own that nothing linked to: the whole feature
// worked and was unreachable. Asserting the forms are in the tab is the check
// that a route existing is not the same as a way in.
func TestTheDomainsTabOffersDomainControls(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test",
		HTTPSOnly: true, CNAMEOnly: true,
		Custom: []domain.Custom{{ID: uuid.New(), Host: "shop.example.com", Target: "edge.yacht.test"}},
	}})

	body := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String()

	for _, want := range []string{
		`action="/apps/web/domains"`, // add
		`/domains/`, `/verify"`,      // ask for a check now
		`/delete"`,                      // remove it
		`action="/apps/web/networking"`, // the toggles
		"web.apps.test",                 // the platform hostname
		"shop.example.com",              // the claim itself
		"edge.yacht.test",               // what to point it at
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Domains tab does not offer %q", want)
		}
	}
}

// And the tab is reachable from the app's own navigation, which is the half
// that was missing when this lived on a page of its own.
func TestTheDomainsTabIsLinkedFromTheApp(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test",
	}})

	body := do(h, signedIn(http.MethodGet, "/apps/web")).Body.String()
	if !strings.Contains(body, `href="/apps/web/domains"`) {
		t.Error("nothing on the app page links to its domains")
	}
}

// With nowhere to point a domain, the form is replaced by the reason rather
// than shown and doomed.
func TestWithNoTargetTheDomainFormIsNotOffered(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{Managed: "web.apps.test"}})

	body := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String()

	if strings.Contains(body, `action="/apps/web/domains"`) {
		t.Error("a domain form is offered with no target to verify against")
	}
	if !strings.Contains(body, "No CNAME target is configured") {
		t.Error("nothing explains why domains cannot be added")
	}
}

// Every state says something different, and says what was found.
//
// The whole complaint about the old UI in one test: a single amber "not
// verified" was true of a name that did not exist, a name still propagating and
// a name pointing at somebody else's platform, and offered the same advice to
// all three.
func TestEachDomainStateSaysSomethingDifferent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state domain.State
		obs   string
		want  []string
	}{
		{
			name:  "no record yet",
			state: domain.StateAwaitingDNS,
			want:  []string{"waiting for DNS", "No record found yet"},
		},
		{
			name:  "pointing at somebody else",
			state: domain.StateMisdirected,
			obs:   "points at ghs.googlehosted.com",
			want: []string{
				"points elsewhere",
				"points at ghs.googlehosted.com", // what it actually found
				"edge.yacht.test",                // and what it should be
			},
		},
		{
			name:  "live",
			state: domain.StateRouted,
			want:  []string{"live", "Requests for this name reach the app"},
		},
		{
			name:  "was live and is not any more",
			state: domain.StateDrifted,
			obs:   "points at ghs.googlehosted.com",
			want: []string{
				"needs attention",
				"was live and no longer points here",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := netServer(t, &fakeNets{net: app.Networking{
				Managed: "web.apps.test", Target: "edge.yacht.test",
				Custom: []domain.Custom{{
					ID: uuid.New(), Host: "shop.example.com", Target: "edge.yacht.test",
					State: tc.state, Observed: tc.obs,
				}},
			}})

			body := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("a %q domain does not say %q", tc.state, want)
				}
			}
		})
	}
}

// The poll stops on its own once nothing can change.
//
// A page left open on a settled list must not keep a request every three
// seconds going all afternoon. The fragment replaces the element that carries
// the trigger, so a settled render simply has none.
func TestTheDomainPollStopsWhenNothingIsMoving(t *testing.T) {
	live := &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test",
		Custom: []domain.Custom{{
			ID: uuid.New(), Host: "shop.example.com",
			Target: "edge.yacht.test", State: domain.StateRouted,
		}},
	}}
	body := do(netServer(t, live), signedIn(http.MethodGet, "/apps/web/domains")).Body.String()
	if strings.Contains(body, `hx-trigger="every 3s"`) {
		t.Error("a settled list is still polling")
	}

	waiting := &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test",
		Custom: []domain.Custom{{
			ID: uuid.New(), Host: "shop.example.com",
			Target: "edge.yacht.test", State: domain.StateAwaitingDNS,
		}},
	}}
	body = do(netServer(t, waiting), signedIn(http.MethodGet, "/apps/web/domains")).Body.String()
	if !strings.Contains(body, `hx-trigger="every 3s"`) {
		t.Error("a domain that is still moving is not being watched")
	}
}

// The fragment returns the element carrying the trigger, not just its contents.
// Swapping only the inside would leave the trigger on a wrapper nothing
// replaces, asking forever.
func TestTheDomainFragmentCarriesItsOwnTrigger(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test",
		Custom: []domain.Custom{{
			ID: uuid.New(), Host: "shop.example.com",
			Target: "edge.yacht.test", State: domain.StateAwaitingDNS,
		}},
	}})

	rec := do(h, signedIn(http.MethodGet, "/apps/web/domains/fragment"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Error("the fragment does not replace the element that polls")
	}
	if !strings.Contains(body, "shop.example.com") {
		t.Error("the fragment does not contain the domain")
	}
}

// A brought domain never has a certificate here, and the page says so instead
// of showing a green tick beside a browser warning.
func TestALiveCustomDomainAdmitsItHasNoCertificate(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test", HTTPSOnly: true,
		Custom: []domain.Custom{{
			ID: uuid.New(), Host: "shop.example.com",
			Target: "edge.yacht.test", State: domain.StateRouted,
		}},
	}})

	body := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String()
	if !strings.Contains(body, "No certificate covers this name") {
		t.Error("a live custom domain does not admit it has no certificate")
	}
}

// The record to create is shown as a record, in the three fields every provider
// asks for.
func TestTheRecordToCreateIsShownAsARecord(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test",
		Custom: []domain.Custom{{
			ID: uuid.New(), Host: "shop.example.com",
			Target: "edge.yacht.test", State: domain.StateAwaitingDNS,
		}},
	}})

	body := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String()
	for _, want := range []string{"CNAME", "Create this record", `data-copy="edge.yacht.test"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the record is missing %q", want)
		}
	}
}

// A refusal keeps the panel, so the answer arrives where the question was.
func TestARefusedDomainStillShowsThePanel(t *testing.T) {
	h := netServer(t, &fakeNets{
		err: domain.ErrHostTaken,
		net: app.Networking{Managed: "web.apps.test", Target: "edge.yacht.test"},
	})

	rec := do(h, signedInForm(http.MethodPost, "/apps/web/domains", "host=taken.example.com"))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="/apps/web/domains"`) {
		t.Error("the refusal dropped the panel the question was asked on")
	}
}
