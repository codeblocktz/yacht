package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// fakeSleeper is the sleep service without a database or a cluster: the apps
// it knows by hostname, and a wake that does whatever the test says.
type fakeSleeper struct {
	mu sync.Mutex

	byHost  map[string]app.App
	status  app.SleepStatus
	team    app.SleepPolicy
	backend string

	// wake is what a wake does; nil wakes at once.
	wake func(ctx context.Context) error

	slept, woken int
	setting      *app.SleepSetting
	teamSet      *app.SleepPolicy
}

func (f *fakeSleeper) CanSleep() bool { return true }

func (f *fakeSleeper) SleepStatus(context.Context, string, string) (app.SleepStatus, error) {
	return f.status, nil
}

func (f *fakeSleeper) SetSleepSetting(_ context.Context, _, _ string, in app.SleepSetting) (app.App, error) {
	if err := in.Validate(); err != nil {
		return app.App{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setting = &in
	return app.App{}, nil
}

func (f *fakeSleeper) SleepNow(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slept++
	return nil
}

func (f *fakeSleeper) Wake(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.woken++
	return nil
}

func (f *fakeSleeper) TeamSleepDefault(context.Context, string) (app.SleepPolicy, error) {
	return f.team, nil
}

func (f *fakeSleeper) SetTeamSleepDefault(_ context.Context, _ string, p app.SleepPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	f.teamSet = &p
	return nil
}

func (f *fakeSleeper) WakeTarget(_ context.Context, host string) (app.App, error) {
	a, ok := f.byHost[host]
	if !ok {
		return app.App{}, app.ErrNotFound
	}
	return a, nil
}

func (f *fakeSleeper) WakeForRequest(ctx context.Context, _ app.App) error {
	if f.wake == nil {
		return nil
	}
	return f.wake(ctx)
}

func (f *fakeSleeper) Backend(context.Context, app.App) (string, error) {
	if f.backend == "" {
		return "", orchestrator.ErrNotSupported
	}
	return f.backend, nil
}

func sleepingApp() app.App {
	a := sampleApp("owner-1", "shop")
	since := time.Now().Add(-40 * time.Minute)
	last := time.Now().Add(-75 * time.Minute)
	a.Host = "shop.apps.example.com"
	a.Sleep = app.Sleep{
		Setting: app.SleepSetting{Mode: app.SleepInherit}, State: app.SleepAsleep,
		Since: &since, LastRequestAt: &last,
	}
	a.Status = orchestrator.AppStatus{Phase: orchestrator.PhaseStopped, Desired: 0}
	return a
}

// wakerFor is the waker's handler over a fake sleep service.
func wakerFor(t *testing.T, f *fakeSleeper) http.Handler {
	t.Helper()
	s, err := New(Options{
		Orchestrator: orchestrator.NewNoop(),
		Identity:     identity.NewSingleOwner(identity.Owner{ID: "owner-1"}),
		Apps:         newFakeApps(), Sleep: f, WakerBrand: "Kilicore",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Waker()
}

func wakeRequest(method, host, accept string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, "http://"+host+"/orders?page=2", body)
	r.Host = host
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	return r
}

func TestTheWakerShowsABrowserAWakingUpPage(t *testing.T) {
	a := sleepingApp()
	f := &fakeSleeper{
		byHost: map[string]app.App{a.Host: a},
		// Slower than a browser is held for.
		wake: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	}
	rec := httptest.NewRecorder()
	wakerFor(t, f).ServeHTTP(rec, wakeRequest(http.MethodGet, a.Host, "text/html,application/xhtml+xml", nil))

	body := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "3" {
		t.Fatalf("status %d, Retry-After %q; want 503, 3", rec.Code, rec.Header().Get("Retry-After"))
	}
	for _, want := range []string{"Waking up…", "shop was asleep and is waking up", `http-equiv="refresh" content="3"`,
		"hosted on Kilicore", `data-waker-state="waking"`} {
		if !strings.Contains(body, want) {
			t.Errorf("waking-up page missing %q", want)
		}
	}
	// A page served on the app's hostname cannot lean on the dashboard's
	// stylesheet, which is not there.
	if strings.Contains(body, "/assets/") {
		t.Errorf("waking-up page links the dashboard's assets")
	}
}

func TestTheWakerHoldsAnAPIRequestAndHandsItToTheApp(t *testing.T) {
	var got struct {
		host, fwd, body, query string
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.host, got.fwd, got.body, got.query = r.Host, r.Header.Get("X-Forwarded-Proto"), string(b), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()

	a := sleepingApp()
	started := time.Now()
	f := &fakeSleeper{
		byHost:  map[string]app.App{a.Host: a},
		backend: strings.TrimPrefix(backend.URL, "http://"),
		// Longer than a browser would be held, and the API client waits.
		wake: func(context.Context) error { time.Sleep(2 * browserHold); return nil },
	}
	req := wakeRequest(http.MethodPost, a.Host, "application/json", strings.NewReader(`{"item":7}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	wakerFor(t, f).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("held request = %d %q; want the app's own 201", rec.Code, rec.Body.String())
	}
	if time.Since(started) < 2*browserHold {
		t.Fatalf("answered before the app woke")
	}
	if got.host != a.Host || got.body != `{"item":7}` || got.query != "page=2" || got.fwd != "https" {
		t.Fatalf("the app saw host %q, body %q, query %q, proto %q; want the request as sent",
			got.host, got.body, got.query, got.fwd)
	}
}

func TestTheWakerSaysBusyWhenThereIsNoRoom(t *testing.T) {
	a := sleepingApp()
	f := &fakeSleeper{
		byHost: map[string]app.App{a.Host: a},
		wake:   func(context.Context) error { return app.ErrWakeNoRoom },
	}
	h := wakerFor(t, f)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, wakeRequest(http.MethodGet, a.Host, "text/html", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "60" ||
		!strings.Contains(rec.Body.String(), "Busy right now") ||
		!strings.Contains(rec.Body.String(), "Try again in a minute") {
		t.Fatalf("browser = %d, Retry-After %q:\n%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, wakeRequest(http.MethodGet, a.Host, "application/json", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no room to wake it") ||
		strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("API client = %d %q; want a plain 503 saying so", rec.Code, rec.Body.String())
	}
}

func TestTheWakerSaysWhenAWakeTimedOut(t *testing.T) {
	a := sleepingApp()
	f := &fakeSleeper{
		byHost: map[string]app.App{a.Host: a},
		wake:   func(context.Context) error { return app.ErrWakeTimeout },
	}
	rec := httptest.NewRecorder()
	wakerFor(t, f).ServeHTTP(rec, wakeRequest(http.MethodGet, a.Host, "*/*", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" ||
		!strings.Contains(rec.Body.String(), "took too long to start") {
		t.Fatalf("timed-out wake = %d %q", rec.Code, rec.Body.String())
	}
}

func TestTheWakerIsNotTheDashboard(t *testing.T) {
	f := &fakeSleeper{byHost: map[string]app.App{}}
	h := wakerFor(t, f)
	for _, path := range []string{"/", "/admin/teams", "/apps"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "dashboard.example.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("GET %s on an unknown hostname = %d; want a bare 404, not a dashboard page", path, rec.Code)
		}
	}
}

func TestASleepingAppSaysSoAndOffersWakeNow(t *testing.T) {
	a := sleepingApp()
	f := &fakeSleeper{status: app.SleepStatus{
		Setting: a.Sleep.Setting, Team: app.SleepPolicy{Enabled: true, After: 30 * time.Minute},
		Effective: app.SleepPolicy{Enabled: true, After: 30 * time.Minute}, Sleep: a.Sleep,
	}}
	h := testServer(t, Options{Apps: newFakeApps(a), Sleep: f})

	body := get(t, h, "/apps/shop").Body.String()
	for _, want := range []string{`class="status status-sleep"`, ">sleeping<", "Wake now", "/apps/shop/wake",
		"Asleep since 40 minutes ago", "last request 1 hour ago"} {
		if !strings.Contains(body, want) {
			t.Errorf("sleeping app page missing %q", want)
		}
	}
	if strings.Contains(body, "Sleep now") {
		t.Errorf("a sleeping app offered Sleep now")
	}
	list := get(t, h, "/apps").Body.String()
	if !strings.Contains(list, ">sleeping<") {
		t.Errorf("the app list does not say the app is sleeping")
	}

	if rec := post(t, h, "/apps/shop/wake", nil); rec.Code != http.StatusSeeOther || f.woken != 1 {
		t.Fatalf("Wake now = %d, woke %d times", rec.Code, f.woken)
	}
}

func TestAnAwakeAppOffersSleepNowAndItsSetting(t *testing.T) {
	a := sampleApp("owner-1", "api")
	f := &fakeSleeper{status: app.SleepStatus{
		Setting: app.SleepSetting{Mode: app.SleepInherit}, Team: app.DefaultSleepPolicy(),
		Effective: app.DefaultSleepPolicy(), Sleep: app.Sleep{State: app.SleepAwake},
	}}
	h := testServer(t, Options{Apps: newFakeApps(a), Sleep: f})

	body := get(t, h, "/apps/api/settings").Body.String()
	for _, want := range []string{"Sleep now", `id="sleep"`, "Sleep when idle", "Team default — off",
		`id="sleep-mode"`, "Never sleep", "never sleeps", "It has not slept yet."} {
		if !strings.Contains(body, want) {
			t.Errorf("settings missing %q", want)
		}
	}

	rec := post(t, h, "/apps/api/sleep/settings", url.Values{"mode": {"on"}, "after": {"15"}})
	if rec.Code != http.StatusSeeOther || f.setting == nil ||
		f.setting.Mode != app.SleepOn || f.setting.After != 15*time.Minute {
		t.Fatalf("save = %d, setting %+v; want on after 15 minutes", rec.Code, f.setting)
	}
	f.setting = nil
	if rec := post(t, h, "/apps/api/sleep/settings", url.Values{"mode": {"on"}, "after": {"2"}}); rec.Code != http.StatusSeeOther || f.setting != nil {
		t.Fatalf("a two-minute idle time was saved")
	}
	if rec := post(t, h, "/apps/api/sleep", nil); rec.Code != http.StatusSeeOther || f.slept != 1 {
		t.Fatalf("Sleep now = %d, slept %d times", rec.Code, f.slept)
	}
}

func TestCapacityShowsTheSleepersAndTheWakeReserve(t *testing.T) {
	snap := snapshotAt(40, 30, true)
	snap.Sleeping = app.SleepingCapacity{
		Apps: 12, CPUMillis: 3000, MemoryBytes: 6 << 30, ReservePercent: 25,
		ReserveCPUMillis: 750, ReserveMemoryBytes: 3 << 29,
	}
	c := &fakeCapacity{snap: snap}
	s := capacityServer(t, c)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, requestAs("/admin/capacity", true))
	body := rec.Body.String()
	for _, want := range []string{"Sleeping apps", "room kept for 25% of them to wake at once", `name="wake_reserve"`,
		"Wake reserve", "Freed for sale"} {
		if !strings.Contains(body, want) {
			t.Errorf("capacity page missing %q", want)
		}
	}

	form := CapacityPolicyForm{CPURatio: "1", MemoryRatio: "1", Reserve: "0", Warn: "80", WakeReserve: "40"}
	p, err := form.Parse()
	if err != nil || p.WakeReservePercent != 40 {
		t.Fatalf("Parse = %+v, %v; want a 40%% wake reserve", p, err)
	}
	form.WakeReserve = "140"
	if _, err := form.Parse(); err == nil {
		t.Fatalf("a 140%% wake reserve parsed")
	}
	form.WakeReserve = ""
	if p, _ := form.Parse(); p.WakeReservePercent != 25 {
		t.Fatalf("an empty wake reserve = %d%%, want the default 25%%", p.WakeReservePercent)
	}
}
