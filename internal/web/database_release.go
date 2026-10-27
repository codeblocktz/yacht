package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// DatabaseReleases is a database's version and what its makers have
// released since: asked on the Deployments tab after it has drawn, since the
// answer can mean asking the database, a registry and postgresql.org.
//
// Optional, asserted on Apps like Databases.
type DatabaseReleases interface {
	DatabaseRelease(ctx context.Context, ownerID, name string) (app.DatabaseRelease, error)
}

func (s *Server) databaseReleases() DatabaseReleases {
	d, _ := s.apps.(DatabaseReleases)
	return d
}

// DatabaseReleaseData is the version line, or the warning in its place.
type DatabaseReleaseData struct {
	Name, Image string
	R           app.DatabaseRelease
}

// Tag is the part of the image after the colon, as a person wrote it:
// "16-alpine".
func (d DatabaseReleaseData) Tag() string {
	i := strings.LastIndex(d.Image, ":")
	if i < 0 || strings.Contains(d.Image[i:], "/") {
		return "latest"
	}
	return d.Image[i+1:]
}

func (s *Server) databaseRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	a, err := s.apps.Get(ctx, owner.ID, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel, err := s.databaseReleases().DatabaseRelease(ctx, owner.ID, name)
	if err != nil {
		// Nothing to say is said with nothing: the slot is left empty.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	d := DatabaseReleaseData{Name: a.Name, Image: a.Image, R: rel}
	if err := databaseReleaseBlock(d).Render(ctx, w); err != nil {
		s.log.Error("render database release", slog.String("error", err.Error()))
	}
}

func securityFixes(n int) string {
	if n == 1 {
		return "1 security fix"
	}
	return strconv.Itoa(n) + " security fixes"
}

// isSerious is a fix worth the red: high or critical.
func isSerious(severity string) bool {
	return severity == "critical" || severity == "high"
}

func severityStatus(severity string) string {
	switch severity {
	case "critical", "high":
		return "status-err"
	case "medium":
		return "status-warn"
	}
	return "status-neutral"
}

func outOfSupport(eol time.Time) string {
	if eol.IsZero() {
		return "It is out of support."
	}
	return "It ran out of support on " + eol.Format("2 January 2006") + "."
}

// quietRelease is what follows the version when there is nothing to do.
func quietRelease(r app.DatabaseRelease) string {
	switch {
	case !r.Known:
		return ""
	case !r.Report.Behind():
		return " · the newest " + r.Report.Running.Major + " release"
	case r.NewerImage:
		return " · " + r.Report.Latest.String() + " is out, with no security fixes this lacks · Redeploy to update"
	}
	return " · " + r.Report.Latest.String() + " is out, with no security fixes this lacks"
}

func fixesThem(n int) string {
	if n == 1 {
		return " fixes it."
	}
	return " fixes them."
}
