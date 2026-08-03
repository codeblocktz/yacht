package web

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/registry"
)

// Registries is the dashboard's view of where images go.
type Registries interface {
	Settings(ctx context.Context) (registry.Settings, error)
	Set(ctx context.Context, by uuid.UUID, host, repository, username, password string, insecure bool) error
	Clear(ctx context.Context) error

	// CanStore reports whether a password can be sealed. Asked here rather
	// than handing the web layer an encryption key it has no other use for.
	CanStore() bool
}

// RegistryData is the install's registry page.
type RegistryData struct {
	Registry registry.Settings

	// HasKey reports whether a password can be stored at all. The page says so
	// before showing a form rather than after a submission fails.
	HasKey bool

	Notice string
	Error  string
}

func (s *Server) registrySettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, RegistryPage(s.registryData(r, "", "")))
}

func (s *Server) registryData(r *http.Request, note, fail string) RegistryData {
	d := RegistryData{Notice: note, Error: fail, HasKey: s.registries.CanStore()}
	set, err := s.registries.Settings(r.Context())
	if err != nil {
		s.log.Error("read registry settings", slog.String("error", err.Error()))
		if d.Error == "" {
			d.Error = "Could not read the registry settings."
		}
		return d
	}
	d.Registry = set
	return d
}

func (s *Server) registrySet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Who changed it, when that is knowable. This credential can push images
	// the cluster then runs, so the answer is the first thing anyone will want.
	var by uuid.UUID
	if s.accounts != nil {
		if sess, err := s.accounts.ResolveSession(ctx, sessionToken(r)); err == nil {
			by = sess.UserID
		}
	}

	password := r.FormValue("password")
	if password == "" {
		// Empty means "leave it alone", not "set it to nothing". Otherwise
		// correcting a typo in the repository field silently wipes the
		// credential, and the next build is the thing that finds out.
		current, err := s.registries.Settings(ctx)
		if err == nil && current.Configured() {
			d := s.registryData(r, "", "Enter the password again to change any of these — "+
				"it is stored sealed and cannot be read back to keep unchanged.")
			s.renderStatus(w, r, http.StatusUnprocessableEntity, RegistryPage(d))
			return
		}
	}

	err := s.registries.Set(ctx, by,
		r.FormValue("host"), r.FormValue("repository"), r.FormValue("username"), password,
		r.FormValue("insecure") == "on")
	if err != nil {
		d := s.registryData(r, "", err.Error())
		s.renderStatus(w, r, http.StatusUnprocessableEntity, RegistryPage(d))
		return
	}
	http.Redirect(w, r, "/cluster/registry", http.StatusSeeOther)
}

func (s *Server) registryClear(w http.ResponseWriter, r *http.Request) {
	if err := s.registries.Clear(r.Context()); err != nil {
		d := s.registryData(r, "", err.Error())
		s.renderStatus(w, r, http.StatusUnprocessableEntity, RegistryPage(d))
		return
	}
	http.Redirect(w, r, "/cluster/registry", http.StatusSeeOther)
}

// passwordPlaceholder says whether one is already stored.
//
// A sealed password cannot be read back to prefill, so an empty box is
// ambiguous: it looks the same whether one is set or not.
func passwordPlaceholder(s registry.Settings) string {
	if s.Configured() {
		return "stored — enter it again to change anything"
	}
	return ""
}
