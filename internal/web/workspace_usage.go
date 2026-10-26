package web

import (
	"log/slog"
	"net/http"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// WorkspaceUsageData is the workspace's Usage page: what each project has
// cost this month and will come to, as the meter writes it.
type WorkspaceUsageData struct {
	Projects []ProjectUsageRow
	// Note is the meter's fine print, once for the page.
	Note string
}

// ProjectUsageRow is one project's usage.
type ProjectUsageRow struct {
	Project app.Project
	Usage   ProjectUsage
}

// workspaceUsage prices every project. The amounts arrive written in the
// wrapping application's money, so they are listed rather than added up:
// the engine has no arithmetic for a currency it does not know, and the
// total is the wrapper's to show — its billing page, say.
func (s *Server) workspaceUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	var d WorkspaceUsageData
	projects, err := s.apps.Projects(ctx, owner.ID)
	if err != nil {
		s.log.Error("list projects for usage", slog.String("error", err.Error()))
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}
	for _, p := range projects {
		apps, err := s.apps.ListInProject(ctx, owner.ID, p.ID)
		if err != nil {
			s.log.Warn("list the project's apps", slog.String("project", p.Slug), slog.String("error", err.Error()))
			continue
		}
		u := s.projectUsage(ctx, owner.ID, p, apps)
		if u == nil {
			continue
		}
		if d.Note == "" {
			d.Note = u.Note
		}
		d.Projects = append(d.Projects, ProjectUsageRow{Project: p, Usage: *u})
	}
	s.render(w, r, WorkspaceUsage(d))
}
