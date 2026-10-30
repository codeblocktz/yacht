package web

import (
	"context"
	"io"
	"iter"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/observe"
)

// fakeLogs is a log store that remembers the scope of every read.
type fakeLogs struct {
	scopes  []observe.Scope
	queries []observe.LogQuery
	entries []observe.Entry
}

func (f *fakeLogs) Query(_ context.Context, s observe.Scope, q observe.LogQuery) ([]observe.Entry, error) {
	f.scopes, f.queries = append(f.scopes, s), append(f.queries, q)
	return f.entries, nil
}

func (f *fakeLogs) Histogram(_ context.Context, s observe.Scope, q observe.LogQuery) ([]observe.Bucket, error) {
	f.scopes = append(f.scopes, s)
	return observe.CountLines(f.entries, q.Start, q.End), nil
}

func (f *fakeLogs) Tail(_ context.Context, s observe.Scope, _ observe.LogQuery) (iter.Seq2[observe.Entry, error], error) {
	f.scopes = append(f.scopes, s)
	return func(yield func(observe.Entry, error) bool) {
		for _, e := range f.entries {
			if !yield(e, nil) {
				return
			}
		}
	}, nil
}

func (f *fakeLogs) Export(_ context.Context, s observe.Scope, _ observe.LogQuery, w io.Writer) error {
	f.scopes = append(f.scopes, s)
	for _, e := range f.entries {
		io.WriteString(w, e.Service+" "+e.Message+"\n")
	}
	return nil
}

// fakeMetrics is a metrics store that remembers the scope of every read.
type fakeMetrics struct{ scopes []observe.Scope }

func (f *fakeMetrics) Dashboard(_ context.Context, s observe.Scope, w observe.Window) ([]observe.Panel, error) {
	f.scopes = append(f.scopes, s)
	var series []observe.Series
	for _, svc := range s.Services() {
		series = append(series, observe.Series{Name: svc.Name, Points: []observe.Point{{At: w.End, V: 0.25}}})
	}
	return []observe.Panel{
		{Key: observe.PanelCPU, Title: "CPU", Unit: observe.UnitCores, Series: series, Start: w.Start, End: w.End},
		{Key: observe.PanelRequests, Title: "Requests", Unit: observe.UnitReqSec, Stacked: true, Start: w.Start, End: w.End},
	}, nil
}

func (f *fakeMetrics) PublicEgress(_ context.Context, s observe.Scope, _, _ time.Time) (map[string]int64, error) {
	f.scopes = append(f.scopes, s)
	return map[string]int64{"web": 3 << 30}, nil
}

// twoTeams is one install with owner-1's web and postgres, and owner-2's
// secret: the app every scope here must leave out.
func twoTeams() *fakeApps {
	return newFakeApps(sampleApp("owner-1", "web"), sampleApp("owner-1", "postgres"), sampleApp("owner-2", "secret"))
}

func scopeNames(s observe.Scope) []string {
	var out []string
	for _, svc := range s.Services() {
		out = append(out, svc.Name+"@"+svc.Namespace)
	}
	return out
}

// The scope of a project's log is the signed-in team's apps in it and
// nothing else, and a filter can narrow it but never name another team's app.
func TestAProjectsLogsAreScopedToTheTeamsOwnApps(t *testing.T) {
	logs := &fakeLogs{entries: []observe.Entry{
		{At: time.Now().Add(-time.Minute), Service: "web", Message: "GET /orders 200"},
		{At: time.Now(), Service: "postgres", Message: "FATAL: role does not exist", Error: true},
	}}
	h := testServer(t, Options{Apps: twoTeams(), LogHistory: logs, LogRetention: 7 * 24 * time.Hour})

	body := get(t, h, "/projects/default/logs").Body.String()
	for _, want := range []string{"Filter and search logs", "GET /orders 200", "logx-err", "Live tail", "Download",
		`aria-label="Logs"`, `data-bands="logs"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the logs page does not have %q", want)
		}
	}
	if len(logs.scopes) == 0 {
		t.Fatal("the store was never asked")
	}
	for _, s := range logs.scopes {
		if s.Owner() != "owner-1" {
			t.Errorf("scoped to owner %q", s.Owner())
		}
		got := strings.Join(scopeNames(s), " ")
		want := "postgres@" + app.Namespace("owner-1", "postgres") + " web@" + app.Namespace("owner-1", "web")
		if got != want {
			t.Errorf("scope = %s, want %s", got, want)
		}
	}

	logs.scopes = nil
	body = get(t, h, "/projects/default/logs?q=%40service%3Asecret+password").Body.String()
	if len(logs.scopes) != 0 {
		t.Errorf("a filter naming another team's app reached the store with %v", scopeNames(logs.scopes[0]))
	}
	if !strings.Contains(body, "No service in this project is called secret") {
		t.Error("the page does not say the filter named no service")
	}

	get(t, h, "/projects/default/logs?q=%40service%3Aweb+%40level%3Aerror+timeout")
	if got := scopeNames(logs.scopes[0]); len(got) != 1 || !strings.HasPrefix(got[0], "web@") {
		t.Errorf("@service:web scoped to %v", got)
	}
	if q := logs.queries[len(logs.queries)-1]; q.Text != "timeout" || !q.ErrorsOnly {
		t.Errorf("the query was %+v", q)
	}

	if rec := get(t, h, "/projects/nowhere/logs"); rec.Code != http.StatusNotFound {
		t.Errorf("another project = %d", rec.Code)
	}
}

func TestTheProjectsLogStreamsAndDownloads(t *testing.T) {
	logs := &fakeLogs{entries: []observe.Entry{{At: time.Now(), Service: "web", Message: "<script>x</script>"}}}
	h := testServer(t, Options{Apps: twoTeams(), LogHistory: logs})

	rec := get(t, h, "/projects/default/logs/stream")
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("stream content type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `data: {"at":`) || strings.Contains(rec.Body.String(), "<script>") {
		t.Errorf("stream = %q; want JSON events with the message escaped", rec.Body.String())
	}

	rec = get(t, h, "/projects/default/logs/download?range=1h")
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="default-logs-`) {
		t.Errorf("download disposition %q", cd)
	}
	if !strings.Contains(rec.Body.String(), "web <script>x</script>") {
		t.Errorf("download = %q", rec.Body.String())
	}
}

// Without a store the page says so and reads the running pods instead.
func TestWithoutALogStoreTheLogsPageSaysItShowsTheRunningPods(t *testing.T) {
	h := testServer(t, Options{Apps: twoTeams()})
	body := get(t, h, "/projects/default/logs").Body.String()
	for _, want := range []string{"Showing the running pods", "Its operator can turn history on."} {
		if !strings.Contains(body, want) {
			t.Errorf("the logs page does not have %q", want)
		}
	}
	if strings.Contains(body, "YACHT_") {
		t.Error("the logs page names one of the engine's settings")
	}
	if rec := get(t, h, "/projects/default/logs/stream"); rec.Code != http.StatusNotFound {
		t.Errorf("a stream with no store = %d", rec.Code)
	}
}

func TestObservabilityIsScopedToTheTeamAndSaysWhatIsMissing(t *testing.T) {
	m := &fakeMetrics{}
	h := testServer(t, Options{Apps: twoTeams(), MetricHistory: m})
	body := get(t, h, "/projects/default/observability?range=6h").Body.String()
	for _, want := range []string{"Observability", "CPU", "data-series-chart", "No requests through a public domain", `aria-current="true">6h`} {
		if !strings.Contains(body, want) {
			t.Errorf("the dashboard does not have %q", want)
		}
	}
	for _, s := range m.scopes {
		if s.Owner() != "owner-1" || len(s.Services()) != 2 {
			t.Errorf("the dashboard read %s for %s", scopeNames(s), s.Owner())
		}
	}

	h = testServer(t, Options{Apps: twoTeams()})
	body = get(t, h, "/projects/default/observability").Body.String()
	if !strings.Contains(body, "need a metrics store, which this") {
		t.Error("without a store the dashboard does not say which is missing")
	}
	if strings.Contains(body, "YACHT_") {
		t.Error("the dashboard names one of the engine's settings")
	}
}

func TestAnAppsMetricChartsAreItsOwn(t *testing.T) {
	m := &fakeMetrics{}
	h := testServer(t, Options{Apps: twoTeams(), MetricHistory: m})
	body := get(t, h, "/apps/web/metrics/charts").Body.String()
	if !strings.Contains(body, `id="app-metric-charts"`) || !strings.Contains(body, "app-chart-cpu") {
		t.Errorf("the charts fragment = %s", body)
	}
	if len(m.scopes) != 1 || len(m.scopes[0].Services()) != 1 || m.scopes[0].Services()[0].Name != "web" {
		t.Errorf("the tab read %v", m.scopes)
	}
	if rec := get(t, h, "/apps/secret/metrics/charts"); rec.Code != http.StatusNotFound {
		t.Errorf("another team's app = %d", rec.Code)
	}
	if body := get(t, h, "/apps/web/metrics").Body.String(); !strings.Contains(body, `hx-get="/apps/web/metrics/charts"`) {
		t.Error("the Metrics tab does not load its charts")
	}
}

// fakeMeter prices every app at one amount.
type fakeMeter struct{ got UsageRequest }

func (f *fakeMeter) ProjectUsage(_ context.Context, req UsageRequest) (ProjectUsage, error) {
	f.got = req
	u := ProjectUsage{Period: "1–25 Oct", Current: "TZS 1,000", Estimated: "TZS 1,240",
		Resources: []UsageLine{{Name: "Memory", Detail: "1.0 GiB", Current: "TZS 600", Estimated: "TZS 744"}}}
	for _, a := range req.Apps {
		u.Services = append(u.Services, UsageLine{Name: a.Name, Current: "TZS 500", Estimated: "TZS 620"})
	}
	return u, nil
}

func TestAProjectsSettingsShowWhatItCosts(t *testing.T) {
	meter := &fakeMeter{}
	h := testServer(t, Options{Apps: twoTeams(), Usage: meter, MetricHistory: &fakeMetrics{}})
	body := get(t, h, "/projects/default/settings").Body.String()
	for _, want := range []string{`href="#usage"`, "Current usage", "TZS 1,240", "Memory", "postgres"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings do not show %q", want)
		}
	}
	if meter.got.OwnerID != "owner-1" || len(meter.got.Apps) != 2 || !meter.got.EgressKnown {
		t.Errorf("the meter was asked %+v", meter.got)
	}
	for _, a := range meter.got.Apps {
		if a.Name == "web" && a.EgressBytes != 3<<30 {
			t.Errorf("web's egress = %d", a.EgressBytes)
		}
	}
	if !meter.got.Since.Equal(monthStart(meter.got.Now)) || meter.got.Since.Day() != 1 {
		t.Errorf("the period starts %v", meter.got.Since)
	}

	h = testServer(t, Options{Apps: twoTeams()})
	if body := get(t, h, "/projects/default/settings").Body.String(); strings.Contains(body, `id="usage"`) {
		t.Error("an install with no meter shows a Usage section")
	}
}

func TestTheRailOffersObservabilityAndLogs(t *testing.T) {
	h := testServer(t, Options{Apps: twoTeams()})
	body := get(t, h, "/projects/default").Body.String()
	for _, want := range []string{`href="/projects/default/observability"`, `href="/projects/default/logs"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the rail does not link %q", want)
		}
	}
	body = get(t, h, "/projects/default/observability").Body.String()
	if !strings.Contains(body, `href="/projects/default/observability" aria-label="Observability" aria-current="page"`) {
		t.Error("the rail does not mark Observability as the page")
	}
}
