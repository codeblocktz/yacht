package domain

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Proving a domain, without anybody pressing anything.
//
// Before this, a claim sat unverified until somebody came back to the page and
// clicked Verify. That is a bad deal on both ends: DNS takes minutes to
// propagate, so the click is usually too early, and once it does propagate
// nothing notices — the domain waits for a person who has moved on.
//
// Level-triggered against DNS, for the same reason ReconcileBuilds is
// level-triggered against the cluster: what a domain resolves to is a fact
// anybody can look up, not something a goroutine has to remember. That is what
// makes this safe to run on every replica and safe to restart.

// CheckInterval is how often the checker looks for work.
//
// Short, because the work it finds is mostly nothing: the query is an index
// scan over rows whose next check is due, and the schedule on each row is what
// actually decides how often any given domain is looked at. This is the
// granularity of the whole system, not its load.
const CheckInterval = 5 * time.Second

// checkBatch is how many domains one pass claims.
//
// Small deliberately. The lookups run one after another, so a large batch is a
// long pass and a long lease; a small one means a busy install spreads its work
// across passes rather than stalling on a slow resolver.
const checkBatch = 20

// lookupTimeout bounds one domain's lookups.
//
// A resolver that has stopped answering must cost this and not more. Without it
// a single unreachable nameserver holds up every other domain in the batch, and
// the symptom is a dashboard where nothing updates for reasons that have
// nothing to do with the domain being looked at.
const lookupTimeout = 5 * time.Second

// checkLease is how long a claimed domain is hidden from other checkers.
//
// Comfortably longer than a batch can take, so a slow pass does not have its
// own rows stolen, and short enough that a checker killed mid-pass has its work
// picked up promptly rather than at the domain's own next interval.
const checkLease = 2 * time.Minute

// Router records that an app's routing projection changed.
//
// An interface here, implemented by the app service, because a domain becoming
// routable has to reach the Ingress and this package must not import the one
// that owns workloads — app already imports this one.
//
// The app reconciler owns the actual cluster apply and promotes verified
// domains only after its workload commit marker is observable.
type Router interface {
	ApplyRouting(ctx context.Context, ownerID string, appID uuid.UUID) error
}

// Checker proves claimed domains in the background.
type Checker struct {
	pool   *pgxpool.Pool
	res    Resolver
	router Router
	log    *slog.Logger

	// now is injected so the schedule can be tested without waiting for it.
	now func() time.Time
}

// NewChecker builds a checker. A nil resolver disables it entirely, which is
// the right answer for an install that cannot look anything up: a checker that
// recorded a lookup failure every five seconds forever would fill the log with
// one fact.
func NewChecker(
	pool *pgxpool.Pool, res Resolver, router Router, log *slog.Logger,
) *Checker {
	if log == nil {
		log = slog.Default()
	}
	return &Checker{pool: pool, res: res, router: router, log: log, now: time.Now}
}

// Run checks domains until the context is cancelled.
//
// Runs once immediately. A restart is exactly when a domain is most likely to
// be sitting in a state nothing has looked at — the process that would have
// checked it went away — and waiting a full interval to notice adds that delay
// to every claim made while it was down.
func (c *Checker) Run(ctx context.Context) {
	if c == nil || c.res == nil {
		return
	}

	tick := time.NewTicker(CheckInterval)
	defer tick.Stop()

	for {
		if err := c.Pass(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("domain check pass", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Pass claims the domains that are due and checks each one.
//
// Exported so a test can run exactly one pass rather than starting a loop and
// waiting on a wall clock.
func (c *Checker) Pass(ctx context.Context) error {
	now := c.now()

	q := dbgen.New(c.pool)
	rows, err := q.ClaimDomainsDueForCheck(ctx, dbgen.ClaimDomainsDueForCheckParams{
		DueBefore:  now,
		LeaseUntil: now.Add(checkLease),
		Lim:        checkBatch,
	})
	if err != nil {
		return err
	}

	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.checkOne(ctx, q, row, now)
	}
	return nil
}

// checkOne looks one domain up, records it, and acts on a transition.
//
// Failures are logged and dropped rather than returned. One domain that cannot
// be written is not a reason to abandon the rest of the batch, and every one of
// them will be tried again on its own schedule.
func (c *Checker) checkOne(
	ctx context.Context, q *dbgen.Queries, row dbgen.Domain, now time.Time,
) {
	previous := State(row.State)

	lookup, cancel := context.WithTimeout(ctx, lookupTimeout)
	state, err := Check(lookup, q, c.res, row, now)
	cancel()
	if err != nil {
		c.log.Warn("could not record a domain check",
			slog.String("host", row.Host), slog.String("error", err.Error()))
		return
	}

	if state != previous {
		c.log.Info("custom domain changed state",
			slog.String("host", row.Host),
			slog.String("from", string(previous)),
			slog.String("to", string(state)))
	}

	// Decided from the state the domain is in, not from the fact that it just
	// changed. The difference matters: verified means proven and not yet in the
	// Ingress, so a domain sitting in it is one whose apply has not succeeded
	// yet — including because the last attempt failed. Acting only on the
	// transition would check that domain forever and never retry the apply,
	// which is the bug this replaced in a different shape.
	switch {
	case state == StateVerified:
		c.route(ctx, row)
	case previous == StateRouted && state != StateRouted:
		// Fell out of routing. The Ingress is rebuilt from the routable-hosts
		// query, so re-applying is what drops the host — there is nothing to
		// withdraw by name.
		c.apply(ctx, row, "withdraw")
	}
}

// route records the routing projection as dirty. The app reconciler marks the
// domain routed only after the matching workload key is observable.
func (c *Checker) route(ctx context.Context, row dbgen.Domain) {
	c.apply(ctx, row, "route")
}

// apply asks the router to dirty an app's routing projection.
func (c *Checker) apply(ctx context.Context, row dbgen.Domain, why string) bool {
	if c.router == nil {
		return false
	}
	// Scoped by the domain row's own owner. The checker acts on no principal's
	// behalf, so the row is the only thing that can say who this belongs to.
	if err := c.router.ApplyRouting(ctx, row.OwnerID, row.AppID); err != nil {
		c.log.Warn("could not apply routing for a domain",
			slog.String("host", row.Host),
			slog.String("reason", why),
			slog.String("error", err.Error()))
		return false
	}
	return true
}
