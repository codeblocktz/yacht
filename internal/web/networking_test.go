package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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
	net          app.Networking
	added        []string
	removed      []uuid.UUID
	err          error
	targetBroken bool

	// What the public networking forms asked for.
	generated []int32
	set       []string
	unset     int
	proxied   int
	unproxied int
}

func (f *fakeNets) GenerateDomain(_ context.Context, _, _ string, port int32) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.generated = append(f.generated, port)
	return "web-production.up.apps.test", nil
}

func (f *fakeNets) SetPublicDomain(_ context.Context, _, _, label string, port int32) error {
	if f.err != nil {
		return f.err
	}
	f.set = append(f.set, label+":"+strconv.Itoa(int(port)))
	return nil
}

func (f *fakeNets) RemovePublicDomain(context.Context, string, string) error {
	f.unset++
	return f.err
}

func (f *fakeNets) EnableTCPProxy(context.Context, string, string) (app.TCPProxy, error) {
	if f.err != nil {
		return app.TCPProxy{}, f.err
	}
	f.proxied++
	return app.TCPProxy{Available: true, Host: "proxy.apps.test", Port: 30042, Target: 5432}, nil
}

func (f *fakeNets) DisableTCPProxy(context.Context, string, string) error {
	f.unproxied++
	return f.err
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

func (f *fakeNets) AllDomains(context.Context, string) ([]app.InstallDomain, error) {
	out := make([]app.InstallDomain, 0, len(f.net.Custom))
	for _, c := range f.net.Custom {
		out = append(out, app.InstallDomain{Custom: c, App: "web"})
	}
	return out, nil
}

func (f *fakeNets) TargetStatus(_ context.Context, target string) string {
	if target == "" {
		return ""
	}
	if f.targetBroken {
		return "does not resolve"
	}
	return "resolves to 198.51.100.1"
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

// And domains are reachable from the app's own navigation, which is the
// half that was missing when this lived on a page of its own. They are a
// section of Settings now, as on Railway, and the old address lands there.
func TestTheDomainsTabIsLinkedFromTheApp(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Managed: "web.apps.test", Target: "edge.yacht.test",
	}})

	body := do(h, signedIn(http.MethodGet, "/apps/web")).Body.String()
	if !strings.Contains(body, `href="/apps/web/settings"`) {
		t.Error("nothing on the app page links to its settings")
	}
	settings := do(h, signedIn(http.MethodGet, "/apps/web/settings")).Body.String()
	if !strings.Contains(settings, `href="#domains"`) || !strings.Contains(settings, `id="domains"`) {
		t.Error("the settings do not hold the domains")
	}
	if old := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String(); !strings.Contains(old, `id="domains"`) {
		t.Error("the old Domains address no longer shows them")
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

// dnsServer builds a server with the DNS page mounted, which needs a joiner
// and an owner — that page is owner-only.
func dnsServer(t *testing.T, n Nets) http.Handler {
	t.Helper()
	const team = "net-team"
	s, err := New(Options{
		Orchestrator:    orchestrator.NewNoop(),
		Apps:            newFakeApps(sampleApp(team, "web")),
		Identity:        identity.NewSingleOwner(identity.Owner{ID: team}),
		Accounts:        &roledAccounts{fakeAccounts: &fakeAccounts{}, team: team, role: account.RoleOwner},
		Mailer:          &fakeMailer{},
		BaseURL:         "https://yacht.test",
		BootstrapTeamID: team,
		Nets:            n,
		// fakeJoiner already answers with edge.yacht.test / extdns-.
		Joiner: &fakeJoiner{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

// The install-wide view is where the background checker's work becomes
// visible. Without it a domain lives on one app's page, and an operator
// watching several has to visit each in turn.
func TestTheDNSPageListsEveryDomainOnTheInstall(t *testing.T) {
	h := dnsServer(t, &fakeNets{net: app.Networking{
		Target: "edge.yacht.test",
		Custom: []domain.Custom{
			{ID: uuid.New(), Host: "shop.example.com", State: domain.StateDrifted,
				Observed: "points at ghs.googlehosted.com"},
		},
	}})

	body := do(h, signedIn(http.MethodGet, "/cluster/dns")).Body.String()
	for _, want := range []string{
		"shop.example.com",
		"needs attention",
		"points at ghs.googlehosted.com",
		`href="/apps/web/domains"`, // and it links back to where it can be fixed
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the DNS page does not show %q", want)
		}
	}
}

// A well-formed target that does not exist is accepted by validation and then
// fails every customer's domain, reporting the failure against their name
// rather than against the setting that caused it.
func TestTheDNSPageSaysWhenTheTargetDoesNotResolve(t *testing.T) {
	h := dnsServer(t, &fakeNets{targetBroken: true, net: app.Networking{Target: "edge.yacht.test"}})

	body := do(h, signedIn(http.MethodGet, "/cluster/dns")).Body.String()
	if !strings.Contains(body, "does not resolve") {
		t.Error("a target that does not resolve is not flagged")
	}
	if !strings.Contains(body, "no custom domain on this install can be verified") {
		t.Error("nothing says what the consequence is")
	}
}

// The prefix is recorded and never applied. Buried in prose that read as a
// setting which takes effect from here.
func TestTheOwnershipPrefixSaysItIsNotApplied(t *testing.T) {
	h := dnsServer(t, &fakeNets{net: app.Networking{Target: "edge.yacht.test"}})

	body := do(h, signedIn(http.MethodGet, "/cluster/dns")).Body.String()
	if !strings.Contains(body, "Recorded here, not applied") {
		t.Error("the page does not say the prefix is never applied")
	}
	if !strings.Contains(body, "callout-warn") {
		t.Error("that fact is not called out — it reads as another paragraph of prose")
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

// An app with no public domain is offered Generate Domain, with the port
// picker and the name it will get, rather than an address it does not have.
func TestNetworkingOffersGenerateDomain(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Public: app.PublicDomain{
			Domain: "up.apps.test", Suggested: "web-production",
			Port: 3000, Suggestions: []int32{3000, 8080},
		},
		Private:  app.PrivateEndpoint{Name: "web.internal", Host: "web.yacht-abc.svc.cluster.local", Port: 80},
		TCPProxy: app.TCPProxy{Available: true, Host: "proxy.apps.test", Target: 3000},
	}})

	body := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String()
	for _, want := range []string{
		`action="/apps/web/networking/domain/generate"`,
		"web-production.up.apps.test",
		`name="port"`, `value="3000"`, `data-port="8080"`,
		`name="tab" value="domains"`,
		// Private networking, copyable.
		"web.yacht-abc.svc.cluster.local", `data-copy="web.yacht-abc.svc.cluster.local"`,
		// Its short name, as Railway shows <service>.railway.internal.
		`<span class="record-k">Private</span>`, `data-copy="web.internal"`,
		"${{web.PRIVATE_HOST}}",
		// The TCP proxy on offer.
		`action="/apps/web/networking/tcp-proxy"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the networking section is missing %q", want)
		}
	}
	if strings.Contains(body, `action="/apps/web/networking/domain"`) {
		t.Error("an app with no domain was offered the form to edit one")
	}
}

// With a domain: the address, copyable, its port, and the edit form; with a
// proxy: its address and the way to remove it.
func TestNetworkingShowsTheDomainAndProxy(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Managed: "web-production.up.apps.test",
		Public: app.PublicDomain{
			Host: "web-production.up.apps.test", Label: "web-production", Generated: true, Routed: true,
			Domain: "up.apps.test", Port: 5001, Suggestions: []int32{5001},
		},
		TCPProxy: app.TCPProxy{Available: true, Host: "proxy.apps.test", Port: 30042, Target: 5001},
	}})

	body := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String()
	for _, want := range []string{
		`href="https://web-production.up.apps.test"`,
		`data-copy="web-production.up.apps.test"`,
		`action="/apps/web/networking/domain"`, `value="web-production"`, ".up.apps.test",
		`action="/apps/web/networking/domain/delete"`,
		"proxy.apps.test:30042", `action="/apps/web/networking/tcp-proxy/delete"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the networking section is missing %q", want)
		}
	}
	if strings.Contains(body, "/networking/domain/generate") {
		t.Error("an app with a domain was offered another")
	}
}

// What cannot be done is said, rather than offered and refused.
func TestNetworkingSaysWhyThereIsNoDomain(t *testing.T) {
	h := netServer(t, &fakeNets{net: app.Networking{
		Public: app.PublicDomain{Unavailable: "This app is internal."},
	}})
	body := do(h, signedIn(http.MethodGet, "/apps/web/domains")).Body.String()
	if !strings.Contains(body, "This app is internal.") || strings.Contains(body, "/networking/domain/generate") {
		t.Error("an app that cannot have a domain was not told why, or was offered one anyway")
	}
	if !strings.Contains(body, "Ask your operator to turn them on.") {
		t.Error("an install without TCP proxies does not say so")
	}
	// The customer cannot set the engine's settings, and on a product built
	// on it they are not even called that.
	if strings.Contains(body, "YACHT_") {
		t.Error("a customer's page names one of the engine's settings")
	}
}

// The forms reach the service with what was typed, and come back to the tab
// they were posted from.
func TestNetworkingFormsReachTheService(t *testing.T) {
	nets := &fakeNets{}
	h := netServer(t, nets)

	cases := []struct{ path, body string }{
		{"/apps/web/networking/domain/generate", "port=5001"},
		{"/apps/web/networking/domain", url.Values{"label": {"shop"}, "port": {"3000"}, "tab": {"settings"}}.Encode()},
		{"/apps/web/networking/domain/delete", ""},
		{"/apps/web/networking/tcp-proxy", ""},
		{"/apps/web/networking/tcp-proxy/delete", "tab=elsewhere"},
	}
	wantTab := []string{"domains", "settings", "domains", "domains", "domains"}
	for i, c := range cases {
		rec := do(h, signedInForm(http.MethodPost, c.path, c.body))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/web/"+wantTab[i] {
			t.Errorf("POST %s = %d to %q, want a redirect to /apps/web/%s",
				c.path, rec.Code, rec.Header().Get("Location"), wantTab[i])
		}
	}
	if len(nets.generated) != 1 || nets.generated[0] != 5001 {
		t.Errorf("generated = %v, want one at 5001", nets.generated)
	}
	if len(nets.set) != 1 || nets.set[0] != "shop:3000" {
		t.Errorf("set = %v, want shop:3000", nets.set)
	}
	if nets.unset != 1 || nets.proxied != 1 || nets.unproxied != 1 {
		t.Errorf("removed %d, proxied %d, unproxied %d; want one each", nets.unset, nets.proxied, nets.unproxied)
	}

	if rec := do(h, signedInForm(http.MethodPost, "/apps/web/networking/domain/generate", "port=eighty")); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a port that is not a number = %d, want 422", rec.Code)
	}
}

// A refusal is shown on the section it came from.
func TestANetworkingRefusalKeepsTheSection(t *testing.T) {
	h := netServer(t, &fakeNets{err: errors.New("every TCP proxy port on this install is in use")})
	rec := do(h, signedInForm(http.MethodPost, "/apps/web/networking/tcp-proxy", ""))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "every TCP proxy port") {
		t.Errorf("refusal = %d, without the reason on the page", rec.Code)
	}
}
