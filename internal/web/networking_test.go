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
// worked and was unreachable. Asserting the forms are in the Settings tab is
// the check that a route existing is not the same as a way in.
func TestTheSettingsTabOffersDomainControls(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test",
		HTTPSOnly: true, CNAMEOnly: true,
		Custom: []domain.Custom{{ID: uuid.New(), Host: "shop.example.com", Target: "edge.yacht.test"}},
	}})

	body := do(h, signedIn(http.MethodGet, "/apps/web/settings")).Body.String()

	for _, want := range []string{
		`action="/apps/web/domains"`, // add
		`/domains/`, `/verify"`,      // verify the claim
		`/delete"`,                      // remove it
		`action="/apps/web/networking"`, // the toggles
		"web.apps.test",                 // the platform hostname
		"shop.example.com",              // the claim itself
		"edge.yacht.test",               // what to point it at
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Settings tab does not offer %q", want)
		}
	}
}

// With nowhere to point a domain, the form is replaced by the reason rather
// than shown and doomed.
func TestWithNoTargetTheDomainFormIsNotOffered(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{Managed: "web.apps.test"}})

	body := do(h, signedIn(http.MethodGet, "/apps/web/settings")).Body.String()

	if strings.Contains(body, `action="/apps/web/domains"`) {
		t.Error("a domain form is offered with no target to verify against")
	}
	if !strings.Contains(body, "No CNAME target is configured") {
		t.Error("nothing explains why domains cannot be added")
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
