// Package observe reads what the install remembers about its apps after the
// pods that produced it are gone: their logs, from a VictoriaLogs store, and
// their resource use and traffic, from a VictoriaMetrics store.
//
// Both stores hold every team's data together, because one collector on each
// node reads every container and one scraper reads every ingress counter. So
// the whole tenancy boundary of this package is the Scope: a query is only
// ever built from a Scope, a Scope is only ever built from apps an
// owner-scoped database read returned, and nothing a person types is allowed
// to widen it. What they type narrows a result inside the scope, escaped as
// a literal, and never names one.
//
// Neither store is required. An install without them has the live pod logs
// and metrics-server it always had, and the pages that read from here say
// which store is missing rather than drawing empty charts.
package observe

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// ErrEmptyScope is a query with no services in it.
//
// Refused rather than run, because the honest result of "this team's logs"
// for a team with no apps is nothing, and the query a store runs for an empty
// filter is everything — every team's output. The difference between those
// two is the whole of this package's job.
var ErrEmptyScope = errors.New("observe: a query needs at least one service to scope it")

// ErrNotConfigured is a store the install was not given.
var ErrNotConfigured = errors.New("observe: not configured")

// Service is one app as a query sees it: the name a person knows it by, and
// the namespace its pods run in.
//
// Each app has a namespace of its own (app.Namespace), so the namespace is
// what identifies an app's output in a store shared by every team. The name
// is carried only to label what comes back.
type Service struct {
	Name      string
	Namespace string

	// Port is the port the app's Service publishes, which is part of the
	// name Traefik counts its requests under. Zero is the engine's own,
	// orchestrator.PublicServicePort.
	Port int32
}

// Scope is the services one query may read, and the team they belong to.
//
// Built only by NewScope, from rows the caller read for Owner. Its fields are
// unexported so that a handler cannot assemble one from a request — the only
// way in is through the constructor, which checks every value that ends up in
// a query.
type Scope struct {
	owner    string
	services []Service
	byNS     map[string]string
}

// NewScope builds the scope for one team's services.
//
// Every namespace must be a DNS label, which is what the engine names them
// with. That is checked rather than assumed because the namespaces are
// written into LogsQL and PromQL filters, and a DNS label has no character
// either language would read as anything but part of the name.
func NewScope(owner string, services []Service) (Scope, error) {
	if strings.TrimSpace(owner) == "" {
		return Scope{}, errors.New("observe: a scope needs an owner")
	}
	s := Scope{owner: owner, byNS: map[string]string{}}
	for _, svc := range services {
		if err := orchestrator.ValidateDNSLabel("namespace", svc.Namespace); err != nil {
			return Scope{}, fmt.Errorf("observe: %w", err)
		}
		if err := orchestrator.ValidateDNSLabel("service", svc.Name); err != nil {
			return Scope{}, fmt.Errorf("observe: %w", err)
		}
		if _, dup := s.byNS[svc.Namespace]; dup {
			continue
		}
		if svc.Port == 0 {
			svc.Port = orchestrator.PublicServicePort
		}
		s.byNS[svc.Namespace] = svc.Name
		s.services = append(s.services, svc)
	}
	// Sorted, so the same team's query is the same text every time: easier
	// to read in a store's slow-query log, and stable under test.
	sort.Slice(s.services, func(i, j int) bool { return s.services[i].Name < s.services[j].Name })
	return s, nil
}

// Empty reports whether the scope has nothing in it to read.
func (s Scope) Empty() bool { return len(s.services) == 0 }

// Owner is the team the scope belongs to.
func (s Scope) Owner() string { return s.owner }

// Services are the services in the scope, by name.
func (s Scope) Services() []Service { return append([]Service(nil), s.services...) }

// ServiceFor names the service a namespace belongs to, and whether it is one
// of this scope's at all. A result from a store is labelled through this, so
// a row whose namespace is not in the scope is recognisable as one that
// should never have come back.
func (s Scope) ServiceFor(namespace string) (string, bool) {
	name, ok := s.byNS[namespace]
	return name, ok
}

// Narrow keeps only the named services, ignoring any name that is not one of
// this scope's. It can make a scope smaller and never larger, which is what
// lets a filter typed into a search box be honoured at all.
func (s Scope) Narrow(names []string) Scope {
	if len(names) == 0 {
		return s
	}
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(strings.TrimSpace(n))] = true
	}
	out := Scope{owner: s.owner, byNS: map[string]string{}}
	for _, svc := range s.services {
		if want[svc.Name] {
			out.services = append(out.services, svc)
			out.byNS[svc.Namespace] = svc.Name
		}
	}
	return out
}

// logFilter is the LogsQL that confines a query to the scope.
//
// Two conditions, both of which every line must meet: the namespace is one
// of the scope's, as a stream filter, which is also what makes the store read
// only those streams from disk; and the pod carries the team's owner label.
// The namespace alone is enough — each is one app's, made by the engine — and
// the owner is checked as well so that a namespace that somehow changed hands
// still shows nothing to the team it no longer belongs to.
//
// It is sent to the store as extra_filters, not only joined to the query. The
// store applies extra filters to the whole query, whatever the query says, so
// even a query that a bug had let a person rewrite could not read past it.
func (s Scope) logFilter() (string, error) {
	if s.Empty() {
		return "", ErrEmptyScope
	}
	quoted := make([]string, len(s.services))
	for i, svc := range s.services {
		quoted[i] = strconv.Quote(svc.Namespace)
	}
	return "{" + fieldNamespace + " in (" + strings.Join(quoted, ",") + ")} " +
		strconv.Quote(fieldOwner) + ":=" + strconv.Quote(s.owner), nil
}

// namespaceMatcher is the PromQL label matcher for the scope's namespaces,
// anchored as PromQL always anchors one.
func (s Scope) namespaceMatcher() (string, error) {
	if s.Empty() {
		return "", ErrEmptyScope
	}
	parts := make([]string, len(s.services))
	for i, svc := range s.services {
		parts[i] = regexp.QuoteMeta(svc.Namespace)
	}
	return `namespace=~` + strconv.Quote(strings.Join(parts, "|")), nil
}

// serviceMatcher is the PromQL label matcher for the names Traefik counts the
// scope's requests under.
func (s Scope) serviceMatcher() (string, error) {
	if s.Empty() {
		return "", ErrEmptyScope
	}
	parts := make([]string, len(s.services))
	for i, svc := range s.services {
		parts[i] = regexp.QuoteMeta(TraefikService(svc))
	}
	return `service=~` + strconv.Quote(strings.Join(parts, "|")), nil
}

// TraefikService is the name Traefik's metrics count an app's requests under:
// the namespace, the Service, its port, and the provider the route came from.
//
// The same rule the sleeper counts requests by (orchestrator/k8s,
// traefikServiceName); a test there holds the two together.
func TraefikService(svc Service) string {
	port := svc.Port
	if port == 0 {
		port = orchestrator.PublicServicePort
	}
	return svc.Namespace + "-" + svc.Name + "-" + strconv.Itoa(int(port)) + "@kubernetes"
}

// serviceForTraefik maps a Traefik service label back to one of the scope's.
func (s Scope) serviceForTraefik(label string) (string, bool) {
	for _, svc := range s.services {
		if TraefikService(svc) == label {
			return svc.Name, true
		}
	}
	return "", false
}
