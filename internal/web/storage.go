package web

import (
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

	w.WriteHeader(status)
	s.render(w, r, AppDetail(AppDetailData{
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

	w.WriteHeader(status)
	s.render(w, r, AppDetail(AppDetailData{
		App: a, Tab: "variables", Error: cause.Error(),
	}))
}
