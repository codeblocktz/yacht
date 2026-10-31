package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
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

// attachReferrers gives the Settings tab's delete the variables of other
// apps that refer to this one, which the delete would leave pointing at
// nothing: the first anybody heard of it was the other app's next deploy
// failing on "refers to db, which no longer exists".
func (s *Server) attachReferrers(ctx context.Context, d *AppDetailData) {
	if d.Tab != "settings" || s.apps == nil {
		return
	}
	src, ok := s.apps.(interface {
		Referrers(ctx context.Context, ownerID, appName string) ([]app.Referrer, error)
	})
	if !ok {
		return
	}
	refs, err := src.Referrers(ctx, d.App.OwnerID, d.App.Name)
	if err != nil {
		s.log.Warn("list referrers", slog.String("error", err.Error()))
		return
	}
	d.Referrers = refs
}

// deleteConfirm is a delete's confirmation, with what it breaks added.
func deleteConfirm(text string, refs []app.Referrer) string {
	switch len(refs) {
	case 0:
		return text
	case 1:
		return text + " " + refs[0].App + "'s " + refs[0].Key + " refers to it and will fail at its next deploy."
	}
	return text + " " + strconv.Itoa(len(refs)) + " variables of other services refer to it and will fail at their next deploy."
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
