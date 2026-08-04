package web

import (
	"cmp"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// gib is the unit the form works in. Bytes are what the engine stores, because
// comparing sizes is what enforces grow-only and arithmetic on bytes has no
// rounding to argue about; gigabytes are what a person types.
const gib = 1 << 30

// storageAttach adds a volume to an app.
func (s *Server) storageAttach(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	size, err := sizeFromForm(r.FormValue("size_gb"))
	if err != nil {
		s.storageFailed(w, r, name, err)
		return
	}

	if _, err := s.apps.AttachVolume(ctx, owner.ID, name, app.VolumeInput{
		Name:      strings.TrimSpace(r.FormValue("name")),
		MountPath: strings.TrimSpace(r.FormValue("mount_path")),
		SizeBytes: size,
	}); err != nil {
		s.storageFailed(w, r, name, err)
		return
	}

	http.Redirect(w, r, "/apps/"+name+"/storage", http.StatusSeeOther)
}

// storageResize grows a volume. It cannot shrink one.
func (s *Server) storageResize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	size, err := sizeFromForm(r.FormValue("size_gb"))
	if err != nil {
		s.storageFailed(w, r, name, err)
		return
	}

	if err := s.apps.ResizeVolume(
		ctx, owner.ID, name, chi.URLParam(r, "volume"), size,
	); err != nil {
		s.storageFailed(w, r, name, err)
		return
	}

	http.Redirect(w, r, "/apps/"+name+"/storage", http.StatusSeeOther)
}

// storageDelete removes a volume, once its name has been typed back.
//
// Confirmation is the volume's own name rather than a checkbox, for the same
// reason the app delete form asks: the cost of getting this wrong is data that
// does not come back, and a checkbox is something a person clicks past.
func (s *Server) storageDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	volume := chi.URLParam(r, "volume")

	if strings.TrimSpace(r.FormValue("confirm")) != volume {
		s.storageFailed(w, r, name, errors.New(
			"type the volume's name to confirm — deleting it destroys what it holds"))
		return
	}

	// Detached in the same call: the workload is applied without the mount
	// before the row goes, so the pod has let go by the time the claim is
	// unreferenced.
	if err := s.apps.DeleteVolume(ctx, owner.ID, name, volume, true); err != nil {
		s.storageFailed(w, r, name, err)
		return
	}

	http.Redirect(w, r, "/apps/"+name+"/storage", http.StatusSeeOther)
}

// sizeFromForm reads a size in gigabytes.
func sizeFromForm(raw string) (int64, error) {
	gb, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || gb <= 0 {
		return 0, errors.New("size must be a whole number of gigabytes, at least 1")
	}
	if gb > 1024 {
		return 0, errors.New("size must be at most 1024 GB")
	}
	return gb * gib, nil
}

// storageFailed re-renders the tab with the reason.
//
// Rendered rather than redirected, so the message arrives with the form that
// produced it and the person can see what they typed.
func (s *Server) storageFailed(
	w http.ResponseWriter, r *http.Request, appName string, cause error,
) {
	status := http.StatusUnprocessableEntity
	switch {
	case errors.Is(cause, app.ErrNotFound), errors.Is(cause, app.ErrVolumeNotFound):
		status = http.StatusNotFound
	case errors.Is(cause, app.ErrVolumeAttached):
		status = http.StatusConflict
	default:
		s.log.Info("storage change refused",
			slog.String("app", appName), slog.String("reason", cause.Error()))
	}

	a, err := s.apps.Get(r.Context(), identity.MustFromContext(r.Context()).ID, appName)
	if err != nil {
		http.Error(w, cause.Error(), status)
		return
	}

	s.renderStatus(w, r, status, AppDetail(AppDetailData{
		App: a, Tab: "storage", Error: cause.Error(),
	}))
}

// variableSet stores an environment variable.
func (s *Server) variableSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	err := s.apps.SetVariable(ctx, owner.ID, name, app.VariableInput{
		Key: r.FormValue("key"), Value: r.FormValue("value"),
		Secret: r.FormValue("secret") != "",
	})
	if err != nil {
		s.variableFailed(w, r, name, err)
		return
	}
	http.Redirect(w, r, "/apps/"+name+"/variables", http.StatusSeeOther)
}

// variableDelete removes one.
func (s *Server) variableDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	if err := s.apps.DeleteVariable(ctx, owner.ID, name, chi.URLParam(r, "key")); err != nil {
		s.variableFailed(w, r, name, err)
		return
	}
	http.Redirect(w, r, "/apps/"+name+"/variables", http.StatusSeeOther)
}

// variableFailed re-renders the tab with the reason.
func (s *Server) variableFailed(
	w http.ResponseWriter, r *http.Request, appName string, cause error,
) {
	status := http.StatusUnprocessableEntity
	switch {
	case errors.Is(cause, app.ErrNotFound), errors.Is(cause, app.ErrVariableNotFound):
		status = http.StatusNotFound
	case errors.Is(cause, app.ErrNoSecretKey):
		// The install cannot do what was asked, which is a server-side
		// condition rather than a mistake in what was typed.
		status = http.StatusNotImplemented
	}

	a, err := s.apps.Get(r.Context(), identity.MustFromContext(r.Context()).ID, appName)
	if err != nil {
		http.Error(w, cause.Error(), status)
		return
	}

	s.renderStatus(w, r, status, AppDetail(AppDetailData{
		App: a, Tab: "variables", Error: cause.Error(),
	}))
}

// healthSet points the app's probe at a path, or removes it.
func (s *Server) healthSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	err := s.apps.SetHealth(ctx, owner.ID, name,
		r.FormValue("health_path"), r.FormValue("liveness") != "")
	if err != nil {
		s.appActionFailed(w, r, name, "settings", err)
		return
	}
	http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
}

// appActionFailed re-renders a tab with the reason it refused.
func (s *Server) appActionFailed(
	w http.ResponseWriter, r *http.Request, appName, tab string, cause error,
) {
	status := http.StatusUnprocessableEntity
	if errors.Is(cause, app.ErrNotFound) {
		status = http.StatusNotFound
	}

	a, err := s.apps.Get(r.Context(), identity.MustFromContext(r.Context()).ID, appName)
	if err != nil {
		http.Error(w, cause.Error(), status)
		return
	}

	s.renderStatus(w, r, status, AppDetail(s.detailWith(r, a, tab, "", cause.Error())))
}

// formNumber reads a slider's value.
//
// Absent or unparseable is zero, which is "no limit" — the same answer an empty
// text field used to give, and the right one for a form posted by a browser
// that never rendered the control.
func formNumber(r *http.Request, name string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// appBranches lists a repository's branches, filtered by what has been typed.
//
// Reads the remote with ls-remote rather than cloning. It runs while somebody
// is typing, so it fails quietly: a private repository, a typo and an outage
// all come back as a sentence under the field, and the field stays usable as
// free text either way.
func (s *Server) appBranches(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	a, err := s.apps.Get(ctx, owner.ID, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// The URL being typed in the same form wins over the stored one, so the
	// picker follows a repository that has been changed and not yet saved.
	repoURL := strings.TrimSpace(r.URL.Query().Get("repo_url"))
	if repoURL == "" {
		repoURL = a.Repo.URL
	}
	query := strings.TrimSpace(r.URL.Query().Get("repo_branch"))

	d := BranchData{Searched: query != ""}
	switch branches, err := s.apps.Branches(ctx, repoURL, query); {
	case err == nil:
		d.Branches = branches
	case errors.Is(err, app.ErrRepoUnreachable):
		d.Error = "Could not read that repository"
	default:
		d.Error = err.Error()
	}

	if err := BranchOptions(d).Render(ctx, w); err != nil {
		s.log.Error("render branches", slog.String("error", err.Error()))
	}
}

// appDirectories browses the repository for the root-directory field.
//
// Fails as quietly as the branch picker does, and for the same reason: it runs
// while somebody is typing, and a host whose tree cannot be read is a limit to
// state rather than a fault to shout about.
func (s *Server) appDirectories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	a, err := s.apps.Get(ctx, owner.ID, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	repoURL := strings.TrimSpace(r.URL.Query().Get("repo_url"))
	if repoURL == "" {
		repoURL = a.Repo.URL
	}
	path := strings.Trim(strings.TrimSpace(r.URL.Query().Get("repo_subdir")), "/")

	d := DirectoryData{Path: path, Parent: parentPath(path), Searched: true}
	switch dirs, err := s.apps.Directories(ctx, repoURL, path); {
	case err == nil:
		d.Directories = dirs
	case errors.Is(err, app.ErrTreeUnsupported):
		d.Error = "Only GitHub repositories can be browsed"
	case errors.Is(err, app.ErrRepoUnreachable):
		d.Error = "Could not read that repository"
	default:
		d.Error = err.Error()
	}

	if err := DirectoryOptions(d).Render(ctx, w); err != nil {
		s.log.Error("render directories", slog.String("error", err.Error()))
	}
}

// appSourceDisconnect detaches an app from its repository.
//
// The app keeps running: it is serving whatever image its last build produced,
// and detaching a source does not stop a container. What stops is anything new
// being built, which is what the confirmation says.
func (s *Server) appSourceDisconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	a, err := s.apps.Get(ctx, owner.ID, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if _, err := s.apps.Update(ctx, owner.ID, name, app.UpdateInput{
		Port:          a.Port,
		CPURequest:    a.CPURequest,
		CPULimit:      a.CPULimit,
		MemoryRequest: a.MemoryRequest,
		MemoryLimit:   a.MemoryLimit,
		Internal:      a.Internal,
		Repo:          app.Repo{},
	}); err != nil {
		s.flashErr(w, r, err.Error())
		http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
		return
	}

	s.flashOK(w, r, "Repository disconnected. "+name+" keeps running on its last built image.")
	http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
}

// appRuntime changes what an app runs and how much it may use.
//
// One endpoint for the whole runtime rather than one per field. The fields roll
// the app out together — a port change without the image beside it is still a
// rollout — and splitting them would mean several rollouts to make one change.
func (s *Server) appRuntime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	port, err := strconv.Atoi(strings.TrimSpace(cmp.Or(r.FormValue("port"), "0")))
	if err != nil {
		s.flashErr(w, r, "Port must be a number.")
		http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
		return
	}

	// The sliders post plain numbers — millicores and mebibytes — rather than
	// Kubernetes' string grammar, so nothing here has to parse "500m". The
	// formatting back into that grammar happens in one place.
	in := app.UpdateInput{
		Image:         strings.TrimSpace(r.FormValue("image")),
		Port:          int32(port),
		CPURequest:    FormatCPU(formNumber(r, "cpu_request")),
		CPULimit:      FormatCPU(formNumber(r, "cpu_limit")),
		MemoryRequest: FormatMemory(formNumber(r, "memory_request")),
		MemoryLimit:   FormatMemory(formNumber(r, "memory_limit")),
		Internal:      r.FormValue("internal") == "1",
		Repo: app.Repo{
			URL:    strings.TrimSpace(r.FormValue("repo_url")),
			Branch: strings.TrimSpace(r.FormValue("repo_branch")),
			Subdir: strings.TrimSpace(r.FormValue("repo_subdir")),
		},
	}

	if _, err := s.apps.Update(ctx, owner.ID, name, in); err != nil {
		if errors.Is(err, app.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		// A flash and a redirect rather than a re-render. This form is also
		// reachable as a dialog over the canvas, and re-rendering the
		// standalone settings page would answer a failed save by moving
		// somebody somewhere else.
		s.log.Error("update runtime", slog.String("app", name), slog.String("error", err.Error()))
		s.flashErr(w, r, err.Error())
		http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
		return
	}

	s.flashOK(w, r, "Saved. "+name+" is rolling out.")
	http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
}

// appActionNoticed re-renders a tab with something worth saying.
//
// The counterpart to appActionFailed, for an action that succeeded and left
// work for the person: adding a domain changes no routing until they create a
// DNS record, and a bare redirect would leave them to work that out.
func (s *Server) appActionNoticed(
	w http.ResponseWriter, r *http.Request, appName, tab, notice string,
) {
	a, err := s.apps.Get(r.Context(), identity.MustFromContext(r.Context()).ID, appName)
	if err != nil {
		http.Redirect(w, r, "/apps/"+appName+"/"+tab, http.StatusSeeOther)
		return
	}
	s.render(w, r, AppDetail(s.detailWith(r, a, tab, notice, "")))
}

// detailWith builds the tab's data around a message.
//
// The routing is loaded here as well as on the ordinary render: a refusal that
// dropped the domains panel would answer "that name is taken" on a page with
// nowhere to see the domains.
func (s *Server) detailWith(
	r *http.Request, a app.App, tab, notice, fail string,
) AppDetailData {
	d := AppDetailData{App: a, Tab: tab, Notice: notice, Error: fail}
	if s.nets != nil {
		if n, err := s.nets.Networking(r.Context(), a.OwnerID, a.Name); err == nil {
			d.Net = n
		}
	}
	return d
}
