package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/cluster"
	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Nets is the dashboard's view of an app's routing.
type Nets interface {
	Networking(ctx context.Context, ownerID, name string) (app.Networking, error)
	SetNetworking(ctx context.Context, ownerID, name string, httpsOnly, cnameOnly bool) error
	AddDomain(ctx context.Context, ownerID, name, host string) error
	VerifyDomain(ctx context.Context, ownerID, name string, id uuid.UUID) error
	RemoveDomain(ctx context.Context, ownerID, name string, id uuid.UUID) error
}

// networkingFor gathers the panel's state.
func (s *Server) networkingFor(r *http.Request, note, fail string) NetworkingData {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	d := NetworkingData{App: name, Notice: note, Error: fail}
	n, err := s.nets.Networking(ctx, owner.ID, name)
	if err != nil {
		s.log.Error("read networking", slog.String("error", err.Error()))
		if d.Error == "" {
			d.Error = "Could not read this app's routing."
		}
		return d
	}
	d.Net = n
	return d
}

// networkingPage renders the panel on its own page.
func (s *Server) networkingPage(w http.ResponseWriter, r *http.Request) {
	s.renderNetworking(w, r, "", "")
}

// networkingSet stores the routing toggles.
func (s *Server) networkingSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	err := s.nets.SetNetworking(ctx, owner.ID, name,
		r.FormValue("https_only") == "on", r.FormValue("cname_only") == "on")
	if err != nil {
		s.renderNetworking(w, r, "", err.Error())
		return
	}
	http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
}

// domainAdd claims a hostname.
func (s *Server) domainAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	if err := s.nets.AddDomain(ctx, owner.ID, name, r.FormValue("host")); err != nil {
		s.renderNetworking(w, r, "", err.Error())
		return
	}
	// Said plainly, because nothing routes yet and the next step is theirs.
	s.renderNetworking(w, r,
		"Domain added. Create the DNS record below, then verify it — "+
			"nothing is routed until it resolves here.", "")
}

// domainVerify proves a claim.
func (s *Server) domainVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch err := s.nets.VerifyDomain(ctx, owner.ID, name, id); {
	case err == nil:
		s.renderNetworking(w, r, "Verified. This domain is now routed to the app.", "")
	case errors.Is(err, domain.ErrNotVerified):
		// The common case, and not really an error: DNS takes a while, and
		// saying so beats an error that reads like something is broken.
		s.renderNetworking(w, r, "",
			"That name does not resolve here yet. DNS changes can take a while to "+
				"spread; check the record and try again.")
	case errors.Is(err, domain.ErrDomainNotFound):
		http.NotFound(w, r)
	default:
		s.log.Error("verify domain", slog.String("error", err.Error()))
		s.renderNetworking(w, r, "", err.Error())
	}
}

// domainRemove releases a claim.
func (s *Server) domainRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.nets.RemoveDomain(ctx, owner.ID, name, id); err != nil {
		if errors.Is(err, domain.ErrDomainNotFound) {
			http.NotFound(w, r)
			return
		}
		s.renderNetworking(w, r, "", err.Error())
		return
	}
	http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
}

func (s *Server) renderNetworking(w http.ResponseWriter, r *http.Request, note, fail string) {
	d := s.networkingFor(r, note, fail)
	if fail != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	s.renderWithCrumb(w, r, NetworkingPanel(d), d.App)
}

// ---- install-wide DNS

// PlatformDNSData is the install's DNS settings.
type PlatformDNSData struct {
	DNS    cluster.DNS
	Error  string
	Notice string
}

func (s *Server) dnsSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, PlatformDNS(s.dnsData(r, "", "")))
}

func (s *Server) dnsData(r *http.Request, note, fail string) PlatformDNSData {
	d := PlatformDNSData{Notice: note, Error: fail}
	dns, err := s.joiner.DNS(r.Context())
	if err != nil {
		s.log.Error("read dns settings", slog.String("error", err.Error()))
		if d.Error == "" {
			d.Error = "Could not read the DNS settings."
		}
		return d
	}
	d.DNS = dns
	return d
}

func (s *Server) dnsSet(w http.ResponseWriter, r *http.Request) {
	err := s.joiner.SetDNS(r.Context(), r.FormValue("cname_target"), r.FormValue("txt_prefix"))
	if err != nil {
		d := s.dnsData(r, "", err.Error())
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, PlatformDNS(d))
		return
	}
	http.Redirect(w, r, "/cluster/dns", http.StatusSeeOther)
}
