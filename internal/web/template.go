package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Stacks is the dashboard's view of deploying a template.
//
// Separate from Apps because it is a different verb: Apps creates one workload,
// this creates several and the project holding them. Keeping it apart means the
// templates page cannot reach anything else on the app service by accident.
type Stacks interface {
	DeployTemplate(ctx context.Context, ownerID, slug, stack string) (app.Project, error)
}

// TemplateListData is the page offering the stacks that can be deployed.
type TemplateListData struct {
	Templates []app.Template
	Stack     string
	Slug      string
	Error     string
}

func (s *Server) templateList(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, TemplateList(TemplateListData{Templates: app.Templates()}))
}

// templateDeploy creates the stack and opens its canvas.
func (s *Server) templateDeploy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	slug := r.FormValue("template")
	stack := r.FormValue("stack")

	project, err := s.stacks.DeployTemplate(ctx, owner.ID, slug, stack)
	if err != nil {
		// Re-rendered with what was typed, so a refused name can be corrected
		// rather than typed again from nothing.
		s.log.Error("deploy template",
			slog.String("template", slug), slog.String("error", err.Error()))
		data := TemplateListData{
			Templates: app.Templates(), Stack: stack, Slug: slug, Error: err.Error(),
		}
		if errors.Is(err, app.ErrTemplateNotFound) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, TemplateList(data))
		return
	}

	// Onto the canvas, because the wiring is the thing worth seeing and it is
	// the one place that shows it.
	http.Redirect(w, r, "/projects/"+project.Slug, http.StatusSeeOther)
}
