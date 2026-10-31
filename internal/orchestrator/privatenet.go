package orchestrator

import (
	"context"
	"errors"
	"fmt"
)

// A project's private network, Railway's: inside one project and
// environment, a service reaches another by its name — `db`, or
// `db.internal` — and nothing outside the project reaches it at all.
//
// Every app has a namespace of its own, so Kubernetes' short names, which
// only reach a Service in the asker's own namespace, reach none of its
// siblings. Each namespace is therefore given an alias for every sibling: a
// Service of type ExternalName, named for the sibling's service, which cluster
// DNS answers with the sibling's full name. The alias lives only in the
// namespaces of the same project and environment, so the name resolves
// nowhere else — a team's `db` and another's never meet.
//
// `<name>.internal` is the same alias by another route: a pod's resolver tries
// `db.internal.<its namespace>.svc.cluster.local` first, and the cluster's DNS
// rewrites that to `db.<its namespace>.svc.cluster.local`. That rewrite is the
// one thing outside the engine, written once for the whole cluster by whoever
// installs it (PrivateDNSRewrite).

const (
	// LabelProject is on every app's namespace: the project its app is drawn
	// on. Isolation lets in the namespaces that share it, and the owner.
	LabelProject = "yacht/project"

	// LabelPrivateAlias marks a Service that is another app's private name,
	// so a sync knows which of a namespace's Services it may replace or
	// remove. An app's own Service never carries it.
	LabelPrivateAlias = "yacht/private-alias"

	// AnnotationPrivatePeers lists, comma-separated, the namespaces of the
	// owner's apps in other projects that may still connect: those whose
	// variables name this app. Read by isolation wherever it is rebuilt.
	AnnotationPrivatePeers = "yacht/private-peers"
)

// PrivateDomainSuffix ends the short private name the dashboard shows.
const PrivateDomainSuffix = "internal"

// PrivateDNSRewrite is the CoreDNS rule that makes `<name>.internal` the alias
// `<name>` in the asker's own namespace, in a k3s `coredns-custom` ConfigMap
// as a `*.override` entry. Restricted to the engine's namespaces, so nothing
// else on the cluster changes meaning. `answer auto` gives the answer back
// under the name that was asked, which resolvers check.
const PrivateDNSRewrite = `rewrite stop {
  name regex ^([a-z0-9]([-a-z0-9]*[a-z0-9])?)\.internal\.(yacht-[a-z0-9-]+)\.svc\.cluster\.local\.$ {1}.{3}.svc.cluster.local.
  answer auto
}
`

// PrivateAlias is one sibling's private name in a namespace.
type PrivateAlias struct {
	// Name is the sibling's service name, what the alias Service is called.
	Name string
	// Host is the sibling's full name in cluster DNS.
	Host string
}

// PrivateNetwork is where one app's namespace stands on the private network.
type PrivateNetwork struct {
	Owner     OwnerID
	Namespace string

	// Project is the app's project; empty leaves isolation at the team.
	Project string

	// Aliases are its siblings' names: every other service in the same
	// project and environment, and its own where that differs from its
	// Service's. Any alias the namespace has and this list lacks goes.
	Aliases []PrivateAlias

	// Peers are namespaces of the owner's apps in other projects that may
	// connect: those that refer to this app in a variable, which used to
	// reach it when isolation stopped at the team, and must still.
	Peers []string
}

// Validate checks the spec before any of it reaches a cluster.
func (p PrivateNetwork) Validate() error {
	if p.Owner == "" {
		return errors.New("private network: owner is required")
	}
	if err := ValidateDNSLabel("namespace", p.Namespace); err != nil {
		return err
	}
	if err := ValidateLabelValue("project", p.Project); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, a := range p.Aliases {
		if err := ValidateDNSLabel("alias", a.Name); err != nil {
			return err
		}
		if seen[a.Name] {
			return fmt.Errorf("private network: two aliases named %q", a.Name)
		}
		seen[a.Name] = true
		if !hostRE.MatchString(a.Host) {
			return fmt.Errorf("private network: alias %s: %q is not a hostname", a.Name, a.Host)
		}
	}
	for _, n := range p.Peers {
		if err := ValidateDNSLabel("peer namespace", n); err != nil {
			return err
		}
	}
	return nil
}

// PrivateNetworker is an orchestrator that keeps projects' private networks:
// the label, aliases and isolation that go with each app's namespace. A
// namespace that does not exist yet is left alone, not made, and reported as
// ErrNotFound: the app has not been deployed, and its network is still to do.
type PrivateNetworker interface {
	SyncPrivateNetwork(ctx context.Context, p PrivateNetwork) error
}
