package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// What the Variables tab, the canvas's changes bar and the project's shared
// variables need beyond the Apps interface. Each is asserted rather than
// added to Apps, so a wrapping application's own implementation keeps
// compiling; without one, the part of the page that needs it is left out.
type (
	// changeStager is a service whose edits wait for a deploy.
	changeStager interface {
		StagesChanges() bool
		StagedChanges(ctx context.Context, ownerID, appName string) ([]app.StagedChange, error)
		DeployStaged(ctx context.Context, ownerID, appName string) error
		DiscardStaged(ctx context.Context, ownerID, appName string) ([]string, error)
	}

	// projectStager deploys or discards a whole project's changes at once.
	projectStager interface {
		ProjectStagedChanges(ctx context.Context, ownerID, projectSlug string) ([]app.StagedChange, error)
		DeployProjectStaged(ctx context.Context, ownerID, projectSlug string) ([]string, error)
		DiscardProjectStaged(ctx context.Context, ownerID, projectSlug string) ([]string, error)
	}

	// variableRevealer opens one value for the person asking.
	variableRevealer interface {
		RevealVariable(ctx context.Context, ownerID, appName, key string) (string, error)
	}

	// rawVariables replaces the plain set from a .env file.
	rawVariables interface {
		SetRawVariables(ctx context.Context, ownerID, appName, raw string) error
	}

	// platformVariables lists what the platform gives every app.
	platformVariables interface {
		PlatformVariables(ctx context.Context, ownerID, appName string) ([]app.Variable, error)
	}

	// sharedVariables are a project's, used as ${{shared.KEY}}.
	sharedVariables interface {
		SharedVariables(ctx context.Context, ownerID, projectSlug string) ([]app.Variable, error)
		SetSharedVariable(ctx context.Context, ownerID, projectSlug string, in app.VariableInput) error
		DeleteSharedVariable(ctx context.Context, ownerID, projectSlug, key string) error
		RevealSharedVariable(ctx context.Context, ownerID, projectSlug, key string) (string, error)
	}
)

// VariablesView is what the Variables tab shows beyond the app's own list.
type VariablesView struct {
	// Stages says saves wait for Deploy, and Changes is what is waiting.
	Stages  bool
	Changes []app.StagedChange

	// Platform is what the platform adds at deploy, and Brand the name it
	// is listed under: "N variables added by datahuru".
	Platform []app.Variable
	Brand    string

	// CanReveal is whether the eye buttons can ask for a secret, and Raw
	// whether the Raw Editor can save.
	CanReveal bool
	Raw       bool
}

// Shadowed reports whether one of the app's own variables replaces the
// platform's of that name.
func (v VariablesView) Shadowed(a app.App, key string) bool {
	for _, own := range a.Variables {
		if own.Key == key {
			return true
		}
	}
	return false
}

// attachVariables fills the Variables tab's view. Only for that tab: none of
// it is needed anywhere else in the panel.
func (s *Server) attachVariables(ctx context.Context, d *AppDetailData) {
	if d.Tab != "variables" || s.apps == nil {
		return
	}
	d.Vars.Brand = s.wakerBrand
	if d.Vars.Brand == "" {
		d.Vars.Brand = DefaultBrandName
	}
	if st, ok := s.apps.(changeStager); ok {
		d.Vars.Stages = st.StagesChanges()
		if changes, err := st.StagedChanges(ctx, d.App.OwnerID, d.App.Name); err == nil {
			d.Vars.Changes = changes
		} else {
			s.log.Warn("list staged changes", slog.String("app", d.App.Name), slog.String("error", err.Error()))
		}
	}
	if pv, ok := s.apps.(platformVariables); ok {
		if vars, err := pv.PlatformVariables(ctx, d.App.OwnerID, d.App.Name); err == nil {
			d.Vars.Platform = vars
		} else {
			s.log.Warn("list platform variables", slog.String("app", d.App.Name), slog.String("error", err.Error()))
		}
	}
	_, d.Vars.CanReveal = s.apps.(variableRevealer)
	_, d.Vars.Raw = s.apps.(rawVariables)
}

// stages reports whether this server's edits wait for Deploy.
func (s *Server) stages() bool {
	st, ok := s.apps.(changeStager)
	return ok && st.StagesChanges()
}

// actorOf names who is making a request, for an audit line: the person,
// where there are accounts, and otherwise the team the request acts as —
// all an install without accounts knows.
func (s *Server) actorOf(r *http.Request) string {
	owner := identity.MustFromContext(r.Context())
	if s.accounts != nil {
		if sess, err := s.accounts.ResolveSession(r.Context(), sessionToken(r)); err == nil {
			return "user:" + sess.UserID.String()
		}
	}
	return "team:" + owner.ID
}

// variableReveal answers one masked value's eye button.
//
// Logged every time, with who asked, which app and which key — never the
// value. A secret shown on a screen is a secret somebody saw, and the log is
// the only record that they did.
func (s *Server) variableReveal(w http.ResponseWriter, r *http.Request) {
	rv, ok := s.apps.(variableRevealer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name, key := chi.URLParam(r, "name"), chi.URLParam(r, "key")
	value, err := rv.RevealVariable(ctx, owner.ID, name, key)
	if !s.revealed(w, r, err, "app", name, key) {
		return
	}
	s.renderRevealed(w, r, value)
}

// sharedVariableReveal is variableReveal for a project's shared variable.
func (s *Server) sharedVariableReveal(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.apps.(sharedVariables)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug, key := chi.URLParam(r, "slug"), chi.URLParam(r, "key")
	value, err := sv.RevealSharedVariable(ctx, owner.ID, slug, key)
	if !s.revealed(w, r, err, "project", slug, key) {
		return
	}
	s.renderRevealed(w, r, value)
}

// renderRevealed writes the value alone, for the eye button to swap in:
// a fragment, never a page.
func (s *Server) renderRevealed(w http.ResponseWriter, r *http.Request, value string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := revealedValue(value).Render(r.Context(), w); err != nil {
		s.log.Error("render revealed value", slog.String("error", err.Error()))
	}
}

// revealed answers a reveal that failed, and logs one that did not.
func (s *Server) revealed(w http.ResponseWriter, r *http.Request, err error, scope, name, key string) bool {
	switch {
	case err == nil:
		s.log.Info("variable revealed",
			slog.String(scope, name), slog.String("key", key), slog.String("by", s.actorOf(r)))
		return true
	case errors.Is(err, app.ErrNotFound), errors.Is(err, app.ErrVariableNotFound),
		errors.Is(err, app.ErrProjectNotFound):
		http.NotFound(w, r)
	default:
		// The reason stays in the log: why a secret would not open names
		// keys and ciphertext, which is not the page's business.
		s.log.Error("reveal variable",
			slog.String(scope, name), slog.String("key", key), slog.String("error", err.Error()))
		http.Error(w, "could not open that value", http.StatusInternalServerError)
	}
	return false
}

// variablesRaw saves the Raw Editor: every variable that is not a secret,
// replaced by what the text says.
func (s *Server) variablesRaw(w http.ResponseWriter, r *http.Request) {
	rv, ok := s.apps.(rawVariables)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	err := rv.SetRawVariables(ctx, owner.ID, name, r.FormValue("raw"))
	if errors.Is(err, app.ErrSavedWhileDeploying) {
		s.flashOK(w, r, "Saved. A deploy is already running — redeploy once it finishes to apply it.")
		http.Redirect(w, r, "/apps/"+name+"/variables", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.variableFailed(w, r, name, err)
		return
	}
	s.flashSaved(w, r, "Variables saved.")
	http.Redirect(w, r, "/apps/"+name+"/variables", http.StatusSeeOther)
}

// flashSaved says a save went through, and — where saves wait — that it is
// waiting, so nobody reads the absence of a deploy as the save having failed.
func (s *Server) flashSaved(w http.ResponseWriter, r *http.Request, text string) {
	if s.stages() {
		text += " Deploy when you are ready."
	}
	s.flashOK(w, r, text)
}

// appDeployStaged ships one app's waiting changes.
func (s *Server) appDeployStaged(w http.ResponseWriter, r *http.Request) {
	st, ok := s.apps.(changeStager)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	back := backTo(r, "/apps/"+name+"/variables")
	switch err := st.DeployStaged(ctx, owner.ID, name); {
	case err == nil:
		s.flashOK(w, r, "Deploying "+name+".")
	case errors.Is(err, app.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, app.ErrNothingStaged):
		s.flashOK(w, r, "Nothing to deploy — "+name+" is running everything saved.")
	case errors.Is(err, app.ErrOperationInFlight):
		s.flashWarn(w, r, "A deploy of "+name+" is already running. Deploy again once it finishes.")
	default:
		s.flashErr(w, r, strings.TrimPrefix(err.Error(), "app: "))
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// appDiscardStaged throws one app's waiting changes away.
func (s *Server) appDiscardStaged(w http.ResponseWriter, r *http.Request) {
	st, ok := s.apps.(changeStager)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	back := backTo(r, "/apps/"+name+"/variables")
	kept, err := st.DiscardStaged(ctx, owner.ID, name)
	switch {
	case err == nil:
		s.flashOK(w, r, discardedMessage(kept))
	case errors.Is(err, app.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, app.ErrOperationInFlight):
		s.flashWarn(w, r, "A deploy of "+name+" is running. Discard once it finishes.")
	default:
		s.flashErr(w, r, strings.TrimPrefix(err.Error(), "app: "))
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// projectDeployStaged is the bar's Deploy: every app in the project with
// something waiting.
func (s *Server) projectDeployStaged(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.apps.(projectStager)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug := chi.URLParam(r, "slug")
	ctx = app.WithEnvironment(ctx, chosenEnv(r, slug))
	deployed, err := ps.DeployProjectStaged(ctx, owner.ID, slug)
	switch {
	case errors.Is(err, app.ErrProjectNotFound):
		http.NotFound(w, r)
		return
	case err != nil && len(deployed) > 0:
		s.flashWarn(w, r, "Deploying "+strings.Join(deployed, ", ")+". Not deployed — "+
			strings.TrimPrefix(err.Error(), "app: "))
	case err != nil:
		s.flashErr(w, r, strings.TrimPrefix(err.Error(), "app: "))
	case len(deployed) == 0:
		s.flashOK(w, r, "Nothing to deploy.")
	default:
		s.flashOK(w, r, "Deploying "+strings.Join(deployed, ", ")+".")
	}
	http.Redirect(w, r, backTo(r, "/projects/"+slug), http.StatusSeeOther)
}

// projectDiscardStaged is the bar's Discard.
func (s *Server) projectDiscardStaged(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.apps.(projectStager)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug := chi.URLParam(r, "slug")
	ctx = app.WithEnvironment(ctx, chosenEnv(r, slug))
	kept, err := ps.DiscardProjectStaged(ctx, owner.ID, slug)
	switch {
	case errors.Is(err, app.ErrProjectNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.flashErr(w, r, strings.TrimPrefix(err.Error(), "app: "))
	default:
		s.flashOK(w, r, discardedMessage(kept))
	}
	http.Redirect(w, r, backTo(r, "/projects/"+slug), http.StatusSeeOther)
}

func discardedMessage(kept []string) string {
	if len(kept) == 0 {
		return "Changes discarded."
	}
	// Each one's note in Details says why: a secret whose earlier value was
	// never kept, or a change made on another app or the project.
	return "Changes discarded, except " + strings.Join(kept, ", ") +
		", which Discard cannot undo — Details says why."
}

// backTo is where a form posted from the canvas or the panel goes back to:
// the page it was posted from when that is one of ours, so Deploy on the
// canvas leaves somebody on the canvas, and fallback otherwise.
func backTo(r *http.Request, fallback string) string {
	next := r.FormValue("next")
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.Contains(next, "\\") {
		return next
	}
	return fallback
}

// sharedVariableSet stores one of a project's shared variables.
func (s *Server) sharedVariableSet(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.apps.(sharedVariables)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug := chi.URLParam(r, "slug")
	in := app.VariableInput{
		Key: r.FormValue("key"), Value: r.FormValue("value"), Secret: formChecked(r, "secret"),
	}
	if err := sv.SetSharedVariable(ctx, owner.ID, slug, in); err != nil {
		s.renderProjectSettings(w, r, http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), "app: "))
		return
	}
	s.flashOK(w, r, "Saved "+strings.TrimSpace(in.Key)+". Apps that use it read it when they next deploy.")
	http.Redirect(w, r, "/projects/"+slug+"/settings#shared-variables", http.StatusSeeOther)
}

// sharedVariableDelete removes one.
func (s *Server) sharedVariableDelete(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.apps.(sharedVariables)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	slug, key := chi.URLParam(r, "slug"), chi.URLParam(r, "key")
	if err := sv.DeleteSharedVariable(ctx, owner.ID, slug, key); err != nil {
		if errors.Is(err, app.ErrVariableNotFound) {
			http.NotFound(w, r)
			return
		}
		s.renderProjectSettings(w, r, http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), "app: "))
		return
	}
	s.flashOK(w, r, "Removed "+key+".")
	http.Redirect(w, r, "/projects/"+slug+"/settings#shared-variables", http.StatusSeeOther)
}

// attachSharedVariables gives the project settings page its shared
// variables, where the service has them.
func (s *Server) attachSharedVariables(r *http.Request, ownerID string, d *ProjectSettingsData) {
	ctx := r.Context()
	sv, ok := s.apps.(sharedVariables)
	if !ok {
		return
	}
	d.SharedOn = true
	// Changing one is setting a variable, which is an admin's, whatever the
	// rest of the page offers.
	d.SharedAdmin = s.Allows(r, account.RoleAdmin)
	vars, err := sv.SharedVariables(ctx, ownerID, d.Project.Slug)
	if err != nil {
		s.log.Warn("list shared variables", slog.String("project", d.Project.Slug), slog.String("error", err.Error()))
		return
	}
	d.Shared = vars
}

// StagedBar is the bar across the top of a project's canvas: how many
// changes are waiting, and what they are, app by app.
type StagedBar struct {
	Changes []app.StagedChange
	ByApp   []StagedApp
}

// StagedApp is one app's part of the bar.
type StagedApp struct {
	App     string
	Changes []app.StagedChange
}

// Count is the number of changes the bar says.
func (b *StagedBar) Count() int {
	if b == nil {
		return 0
	}
	return len(b.Changes)
}

// For is how many changes one app has waiting, for its card's marker.
func (b *StagedBar) For(name string) int {
	if b == nil {
		return 0
	}
	for _, a := range b.ByApp {
		if a.App == name {
			return len(a.Changes)
		}
	}
	return 0
}

// attachStaged gives the canvas its changes bar, where saves wait for one.
func (s *Server) attachStaged(ctx context.Context, ownerID string, d *CanvasData) {
	if !s.stages() {
		return
	}
	ps, ok := s.apps.(projectStager)
	if !ok {
		return
	}
	if d.Env != "" {
		ctx = app.WithEnvironment(ctx, d.Env)
	}
	changes, err := ps.ProjectStagedChanges(ctx, ownerID, d.Project.Slug)
	if err != nil {
		s.log.Warn("list the project's staged changes",
			slog.String("project", d.Project.Slug), slog.String("error", err.Error()))
		return
	}
	if len(changes) == 0 {
		return
	}
	bar := &StagedBar{Changes: changes}
	index := map[string]int{}
	for _, c := range changes {
		i, seen := index[c.App]
		if !seen {
			i = len(bar.ByApp)
			index[c.App] = i
			bar.ByApp = append(bar.ByApp, StagedApp{App: c.App})
		}
		bar.ByApp[i].Changes = append(bar.ByApp[i].Changes, c)
	}
	sort.SliceStable(bar.ByApp, func(i, j int) bool { return bar.ByApp[i].App < bar.ByApp[j].App })
	d.Staged = bar
}

// changeLabel is one staged change as the details list says it.
func changeLabel(c app.StagedChange) string {
	if c.Setting {
		return c.Key + ": " + c.From + " → " + c.To
	}
	return c.Key
}

// changeVerb is the word beside it.
func changeVerb(c app.StagedChange) string {
	switch c.Kind {
	case app.StagedAdded:
		return "Added"
	case app.StagedRemoved:
		return "Removed"
	}
	if c.Setting {
		return "Setting"
	}
	return "Changed"
}

func changeClass(c app.StagedChange) string {
	switch c.Kind {
	case app.StagedAdded:
		return "staged-added"
	case app.StagedRemoved:
		return "staged-removed"
	}
	return "staged-changed"
}

// secretKeys names the secrets in a list, for the Raw Editor's note.
func secretKeys(vars []app.Variable) []string {
	var out []string
	for _, v := range vars {
		if v.Secret {
			out = append(out, v.Key)
		}
	}
	return out
}

// rawRows sizes the Raw Editor to its text, within reason: a textarea that
// scrolls inside a scrolling panel is two scrollbars for one list.
func rawRows(vars []app.Variable) int {
	n := strings.Count(app.FormatEnv(vars), "\n") + 2
	return min(max(n, 6), 24)
}

// canvasHere is the canvas a bar's form returns to: the open panel, if one
// is, so deploying from it leaves somebody where they were.
func canvasHere(d CanvasData) string {
	if d.Open != nil {
		path := "/apps/" + d.Open.App.Name
		if d.Open.Tab != "" {
			path += "/" + d.Open.Tab
		}
		return path
	}
	return "/projects/" + d.Project.Slug
}
