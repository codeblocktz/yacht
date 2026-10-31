package web

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
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

	in := app.VariableInput{
		Key: r.FormValue("key"), Value: r.FormValue("value"),
		Secret: formChecked(r, "secret"),
	}
	form := variableFormSet
	if ref, ok := referenceInput(r); ok {
		in, form = ref, variableFormRef
	}
	err := s.apps.SetVariable(ctx, owner.ID, name, in)
	if errors.Is(err, app.ErrSavedWhileDeploying) {
		s.flashOK(w, r, "Saved "+strings.TrimSpace(in.Key)+". A deploy is already running — redeploy once it finishes to apply it.")
		variableSaved(w, r, name)
		return
	}
	if err != nil {
		s.variableFailed(w, r, name, form, err)
		return
	}
	if s.stages() {
		s.flashSaved(w, r, "Saved "+strings.TrimSpace(in.Key)+".")
	}
	variableSaved(w, r, name)
}

// variableDelete removes one.
func (s *Server) variableDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	err := s.apps.DeleteVariable(ctx, owner.ID, name, chi.URLParam(r, "key"))
	if errors.Is(err, app.ErrSavedWhileDeploying) {
		s.flashOK(w, r, "Removed. A deploy is already running — redeploy once it finishes to apply it.")
		http.Redirect(w, r, "/apps/"+name+"/variables", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.variableFailed(w, r, name, "", err)
		return
	}
	if s.stages() {
		s.flashSaved(w, r, "Removed "+chi.URLParam(r, "key")+".")
	}
	http.Redirect(w, r, "/apps/"+name+"/variables", http.StatusSeeOther)
}

// variableFailed re-renders the Variables tab with the reason, beside the
// form that was refused and with what was typed into it still there.
//
// The tab is drawn on the canvas, as it is on every other visit, and not on a
// page of its own: a refusal is a correction to make, and the Raw Editor's
// text especially is too long to type twice. A request from htmx gets the
// tab alone, swapped in where it was, so the address stays the tab's and a
// reload does not post the text again.
func (s *Server) variableFailed(
	w http.ResponseWriter, r *http.Request, appName, form string, cause error,
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
	if errors.Is(cause, app.ErrNotFound) {
		http.Error(w, cause.Error(), status)
		return
	}

	refused := func(d *AppDetailData) {
		d.Error = strings.TrimPrefix(cause.Error(), "app: ")
		d.Vars.Failed = form
		switch form {
		case variableFormRaw:
			d.Vars.RawText = r.FormValue("raw")
			var le *app.EnvLineError
			if errors.As(cause, &le) {
				d.Vars.RawLine = le.Line
				d.Vars.RawLineText = nthLine(d.Vars.RawText, le.Line)
				d.Error = strings.TrimPrefix(le.Err.Error(), "app: ")
			}
		case variableFormSet:
			d.Vars.SetKey, d.Vars.SetValue = r.FormValue("key"), r.FormValue("value")
			d.Vars.SetSecret = formChecked(r, "secret")
		}
	}

	if r.Header.Get("HX-Request") != "true" {
		s.renderCanvasStatus(w, r, status, "", appName, "variables", refused)
		return
	}
	ctx := r.Context()
	a, err := s.apps.Get(ctx, identity.MustFromContext(ctx).ID, appName)
	if err != nil {
		http.Error(w, cause.Error(), status)
		return
	}
	d := s.appPanel(ctx, a, "variables")
	refused(d)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := variablesTab(*d).Render(ctx, w); err != nil {
		s.log.Error("render variables tab", slog.String("error", err.Error()))
	}
}

// The Variables tab's forms, as variableFailed names the one refused.
const (
	variableFormSet = "set"
	variableFormRef = "ref"
	variableFormRaw = "raw"
)

// variableSaved sends somebody back to the Variables tab after a save. A
// save from htmx is told to load the page, rather than have the redirect's
// page swapped into the tab, so the canvas's changes bar counts it too.
func variableSaved(w http.ResponseWriter, r *http.Request, appName string) {
	to := "/apps/" + appName + "/variables"
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// nthLine is line n of text, counted from 1, or "" past its end.
func nthLine(text string, n int) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}

// healthSet points the app's probe at a path, or removes it.
func (s *Server) healthSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	err := s.apps.SetHealth(ctx, owner.ID, name,
		r.FormValue("health_path"), formChecked(r, "liveness"))
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

// formChecked reads a checkbox without caring what it is worth.
//
// A browser submits a checked box and omits an unchecked one, so presence is
// the whole signal — the value is whatever the markup happened to say, and
// nothing should depend on which. Two handlers did: they compared against "on",
// the value a native checkbox submits when the markup gives it none, and both
// silently started reading false the moment the control began sending
// something else.
//
// Presence, then, in one place, so there is no second way to get this wrong.
func formChecked(r *http.Request, name string) bool {
	return strings.TrimSpace(r.FormValue(name)) != ""
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

	// A repository chosen from GitHub is read through the App, so a private
	// one answers: the one chosen in the form, or the one stored when the
	// form says nothing about either.
	if ref, ok := s.pickedGitHubRepo(r, a); ok {
		s.renderGitHubBranches(w, r, ref)
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

	if ref, ok := s.pickedGitHubRepo(r, a); ok {
		s.renderGitHubDirectories(w, r, ref)
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
		// Carried as they are: disconnecting a repository is not a reason
		// to forget how the image it built is started.
		StartCommand:         a.StartCommand,
		PreDeployCommand:     a.PreDeployCommand,
		CronSchedule:         a.CronSchedule,
		RestartPolicy:        a.RestartPolicy,
		RestartMaxRetries:    a.RestartMaxRetries,
		HealthTimeoutSeconds: a.HealthTimeoutSeconds,
		DockerfilePath:       a.DockerfilePath,
		WatchPaths:           a.WatchPaths,
	}); err != nil {
		s.flashErr(w, r, err.Error())
		http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
		return
	}

	s.flashOK(w, r, "Repository disconnected. "+name+" keeps running on its last built image.")
	http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
}

// runtimeVersionField carries runtimeVersion in the settings form.
const runtimeVersionField = "settings_version"

// runtimeVersion fingerprints the settings the runtime form edits, as the
// page showing them read them. Their values rather than the app's updated
// time, so that what the form does not touch — a scale, a domain, a deploy —
// does not make a save from an open page look stale. Not a Git app's image:
// a build sets that, and the form never edits it.
func runtimeVersion(a app.App) string {
	h := sha256.New()
	if a.Source != app.SourceGit {
		fmt.Fprintf(h, "image=%q\n", a.Image)
	}
	fmt.Fprintf(h, "port=%d internal=%t\n", a.Port, a.Internal)
	fmt.Fprintf(h, "cpu=%q/%q mem=%q/%q\n", a.CPURequest, a.CPULimit, a.MemoryRequest, a.MemoryLimit)
	fmt.Fprintf(h, "repo=%+v\n", a.Repo)
	fmt.Fprintf(h, "start=%q pre=%q cron=%q\n", a.StartCommand, a.PreDeployCommand, a.CronSchedule)
	fmt.Fprintf(h, "restart=%q/%d health=%d\n", a.RestartPolicy, a.RestartMaxRetries, a.HealthTimeoutSeconds)
	fmt.Fprintf(h, "dockerfile=%q watch=%q\n", a.DockerfilePath, a.WatchPaths)
	return hex.EncodeToString(h.Sum(nil))[:16]
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
	githubRef, err := formGitHubRef(r)
	if err != nil {
		s.flashErr(w, r, err.Error())
		http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
		return
	}
	in := app.UpdateInput{
		Image:         strings.TrimSpace(r.FormValue("image")),
		Port:          int32(port),
		CPURequest:    FormatCPU(formNumber(r, "cpu_request")),
		CPULimit:      FormatCPU(formNumber(r, "cpu_limit")),
		MemoryRequest: FormatMemory(formNumber(r, "memory_request")),
		MemoryLimit:   FormatMemory(formNumber(r, "memory_limit")),
		Internal:      formChecked(r, "internal"),
		Repo: app.Repo{
			URL:    strings.TrimSpace(r.FormValue("repo_url")),
			Branch: strings.TrimSpace(r.FormValue("repo_branch")),
			Subdir: strings.TrimSpace(r.FormValue("repo_subdir")),
			GitHub: githubRef,
		},
		StartCommand:     r.FormValue("start_command"),
		PreDeployCommand: r.FormValue("pre_deploy_command"),
		CronSchedule:     r.FormValue("cron_schedule"),
		// Out of range is refused by the service, with the bounds, rather
		// than clamped here into something nobody typed.
		RestartMaxRetries:    int32(min(formNumber(r, "restart_max_retries"), math.MaxInt32)),
		HealthTimeoutSeconds: int32(min(formNumber(r, "health_timeout"), math.MaxInt32)),
		DockerfilePath:       r.FormValue("dockerfile_path"),
		WatchPaths:           []string{r.FormValue("watch_paths")},
	}

	before, err := s.apps.Get(ctx, owner.ID, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The form posts every field, so a save from a page opened before
	// somebody else's would put back what they changed, with nothing said.
	// Refused instead, and the page reloaded to show what is there now. A
	// form without the version, from a script, saves as it always did.
	if v := r.FormValue(runtimeVersionField); v != "" && v != runtimeVersion(before) {
		s.flashErr(w, r, "These settings changed since you opened them, so nothing was saved. "+
			"This is what they are now: make your change again.")
		http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
		return
	}
	// The policy's picker fills its field from script. A form sent without
	// it keeps the policy the app has, rather than quietly resetting it to
	// the default.
	in.RestartPolicy = cmp.Or(
		orchestrator.RestartPolicy(strings.TrimSpace(r.FormValue("restart_policy"))),
		before.RestartPolicy)
	updated, err := s.apps.Update(ctx, owner.ID, name, in)
	if err != nil {
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

	// Said as it happened: a save that changed nothing, or only what the
	// next build reads, rolls nothing out.
	switch {
	case app.RollsOut(before, updated) && !imageIsPending(updated.Image):
		s.flashOK(w, r, "Saved. "+name+" is rolling out.")
		http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
		return
	case app.RollsOut(before, updated), before.Repo != updated.Repo,
		before.DockerfilePath != updated.DockerfilePath,
		!slices.Equal(before.WatchPaths, updated.WatchPaths):
		s.flashOK(w, r, "Saved. The next deploy uses it.")
	default:
		s.flashOK(w, r, "Nothing changed.")
	}
	http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
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
	d := AppDetailData{App: a, Tab: tab, Notice: notice, Error: fail, CLI: s.cliName}
	if s.nets != nil {
		if n, err := s.nets.Networking(r.Context(), a.OwnerID, a.Name); err == nil {
			d.Net = n
		}
	}
	s.attachHook(r.Context(), r, &d)
	s.attachSleep(r.Context(), &d)
	s.attachRegion(r.Context(), &d)
	s.attachProjects(r.Context(), &d)
	s.attachReferenceTargets(r.Context(), &d)
	s.attachReferrers(r.Context(), &d)
	d.Stages = s.stages()
	return d
}
