package web

import (
	"math"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/observe"
)

// The states a project's Logs, Observability and Usage add: with the stores
// and without them, which is the difference between an install that keeps
// history and one that reads only what is running.
func observeGalleryPages() []galleryPage {
	now := time.Now().UTC().Truncate(time.Second)
	storefront := app.Project{Slug: "storefront", Name: "Storefront"}
	services := []string{"api", "mailer", "postgres", "web", "worker"}

	var entries []observe.Entry
	msgs := map[string][]string{
		"api":      {"GET /v1/orders 200 12ms", "POST /v1/checkout 201 84ms", "GET /v1/cart 200 6ms"},
		"web":      {"GET / 200 3ms", "GET /assets/app.css 304 1ms"},
		"worker":   {"job send-receipt done in 140ms", "job reindex-products started"},
		"postgres": {"checkpoint complete: wrote 412 buffers (2.5%)"},
		"mailer":   {"delivered to ana@example.com"},
	}
	for i := 0; i < 90; i++ {
		svc := services[i%len(services)]
		m := msgs[svc][i%len(msgs[svc])]
		e := observe.Entry{At: now.Add(-time.Duration(90-i) * 9 * time.Second), Service: svc, Message: m}
		if i%23 == 7 {
			e.Message, e.Error = "ERROR stripe: card_declined (request req_8fK2) — retrying in 30s", true
			e.Service = "api"
		}
		if i == 61 {
			e.Service, e.Error = "postgres", true
			e.Message = `FATAL:  password authentication failed for user "shop"`
		}
		entries = append(entries, e)
	}
	ranges := func(key string, max time.Duration) []RangeChoice {
		var out []RangeChoice
		for _, c := range observeRanges {
			if max == 0 || c.Span <= max {
				c.Active = c.Key == key
				out = append(out, c)
			}
		}
		return out
	}
	q := observe.LogQuery{Start: now.Add(-15 * time.Minute), End: now}
	logs := ProjectLogsData{
		Project: storefront, Services: services, Entries: entries,
		Buckets: toLogBuckets(observe.CountLines(entries, q.Start, q.End)),
		Range:   observeRanges[0], Ranges: ranges("15m", 7*24*time.Hour),
		Stored: true, Retention: 7 * 24 * time.Hour,
	}
	searched := logs
	searched.Query = "@level:error stripe"
	searched.Search = observe.ParseSearch(searched.Query)
	searched.Entries = nil
	for _, e := range entries {
		if e.Error && searched.Search.Text != "" && e.Service == "api" {
			searched.Entries = append(searched.Entries, e)
		}
	}
	searched.Buckets = toLogBuckets(observe.CountLines(searched.Entries, q.Start, q.End))
	live := logs
	live.Stored, live.Live, live.Entries = false, true, entries[60:]
	live.Buckets = toLogBuckets(observe.CountLines(live.Entries, q.Start, q.End))
	live.Ranges = ranges("15m", 0)
	empty := logs
	empty.Entries, empty.Buckets, empty.Query = nil, nil, "timeout"

	window := observe.WindowFor(now, time.Hour)
	wave := func(name string, base, amp, phase float64) observe.Series {
		s := observe.Series{Name: name}
		for t := window.Start; !t.After(window.End); t = t.Add(window.Step) {
			x := float64(t.Unix()) / 600
			s.Points = append(s.Points, observe.Point{At: t, V: math.Max(0, base+amp*math.Sin(x+phase))})
		}
		return s
	}
	panel := func(key, title, unit string, stacked bool, series ...observe.Series) observe.Panel {
		return observe.Panel{Key: key, Title: title, Unit: unit, Stacked: stacked, Series: series,
			Start: window.Start, End: window.End}
	}
	panels := []observe.Panel{
		panel(observe.PanelCPU, "CPU", observe.UnitCores, false,
			wave("api", 0.18, 0.06, 0), wave("postgres", 0.09, 0.02, 1), wave("web", 0.03, 0.01, 2), wave("worker", 0.12, 0.08, 3)),
		panel(observe.PanelMemory, "Memory", observe.UnitBytes, false,
			wave("api", 210<<20, 12<<20, 0), wave("postgres", 380<<20, 6<<20, 1), wave("web", 64<<20, 2<<20, 2), wave("worker", 150<<20, 30<<20, 3)),
		panel(observe.PanelEgress, "Public network egress", observe.UnitBytesSec, false,
			wave("api", 180<<10, 60<<10, 0), wave("web", 420<<10, 140<<10, 1)),
		panel(observe.PanelIngress, "Public network ingress", observe.UnitBytesSec, false,
			wave("api", 22<<10, 6<<10, 0), wave("web", 9<<10, 3<<10, 1)),
		panel(observe.PanelRequests, "Requests", observe.UnitReqSec, true,
			wave("2xx", 38, 12, 0), wave("3xx", 6, 2, 0), wave("4xx", 2, 1, 1), wave("5xx", 0.4, 0.4, 2)),
		panel(observe.PanelErrorRate, "Request error rate", observe.UnitRatio, false,
			wave("api", 0.012, 0.01, 2), wave("web", 0.001, 0.001, 0)),
		panel(observe.PanelLatency, "Response time", observe.UnitSeconds, false,
			wave("p50", 0.018, 0.004, 0), wave("p90", 0.062, 0.015, 0), wave("p95", 0.11, 0.03, 0), wave("p99", 0.34, 0.12, 0)),
	}
	dash := ObservabilityData{Project: storefront, Stored: true, Range: observeRanges[1], Ranges: ranges("1h", 0),
		Panels: chartPanels("project", panels, []string{"api", "postgres", "web", "worker"})}
	quiet := dash
	quiet.Panels = chartPanels("project", []observe.Panel{panels[0], panels[1],
		panel(observe.PanelRequests, "Requests", observe.UnitReqSec, true),
		panel(observe.PanelLatency, "Response time", observe.UnitSeconds, false)}, []string{"api", "postgres", "web", "worker"})
	noStore := ObservabilityData{Project: storefront, NowKnown: true, Now: []ServiceNow{
		{Name: "api", CPU: "182m vCPU", Memory: "214 MiB", Running: true},
		{Name: "postgres", CPU: "91m vCPU", Memory: "380 MiB", Running: true},
		{Name: "worker", CPU: "—", Memory: "—"},
	}}

	settings := ProjectSettingsData{Project: storefront, Apps: 5, CanAdmin: true, Usage: &ProjectUsage{
		Period: "1–25 Oct", Current: "TZS 48,210", Estimated: "TZS 59,780",
		Resources: []UsageLine{
			{Name: "Memory", Detail: "1.1 GiB average", Current: "TZS 29,610", Estimated: "TZS 36,710"},
			{Name: "CPU", Detail: "0.45 vCPU average", Current: "TZS 18,300", Estimated: "TZS 22,690"},
			{Name: "Egress", Detail: "14.2 GiB", Current: "included", Estimated: "included"},
			{Name: "Volume", Detail: "12 GiB", Current: "TZS 300", Estimated: "TZS 380"},
		},
		Services: []UsageLine{
			{Name: "postgres", Detail: "1 replica", Current: "TZS 21,040", Estimated: "TZS 26,090"},
			{Name: "api", Detail: "2 replicas", Current: "TZS 17,520", Estimated: "TZS 21,720"},
			{Name: "worker", Detail: "1 replica", Current: "TZS 6,110", Estimated: "TZS 7,580"},
			{Name: "web", Detail: "1 replica", Current: "TZS 3,540", Estimated: "TZS 4,390"},
		},
		Note: "Prices before VAT. Your plan's included usage is drawn first; Billing shows what is charged.",
	}}

	appCharts := AppMetricsData{App: "api", Stored: true, Range: observeRanges[1], Ranges: ranges("1h", 0),
		Panels: chartPanels("app", []observe.Panel{
			panel(observe.PanelRequests, "Requests", observe.UnitReqSec, true, panels[4].Series...),
			panel(observe.PanelErrorRate, "Request error rate", observe.UnitRatio, false, panels[5].Series[0]),
			panel(observe.PanelLatency, "Response time", observe.UnitSeconds, false, panels[6].Series...),
			panel(observe.PanelCPU, "CPU", observe.UnitCores, false, panels[0].Series[0]),
			panel(observe.PanelMemory, "Memory", observe.UnitBytes, false, panels[1].Series[0]),
		}, []string{"api"})}

	// The gallery draws no page full-bleed, which is what loads the chart
	// scripts on a real install; these pages load them themselves.
	const charts = `var s = document.createElement("script"); s.src = "/assets/js/charts.js";` +
		` s.onload = function () { YachtCharts.mountTimelines(); YachtCharts.mountSeries(); };` +
		` document.head.appendChild(s);`
	pages := []galleryPage{
		{file: "project-logs.html", path: "/projects/storefront/logs", page: ProjectLogs(logs)},
		{file: "project-logs-search.html", path: "/projects/storefront/logs", page: ProjectLogs(searched)},
		{file: "project-logs-live-no-store.html", path: "/projects/storefront/logs", page: ProjectLogs(live)},
		{file: "project-logs-empty.html", path: "/projects/storefront/logs", page: ProjectLogs(empty)},
		{file: "project-observability.html", path: "/projects/storefront/observability", page: ProjectObservability(dash)},
		{file: "project-observability-quiet.html", path: "/projects/storefront/observability", page: ProjectObservability(quiet)},
		{file: "project-observability-no-store.html", path: "/projects/storefront/observability", page: ProjectObservability(noStore)},
		{file: "project-settings-usage.html", path: "/projects/storefront/settings", page: ProjectSettings(settings)},
		{file: "app-metric-charts.html", path: "/projects/storefront/observability", page: AppMetricCharts(appCharts)},
	}
	for i := range pages {
		pages[i].boot = charts
	}
	return pages
}
