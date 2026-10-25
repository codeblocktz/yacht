package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/observe"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// ChartPanel is one block of a dashboard: the panel as read, a headline for
// it, and the id its data is embedded under.
type ChartPanel struct {
	observe.Panel
	ID string
	// Headline is the panel's latest value, in words: what the chart ends
	// on, so the block answers "how is it now" before anybody reads a line.
	Headline string
	// Legend is the series named, in the order they are coloured.
	Domain []string
}

// chartJSON is what the chart script reads: the panel and the colour order.
type chartJSON struct {
	observe.Panel
	Domain []string `json:"domain"`
}

func (p ChartPanel) payload() chartJSON { return chartJSON{Panel: p.Panel, Domain: p.Domain} }

// ServiceNow is one service's use at this moment, from metrics-server, for an
// install with no metrics store.
type ServiceNow struct {
	Name        string
	CPU, Memory string
	Running     bool
}

// ObservabilityData is a project's dashboard.
type ObservabilityData struct {
	Project app.Project
	Range   RangeChoice
	Ranges  []RangeChoice

	// Stored is whether there is a metrics store; Panels are its blocks.
	Stored bool
	Panels []ChartPanel

	// Now is what metrics-server says, when there is no store or it failed.
	Now      []ServiceNow
	NowKnown bool

	Empty bool
	Error string
}

func (d ObservabilityData) rangeHref(key string) string {
	return "/projects/" + d.Project.Slug + "/observability?range=" + key
}

// AppMetricsData is an app's Metrics tab charts, fetched into the tab.
type AppMetricsData struct {
	App    string
	Range  RangeChoice
	Ranges []RangeChoice
	Stored bool
	Panels []ChartPanel
	Now    *ServiceNow
	Error  string
}

func (d AppMetricsData) rangeHref(key string) string {
	return "/apps/" + d.App + "/metrics/charts?range=" + key
}

// projectObservability draws the dashboard.
func (s *Server) projectObservability(w http.ResponseWriter, r *http.Request) {
	p, apps, scope, err := s.projectApps(r)
	if s.projectNotFound(w, r, err) {
		return
	}
	d := ObservabilityData{Project: p, Stored: s.metricStore != nil, Empty: scope.Empty()}
	d.Range, d.Ranges = pickRange(r, "1h", 0)
	if !d.Empty {
		if d.Stored {
			panels, err := s.metricStore.Dashboard(r.Context(), scope, observe.WindowFor(time.Now(), d.Range.Span))
			if err != nil {
				s.log.Error("read the metrics store", slog.String("error", err.Error()))
				d.Error = "The metrics store could not be read."
			}
			d.Panels = chartPanels("project", panels, serviceNames(scope))
		}
		if !d.Stored || d.Error != "" {
			d.Now, d.NowKnown = s.usageNow(r.Context(), apps)
		}
	}
	s.render(w, r, ProjectObservability(d))
}

// appMetricCharts is an app's Metrics tab charts: the project dashboard's
// blocks for one service.
func (s *Server) appMetricCharts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	a, err := s.apps.Get(ctx, owner.ID, chi.URLParam(r, "name"))
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("get app", slog.String("error", err.Error()))
		http.Error(w, "could not load the app", http.StatusInternalServerError)
		return
	}
	d := AppMetricsData{App: a.Name, Stored: s.metricStore != nil}
	d.Range, d.Ranges = pickRange(r, "1h", 0)
	scope, err := scopeFor(owner.ID, []app.App{a})
	if err != nil {
		s.log.Error("scope app metrics", slog.String("error", err.Error()))
		d.Error = "This app's metrics could not be read."
	} else if d.Stored {
		panels, err := s.metricStore.Dashboard(ctx, scope, observe.WindowFor(time.Now(), d.Range.Span))
		if err != nil {
			s.log.Error("read the metrics store", slog.String("error", err.Error()))
			d.Error = "The metrics store could not be read."
		}
		d.Panels = chartPanels("app", panels, []string{a.Name})
	}
	if !d.Stored || d.Error != "" {
		if now, ok := s.usageNow(ctx, []app.App{a}); ok && len(now) == 1 {
			d.Now = &now[0]
		}
	}
	if err := AppMetricCharts(d).Render(ctx, w); err != nil {
		s.log.Error("render app metrics", slog.String("error", err.Error()))
	}
}

// chartPanels dresses the store's panels for the page.
func chartPanels(prefix string, panels []observe.Panel, services []string) []ChartPanel {
	out := make([]ChartPanel, 0, len(panels))
	for _, p := range panels {
		c := ChartPanel{Panel: p, ID: prefix + "-chart-" + p.Key, Headline: headline(p)}
		switch p.Key {
		case observe.PanelRequests:
			c.Domain = []string{"2xx", "3xx", "4xx", "5xx"}
		case observe.PanelLatency:
			c.Domain = []string{"p50", "p90", "p95", "p99"}
		default:
			c.Domain = services
		}
		out = append(out, c)
	}
	return out
}

// headline is what a panel ends on: the services added up where adding means
// something, the worst of them where it does not, and p95 for response time.
func headline(p observe.Panel) string {
	if p.Empty() {
		return "—"
	}
	var sum, worst float64
	for _, s := range p.Series {
		v, ok := s.Last()
		if !ok {
			continue
		}
		sum += v
		if v > worst {
			worst = v
		}
		if p.Key == observe.PanelLatency && s.Name == "p95" {
			return formatSeconds(v) + " p95"
		}
	}
	switch p.Unit {
	case observe.UnitCores:
		return formatCores(sum)
	case observe.UnitBytes:
		return formatBytes(int64(sum))
	case observe.UnitBytesSec:
		return formatBytes(int64(sum)) + "/s"
	case observe.UnitReqSec:
		return formatRate(sum) + " req/s"
	case observe.UnitRatio:
		return formatPercent(worst)
	case observe.UnitSeconds:
		return formatSeconds(worst)
	}
	return fmt.Sprintf("%.3g", sum)
}

func formatCores(v float64) string {
	if v < 1 {
		return fmt.Sprintf("%.0fm vCPU", v*1000)
	}
	return fmt.Sprintf("%.2f vCPU", v)
}

func formatRate(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v < 0.1:
		return fmt.Sprintf("%.3f", v)
	case v < 10:
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

func formatPercent(v float64) string {
	p := v * 100
	switch {
	case p == 0:
		return "0%"
	case p < 0.1:
		return "<0.1%"
	case p < 10:
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

func formatSeconds(v float64) string {
	switch {
	case v < 0.001:
		return fmt.Sprintf("%.0f µs", v*1e6)
	case v < 1:
		return fmt.Sprintf("%.0f ms", v*1000)
	}
	return fmt.Sprintf("%.2f s", v)
}

// usageNow is each app's use now, where the orchestrator can say.
func (s *Server) usageNow(ctx context.Context, apps []app.App) ([]ServiceNow, bool) {
	reader, ok := s.orch.(orchestrator.UsageReader)
	if !ok || len(apps) == 0 {
		return nil, false
	}
	namespaces := make([]string, len(apps))
	for i, a := range apps {
		namespaces[i] = a.Namespace
	}
	usage, err := reader.NamespaceUsage(ctx, namespaces)
	if err != nil {
		if !errors.Is(err, orchestrator.ErrNotSupported) {
			s.log.Warn("read usage now", slog.String("error", err.Error()))
		}
		return nil, false
	}
	out := make([]ServiceNow, 0, len(apps))
	for _, a := range apps {
		u, running := usage[a.Namespace]
		n := ServiceNow{Name: a.Name, Running: running, CPU: "—", Memory: "—"}
		if running {
			n.CPU = formatCores(float64(u.CPUMillis) / 1000)
			n.Memory = formatBytes(u.MemoryBytes)
		}
		out = append(out, n)
	}
	return out, true
}

// panelNote is what an empty panel says, which depends on what it reads.
func panelNote(p ChartPanel) string {
	if p.Err != "" {
		return "This block could not be read."
	}
	switch p.Key {
	case observe.PanelEgress, observe.PanelIngress, observe.PanelRequests,
		observe.PanelErrorRate, observe.PanelLatency:
		return "No requests through a public domain in this range."
	}
	if strings.HasPrefix(p.Key, "cpu") || p.Key == observe.PanelMemory {
		return "Nothing was running in this range."
	}
	return "No data in this range."
}
