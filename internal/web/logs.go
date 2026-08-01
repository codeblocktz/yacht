package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Logger is the dashboard's view of container output.
type Logger interface {
	Logs(ctx context.Context, ownerID, appName string, req app.LogRequest) (app.Logs, error)
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
