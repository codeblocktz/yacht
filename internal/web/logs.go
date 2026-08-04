package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Logger is the dashboard's view of container output.
type Logger interface {
	Logs(ctx context.Context, ownerID, appName string, req app.LogRequest) (app.Logs, error)

	// LogStream follows an app's container output until ctx is done. The
	// iterator holds a connection open while it is read, so the handler must
	// stop reading when the client goes away.
	LogStream(
		ctx context.Context, ownerID, appName string, req app.LogRequest,
	) (iter.Seq2[orchestrator.LogLine, error], error)
	DeploymentLogs(
		ctx context.Context, ownerID, appName string, deployID uuid.UUID, req app.LogRequest,
	) (app.DeployLogs, error)
	Deployment(
		ctx context.Context, ownerID, appName string, deployID uuid.UUID,
	) (app.Deployment, error)

	// BuildForDeployment is the build behind one deployment, when there was
	// one. ErrNotFound means the app was deployed from an image nobody built.
	BuildForDeployment(
		ctx context.Context, ownerID string, deployID uuid.UUID,
	) (app.Build, error)

	// DeploymentHTTPLogs is what the ingress controller recorded for this
	// app's hostnames.
	DeploymentHTTPLogs(ctx context.Context, ownerID, appName string) (app.HTTPLogs, error)
}

// The log views one deployment's sheet offers.
//
// Named after what produced the output rather than after where it is read
// from, because that is the question somebody has when a deploy goes wrong:
// did it fail to build, fail to start, or start and serve errors.
const (
	viewDeploy = "deploy"
	viewBuild  = "build"
	viewHTTP   = "http"
)

// DeployLogsData is the sheet opened from one deployment.
type DeployLogsData struct {
	App    string
	Deploy app.DeployLogs

	// Build is what produced this deployment's image, when anything did.
	Build    app.Build
	HasBuild bool

	// HTTP is what the ingress controller recorded for this app.
	HTTP app.HTTPLogs

	// HTTPBuckets is the whole timeline, computed before paging.
	HTTPBuckets []app.HTTPBucket

	// Find is the search inside a log's own text, and Method narrows the
	// request list to one verb. Separate from the page's query only in name:
	// they travel the same way and are read the same way.
	Find   string
	Method string

	// Methods is what this app's requests actually used, so the selector
	// offers nothing that could only return an empty list.
	Methods []string

	// HTTPPage is the window of requests being shown. The chart above them is
	// deliberately not paged: it summarises the traffic, and a summary of one
	// page of it would answer a question nobody asked.
	HTTPPage Page

	// View is which of the three logs is showing.
	View     string
	Previous bool
	Error    string

	// DeployID is this deployment, so the build pane can address itself for
	// the poll that follows a running build.
	DeployID string

	// Pane asks for one pane rather than the whole sheet.
	Pane bool
}

// buildFragmentHref is where the build pane re-fetches itself.
//
// pane=1 renders the pane alone rather than the whole sheet, so a poll every
// two seconds does not replace the tab strip and the search box above it —
// including the one somebody is typing into while the build runs.
func buildFragmentHref(d DeployLogsData) string {
	return "/apps/" + d.App + "/deployments/" + d.DeployID + "/logs?view=build&pane=1"
}

// deployLogs renders the log sheet for one deployment.
//
// A fragment swapped into the sheet rather than a page: the deployments list
// stays where it was, which is the point of opening a panel over it instead of
// navigating away from it.
func (s *Server) deployLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	d := DeployLogsData{
		App:      name,
		DeployID: id.String(),
		Pane:     r.URL.Query().Get("pane") == "1",
		View:     logView(r.URL.Query().Get("view")),
		Previous: r.URL.Query().Get("previous") == "1",
		Find:     strings.TrimSpace(r.URL.Query().Get("q")),
		Method:   strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("method"))),
	}

	// The build and HTTP views read no container output: there is none for
	// them. Only the deployment itself is fetched, for the heading — and the
	// build, which is stored rather than read from the cluster.
	if d.View != viewDeploy {
		dep, err := s.logs.Deployment(ctx, owner.ID, name, id)
		switch {
		case errors.Is(err, app.ErrNotFound):
			http.NotFound(w, r)
			return
		case err != nil:
			s.log.Error("read deployment", slog.String("error", err.Error()))
			d.Error = "Could not read the deployment."
		default:
			d.Deploy.Deployment = dep
		}
		switch d.View {
		case viewBuild:
			d.Build, d.HasBuild = s.buildFor(ctx, owner.ID, id)
		case viewHTTP:
			if d.HTTP, err = s.logs.DeploymentHTTPLogs(ctx, owner.ID, name); err != nil {
				s.log.Error("read http logs", slog.String("error", err.Error()))
				d.Error = "Could not read the ingress controller's log."
			}
			d.pageHTTP(r, name, id)
		}
		s.renderPanel(ctx, w, d)
		return
	}

	dl, err := s.logs.DeploymentLogs(ctx, owner.ID, name, id, app.LogRequest{
		Pod: r.URL.Query().Get("pod"), Previous: d.Previous,
	})
	switch {
	case errors.Is(err, app.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("read deployment logs", slog.String("error", err.Error()))
		d.Error = "Could not read the logs."
	default:
		d.Deploy = dl
	}
	// The container log is searched in place rather than paged: it is already
	// capped at what the API returns, and a page control over it would break
	// the one thing a log is for, which is reading downwards.
	d.Deploy.Logs.Lines = filterLines(d.Deploy.Logs.Lines, d.Find)
	s.renderPanel(ctx, w, d)
}

// buildFor reads the build behind a deployment.
//
// A missing one is not a failure: an app deployed from an image nobody built
// has no build, which is the ordinary case and has its own answer on the page.
func (s *Server) buildFor(
	ctx context.Context, ownerID string, deployID uuid.UUID,
) (app.Build, bool) {
	b, err := s.logs.BuildForDeployment(ctx, ownerID, deployID)
	switch {
	case errors.Is(err, app.ErrNotFound):
		return app.Build{}, false
	case err != nil:
		s.log.Error("read build", slog.String("error", err.Error()))
		return app.Build{}, false
	}
	return b, true
}

// logView keeps an unknown view from rendering a sheet with no body.
func logView(v string) string {
	switch v {
	case viewBuild, viewHTTP:
		return v
	default:
		return viewDeploy
	}
}

func (s *Server) renderPanel(ctx context.Context, w http.ResponseWriter, d DeployLogsData) {
	// pane=1 asks for one pane rather than the whole sheet. The build pane
	// polls itself while a build runs, and swapping the sheet on every tick
	// would replace the tab strip and the search box above it — including the
	// one somebody is typing into.
	if d.Pane {
		if err := buildLogView(d).Render(ctx, w); err != nil {
			s.log.Error("render build pane", slog.String("error", err.Error()))
		}
		return
	}
	if err := DeployLogPanel(d).Render(ctx, w); err != nil {
		s.log.Error("render deployment logs", slog.String("error", err.Error()))
	}
}

// logsFragmentHref keeps the poll on the same view the controls chose.
func logsFragmentHref(d LogsData) string {
	q := "/apps/" + d.App + "/logs/lines?pod=" + d.Logs.Pod
	if d.Previous {
		q += "&previous=1"
	}
	return q
}

// logsStreamHref is the event stream for the pod the controls chose.
//
// Previous runs have none: that container has already died, so a stream would
// hold a connection open on output that can never grow.
func logsStreamHref(d LogsData) string {
	return "/apps/" + d.App + "/logs/stream?pod=" + url.QueryEscape(d.Logs.Pod)
}

// LogsData is the log pane.
type LogsData struct {
	App      string
	Logs     app.Logs
	Error    string
	Previous bool
}

// appLogs renders the log pane for an app.
func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	d := s.logsData(r)
	s.renderWithCrumb(w, r, AppLogs(d), d.App+" logs")
}

// maxStreamAge bounds how long one log stream stays open.
//
// A followed stream has no natural end, so without this a tab left open on a
// forgotten log holds a goroutine and a connection to the apiserver for as long
// as the browser lives. EventSource reconnects on its own, so the reader sees
// nothing; what it stops is an install accumulating streams it will never close.
const maxStreamAge = 30 * time.Minute

// appLogsStream follows the log over server-sent events.
//
// SSE rather than a socket: the data goes one way and is already text, so a
// socket would add framing and a keepalive loop to carry the same bytes — and
// EventSource reconnects by itself, which is most of the error handling.
func (s *Server) appLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Nothing can be streamed through a writer that cannot flush; every
		// line would arrive at once when the handler returned.
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), maxStreamAge)
	defer cancel()

	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	lines, err := s.logs.LogStream(ctx, owner.ID, name, app.LogRequest{
		Pod: r.URL.Query().Get("pod"),
	})
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("stream logs", slog.String("error", err.Error()))
		http.Error(w, "could not read the logs", http.StatusInternalServerError)
		return
	}

	// Written before the first line so the browser opens the stream rather than
	// waiting on a body it thinks is a document.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which holds every line until
	// the response ends — and this one does not end.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for line, err := range lines {
		if err != nil {
			s.log.Error("stream logs", slog.String("error", err.Error()))
			return
		}
		writeLogEvent(w, line)
		flusher.Flush()
	}
}

// writeLogEvent writes one line as a server-sent event.
//
// The text is split across data fields rather than sent whole: a newline inside
// a line would end the event early and the remainder would arrive as a field
// name the client does not know, which drops it silently. A stack trace is
// exactly such a line.
func writeLogEvent(w io.Writer, line orchestrator.LogLine) {
	fmt.Fprintf(w, "id: %d\n", line.At.UnixNano())
	for _, part := range strings.Split(line.Text, "\n") {
		fmt.Fprintf(w, "data: %s\n", part)
	}
	fmt.Fprint(w, "\n")
}

// appLogsFragment is the polled body, so following a log does not re-render
// the controls above it under the cursor.
func (s *Server) appLogsFragment(w http.ResponseWriter, r *http.Request) {
	if err := AppLogLines(s.logsData(r)).Render(r.Context(), w); err != nil {
		s.log.Error("render logs", slog.String("error", err.Error()))
	}
}

func (s *Server) logsData(r *http.Request) LogsData {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	previous := r.URL.Query().Get("previous") == "1"
	tail, _ := strconv.ParseInt(r.URL.Query().Get("tail"), 10, 64)

	d := LogsData{App: name, Previous: previous}
	l, err := s.logs.Logs(ctx, owner.ID, name, app.LogRequest{
		Pod: r.URL.Query().Get("pod"), Previous: previous, Tail: tail,
	})
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			d.Error = "No such app or pod."
			return d
		}
		s.log.Error("read logs", slog.String("error", err.Error()))
		d.Error = "Could not read the logs."
		return d
	}
	d.Logs = l
	return d
}

// deployLogsHref keeps the sheet on the same deployment when switching runs.
func deployLogsHref(d DeployLogsData, previous bool) string {
	u := deployViewHref(d, viewDeploy)
	if previous {
		u += "&previous=1"
	}
	return u
}

// deployViewHref switches which log the sheet is showing.
func deployViewHref(d DeployLogsData, view string) string {
	return "/apps/" + d.App + "/deployments/" +
		d.Deploy.Deployment.ID.String() + "/logs?view=" + view
}

// pageHTTP cuts the requests into a page, leaving the chart whole.
//
// The buckets are computed before the slice and kept on their own copy, so
// searching for "500" narrows the rows and the timeline above them still shows
// when the traffic actually happened. A chart that followed the filter would
// redraw itself into whatever the search selected, which is a different and
// much less useful picture.
func (d *DeployLogsData) pageHTTP(r *http.Request, app string, deploy uuid.UUID) {
	d.HTTPBuckets = d.HTTP.Buckets()
	d.Methods = methodsIn(d.HTTP.Lines)

	// The method narrows before anything else, so the count beside the search
	// and the pages under it both describe what is on screen.
	if d.Method != "" {
		kept := d.HTTP.Lines[:0:0]
		for _, l := range d.HTTP.Lines {
			if l.Method == d.Method {
				kept = append(kept, l)
			}
		}
		d.HTTP.Lines = kept
	}

	query, number := pageRequest(r)
	base := "/apps/" + app + "/deployments/" + deploy.String() + "/logs"
	extra := url.Values{"view": {viewHTTP}}
	if d.Method != "" {
		extra.Set("method", d.Method)
	}

	d.HTTP.Lines, d.HTTPPage = paginate(
		d.HTTP.Lines, query, number, base, extra, requestMatches)
}

// requestMatches is what a search over requests looks at.
//
// Every column on the row, including the status as text, so "500" finds server
// errors and "/orders" finds one endpoint. Searching only what is rendered is
// the rule: anything else answers "no matches" about text on the screen.
func requestMatches(l orchestrator.HTTPLogLine, needle string) bool {
	return strings.Contains(strings.ToLower(l.Path), needle) ||
		strings.Contains(strings.ToLower(l.Method), needle) ||
		strings.Contains(strings.ToLower(l.Host), needle) ||
		strings.Contains(l.Client, needle) ||
		strings.Contains(strconv.Itoa(l.Status), needle)
}
