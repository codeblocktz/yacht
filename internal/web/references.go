package web

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/codeblocktz/yacht/internal/app"
)

// attachReferenceTargets gives the variables tab the other apps it can point
// a variable at. Asked of the service only if it can answer, and only for
// that tab; without it the tab is the plain form it always was.
func (s *Server) attachReferenceTargets(ctx context.Context, d *AppDetailData) {
	if d.Tab != "variables" || s.apps == nil {
		return
	}
	s.attachVariables(ctx, d)
	src, ok := s.apps.(interface {
		ReferenceTargets(ctx context.Context, ownerID, appName string) ([]app.ReferenceTarget, error)
	})
	if !ok {
		return
	}
	targets, err := src.ReferenceTargets(ctx, d.App.OwnerID, d.App.Name)
	if err != nil {
		s.log.Warn("list reference targets", slog.String("error", err.Error()))
		return
	}
	d.RefTargets = targets
}

// referenceInput is the variable a reference form asks for: ${{app.KEY}},
// named after the key it points at unless it was given a name of its own —
// a DATABASE_URL is wanted as DATABASE_URL far more often than not.
func referenceInput(r *http.Request) (app.VariableInput, bool) {
	ref := strings.TrimSpace(r.FormValue("ref"))
	target, key, ok := strings.Cut(ref, ".")
	if !ok || target == "" || key == "" {
		return app.VariableInput{}, false
	}
	name := strings.TrimSpace(r.FormValue("key"))
	if name == "" {
		name = key
	}
	return app.VariableInput{Key: name, Value: app.Reference{App: target, Key: key}.String()}, true
}
