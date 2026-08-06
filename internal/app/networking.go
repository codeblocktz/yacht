package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/domain"
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
		Target:    s.cnameTarget(ctx),
	}
	if out.Custom, err = domain.ListCustom(ctx, s.q, ownerID, a.ID); err != nil {
		return Networking{}, err
	}
	return out, nil
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
	if _, err := domain.AddCustom(ctx, s.q, ownerID, a.ID, host,
		s.cnameTarget(ctx), s.opts.AppDomain, s.opts.ReservedDomains); err != nil {
		return err
	}
	if _, err := s.q.IncrementAppConfigVersion(ctx, dbgen.IncrementAppConfigVersionParams{
		OwnerID: ownerID, AppID: a.ID,
	}); err != nil {
		return fmt.Errorf("app: version domain claim: %w", err)
	}
	s.log.Info("custom domain claimed", slog.String("app", name), slog.String("host", host))
	return nil
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
	if err := domain.Verify(ctx, s.q, s.resolver, ownerID, id); err != nil {
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
	return nil
}
