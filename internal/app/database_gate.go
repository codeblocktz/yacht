package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/codeblocktz/yacht/internal/database"
)

// ErrDatabaseBusy is a data browser read refused because the team, or the
// engine as a whole, already has as many running as it allows.
var ErrDatabaseBusy = errors.New(
	"app: the data browser is already busy with other reads for this team — try again in a moment")

// How many data browser reads run at once. Each holds a connection and up to
// a result's worth of somebody's data in the engine's memory, bounded by
// database.Limits; this bounds how many of those there are. Per team, so one
// team's tabs cannot take every slot, and across the engine, so many teams at
// once cannot either. A read waits a moment for a slot before it is refused,
// because a few tabs opened together is ordinary and should not be an error.
const (
	browseSlotsPerTeam = 3
	browseSlotsTotal   = 24
	browseSlotWait     = 3 * time.Second
)

// browseSlots is process-wide: it guards the engine's own memory, which every
// request shares however it reached the browser.
var browseSlots = newBrowseGate(browseSlotsPerTeam, browseSlotsTotal, browseSlotWait)

type browseGate struct {
	perTeam int
	wait    time.Duration
	all     chan struct{}

	mu    sync.Mutex
	teams map[string]*teamSlots
}

// teamSlots is one team's semaphore, and how many reads hold or wait on it,
// so it is dropped once nobody does rather than kept for every team that ever
// opened the tab.
type teamSlots struct {
	sem   chan struct{}
	users int
}

func newBrowseGate(perTeam, total int, wait time.Duration) *browseGate {
	return &browseGate{
		perTeam: perTeam, wait: wait,
		all:   make(chan struct{}, total),
		teams: map[string]*teamSlots{},
	}
}

// acquire takes a team slot and an engine slot, waiting up to the gate's
// wait for both, and returns what gives them back.
func (g *browseGate) acquire(ctx context.Context, ownerID string) (func(), error) {
	g.mu.Lock()
	team := g.teams[ownerID]
	if team == nil {
		team = &teamSlots{sem: make(chan struct{}, g.perTeam)}
		g.teams[ownerID] = team
	}
	team.users++
	g.mu.Unlock()
	leave := func() {
		g.mu.Lock()
		if team.users--; team.users == 0 {
			delete(g.teams, ownerID)
		}
		g.mu.Unlock()
	}

	wctx, cancel := context.WithTimeout(ctx, g.wait)
	defer cancel()
	select {
	case team.sem <- struct{}{}:
	case <-wctx.Done():
		leave()
		return nil, g.refusal(ctx)
	}
	select {
	case g.all <- struct{}{}:
	case <-wctx.Done():
		<-team.sem
		leave()
		return nil, g.refusal(ctx)
	}
	return func() {
		<-g.all
		<-team.sem
		leave()
	}, nil
}

// refusal is the request's own ending when it ended, and otherwise busy.
func (g *browseGate) refusal(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrDatabaseBusy
}

// gatedBrowser takes a slot for each read and gives it back after.
type gatedBrowser struct {
	b       database.Browser
	gate    *browseGate
	ownerID string
}

func gated[T any](ctx context.Context, g gatedBrowser, read func() (T, error)) (T, error) {
	release, err := g.gate.acquire(ctx, g.ownerID)
	if err != nil {
		var zero T
		return zero, err
	}
	defer release()
	return read()
}

func (g gatedBrowser) Tables(ctx context.Context) ([]database.Table, error) {
	return gated(ctx, g, func() ([]database.Table, error) { return g.b.Tables(ctx) })
}

func (g gatedBrowser) Rows(ctx context.Context, schema, table string, page int) (database.Result, error) {
	return gated(ctx, g, func() (database.Result, error) { return g.b.Rows(ctx, schema, table, page) })
}

func (g gatedBrowser) Query(ctx context.Context, sql string) (database.Result, error) {
	return gated(ctx, g, func() (database.Result, error) { return g.b.Query(ctx, sql) })
}

func (g gatedBrowser) Stats(ctx context.Context) (database.Stats, error) {
	return gated(ctx, g, func() (database.Stats, error) { return g.b.Stats(ctx) })
}

func (g gatedBrowser) Config(ctx context.Context) (database.Config, error) {
	return gated(ctx, g, func() (database.Config, error) { return g.b.Config(ctx) })
}
