// Package disposable knows the mail domains that hand out throwaway
// addresses — a mailbox that lives ten minutes, made to receive one sign-up
// link — so open sign-up can refuse them.
//
// The list is the disposable-email-domains project's blocklist
// (github.com/disposable-email-domains/disposable-email-domains), which is
// maintained by its community and released under CC0 1.0: public domain, no
// attribution required, credited here anyway. It is embedded rather than
// fetched, so a start never waits on GitHub and a review sees every change
// to it; `make disposable-domains` refreshes domains.txt from upstream.
package disposable

import (
	_ "embed"
	"strings"
	"sync"
)

//go:embed domains.txt
var domainsTxt string

var (
	once    sync.Once
	domains map[string]struct{}
)

func load() {
	domains = make(map[string]struct{}, 12_000)
	for _, line := range strings.Split(domainsTxt, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		domains[line] = struct{}{}
	}
}

// Domain reports whether a domain is a disposable one, or a subdomain of
// one: a service that answers mail for x.mailinator.com hands out the same
// throwaway mailboxes as mailinator.com does. Case does not matter.
func Domain(domain string) bool {
	once.Do(load)
	d := strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	for d != "" {
		if _, ok := domains[d]; ok {
			return true
		}
		_, rest, found := strings.Cut(d, ".")
		if !found {
			return false
		}
		d = rest
	}
	return false
}

// Address reports whether an email address is at a disposable domain. An
// address with no @ is not one: it is malformed, which is the caller's to say.
func Address(email string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	return Domain(email[at+1:])
}

// Len is how many domains the embedded list holds, for a test to tell an
// empty or truncated refresh from a real one.
func Len() int {
	once.Do(load)
	return len(domains)
}
