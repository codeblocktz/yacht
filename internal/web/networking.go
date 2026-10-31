package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

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

	// ResolverName says which resolver answers about custom domains, so the
	// page can attribute what it reports rather than stating it as fact.
	ResolverName() string

	// AllDomains is every custom domain on the install, unsettled first.
	AllDomains(ctx context.Context, ownerID string) ([]app.InstallDomain, error)

	// TargetStatus reports whether the configured CNAME target resolves,
	// as a sentence. Empty when there was nothing to check.
	TargetStatus(ctx context.Context, target string) string

	// GenerateDomain, SetPublicDomain and RemovePublicDomain are the
	// app's platform hostname: Railway's Generate Domain, its edit form with
	// a target port, and taking it away. A port of zero leaves the app's as
	// it is.
	GenerateDomain(ctx context.Context, ownerID, name string, port int32) (string, error)
	SetPublicDomain(ctx context.Context, ownerID, name, label string, port int32) error
	RemovePublicDomain(ctx context.Context, ownerID, name string) error

	// EnableTCPProxy and DisableTCPProxy are the app's public TCP port.
	EnableTCPProxy(ctx context.Context, ownerID, name string) (app.TCPProxy, error)
	DisableTCPProxy(ctx context.Context, ownerID, name string) error
}

// domainsFragment is the polled half of the custom domain list.
//
// Its own endpoint rather than re-rendering the tab, so a poll every three
// seconds does not replace the add-domain form under somebody typing into it.
func (s *Server) domainsFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	net, err := s.nets.Networking(ctx, owner.ID, name)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("read networking", slog.String("error", err.Error()))
		http.Error(w, "could not read domains", http.StatusInternalServerError)
		return
	}

	d := s.networkingData(name, net, app.App{})
	if err := CustomDomainList(d).Render(ctx, w); err != nil {
		s.log.Error("render domains fragment", slog.String("error", err.Error()))
	}
}

// networkingData assembles what the routing sections render from.
func (s *Server) networkingData(name string, net app.Networking, a app.App) NetworkingData {
	return NetworkingData{
		App:           name,
		Net:           net,
		UntrustedCert: a.UntrustedCert(),
		ResolverName:  s.nets.ResolverName(),
		Settled:       domainsSettled(net),
	}
}

// networkingSet stores the routing toggles.
func (s *Server) networkingSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	err := s.nets.SetNetworking(ctx, owner.ID, name,
		formChecked(r, "https_only"), formChecked(r, "cname_only"))
	if err != nil {
		s.appActionFailed(w, r, name, "domains", err)
		return
	}
	http.Redirect(w, r, "/apps/"+name+"/domains", http.StatusSeeOther)
}

// domainAdd claims a hostname.
func (s *Server) domainAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	if err := s.nets.AddDomain(ctx, owner.ID, name, r.FormValue("host")); err != nil {
		s.settingsRefused(w, r, name, "domains", settingsFormDomain, http.StatusUnprocessableEntity, err)
		return
	}
	// The next step is theirs, so it is said. What is no longer said is "then
	// verify it" — nothing has to be pressed now, and telling somebody to come
	// back and click is how the old flow wasted their time.
	s.flashOK(w, r, "Domain added. Create the record shown; it is checked for on its own.")
	savedTo(w, r, "/apps/"+name+"/domains")
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
		s.flashOK(w, r, "Verified. This domain is now routed to the app.")
	case errors.Is(err, domain.ErrNotVerified):
		// Not a fault, and no longer a dead end: the check just ran, the row
		// now records what it saw, and the page the redirect lands on shows
		// that. A warning rather than an error, because nothing broke — the
		// record simply is not there yet.
		s.flashWarn(w, r, "Checked just now — it does not resolve here yet. "+
			"The steps below show what was found.")
	case errors.Is(err, domain.ErrDomainNotFound):
		http.NotFound(w, r)
		return
	default:
		s.log.Error("verify domain", slog.String("error", err.Error()))
		s.flashErr(w, r, err.Error())
	}
	http.Redirect(w, r, "/apps/"+name+"/domains", http.StatusSeeOther)
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
		s.flashErr(w, r, err.Error())
		http.Redirect(w, r, "/apps/"+name+"/domains", http.StatusSeeOther)
		return
	}
	s.flashOK(w, r, "Domain removed. It is no longer routed to the app.")
	http.Redirect(w, r, "/apps/"+name+"/domains", http.StatusSeeOther)
}

// ---- install-wide DNS

// PlatformDNSData is the install's DNS settings, and every domain that depends
// on them.
type PlatformDNSData struct {
	DNS   cluster.DNS
	Error string

	// Domains is every custom domain on the install, unsettled first. This is
	// where the background checker's work becomes visible: one app's page shows
	// one app's domains, and nothing answered "which are stuck".
	Domains []app.InstallDomain

	// TargetResolves reports whether the configured CNAME target itself
	// resolves. Empty means nothing was checked — no target, or no resolver.
	TargetResolves string

	ResolverName string
}

func (s *Server) dnsSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, PlatformDNS(s.dnsData(r, "")))
}

func (s *Server) dnsData(r *http.Request, fail string) PlatformDNSData {
	ctx := r.Context()
	d := PlatformDNSData{Error: fail}

	dns, err := s.joiner.DNS(ctx)
	if err != nil {
		s.log.Error("read dns settings", slog.String("error", err.Error()))
		if d.Error == "" {
			d.Error = "Could not read the DNS settings."
		}
		return d
	}
	d.DNS = dns

	if s.nets != nil {
		d.ResolverName = s.nets.ResolverName()
		owner := identity.MustFromContext(ctx)
		if domains, err := s.nets.AllDomains(ctx, owner.ID); err == nil {
			d.Domains = domains
		} else {
			s.log.Error("list install domains", slog.String("error", err.Error()))
		}
		// Whether the target itself resolves. A typo here is accepted by the
		// syntax check and then silently fails every verification downstream,
		// with the failure reported against the customer's domain rather than
		// against the setting that caused it.
		d.TargetResolves = s.nets.TargetStatus(ctx, dns.CNAMETarget)
	}
	return d
}

func (s *Server) dnsSet(w http.ResponseWriter, r *http.Request) {
	err := s.joiner.SetDNS(r.Context(), r.FormValue("cname_target"), r.FormValue("txt_prefix"))
	if err != nil {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, PlatformDNS(s.dnsData(r, err.Error())))
		return
	}
	s.flashOK(w, r, "DNS settings saved.")
	http.Redirect(w, r, "/cluster/dns", http.StatusSeeOther)
}

// ---- public networking: generated domain and TCP proxy

// networkingTab is the tab a networking form goes back to: where it was
// posted from. The section is one component placed on whichever tab holds
// it, so it says which in each form rather than every handler assuming one.
// Only tabs that render it are accepted, so the value cannot send the
// redirect anywhere else.
func networkingTab(r *http.Request) string {
	if r.FormValue("tab") == "settings" {
		return "settings"
	}
	return "domains"
}

// formPort reads a port field: zero when it is empty, and an error when it
// is not a number at all.
func formPort(r *http.Request) (int32, error) {
	raw := strings.TrimSpace(r.FormValue("port"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, errors.New("the port must be a number, such as 8080")
	}
	return int32(n), nil
}

// publicDomainGenerate is Generate Domain.
func (s *Server) publicDomainGenerate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name, tab := chi.URLParam(r, "name"), networkingTab(r)

	port, err := formPort(r)
	if err == nil {
		var host string
		if host, err = s.nets.GenerateDomain(ctx, owner.ID, name, port); err == nil {
			s.flashOK(w, r, host+" is on its way. It answers once the route is written, in a few seconds.")
		}
	}
	if err != nil {
		s.settingsRefused(w, r, name, tab, settingsFormGenerate, http.StatusUnprocessableEntity, err)
		return
	}
	savedTo(w, r, "/apps/"+name+"/"+tab)
}

// publicDomainSet is "Update your domain or target port".
func (s *Server) publicDomainSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name, tab := chi.URLParam(r, "name"), networkingTab(r)

	port, err := formPort(r)
	if err == nil {
		err = s.nets.SetPublicDomain(ctx, owner.ID, name, r.FormValue("label"), port)
	}
	if err != nil {
		s.settingsRefused(w, r, name, tab, settingsFormPublic, http.StatusUnprocessableEntity, err)
		return
	}
	s.flashOK(w, r, "Saved. A new name answers once the route is rewritten; a new port once the app has rolled out.")
	savedTo(w, r, "/apps/"+name+"/"+tab)
}

// publicDomainRemove takes the platform hostname away.
func (s *Server) publicDomainRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name, tab := chi.URLParam(r, "name"), networkingTab(r)

	if err := s.nets.RemovePublicDomain(ctx, owner.ID, name); err != nil {
		s.appActionFailed(w, r, name, tab, err)
		return
	}
	s.flashOK(w, r, "Domain removed. The app is no longer on the internet at it.")
	http.Redirect(w, r, "/apps/"+name+"/"+tab, http.StatusSeeOther)
}

// tcpProxyEnable gives the app a public TCP port.
func (s *Server) tcpProxyEnable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name, tab := chi.URLParam(r, "name"), networkingTab(r)

	proxy, err := s.nets.EnableTCPProxy(ctx, owner.ID, name)
	if err != nil {
		s.appActionFailed(w, r, name, tab, err)
		return
	}
	s.flashOK(w, r, "TCP proxy on: "+proxy.Address()+" reaches port "+strconv.Itoa(int(proxy.Target))+
		". Anyone can connect to it, so the app's own password is what keeps them out.")
	http.Redirect(w, r, "/apps/"+name+"/"+tab, http.StatusSeeOther)
}

// tcpProxyDisable takes it away.
func (s *Server) tcpProxyDisable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name, tab := chi.URLParam(r, "name"), networkingTab(r)

	if err := s.nets.DisableTCPProxy(ctx, owner.ID, name); err != nil {
		s.appActionFailed(w, r, name, tab, err)
		return
	}
	s.flashOK(w, r, "TCP proxy removed. Connections from outside the cluster stop reaching the app.")
	http.Redirect(w, r, "/apps/"+name+"/"+tab, http.StatusSeeOther)
}

// netOf adapts the app detail into what the networking sections take.
//
// The sections were written for a page of their own and are now the Domains
// tab; keeping them on their own small type means they still say what they need
// rather than reaching into everything the tab happens to hold.
func netOf(d AppDetailData) NetworkingData {
	tab := "domains"
	if d.Tab == "settings" {
		tab = "settings"
	}
	return NetworkingData{
		App:           d.App.Name,
		Tab:           tab,
		Refused:       d.Refused,
		Net:           d.Net,
		UntrustedCert: d.App.UntrustedCert(),
		ResolverName:  d.ResolverName,
		Settled:       domainsSettled(d.Net),
	}
}

// returnTab is the tab the section's forms come back to.
func (d NetworkingData) returnTab() string {
	if d.Tab == "settings" {
		return "settings"
	}
	return "domains"
}

// publicHost is the app's platform hostname. Managed is read as well as
// Public.Host because it is the field that predates the section, and a caller
// filling in only that still has its hostname shown.
func (d NetworkingData) publicHost() string {
	if d.Net.Public.Host != "" {
		return d.Net.Public.Host
	}
	return d.Net.Managed
}

// portValue is what the port field starts at: the app's port, or the first it
// is known to listen on when it has none.
func portValue(pub app.PublicDomain) string {
	switch {
	case pub.Port > 0:
		return strconv.Itoa(int(pub.Port))
	case len(pub.Suggestions) > 0:
		return strconv.Itoa(int(pub.Suggestions[0]))
	}
	return ""
}
