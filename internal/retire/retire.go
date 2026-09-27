// Package retire takes a machine out of the cluster without customers
// noticing.
//
// The orchestrator supplies single idempotent steps — read the plan, restart
// one app, evict one pod. This package decides the order: close the node to new
// work, move the engine's apps one at a time by rolling restart, waiting for
// each to finish before starting the next, and only then evict what is left.
//
// Nothing is remembered between passes. Every pass reads the cluster and does
// the next thing, so a retirement carries on after a restart of the process
// driving it, and two replicas driving the same one make the same choice.
package retire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// interval is how often retiring nodes are advanced. Short, because a rollout
// that finished is the next app's cue and an idle gap is time the operator
// spends watching nothing happen; each pass is a handful of list calls.
const interval = 5 * time.Second

// Retirer advances node retirements.
type Retirer struct {
	orch orchestrator.Orchestrator
	nm   orchestrator.NodeManager
	nr   orchestrator.NodeRetirer
	log  *slog.Logger
	now  func() time.Time
}

// For returns a Retirer when the orchestrator can both manage nodes and move
// workloads off them.
func For(orch orchestrator.Orchestrator, log *slog.Logger) (*Retirer, bool) {
	nm, ok := orch.(orchestrator.NodeManager)
	if !ok {
		return nil, false
	}
	nr, ok := orch.(orchestrator.NodeRetirer)
	if !ok {
		return nil, false
	}
	return &Retirer{orch: orch, nm: nm, nr: nr, log: log, now: time.Now}, true
}

// NoRoomError is a refusal to start: the rest of the cluster cannot hold what
// the node is running.
type NoRoomError struct {
	Node string
	Room orchestrator.RetireRoom
}

func (e *NoRoomError) Error() string {
	return fmt.Sprintf("retire: the other nodes cannot hold what %s is running", e.Node)
}

// Plan reads what retiring a node involves, as things stand.
func (r *Retirer) Plan(ctx context.Context, node string) (orchestrator.RetirePlan, error) {
	return r.nr.RetirePlan(ctx, node)
}

// Begin starts retiring a node and takes the first step.
//
// The room check is made once, here, and not on every pass: once apps have
// started moving, what is left on the node has already been counted into the
// room elsewhere, and refusing half way would strand the rest on a cordoned
// machine. Beginning a retirement already under way just steps it.
func (r *Retirer) Begin(ctx context.Context, node string) (orchestrator.RetirePlan, error) {
	plan, err := r.nr.RetirePlan(ctx, node)
	if err != nil {
		return plan, err
	}
	if !plan.Retiring() {
		if !plan.Room.Fits() {
			return plan, &NoRoomError{Node: node, Room: plan.Room}
		}
		// Cordoned before marked: the mark is what starts restarts, and a
		// restart onto a node still taking work can land right back on it.
		if err := r.nm.Cordon(ctx, node, true); err != nil {
			return plan, err
		}
		if err := r.nr.MarkRetiring(ctx, node, r.now()); err != nil {
			return plan, err
		}
		r.log.Info("node retirement started", slog.String("node", node),
			slog.Int("apps", len(plan.Apps)), slog.Int("pinned", len(plan.Pinned)))
	}
	return r.Step(ctx, node)
}

// Stop abandons a retirement and opens the node to work again. What has moved
// stays where it went.
func (r *Retirer) Stop(ctx context.Context, node string) error {
	if err := r.nr.MarkRetiring(ctx, node, time.Time{}); err != nil {
		return err
	}
	if err := r.nm.Cordon(ctx, node, false); err != nil {
		return err
	}
	r.log.Info("node retirement stopped", slog.String("node", node))
	return nil
}

// Step does the next thing a retirement needs, and returns the plan it acted
// on. Safe to call at any time and any number of times.
func (r *Retirer) Step(ctx context.Context, node string) (orchestrator.RetirePlan, error) {
	plan, err := r.nr.RetirePlan(ctx, node)
	if err != nil || !plan.Retiring() {
		return plan, err
	}

	// Somebody reopened the node outside the dashboard. The mark still says
	// retire, so it is closed again before anything is restarted.
	if !plan.Cordoned {
		if err := r.nm.Cordon(ctx, node, true); err != nil {
			return plan, err
		}
	}

	// One app at a time. The next surge pod needs room the last one may
	// still be holding, and a cluster only just big enough for the move is
	// exactly the one a retirement is most often run on.
	if _, busy := plan.Moving(); busy {
		return plan, nil
	}
	if next, ok := plan.Next(); ok {
		return plan, r.restart(ctx, plan, next)
	}
	if plan.AppsSettled() {
		return plan, r.evictRest(ctx, plan)
	}
	return plan, nil
}

func (r *Retirer) restart(ctx context.Context, plan orchestrator.RetirePlan, a orchestrator.RetireApp) error {
	err := r.nr.RestartApp(ctx, a.Namespace, a.Name, plan.Token())
	if errors.Is(err, orchestrator.ErrNotFound) {
		// Deleted since the plan was read. Nothing left to move.
		return nil
	}
	if err != nil {
		return err
	}
	r.log.Info("moving app off retiring node", slog.String("node", plan.Node),
		slog.String("app", a.Namespace+"/"+a.Name), slog.Bool("rolling", a.Rolling))
	return nil
}

// evictRest asks everything that is not the engine's to leave, once the apps
// have gone. Last, because nothing is known about how these tolerate a
// restart, and an app's replacement should not have to compete with them for
// room.
func (r *Retirer) evictRest(ctx context.Context, plan orchestrator.RetirePlan) error {
	var errs []error
	for _, p := range plan.Evict {
		if err := r.nr.EvictPod(ctx, p.Namespace, p.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		r.log.Info("evicted from retiring node", slog.String("node", plan.Node),
			slog.String("pod", p.Namespace+"/"+p.Name))
	}
	return errors.Join(errs...)
}

// Run advances every retiring node until ctx ends.
func (r *Retirer) Run(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Retirer) pass(ctx context.Context) {
	nodes, err := r.orch.Nodes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("retire: list nodes", slog.String("error", err.Error()))
		}
		return
	}
	for _, n := range nodes {
		if n.RetiringSince.IsZero() {
			continue
		}
		if _, err := r.Step(ctx, n.Name); err != nil && ctx.Err() == nil {
			r.log.Warn("retire: step", slog.String("node", n.Name), slog.String("error", err.Error()))
		}
	}
}
