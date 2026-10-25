package web

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/observe"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// LogHistory is the log store a project's Logs page reads: every line every
// container wrote, kept after its pod is gone. observe.LogStore is the
// engine's; an interface here so the pages can be tested without one.
//
// Every method takes an observe.Scope, which only this package's projectScope
// builds, from the signed-in team's own apps. Nothing a request carries
// reaches a store except as a filter inside that scope.
type LogHistory interface {
	Query(ctx context.Context, scope observe.Scope, q observe.LogQuery) ([]observe.Entry, error)
	Histogram(ctx context.Context, scope observe.Scope, q observe.LogQuery) ([]observe.Bucket, error)
	Tail(ctx context.Context, scope observe.Scope, q observe.LogQuery) (iter.Seq2[observe.Entry, error], error)
	Export(ctx context.Context, scope observe.Scope, q observe.LogQuery, w io.Writer) error
}

// MetricHistory is the metrics store the Observability page, an app's Metrics
// tab and the project's usage read.
type MetricHistory interface {
	Dashboard(ctx context.Context, scope observe.Scope, w observe.Window) ([]observe.Panel, error)
	PublicEgress(ctx context.Context, scope observe.Scope, since, now time.Time) (map[string]int64, error)
}

// Compile-time checks that the engine's stores are what the pages read.
var (
	_ LogHistory    = (*observe.LogStore)(nil)
	_ MetricHistory = (*observe.MetricStore)(nil)
)

// mountObserve puts a project's Logs and Observability on the member group,
// with the polled and streamed halves of each, and the charts on an app's
// Metrics tab.
//
// Always mounted, store or no store. Without one the pages say so and show
// what the cluster can say now, which is what an app's own log and Metrics tab
// always did — a rail entry that came and went with a setting would be a page
// nobody could find the day it started working.
func (s *Server) mountObserve(r chi.Router) {
	r.Get("/projects/{slug}/logs", s.projectLogs)
	r.Get("/projects/{slug}/logs/lines", s.projectLogLines)
	r.Get("/projects/{slug}/logs/stream", s.projectLogStream)
	r.Get("/projects/{slug}/logs/download", s.projectLogDownload)
	r.Get("/projects/{slug}/observability", s.projectObservability)
	r.Get("/apps/{name}/metrics/charts", s.appMetricCharts)
}

// RangeChoice is one entry in a page's time range picker.
type RangeChoice struct {
	Key    string
	Label  string
	Span   time.Duration
	Active bool
}

// The ranges offered. The logs page offers those within the store's retention
// and the metrics pages all of them.
var observeRanges = []RangeChoice{
	{Key: "15m", Label: "Last 15 min", Span: 15 * time.Minute},
	{Key: "1h", Label: "Last hour", Span: time.Hour},
	{Key: "6h", Label: "Last 6 hours", Span: 6 * time.Hour},
	{Key: "24h", Label: "Last 24 hours", Span: 24 * time.Hour},
	{Key: "7d", Label: "Last 7 days", Span: 7 * 24 * time.Hour},
	{Key: "30d", Label: "Last 30 days", Span: 30 * 24 * time.Hour},
}

// pickRange reads a range from the request, falling back to fallback for one
// that is unknown or longer than max.
func pickRange(r *http.Request, fallback string, max time.Duration) (RangeChoice, []RangeChoice) {
	key := r.URL.Query().Get("range")
	var offered []RangeChoice
	chosen := -1
	for _, c := range observeRanges {
		if max > 0 && c.Span > max {
			continue
		}
		offered = append(offered, c)
		if c.Key == key {
			chosen = len(offered) - 1
		}
	}
	if chosen < 0 {
		for i, c := range offered {
			if c.Key == fallback {
				chosen = i
			}
		}
	}
	if chosen < 0 {
		chosen = 0
	}
	offered[chosen].Active = true
	return offered[chosen], offered
}

// projectApps resolves a project and its apps for the signed-in team, and the
// scope a query about them may read.
//
// The one place an observe.Scope is made from a request, and the request gives
// it only the project's slug: the project is looked up by owner, its apps are
// listed by owner, and the scope is those apps' namespaces under that owner.
// A slug that is not the team's is not found, so it scopes nothing.
func (s *Server) projectApps(r *http.Request) (app.Project, []app.App, observe.Scope, error) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	if s.apps == nil {
		return app.Project{}, nil, observe.Scope{}, app.ErrProjectNotFound
	}
	p, err := s.apps.Project(ctx, owner.ID, chi.URLParam(r, "slug"))
	if err != nil {
		return app.Project{}, nil, observe.Scope{}, err
	}
	apps, err := s.apps.ListInProject(ctx, owner.ID, p.ID)
	if err != nil {
		return p, nil, observe.Scope{}, err
	}
	scope, err := scopeFor(owner.ID, apps)
	return p, apps, scope, err
}

// scopeFor is the scope of some apps the caller already read for owner.
func scopeFor(ownerID string, apps []app.App) (observe.Scope, error) {
	services := make([]observe.Service, 0, len(apps))
	for _, a := range apps {
		if a.OwnerID != ownerID {
			// Never the case for apps read by owner; a guard against a
			// caller that passed some other list.
			continue
		}
		services = append(services, observe.Service{
			Name: a.Name, Namespace: a.Namespace,
			Port: orchestrator.ServicePort(a.Internal, a.Port),
		})
	}
	return observe.NewScope(ownerID, services)
}

// projectNotFound answers a project lookup that failed.
func (s *Server) projectNotFound(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, app.ErrProjectNotFound):
		http.NotFound(w, r)
	default:
		s.log.Error("read project for observability", slog.String("error", err.Error()))
		http.Error(w, "could not load the project", http.StatusInternalServerError)
	}
	return true
}

// serviceNames are the project's services, in the order every chart colours
// them by, so a service is one colour on every panel of the page.
func serviceNames(scope observe.Scope) []string {
	var out []string
	for _, svc := range scope.Services() {
		out = append(out, svc.Name)
	}
	return out
}

// observeQuery keeps a page's controls on the links that change one of them.
func observeQuery(base string, set map[string]string, keep url.Values) string {
	v := url.Values{}
	for k, vals := range keep {
		if len(vals) > 0 && vals[0] != "" {
			v.Set(k, vals[0])
		}
	}
	for k, val := range set {
		if val == "" {
			v.Del(k)
		} else {
			v.Set(k, val)
		}
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// trimmed reads a query parameter.
func trimmed(r *http.Request, key string) string {
	return strings.TrimSpace(r.URL.Query().Get(key))
}

// liveToggle is the live parameter that turns live tail the other way.
func liveToggle(on bool) string {
	if on {
		return ""
	}
	return "1"
}

func joinNames(names []string) string { return strings.Join(names, ", ") }

// hasCounts reports whether a histogram has anything to draw.
func hasCounts(buckets []logBucket) bool {
	for _, b := range buckets {
		if b.OK+b.Err > 0 {
			return true
		}
	}
	return false
}

// logTime is a line's time, with the date only when it is not today's: a
// range of hours is read by the clock, and a range of days needs the day.
func logTime(e observe.Entry) string {
	at := e.At.UTC()
	if y, m, d := at.Date(); y == time.Now().UTC().Year() && m == time.Now().UTC().Month() && d == time.Now().UTC().Day() {
		return at.Format("15:04:05")
	}
	return at.Format("Jan 2 15:04:05")
}

// serviceColour is the categorical slot a service is drawn in: its place in
// the project, as the charts colour it, folded onto eight.
func serviceColour(services []string, name string) string {
	i := slices.Index(services, name)
	if i < 0 {
		i = 0
	}
	return "var(--series-" + strconv.Itoa(i%8+1) + ")"
}

// appPanelOrder puts an app's blocks in the order its tab reads: traffic
// first, because that is what a service's Metrics tab is opened to check,
// then what it uses.
func appPanelOrder(panels []ChartPanel) []ChartPanel {
	order := []string{
		observe.PanelRequests, observe.PanelErrorRate, observe.PanelLatency,
		observe.PanelCPU, observe.PanelMemory, observe.PanelEgress, observe.PanelIngress,
	}
	out := make([]ChartPanel, 0, len(panels))
	for _, key := range order {
		for _, p := range panels {
			if p.Key == key {
				out = append(out, p)
			}
		}
	}
	return out
}
