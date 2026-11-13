// Package domain owns hostnames: the one the platform issues for an app, and
// the rule about which names a tenant may never claim.
//
// The policy in this file is deliberately pure — no database, no Kubernetes.
// The reserved-suffix rule is the piece of this subsystem that is unsafe to
// get subtly wrong, so it is kept somewhere it can be exhaustively tested on
// its own.
package domain

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// ErrNoAppDomain means no platform domain is configured. Callers treat this as
// the feature being switched off rather than as a failure, so it is a sentinel
// rather than a generic error.
var ErrNoAppDomain = errors.New("domain: no app domain configured")

// A single DNS label: what an app name must be to become a hostname.
var labelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// A dotted DNS name of at least two labels. Requiring a dot rejects bare names
// like "localhost", which would produce hostnames that cannot be delegated.
var domainRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

// maxHostname is the DNS limit on a fully qualified name.
const maxHostname = 253

// checkCustomHost reports whether host, already normalised, is a name somebody
// could actually point at an app and get a certificate for.
//
// domainRE alone lets through two things that look like names but are not: a
// label longer than DNS allows, which no resolver will ever answer, and an IP
// address such as 10.0.0.1, which matches as four numeric labels. Neither can
// be verified or certified, so both are refused here rather than left pending
// forever. A top-level label that is all digits is how an IP literal shows
// itself, and no real TLD is numeric, so that is the test.
func checkCustomHost(host string) error {
	switch {
	case host == "":
		return errors.New("a domain is required, for example shop.example.com")
	case len(host) > maxHostname:
		return fmt.Errorf("domain: hostname exceeds %d characters", maxHostname)
	case !domainRE.MatchString(host):
		return fmt.Errorf("domain: %q is not a valid hostname", host)
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if len(l) > MaxLabel {
			return fmt.Errorf("domain: %q is not a valid hostname: a part between dots is at most %d characters", host, MaxLabel)
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return fmt.Errorf("domain: %q is not a valid hostname: an IP address cannot be a custom domain", host)
	}
	return nil
}

// Issue returns the platform hostname for an app: <app name>.<app domain>.
//
// It returns ErrNoAppDomain when no domain is configured, which is not a
// failure — it is how an install with the feature switched off is detected.
func Issue(appName, appDomain string) (string, error) {
	d := normalize(appDomain)
	if d == "" {
		return "", ErrNoAppDomain
	}
	if !domainRE.MatchString(d) {
		return "", fmt.Errorf("domain: %q is not a valid app domain", appDomain)
	}

	n := normalize(appName)
	if !labelRE.MatchString(n) {
		return "", fmt.Errorf("domain: %q is not a valid hostname label", appName)
	}

	host := n + "." + d
	if len(host) > maxHostname {
		return "", fmt.Errorf("domain: hostname %q exceeds %d characters", host, maxHostname)
	}
	return host, nil
}

// generatedLabel is the label generated hostnames live under:
// <name>.up.<app domain>.
//
// One label down from the app domain rather than at its top, because the top
// is shared with everything else the install answers on — its dashboard, its
// registry, the hostnames apps were issued before names were generated — and a
// name somebody picks for their app must not be able to land on one of those.
// Railway keeps its own the same way, under up.railway.app.
const generatedLabel = "up"

// MaxLabel is the DNS limit on one label of a name.
const MaxLabel = 63

// GeneratedDomain is the domain generated hostnames live under, or "" when
// there is no app domain to put it under.
func GeneratedDomain(appDomain string) string {
	d := normalize(appDomain)
	if d == "" {
		return ""
	}
	return generatedLabel + "." + d
}

// ValidateLabel checks the part of a generated hostname somebody chooses: one
// DNS label, which is what a wildcard certificate for the generated domain can
// cover and the most a name in it may be.
func ValidateLabel(label string) error {
	switch l := normalize(label); {
	case l == "":
		return errors.New("a name is required")
	case len(l) > MaxLabel:
		return fmt.Errorf("%q is %d characters; a name can be at most %d", l, len(l), MaxLabel)
	case !labelRE.MatchString(l):
		return fmt.Errorf("%q is not a valid name: use lowercase letters, numbers and dashes, "+
			"starting and ending with a letter or number", l)
	}
	return nil
}

// SuggestLabel is the name Generate Domain proposes for an app in an
// environment: <app>-<environment>, the way Railway spells it, shortened to fit
// in one label when the two together do not.
func SuggestLabel(appName, environment string) string {
	return fitLabel(normalize(appName), "-"+normalize(environment))
}

// WithSuffix is label with -suffix on the end, shortened to fit in one label:
// what a suggested name becomes when somebody already holds it.
func WithSuffix(label, suffix string) string {
	return fitLabel(normalize(label), "-"+normalize(suffix))
}

// fitLabel joins head and tail, trimming head rather than tail when the two do
// not fit, so what distinguishes the name — the environment, or the suffix
// that made it unique — survives. A trailing dash left by the cut is dropped,
// since a label may not end in one.
func fitLabel(head, tail string) string {
	if len(head)+len(tail) > MaxLabel {
		head = strings.TrimRight(head[:max(0, MaxLabel-len(tail))], "-")
	}
	return strings.TrimLeft(head+tail, "-")
}

// Reserved reports whether host falls under the platform domain or any
// additionally reserved domain.
//
// The platform suffix is reserved whether or not an operator remembered to
// list it, because otherwise a tenant could claim a name under it in the
// window before the platform issued that name itself.
//
// Matching is on the label boundary, never on the raw string:
// evilapps.example.com ends with apps.example.com as a substring but is a
// different domain entirely, and treating it as reserved-by-suffix is the
// classic form of this bug.
func Reserved(host, appDomain string, extra []string) bool {
	h := normalize(host)
	if h == "" {
		return false
	}

	for _, d := range append([]string{appDomain}, extra...) {
		d = normalize(d)
		if d == "" {
			continue
		}
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// Reservations are every name no app may bring as a custom domain.
//
// The operator's list was the only thing here once, and it is empty by
// default. That left the install's own names open: verification asks only
// whether a name resolves to the platform, and the dashboard's hostname, the
// registry's and the CNAME target itself all do, by construction. A team could
// claim the dashboard's host, watch it verify on the first check, and be given
// an Ingress and a certificate for it in its own namespace.
type Reservations struct {
	// AppDomain is the platform domain, reserved with everything under it.
	AppDomain string

	// Reserved is the operator's YACHT_RESERVED_DOMAINS, each reserved with
	// everything under it. Said by the operator, so nothing in Allowed
	// overrides it.
	Reserved []string

	// Zones are the install's own names under which a sibling means
	// something — a cookie scoped to the dashboard's parent, a bucket
	// addressed as a subdomain of the endpoint — reserved with every name
	// under them. Except a registrable domain itself, example.com rather than
	// yacht.example.com: an install whose dashboard sits at its apex would
	// otherwise take every name the operator's company has from every team,
	// including the ones already routed. That is refused as a name.
	Zones []string

	// Names are reserved as themselves and nothing under them: the install's
	// names that are only ever addresses. The registry may be somebody else's,
	// ghcr.io, and refusing what sits under a name like that would refuse
	// nothing anybody could verify while saying something untrue.
	Names []string

	// Allowed is YACHT_RESERVED_DOMAINS_ALLOW: names, each with everything
	// under it, that the install's own Zones and Names do not take. The
	// operator's way out when one of those reaches a domain somebody
	// legitimately brings. It never frees the app domain, the operator's own
	// list, or a domain's CNAME target.
	Allowed []string
}

// Refuses reports whether host is one no app may bring as a custom domain.
func (r Reservations) Refuses(host string) bool {
	if Reserved(host, r.AppDomain, r.Reserved) {
		return true
	}
	h := normalize(host)
	if h == "" || Reserved(h, "", r.Allowed) {
		return false
	}
	for _, z := range r.Zones {
		if z = normalize(z); z != "" && (h == z || (!registrable(z) && strings.HasSuffix(h, "."+z))) {
			return true
		}
	}
	return slices.Contains(normalizeAll(r.Names), h)
}

// Issuable reports whether the platform may issue host as an app's own
// hostname, under the app domain.
//
// The app domain is left out, since every issued name is under it by
// construction, and the install's own names are matched as names alone: the
// dashboard may sit at the app domain itself, and taken as a zone it would
// refuse every hostname the platform issues. What is refused is a generated
// label that lands exactly on one of them — tcp.up.apps.example.com chosen as
// a name when that is where the TCP proxy is reached.
func (r Reservations) Issuable(host string) bool {
	h := normalize(host)
	if Reserved(h, "", r.Reserved) {
		return false
	}
	if Reserved(h, "", r.Allowed) {
		return true
	}
	return !slices.Contains(normalizeAll(r.Zones), h) && !slices.Contains(normalizeAll(r.Names), h)
}

func normalizeAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = normalize(s)
	}
	return out
}

// registrable reports whether host is a registrable domain itself — the
// part of a name somebody buys, example.com or example.co.uk — rather than a
// name under one. A name the public suffix list cannot place, a bare label,
// counts as one: nothing under it is a name a custom domain could be.
func registrable(host string) bool {
	apex, err := publicsuffix.EffectiveTLDPlusOne(host)
	return err != nil || apex == host
}

// PlatformHost reads one of the install's own addresses as configured — a
// URL, host[:port], a bare name, or *.example.com — into the hostname a
// custom domain would be spelled as, in ASCII.
//
// Empty, with no error, for nothing and for an IP address, which no custom
// domain can be. Wildcard reports a *. prefix: everything under the name was
// meant, so the name is a zone whichever list it came from. An error for a
// value that is none of these, so a setting that reserves nothing says so
// rather than quietly reserving nothing.
func PlatformHost(s string) (host string, wildcard bool, err error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return "", false, nil
	}
	s = raw
	if strings.Contains(s, "://") {
		u, perr := url.Parse(s)
		if perr != nil || u.Hostname() == "" {
			return "", false, fmt.Errorf("%q is not a URL with a host", raw)
		}
		s = u.Hostname()
	} else {
		if i := strings.IndexByte(s, '/'); i >= 0 {
			s = s[:i]
		}
		if h, _, serr := net.SplitHostPort(s); serr == nil {
			s = h
		}
	}
	s = strings.Trim(s, "[]")
	if net.ParseIP(s) != nil {
		return "", false, nil
	}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		s, wildcard = rest, true
	}
	ascii, ierr := idna.Lookup.ToASCII(normalize(s))
	if ierr != nil || !domainRE.MatchString(ascii) && !labelRE.MatchString(ascii) {
		return "", false, fmt.Errorf("%q is not a hostname", raw)
	}
	return ascii, wildcard, nil
}

// HostOf is PlatformHost's hostname, empty for anything it cannot read.
func HostOf(s string) string {
	h, _, err := PlatformHost(s)
	if err != nil {
		return ""
	}
	return h
}

// CoveredByWildcard reports whether a single *.<appDomain> certificate can
// serve this hostname.
//
// One label, and exactly one. A wildcard matches web.apps.example.com and does
// not match a.b.apps.example.com — that is the rule in RFC 6125, and browsers
// enforce it whatever anybody intended.
//
// The reason this exists: an Ingress used to be given a TLS block listing every
// host on it whenever wildcard TLS was on. A custom domain can never be under
// the platform domain — the reserved-name check guarantees it — so every custom
// domain was being served from a certificate that could not match it, and the
// dashboard showed a green "routed" beside it.
func CoveredByWildcard(host, appDomain string) bool {
	h, d := normalize(host), normalize(appDomain)
	if h == "" || d == "" {
		return false
	}
	label, ok := strings.CutSuffix(h, "."+d)
	if !ok {
		return false
	}
	// A remaining dot means more than one label, which a wildcard does not
	// reach. An empty label means the apex itself, which it also does not.
	return label != "" && !strings.Contains(label, ".")
}

// CoveredByPlatformWildcard reports whether the install's wildcard
// certificate can serve this hostname: one carrying both *.<appDomain> and
// *.up.<appDomain>, the second for generated hostnames, which sit one label
// further down than the first can reach.
func CoveredByPlatformWildcard(host, appDomain string) bool {
	if CoveredByWildcard(host, appDomain) {
		return true
	}
	gen := GeneratedDomain(appDomain)
	return gen != "" && CoveredByWildcard(host, gen)
}

// WildcardHosts keeps only the hosts the platform wildcard certificate can
// serve: those one label under the app domain, or one label under its
// generated domain.
func WildcardHosts(hosts []string, appDomain string) []string {
	covered := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if CoveredByPlatformWildcard(h, appDomain) {
			covered = append(covered, h)
		}
	}
	return covered
}

// normalize lowercases, trims spaces, and drops a trailing root dot, so that
// "Apps.Example.COM." and "apps.example.com" compare equal. DNS is
// case-insensitive and the root dot is optional; comparing raw input would
// make both of those into security-relevant differences.
func normalize(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}
