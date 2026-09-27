package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// fakeCapacity is the install's capacity without a database or a cluster:
// whatever snapshot the test sets, and the last policy saved.
type fakeCapacity struct {
	snap     app.CapacitySnapshot
	saved    *app.CapacityPolicy
	refusals []app.CapacityRefusal
}

func (f *fakeCapacity) Capacity(context.Context) (app.CapacitySnapshot, error) { return f.snap, nil }

func (f *fakeCapacity) SetCapacityPolicy(_ context.Context, p app.CapacityPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	f.saved = &p
	f.snap.Policy = p
	return nil
}

func (f *fakeCapacity) CapacityRefusals(context.Context, time.Time, int32) ([]app.CapacityRefusal, error) {
	return f.refusals, nil
}

// snapshotAt is an install with 4 vCPU and 8 GiB sellable, committed to the
// given share of each.
func snapshotAt(cpuPercent, memPercent int, enforced bool) app.CapacitySnapshot {
	res := func(sellable int64, pct int) app.CapacityResource {
		committed := sellable * int64(pct) / 100
		return app.CapacityResource{Counted: true, Allocatable: sellable, Sellable: sellable,
			Committed: committed, Free: max(sellable-committed, 0)}
	}
	p := app.DefaultCapacityPolicy()
	p.Enforce = enforced
	return app.CapacitySnapshot{
		Known: true, Enforced: enforced, Policy: p, Nodes: 2, Schedulable: 2,
		CPU: res(4000, cpuPercent), Memory: res(8<<30, memPercent),
		WarnPercent: p.WarnPercent,
	}
}

// requestAs is a request whose surfaces say whether its person runs the
// install — what withSurfaces would have put there.
func requestAs(path string, operator bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	return r.WithContext(context.WithValue(r.Context(), surfacesKey{}, Surfaces{Operator: operator}))
}

func capacityServer(t *testing.T, c *fakeCapacity) *Server {
	t.Helper()
	s, err := New(Options{
		Orchestrator: orchestrator.NewNoop(),
		Identity:     identity.NewSingleOwner(identity.Owner{ID: "owner-1", DisplayName: "Eric"}),
		Apps:         newFakeApps(), Quotas: newFakeQuotas("owner-1"), Capacity: c,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// The alert is the operator's, and only when there is something to say.
func TestTheCapacityBannerIsOnlyForOperatorsAndOnlyWhenWarranted(t *testing.T) {
	unknown := snapshotAt(0, 0, true)
	unknown.Known, unknown.CPU, unknown.Memory = false, app.CapacityResource{}, app.CapacityResource{}
	unknownOff := unknown
	unknownOff.Enforced = false
	refused := snapshotAt(10, 10, true)
	refused.RefusedLast24h = 3

	for _, tc := range []struct {
		name   string
		snap   app.CapacitySnapshot
		shown  bool
		severe bool
		says   string
	}{
		{"room to spare", snapshotAt(40, 30, true), false, false, ""},
		{"past the warning line", snapshotAt(85, 30, true), true, false, "sellable CPU is committed"},
		{"memory sold out", snapshotAt(50, 104, false), true, true, "sold all of its memory"},
		{"refusals with room now", refused, true, true, "3 changes were refused"},
		{"unknown while enforcing", unknown, true, false, "Capacity is unknown"},
		{"unknown and not enforcing", unknownOff, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capacityServer(t, &fakeCapacity{snap: tc.snap})
			if c := s.capacityBannerFor(requestAs("/", false)); c != nil {
				t.Fatal("a customer is shown the operator's capacity alert")
			}
			c := s.capacityBannerFor(requestAs("/", true))
			if (c != nil) != tc.shown {
				t.Fatalf("banner shown = %v, want %v", c != nil, tc.shown)
			}
			if c == nil {
				return
			}
			body := renderToString(t, c)
			if !strings.Contains(body, tc.says) {
				t.Errorf("banner does not say %q:\n%s", tc.says, body)
			}
			if got := strings.Contains(body, "callout-err"); got != tc.severe {
				t.Errorf("destructive style = %v, want %v", got, tc.severe)
			}
			if !strings.Contains(body, `href="/admin/capacity"`) {
				t.Error("the banner does not link to the capacity page")
			}
			// No joiner here, so no add-node page to link to.
			if strings.Contains(body, `href="/cluster/nodes/add"`) {
				t.Error("the banner links to an add-node page this install does not have")
			}
			if s.capacityBannerFor(requestAs("/admin/capacity", true)) != nil {
				t.Error("the banner is drawn on the page it links to")
			}
		})
	}
}

// Through the server, as the one principal of an install with no accounts —
// who runs it.
func TestTheOperatorSeesTheCapacityBannerOnEveryPage(t *testing.T) {
	c := &fakeCapacity{snap: snapshotAt(95, 20, true)}
	h := capacityServer(t, c).Handler()
	for _, path := range []string{"/", "/projects", "/settings", "/admin/teams"} {
		if body := get(t, h, path).Body.String(); !strings.Contains(body, "data-capacity-banner") {
			t.Errorf("GET %s has no capacity banner past the warning line", path)
		}
	}
	c.snap = snapshotAt(20, 20, true)
	if body := get(t, h, "/").Body.String(); strings.Contains(body, "data-capacity-banner") {
		t.Error("the capacity banner is drawn with room to spare")
	}
}

// It sits between the acting notice and a wrapper's own banner, and hides
// neither.
func TestTheCapacityBannerComposesWithActingAndAWrappersBanner(t *testing.T) {
	ctx := context.WithValue(context.Background(), surfacesKey{},
		Surfaces{Operator: true, ActingAs: "Customer Co"})
	alert := capacityBanner(capacityAlert{Snapshot: snapshotAt(95, 20, true)})
	own := templ.Raw(`<p data-wrapper-banner>Your balance is low</p>`)

	for name, banner := range map[string]templ.Component{
		"a wrapper's banner":      withCapacityBanner(alert, own),
		"the engine's own chrome": withCapacityBanner(alert, ActingBanner("Customer Co")),
	} {
		var buf bytes.Buffer
		if err := Layout(Slots{Banner: banner}, templ.Raw("page")).Render(ctx, &buf); err != nil {
			t.Fatalf("render: %v", err)
		}
		body := buf.String()
		acting, capacity := strings.Index(body, "data-acting-banner"), strings.Index(body, "data-capacity-banner")
		if acting < 0 || capacity < 0 || acting > capacity {
			t.Errorf("%s: acting at %d, capacity at %d — want both, acting first", name, acting, capacity)
		}
		if n := strings.Count(body, "data-acting-banner"); n != 1 {
			t.Errorf("%s: the acting banner is drawn %d times", name, n)
		}
		if wrapper := strings.Index(body, "data-wrapper-banner"); wrapper >= 0 && wrapper < capacity {
			t.Errorf("%s: the wrapper's banner is above the capacity alert", name)
		}
	}
	if got := renderToString(t, withCapacityBanner(nil, own)); got != renderToString(t, own) {
		t.Errorf("with nothing to say, the banner is not left as it was: %s", got)
	}
	if withCapacityBanner(nil, nil) != nil {
		t.Error("with nothing to say and no banner, a banner appears")
	}
}

// Customers are told calmly that room is short — and nothing else.
func TestCustomersAreToldWhenRoomIsShort(t *testing.T) {
	c := &fakeCapacity{snap: snapshotAt(99, 20, true)}
	c.snap.RoomShort = true
	h := capacityServer(t, c).Handler()

	for _, path := range []string{"/apps/new", "/apps/new?source=image"} {
		body := get(t, h, path).Body.String()
		if !strings.Contains(body, "data-room-notice") ||
			!strings.Contains(body, "Room is limited right now — new apps or more replicas may be refused.") {
			t.Errorf("GET %s does not say room is limited", path)
		}
	}
	notice := renderToString(t, roomNotice())
	for _, leak := range []string{"vCPU", "GiB", "MiB", "%"} {
		if strings.Contains(notice, leak) {
			t.Errorf("the customer's notice gives a figure (%q)", leak)
		}
	}

	c.snap.RoomShort = false
	if body := get(t, h, "/apps/new?source=image").Body.String(); strings.Contains(body, "data-room-notice") {
		t.Error("customers are told room is limited when it is not")
	}
}

func TestTheCapacityPolicyFormSavesAndRefuses(t *testing.T) {
	c := &fakeCapacity{snap: snapshotAt(50, 50, false), refusals: []app.CapacityRefusal{{
		TeamID: "owner-1", TeamName: "Acme", Resource: "memory", Shortfall: 3 << 29, At: time.Now().Add(-time.Hour),
	}}}
	h := capacityServer(t, c).Handler()

	page := get(t, h, "/admin/capacity").Body.String()
	for _, want := range []string{`action="/admin/capacity/policy"`, "Sold", "Free to sell",
		"Refused for want of room", "1.5 GiB memory", "risks OOM kills"} {
		if !strings.Contains(page, want) {
			t.Errorf("the capacity page does not show %q", want)
		}
	}

	rec := post(t, h, "/admin/capacity/policy", url.Values{
		"enforce": {"1"}, "cpu_ratio": {"2"}, "memory_ratio": {"1"},
		"reserve": {"10"}, "warn": {"85"}, "storage": {"500"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST a sensible policy = %d, want 303", rec.Code)
	}
	// No wake reserve in the form is the default one, not none.
	want := app.CapacityPolicy{Enforce: true, CPURatio: 2, MemoryRatio: 1, ReservePercent: 10,
		WarnPercent: 85, StorageBytes: 500 << 30, WakeReservePercent: 25}
	if c.saved == nil || *c.saved != want {
		t.Fatalf("saved %+v, want %+v", c.saved, want)
	}

	rec = post(t, h, "/admin/capacity/policy", url.Values{
		"cpu_ratio": {"0.5"}, "memory_ratio": {"1"}, "reserve": {"0"}, "warn": {"80"},
	})
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "cannot be below 1") ||
		!strings.Contains(body, `value="0.5"`) {
		t.Fatalf("POST a ratio of 0.5 = %d; want 422 saying why, with the value kept", rec.Code)
	}
	if c.saved.CPURatio != 2 {
		t.Error("a refused policy was saved")
	}
}

func TestACapacityPolicyFormRoundTrips(t *testing.T) {
	p := app.CapacityPolicy{Enforce: true, CPURatio: 2.5, MemoryRatio: 1, ReservePercent: 15,
		WarnPercent: 90, StorageBytes: 250 << 30}
	back, err := capacityPolicyForm(p).Parse()
	if err != nil || back != p {
		t.Fatalf("round trip = %+v, %v; want %+v", back, err, p)
	}
	// Empty fields are the defaults, and storage empty is not counted.
	if p, err := (CapacityPolicyForm{}).Parse(); err != nil || p != app.DefaultCapacityPolicy() {
		t.Fatalf("an empty form = %+v, %v; want the defaults", p, err)
	}
	for _, bad := range []CapacityPolicyForm{
		{CPURatio: "two"}, {Reserve: "12.5"}, {Warn: "0"}, {Storage: "-1"}, {MemoryRatio: "11"},
	} {
		if _, err := bad.Parse(); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

// The operator's overview carries how much of the install is sold.
func TestTheOperatorsOverviewShowsWhatIsSold(t *testing.T) {
	h := capacityServer(t, &fakeCapacity{snap: snapshotAt(30, 62, true)}).Handler()
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, "data-capacity-metric") || !strings.Contains(body, "of sellable memory") {
		t.Error("the operator's overview does not say how much is sold")
	}
}

// ------------------------------------------------ against a real database

// Wired to the real service: an operator is told the install cannot see its
// cluster while it is enforcing, and a member of a team never is.
func TestOnlyTheOperatorIsToldAboutCapacity(t *testing.T) {
	rt := newRoleTeam(t, "web-role-capacity", "capacity-app")
	ctx := context.Background()
	p := app.DefaultCapacityPolicy()
	p.Enforce = true
	if err := rt.apps.SetCapacityPolicy(ctx, p); err != nil {
		t.Fatalf("SetCapacityPolicy: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.apps.SetCapacityPolicy(context.Background(), app.DefaultCapacityPolicy()); err != nil {
			t.Errorf("restore the capacity policy: %v", err)
		}
	})

	// The in-memory orchestrator has no machines, so capacity is unknown.
	if body := rt.getAs(t, "/", rt.owner).Body.String(); !strings.Contains(body, "Capacity is unknown") {
		t.Error("the operator is not told capacity is unknown while enforcing")
	}
	for _, who := range []*http.Cookie{rt.member, rt.admin} {
		if body := rt.getAs(t, "/", who).Body.String(); strings.Contains(body, "data-capacity-banner") {
			t.Error("somebody who does not run the install is shown the capacity alert")
		}
	}
}
