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

// networkingSet stores the routing toggles.
func (s *Server) networkingSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	err := s.nets.SetNetworking(ctx, owner.ID, name,
		r.FormValue("https_only") == "on", r.FormValue("cname_only") == "on")
	if err != nil {
		s.appActionFailed(w, r, name, "settings", err)
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
		s.appActionFailed(w, r, name, "settings", err)
		return
	}
	// Nothing routes yet and the next step is theirs, so it is said rather than
	// left for them to discover from the domain sitting there unverified.
	s.appActionNoticed(w, r, name, "settings",
		"Domain added. Create the DNS record shown, then verify it — nothing is "+
			"routed until it resolves here.")
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
		s.appActionNoticed(w, r, name, "settings",
			"Verified. This domain is now routed to the app.")
	case errors.Is(err, domain.ErrNotVerified):
		// The common case, and not really a fault: DNS takes a while to
		// spread, and an error that reads like a breakage sends somebody
		// looking for one.
		s.appActionFailed(w, r, name, "settings", errors.New(
			"that name does not resolve here yet — DNS changes can take a while "+
				"to spread, so check the record and try again"))
	case errors.Is(err, domain.ErrDomainNotFound):
		http.NotFound(w, r)
	default:
		s.log.Error("verify domain", slog.String("error", err.Error()))
		s.appActionFailed(w, r, name, "settings", err)
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
		s.appActionFailed(w, r, name, "settings", err)
		return
	}
	http.Redirect(w, r, "/apps/"+name+"/settings", http.StatusSeeOther)
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

// netOf adapts the app detail into what the networking sections take.
//
// The sections were written for a page of their own and are now part of the
// Settings tab; keeping them on their own small type means they still say what
// they need rather than reaching into everything the tab happens to hold.
func netOf(d AppDetailData) NetworkingData {
	return NetworkingData{
		App: d.App.Name, Net: d.Net, UntrustedCert: d.App.UntrustedCert(),
	}
}
