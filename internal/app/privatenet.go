package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Railway's private network: inside a project and environment, a service
// reaches another as `db.internal`, or plainly `db`, and nothing outside the
// project reaches it at all. See orchestrator.PrivateNetwork for how the
// cluster is made to; this file decides what each namespace should have.

// PrivateNetworkInterval is how often every app's private network is
// compared with what it should be. Short, because a service added to a
// project has no short name in its siblings' namespaces until a pass has run;
// cheap, because a pass writes nothing for a namespace that has not changed.
const PrivateNetworkInterval = 10 * time.Second

// privateNetworkRefresh is how often a pass writes every namespace whether or
// not it changed, so one edited or lost behind the engine's back is put right.
const privateNetworkRefresh = 10 * time.Minute

// PrivateName is the short name the app's siblings reach it by.
func (a App) PrivateName() string {
	return a.ServiceName() + "." + orchestrator.PrivateDomainSuffix
}

// privateNetworkState is what the last pass wrote, per namespace, so the next
// writes only what changed.
type privateNetworkState struct {
	mu      sync.Mutex
	written map[string]string // namespace → fingerprint of what was written
	since   time.Time         // the last full refresh
}

// noProject groups the apps of a team that has never had its default
// project made: on no project's canvas, they are together on none. Never a
// project's id, which is a UUID.
const noProject = "none"

// networkProject is the project an app's namespace is grouped by: the one
// it is drawn on, or for an app on none, the team's default, where the canvas
// shows it — or noProject, before the team has one.
func (s *Service) networkProject(ctx context.Context, q *dbgen.Queries, a App) string {
	if a.ProjectID != uuid.Nil {
		return a.ProjectID.String()
	}
	p, err := s.projectFor(ctx, q, a.OwnerID, uuid.Nil)
	if err != nil {
		if !errors.Is(err, ErrProjectNotFound) {
			s.log.Debug("default project unavailable for the private network",
				slog.String("app", a.Name), slog.String("error", err.Error()))
		}
		return noProject
	}
	return p.ID.String()
}

// PrivateNetworks is every app namespace's private network as it should be.
//
// Grouped by team, project and environment: an app's aliases are the
// services that share all three, so `db` in staging is staging's database and
// never production's. Isolation is drawn at the project, though, not the
// environment: a staging app whose reference falls through to production's
// service — the one there is, when staging has no copy of it — still
// connects, as it did.
func (s *Service) PrivateNetworks(ctx context.Context) ([]orchestrator.PrivateNetwork, error) {
	rows, err := s.q.ListAppsForPrivateNetwork(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list apps for private networks: %w", err)
	}
	defaults, err := s.q.ListDefaultProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list default projects: %w", err)
	}
	links, err := s.q.ListAllAppLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list links for private networks: %w", err)
	}
	defaultOf := map[string]string{}
	for _, d := range defaults {
		defaultOf[d.OwnerID] = d.ID.String()
	}

	type group struct{ owner, project, env string }
	var apps []App
	projectOf := map[uuid.UUID]string{}
	byID := map[uuid.UUID]App{}
	members := map[group][]App{}
	for _, row := range rows {
		a := toApp(row)
		if !a.Source.Workload() {
			continue
		}
		project := cmp.Or(defaultOf[a.OwnerID], noProject)
		if a.ProjectID != uuid.Nil {
			project = a.ProjectID.String()
		}
		apps = append(apps, a)
		projectOf[a.ID] = project
		byID[a.ID] = a
		g := group{a.OwnerID, project, a.Environment()}
		members[g] = append(members[g], a)
	}

	// An app another project's app refers to lets that app in. Within a
	// project the label already does; across teams there are no links.
	peers := map[uuid.UUID][]string{}
	for _, l := range links {
		from, okFrom := byID[l.FromAppID]
		to, okTo := byID[l.ToAppID]
		if !okFrom || !okTo || from.OwnerID != to.OwnerID || projectOf[from.ID] == projectOf[to.ID] {
			continue
		}
		peers[to.ID] = append(peers[to.ID], from.Namespace)
	}

	out := make([]orchestrator.PrivateNetwork, 0, len(apps))
	for _, a := range apps {
		n := orchestrator.PrivateNetwork{
			Owner:     orchestrator.OwnerID(a.OwnerID),
			Namespace: a.Namespace,
			Project:   projectOf[a.ID],
			Peers:     peers[a.ID],
		}
		for _, sib := range members[group{a.OwnerID, projectOf[a.ID], a.Environment()}] {
			// Nothing listens on an app with no port: it has no Service to
			// name. And an app's own name is its Service already.
			if sib.Port == 0 || sib.ServiceName() == a.Name {
				continue
			}
			n.Aliases = append(n.Aliases, orchestrator.PrivateAlias{
				Name: sib.ServiceName(), Host: privateHost(sib),
			})
		}
		out = append(out, n)
	}
	return out, nil
}

// SyncPrivateNetworks brings every app namespace's private network into line,
// writing only those that changed since the last pass, or all of them once
// every privateNetworkRefresh. One namespace failing is logged and does not
// stop the rest. A no-op on an orchestrator without private networks.
func (s *Service) SyncPrivateNetworks(ctx context.Context) error {
	pn, ok := s.orch.(orchestrator.PrivateNetworker)
	if !ok {
		return nil
	}
	networks, err := s.PrivateNetworks(ctx)
	if err != nil {
		return err
	}
	st := &s.privateNet
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.written == nil || time.Since(st.since) > privateNetworkRefresh {
		st.written, st.since = map[string]string{}, time.Now()
	}
	for _, n := range networks {
		fp := fingerprint(n)
		if st.written[n.Namespace] == fp {
			continue
		}
		switch err := pn.SyncPrivateNetwork(ctx, n); {
		case err == nil:
			st.written[n.Namespace] = fp
		case errors.Is(err, orchestrator.ErrNotFound):
			// Not deployed yet: there is nothing to give names to, and the
			// pass after its first deploy will.
		default:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Warn("private network not brought into line",
				slog.String("namespace", n.Namespace), slog.String("error", err.Error()))
		}
	}
	return nil
}

// forgetPrivateNetwork makes the next pass write a namespace whether or not
// it looks changed: a deploy has just made or remade it.
func (s *Service) forgetPrivateNetwork(namespace string) {
	s.privateNet.mu.Lock()
	delete(s.privateNet.written, namespace)
	s.privateNet.mu.Unlock()
}

// RunPrivateNetworks keeps every app's private network in line until ctx
// ends.
func (s *Service) RunPrivateNetworks(ctx context.Context) {
	tick := time.NewTicker(PrivateNetworkInterval)
	defer tick.Stop()
	for {
		if err := s.SyncPrivateNetworks(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("private networks paused", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// fingerprint is a network's content, in an order that does not depend on
// how it was listed.
func fingerprint(n orchestrator.PrivateNetwork) string {
	aliases := make([]string, 0, len(n.Aliases))
	for _, a := range n.Aliases {
		aliases = append(aliases, a.Name+"="+a.Host)
	}
	slices.Sort(aliases)
	peers := slices.Clone(n.Peers)
	slices.Sort(peers)
	return strings.Join([]string{
		string(n.Owner), n.Project, strings.Join(aliases, ","), strings.Join(peers, ","),
	}, "|")
}
