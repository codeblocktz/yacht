package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Networking is an app's routing: the hostnames it answers on and how.
type Networking struct {
	// Managed is the platform-issued hostname, empty when the feature is off
	// or the app cannot have one.
	Managed string

	Custom []domain.Custom

	HTTPSOnly bool
	CNAMEOnly bool

	// Target is what a custom domain's CNAME must point at. Empty when the
	// install has not been told, which is the state the page has to explain
	// rather than show an input that cannot work.
	Target string

	// Issuing is true when the install gives custom domains certificates of
	// their own. False means a custom domain is served over plain HTTP, and
	// the page has to say so rather than promise a certificate.
	Issuing bool

	// Certs is what the cluster holds for each routed custom domain, by host.
	// A host that is missing could not be read, which is not the same as not
	// issued and must not be drawn as a failure.
	Certs map[string]orchestrator.Certificate

	// Namespace is where the app's objects live, so a command the page offers
	// for looking into one can be pasted and run as it is.
	Namespace string

	// Public is the app's platform hostname and what Generate Domain needs;
	// Private, where other apps reach it; TCPProxy, its public TCP port. See
	// public_networking.go.
	Public   PublicDomain
	Private  PrivateEndpoint
	TCPProxy TCPProxy
}

// Networking returns an app's routing.
func (s *Service) Networking(ctx context.Context, ownerID, name string) (Networking, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return Networking{}, err
	}

	out := Networking{
		Managed:   a.Host,
		HTTPSOnly: a.HTTPSOnly,
		CNAMEOnly: a.CNAMEOnly,
		Target:    s.domainTarget(ctx, a),
		Namespace: a.Ref().Namespace,
	}
	if out.Custom, err = domain.ListCustom(ctx, s.q, ownerID, a.ID); err != nil {
		return Networking{}, err
	}
	out.Issuing = s.opts.CertIssuer != ""
	out.Certs = s.certificates(ctx, a, out.Custom)
	if err := s.attachPublicNetworking(ctx, a, &out); err != nil {
		return Networking{}, err
	}
	return out, nil
}

// certificates reads the certificate issued for each routed custom domain.
//
// Only routed ones: nothing is requested for a name until it is on the
// Ingress. A read that fails is logged and left out rather than failing the
// page — the domain list is still worth showing without it.
func (s *Service) certificates(ctx context.Context, a App, custom []domain.Custom) map[string]orchestrator.Certificate {
	reader, ok := s.orch.(orchestrator.CertificateReader)
	if s.opts.CertIssuer == "" || !ok {
		return nil
	}
	out := make(map[string]orchestrator.Certificate)
	for _, c := range custom {
		if c.State != domain.StateRouted {
			continue
		}
		cert, err := reader.Certificate(ctx, a.Ref(), c.Host)
		if err != nil {
			s.log.Warn("read certificate", slog.String("app", a.Name),
				slog.String("host", c.Host), slog.String("error", err.Error()))
			continue
		}
		out[c.Host] = cert
	}
	return out
}

// SetNetworking records the routing choices. The app reconciler owns the
// cluster write so request handlers cannot race a candidate deployment.
func (s *Service) SetNetworking(ctx context.Context, ownerID, name string, httpsOnly, cnameOnly bool) error {
	current, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if current.HTTPSOnly == httpsOnly && current.CNAMEOnly == cnameOnly {
		return nil
	}
	n, err := s.q.SetAppNetworking(ctx, dbgen.SetAppNetworkingParams{
		OwnerID: ownerID, Name: name, HttpsOnly: httpsOnly, CnameOnly: cnameOnly,
	})
	if err != nil {
		return fmt.Errorf("app: set networking: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("app: networking changed concurrently")
	}

	s.log.Info("networking changed", slog.String("app", name),
		slog.Bool("https_only", httpsOnly), slog.Bool("cname_only", cnameOnly))
	return nil
}

// AddDomain claims a hostname for an app.
//
// Nothing routes to it until it is verified, so this deliberately does not
// reapply the workload: an unproven claim changes no Ingress, and reapplying
// would suggest otherwise.
func (s *Service) AddDomain(ctx context.Context, ownerID, name, host string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if !a.Source.Workload() {
		return ErrNoWorkload
	}
	reserved, err := s.CustomDomainReservations(ctx)
	if err != nil {
		return err
	}
	if _, err := domain.AddCustom(ctx, s.q, ownerID, a.ID, host,
		s.domainTarget(ctx, a), reserved); err != nil {
		return err
	}
	if _, err := s.q.IncrementAppConfigVersion(ctx, dbgen.IncrementAppConfigVersionParams{
		OwnerID: ownerID, AppID: a.ID,
	}); err != nil {
		return fmt.Errorf("app: version domain claim: %w", err)
	}
	s.log.Info("custom domain claimed", slog.String("app", name), slog.String("host", host))
	s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: ownerID, AppID: a.ID, App: a.Name})
	return nil
}

// CustomDomainReservations is every name no app may bring as a custom
// domain: the app domain, the operator's reserved list, and every name the
// install itself answers on — the dashboard's, the build source's, the
// buckets', the TCP proxy's and the other endpoints in its config, and from
// its settings the CNAME target, each region's edge and the registry.
//
// Read fresh each time rather than at startup, because the last three are
// settings, changed while the engine runs. An error rather than a partial
// answer: a list missing the registry would let somebody claim it.
func (s *Service) CustomDomainReservations(ctx context.Context) (domain.Reservations, error) {
	r, bad := s.reservationsFromConfig()
	add := func(v string) { bad = cmp.Or(bad, addPlatformHost(&r, v, false)) }
	dns, err := s.q.GetPlatformDNS(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.Reservations{}, fmt.Errorf("app: read the CNAME target: %w", err)
	}
	add(dns.CnameTarget)
	regions, err := s.q.ListRegions(ctx)
	if err != nil {
		return domain.Reservations{}, fmt.Errorf("app: read regions' edges: %w", err)
	}
	for _, rg := range regions {
		add(rg.EdgeHost)
	}
	reg, err := s.q.GetPlatformRegistry(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.Reservations{}, fmt.Errorf("app: read the registry: %w", err)
	}
	add(reg.Host)
	// Every one of these is checked where it is set. One that still cannot be
	// read is refused as a whole, as a failed read is: a partial list is one a
	// name it left out can be claimed through.
	if bad != nil {
		return domain.Reservations{}, fmt.Errorf("app: read the install's own names: %w", bad)
	}
	return r, nil
}

// reservationsFromConfig is CustomDomainReservations without the settings
// kept in the database, and the first of the configured addresses that could
// not be read — config.Load refuses those, so in practice none.
func (s *Service) reservationsFromConfig() (domain.Reservations, error) {
	r := domain.Reservations{
		AppDomain: s.opts.AppDomain,
		Reserved:  slices.Clone(s.opts.ReservedDomains),
		Allowed:   slices.Clone(s.opts.AllowedDomains),
	}
	var bad error
	for _, v := range s.opts.PlatformZones {
		bad = cmp.Or(bad, addPlatformHost(&r, v, true))
	}
	for _, v := range s.opts.PlatformNames {
		bad = cmp.Or(bad, addPlatformHost(&r, v, false))
	}
	return r, bad
}

// configuredReservations is reservationsFromConfig with whatever it could read.
func (s *Service) configuredReservations() domain.Reservations {
	r, _ := s.reservationsFromConfig()
	return r
}

// addPlatformHost adds one of the install's addresses to r: a zone when it
// was configured as one or written *.example.com, a name otherwise.
func addPlatformHost(r *domain.Reservations, v string, zone bool) error {
	h, wildcard, err := domain.PlatformHost(v)
	switch {
	case err != nil:
		return err
	case h == "":
	case zone || wildcard:
		r.Zones = append(r.Zones, h)
	default:
		r.Names = append(r.Names, h)
	}
	return nil
}

// domainTarget is what a custom domain on an app is told to CNAME to: its
// region's edge, so the traffic for it enters where the app lives, or the
// install's target where the region names none.
//
// Recorded on each domain when it is claimed and proven against that. A
// domain claimed before its app moved region, or before the region had an
// edge, keeps the target it was proven against: it goes on working, since an
// ingress anywhere in the cluster routes it, and the page shows the record it
// was told to create.
func (s *Service) domainTarget(ctx context.Context, a App) string {
	if edge := s.regionEdge(ctx, s.q, a.Region); edge.Host != "" {
		return edge.Host
	}
	return s.cnameTarget(ctx)
}

// ApplyRouting records that one app's routing projection changed.
//
// This is what the background domain checker calls when a claim becomes
// provable, or when a live one stops resolving. It takes an owner and an app id
// rather than a name because the checker works from domain rows, which carry
// both — and every table carrying owner_id is precisely so that scoping stays a
// cheap predicate rather than something a background job gets to skip.
func (s *Service) ApplyRouting(ctx context.Context, ownerID string, appID uuid.UUID) error {
	if _, err := s.q.IncrementAppConfigVersion(ctx, dbgen.IncrementAppConfigVersionParams{
		OwnerID: ownerID, AppID: appID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("app: version routing change: %w", err)
	}
	s.log.Info("routing convergence requested", slog.String("app_id", appID.String()))
	s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: ownerID, AppID: appID})
	return nil
}

// VerifyDomain proves a claim now, rather than waiting for the checker.
//
// The background checker will reach this domain on its own, so this exists for
// the person standing in front of the page who has just created the record and
// does not want to wait out the backoff. It runs the same Check the checker
// runs. The app reconciler observes that version and is the only component
// allowed to write the Ingress.
func (s *Service) VerifyDomain(ctx context.Context, ownerID, name string, id uuid.UUID) error {
	if s.resolver == nil {
		return errors.New("app: this install cannot resolve DNS, so a domain cannot be verified here")
	}
	reserved, err := s.CustomDomainReservations(ctx)
	if err != nil {
		return err
	}
	t, err := domain.Verify(ctx, s.q, s.resolver, reserved, ownerID, id)
	if t.Withdrew() {
		// The check took a routed name out — it stopped pointing here, or it
		// is one of the install's own. The config version moved with the
		// state, so the reconciler takes it off the Ingress; the refusal still
		// goes back to the person who asked.
		if row, gerr := s.q.GetCustomDomain(ctx, dbgen.GetCustomDomainParams{OwnerID: ownerID, ID: id}); gerr == nil {
			s.log.Info("custom domain withdrawn", slog.String("app", name), slog.String("host", row.Host))
			s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: ownerID, AppID: row.AppID})
		}
	}
	if err != nil {
		return err
	}

	// Now that it is proven it belongs in the Ingress, and nothing else will
	// put it there — the reconcile happens on apply.
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if _, err := s.q.IncrementAppConfigVersion(ctx, dbgen.IncrementAppConfigVersionParams{
		OwnerID: ownerID, AppID: a.ID,
	}); err != nil {
		return fmt.Errorf("app: version verified domain: %w", err)
	}
	s.log.Info("custom domain verified", slog.String("app", name))
	s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: ownerID, AppID: a.ID, App: a.Name})
	return nil
}

// InstallDomain is one custom domain, with the app it belongs to.
type InstallDomain struct {
	domain.Custom
	App string
}

// AllDomains lists every custom domain on the install, unsettled first.
//
// Nothing answered "which domains are stuck" before this. A domain lives on one
// app's page, so an operator watching several had to visit each in turn and
// remember what they saw — which is how a domain that quietly drifted stayed
// unnoticed until somebody complained.
func (s *Service) AllDomains(ctx context.Context, ownerID string) ([]InstallDomain, error) {
	rows, err := s.q.ListCustomDomainsForOwner(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("app: list custom domains: %w", err)
	}
	out := make([]InstallDomain, 0, len(rows))
	for _, r := range rows {
		out = append(out, InstallDomain{
			Custom: domain.ToCustom(r.Domain),
			App:    r.AppName,
		})
	}
	return out, nil
}

// TargetStatus reports whether the install's CNAME target itself resolves.
//
// ValidateCNAMETarget checks the shape of the string and nothing else, so a
// hostname that is well-formed and does not exist is accepted — and then every
// custom domain on the install fails to verify, reporting the failure against
// the customer's name rather than against the setting that caused it.
//
// A sentence rather than a bool because the answer is shown, and "could not be
// checked" is a third state that must not read as "broken".
func (s *Service) TargetStatus(ctx context.Context, target string) string {
	if target == "" || s.resolver == nil {
		return ""
	}
	addrs, err := s.resolver.LookupHost(ctx, target)
	if err != nil || len(addrs) == 0 {
		return "does not resolve"
	}
	return "resolves to " + strings.Join(addrs, ", ")
}

// ResolverName says which resolver answers questions about custom domains.
//
// Surfaced because "this name does not resolve" is one resolver's opinion, and
// somebody comparing it against their own dig output needs to know whether the
// two asked the same server.
func (s *Service) ResolverName() string {
	if s.resolver == nil {
		return ""
	}
	return domain.ResolverName(s.resolver)
}

// RequestDomainCheck brings a domain's next background check forward.
func (s *Service) RequestDomainCheck(ctx context.Context, ownerID string, id uuid.UUID) error {
	n, err := s.q.RequestDomainCheck(ctx, dbgen.RequestDomainCheckParams{
		OwnerID: ownerID, ID: id, Due: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("app: request domain check: %w", err)
	}
	if n == 0 {
		return domain.ErrDomainNotFound
	}
	return nil
}

// RemoveDomain releases a claim and stops routing it.
func (s *Service) RemoveDomain(ctx context.Context, ownerID, name string, id uuid.UUID) error {
	if err := domain.RemoveCustom(ctx, s.q, ownerID, id); err != nil {
		return err
	}
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if _, err := s.q.IncrementAppConfigVersion(ctx, dbgen.IncrementAppConfigVersionParams{
		OwnerID: ownerID, AppID: a.ID,
	}); err != nil {
		return fmt.Errorf("app: version removed domain: %w", err)
	}
	s.log.Info("custom domain removed", slog.String("app", name))
	s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: ownerID, AppID: a.ID, App: a.Name})
	return nil
}
