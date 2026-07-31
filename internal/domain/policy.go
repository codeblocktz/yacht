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
	"regexp"
	"strings"
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

// normalize lowercases, trims spaces, and drops a trailing root dot, so that
// "Apps.Example.COM." and "apps.example.com" compare equal. DNS is
// case-insensitive and the root dot is optional; comparing raw input would
// make both of those into security-relevant differences.
func normalize(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}
