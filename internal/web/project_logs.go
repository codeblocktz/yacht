package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/observe"
)

// ProjectLogsData is a project's log explorer: every service's output, in one
// list, searchable over a range.
type ProjectLogsData struct {
	Project app.Project

	// Services are the project's, for the filter's hints and the colour
	// each is drawn in.
	Services []string

	// Query is the filter box as typed; Search is it read.
	Query  string
	Search observe.Search

	Range  RangeChoice
	Ranges []RangeChoice

	// Live follows new lines as they arrive, from the store's tail or, with
	// no store, by asking the pods again every few seconds.
	Live bool

	Entries []observe.Entry
	Buckets []logBucket

	// Limited says the list stopped at the read limit, so there are older
	// lines in the range than are shown.
	Limited bool

	// Stored is whether this came from the log store, and so reaches back
	// past the pods running now. Retention is how far.
	Stored    bool
	Retention time.Duration

	// Note explains an empty or partial answer; Error is a failure to read.
	Note  string
	Error string
}

// logBucket is one histogram column in the shape the chart script draws:
// the same three bands as the HTTP timeline, with lines that are not errors
// as ok. No line is a warning: a level of warn is not an error, and a third
// colour for it would be one more thing to read for no decision it changes.
type logBucket struct {
	At   time.Time `json:"at"`
	To   time.Time `json:"to"`
	OK   int64     `json:"ok"`
	Warn int64     `json:"warn"`
	Err  int64     `json:"err"`
}

func toLogBuckets(in []observe.Bucket) []logBucket {
	out := make([]logBucket, len(in))
	for i, b := range in {
		out[i] = logBucket{At: b.At, To: b.To, OK: b.Total - b.Errors, Err: b.Errors}
	}
	return out
}

// Base addresses of the page's parts.
func (d ProjectLogsData) base() string { return "/projects/" + d.Project.Slug + "/logs" }

func (d ProjectLogsData) params() map[string]string {
	m := map[string]string{"q": d.Query, "range": d.Range.Key}
	if d.Live {
		m["live"] = "1"
	}
	return m
}

func (d ProjectLogsData) href(set map[string]string) string {
	p := d.params()
	for k, v := range set {
		p[k] = v
	}
	return observeQuery(d.base(), p, nil)
}

func (d ProjectLogsData) linesHref() string {
	return observeQuery(d.base()+"/lines", d.params(), nil)
}

func (d ProjectLogsData) streamHref() string {
	return observeQuery(d.base()+"/stream", map[string]string{"q": d.Query}, nil)
}

func (d ProjectLogsData) downloadHref() string {
	return observeQuery(d.base()+"/download", map[string]string{"q": d.Query, "range": d.Range.Key}, nil)
}

// logsRetention is how far back the store is said to reach.
func (s *Server) logsRetention() time.Duration {
	if s.logRetention > 0 {
		return s.logRetention
	}
	return 7 * 24 * time.Hour
}

// projectLogs draws the log explorer.
func (s *Server) projectLogs(w http.ResponseWriter, r *http.Request) {
	d, ok := s.projectLogsData(w, r)
	if !ok {
		return
	}
	s.render(w, r, ProjectLogs(d))
}

// projectLogLines is the explorer's results alone, which a live page without
// a store re-fetches every few seconds rather than re-rendering its controls
// under somebody's cursor.
func (s *Server) projectLogLines(w http.ResponseWriter, r *http.Request) {
	d, ok := s.projectLogsData(w, r)
	if !ok {
		return
	}
	if err := projectLogResults(d).Render(r.Context(), w); err != nil {
		s.log.Error("render project logs", slog.String("error", err.Error()))
	}
}

func (s *Server) projectLogsData(w http.ResponseWriter, r *http.Request) (ProjectLogsData, bool) {
	p, apps, scope, err := s.projectApps(r)
	if s.projectNotFound(w, r, err) {
		return ProjectLogsData{}, false
	}
	d := ProjectLogsData{
		Project:   p,
		Services:  serviceNames(scope),
		Query:     trimmed(r, "q"),
		Live:      r.URL.Query().Get("live") == "1",
		Stored:    s.logStore != nil,
		Retention: s.logsRetention(),
	}
	d.Search = observe.ParseSearch(d.Query)
	max := time.Duration(0)
	if d.Stored {
		max = d.Retention
	}
	d.Range, d.Ranges = pickRange(r, "15m", max)

	scope = scope.Narrow(d.Search.Services)
	if scope.Empty() {
		if len(d.Search.Services) > 0 {
			d.Note = "No service in this project is called " + strings.Join(d.Search.Services, " or ") + "."
		} else {
			d.Note = "This project has no services yet, so there is nothing to show."
		}
		return d, true
	}

	now := time.Now().UTC()
	q := observe.LogQuery{
		Text: d.Search.Text, ErrorsOnly: d.Search.ErrorsOnly,
		Start: now.Add(-d.Range.Span), End: now, Limit: observe.DefaultLimit,
	}
	ctx := r.Context()
	if d.Stored {
		entries, err := s.logStore.Query(ctx, scope, q)
		if err != nil {
			s.log.Error("read the log store", slog.String("error", err.Error()))
			d.Error = "The log store could not be read. Live output from the running pods is below."
			d.Stored = false
		} else {
			d.Entries = entries
			d.Limited = len(entries) >= q.Limit
			if buckets, err := s.logStore.Histogram(ctx, scope, q); err == nil {
				d.Buckets = toLogBuckets(buckets)
			} else {
				s.log.Warn("count the log store", slog.String("error", err.Error()))
			}
			return d, true
		}
	}
	d.Entries, d.Note = s.liveProjectLogs(ctx, apps, scope, q)
	d.Buckets = toLogBuckets(observe.CountLines(d.Entries, q.Start, q.End))
	return d, true
}

// livePodTail is how many lines the fallback reads from each service's pod.
const livePodTail = 300

// liveProjectLogs is the explorer without a store: the running pod of each
// service, read the way an app's own log pane reads it, and merged by time.
//
// Through the Logger the app's log pane uses, so each read is scoped as that
// one is: the app resolved by owner, its pods listed in its own namespace.
func (s *Server) liveProjectLogs(
	ctx context.Context, apps []app.App, scope observe.Scope, q observe.LogQuery,
) ([]observe.Entry, string) {
	if s.logs == nil {
		return nil, "This install reads no container output."
	}
	owner := identity.MustFromContext(ctx)
	var out []observe.Entry
	for _, a := range apps {
		if _, in := scope.ServiceFor(a.Namespace); !in {
			continue
		}
		l, err := s.logs.Logs(ctx, owner.ID, a.Name, app.LogRequest{Tail: livePodTail})
		if err != nil {
			s.log.Warn("read live logs", slog.String("app", a.Name), slog.String("error", err.Error()))
			continue
		}
		for _, line := range l.Lines {
			level, isErr := observe.ClassifyLine(line.Text)
			e := observe.Entry{
				At: line.At, Service: a.Name, Pod: l.Pod, Message: line.Text,
				Level: level, Error: isErr,
			}
			if q.Matches(e) {
				out = append(out, e)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b observe.Entry) int { return a.At.Compare(b.At) })
	if len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	note := ""
	if len(out) == 0 {
		note = "No line in the running pods matches. Without a log store, only what each " +
			"service's current pod still holds can be shown."
	}
	return out, note
}

// projectLogStream follows the project's log over server-sent events, from
// the store's tail. Without a store there is nothing to follow, and the page
// polls the lines instead.
func (s *Server) projectLogStream(w http.ResponseWriter, r *http.Request) {
	if s.logStore == nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	_, _, scope, err := s.projectApps(r)
	if s.projectNotFound(w, r, err) {
		return
	}
	search := observe.ParseSearch(trimmed(r, "q"))
	scope = scope.Narrow(search.Services)
	if scope.Empty() {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), maxStreamAge)
	defer cancel()
	lines, err := s.logStore.Tail(ctx, scope, observe.LogQuery{Text: search.Text, ErrorsOnly: search.ErrorsOnly})
	if err != nil {
		s.log.Error("tail the log store", slog.String("error", err.Error()))
		http.Error(w, "could not follow the logs", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for e, err := range lines {
		if err != nil {
			s.log.Warn("tail the log store", slog.String("error", err.Error()))
			return
		}
		// JSON in one data field: the script builds the row with
		// textContent, so a line of somebody's output is never markup.
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
}

// projectLogDownload is the range's lines as a text file.
func (s *Server) projectLogDownload(w http.ResponseWriter, r *http.Request) {
	p, apps, scope, err := s.projectApps(r)
	if s.projectNotFound(w, r, err) {
		return
	}
	search := observe.ParseSearch(trimmed(r, "q"))
	scope = scope.Narrow(search.Services)
	max := time.Duration(0)
	if s.logStore != nil {
		max = s.logsRetention()
	}
	rng, _ := pickRange(r, "15m", max)
	now := time.Now().UTC()
	q := observe.LogQuery{Text: search.Text, ErrorsOnly: search.ErrorsOnly, Start: now.Add(-rng.Span), End: now}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-logs-%s.txt"`,
		p.Slug, now.Format("20060102-150405")))
	if scope.Empty() {
		return
	}
	if s.logStore != nil {
		if err := s.logStore.Export(r.Context(), scope, q, w); err != nil {
			s.log.Error("export logs", slog.String("error", err.Error()))
		}
		return
	}
	q.Limit = observe.MaxLimit
	entries, _ := s.liveProjectLogs(r.Context(), apps, scope, q)
	for _, e := range entries {
		fmt.Fprintf(w, "%s %s %s\n", e.At.UTC().Format(time.RFC3339Nano), e.Service, e.Message)
	}
}
