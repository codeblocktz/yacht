package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Logger is the dashboard's view of container output.
type Logger interface {
	Logs(ctx context.Context, ownerID, appName string, req app.LogRequest) (app.Logs, error)
	DeploymentLogs(
		ctx context.Context, ownerID, appName string, deployID uuid.UUID, req app.LogRequest,
	) (app.DeployLogs, error)
	Deployment(
		ctx context.Context, ownerID, appName string, deployID uuid.UUID,
	) (app.Deployment, error)
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

	// View is which of the three logs is showing. Only viewDeploy has anything
	// to read on this install; the others say what would fill them.
	View     string
	Previous bool
	Error    string
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
		View:     logView(r.URL.Query().Get("view")),
		Previous: r.URL.Query().Get("previous") == "1",
	}

	// The build and HTTP views read nothing from the cluster: there is nothing
	// for them to read. Only the deployment itself is fetched, for the heading.
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
	s.renderPanel(ctx, w, d)
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
