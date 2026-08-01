package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// DefaultTXTPrefix is what ExternalDNS is usually deployed with.
//
// A prefix at all, rather than none, is what keeps an apex domain working: the
// ownership TXT record and the apex's own record would otherwise be the same
// name, and the provider refuses the pair.
const DefaultTXTPrefix = "extdns-"

// DNS is how this install publishes hostnames.
type DNS struct {
	// CNAMETarget is what a custom domain's CNAME must point at, and what
	// ExternalDNS is told to target so it publishes a CNAME rather than the
	// nodes' own addresses.
	CNAMETarget string

	// TXTPrefix must match --txt-prefix on the ExternalDNS controller.
	//
	// Recorded rather than applied. Yacht does not deploy that controller and
	// cannot set a flag on it; keeping the value here is so the two can be kept
	// in step and so the page can say what it should be. Nothing in an Ingress
	// carries it.
	TXTPrefix string

	UpdatedAt string
}

// DNS returns the install's DNS settings, with defaults when none are stored.
func (j *Joiner) DNS(ctx context.Context) (DNS, error) {
	row, err := j.q.GetPlatformDNS(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not an error: an install that has never configured DNS is the
			// normal starting state, and the prefix has a sensible default.
			return DNS{TXTPrefix: DefaultTXTPrefix}, nil
		}
		return DNS{}, fmt.Errorf("cluster: read dns settings: %w", err)
	}
	return DNS{
		CNAMETarget: row.CnameTarget,
		TXTPrefix:   row.TxtPrefix,
		UpdatedAt:   row.UpdatedAt.Format("2006-01-02 15:04"),
	}, nil
}

// SetDNS stores the install's DNS settings.
func (j *Joiner) SetDNS(ctx context.Context, target, prefix string) error {
	target = strings.TrimSpace(strings.ToLower(target))
	prefix = strings.TrimSpace(prefix)

	if err := ValidateCNAMETarget(target); err != nil {
		return err
	}
	if err := ValidateTXTPrefix(prefix); err != nil {
		return err
	}

	if _, err := j.q.SetPlatformDNS(ctx, dbgen.SetPlatformDNSParams{
		CnameTarget: target, TxtPrefix: prefix,
	}); err != nil {
		return fmt.Errorf("cluster: store dns settings: %w", err)
	}
	j.log.Info("dns settings changed",
		slog.String("cname_target", target), slog.String("txt_prefix", prefix))
	return nil
}

// ValidateCNAMETarget checks the name custom domains are pointed at.
//
// Empty is allowed and means the feature is off: an install with no ExternalDNS
// has nothing to target, and refusing to save that would force a value that
// does not exist.
func ValidateCNAMETarget(target string) error {
	if target == "" {
		return nil
	}
	// It goes into DNS and into an annotation, so it has to be a hostname and
	// nothing else — no scheme, no path, no port.
	if strings.ContainsAny(target, "/: \t\n") {
		return errors.New(
			"the CNAME target is a hostname on its own, with no scheme, port or path")
	}
	if !domainLike(target) {
		return fmt.Errorf("%q is not a hostname", target)
	}
	return nil
}

// ValidateTXTPrefix checks the ownership record prefix.
func ValidateTXTPrefix(prefix string) error {
	if prefix == "" {
		// Allowed, and worth a word on the page rather than a refusal: without
		// a prefix ExternalDNS cannot manage an apex domain, because the
		// ownership record collides with the record it owns.
		return nil
	}
	if len(prefix) > 30 {
		return errors.New("the prefix must be at most 30 characters")
	}
	for _, r := range prefix {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return errors.New(
				"the prefix must be lowercase letters, numbers, dashes and underscores")
		}
	}
	return nil
}

// domainLike reports whether s looks like a dotted hostname.
func domainLike(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !alnum && !(r == '-' && i > 0 && i < len(label)-1) {
				return false
			}
		}
	}
	return true
}
