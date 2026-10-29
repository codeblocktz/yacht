package engine

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/observe"
)

// EgressUsage is the bytes one app sent to the public internet in a window.
type EgressUsage struct {
	OwnerID string
	// ProjectID is the project the app is drawn on; uuid.Nil for none.
	ProjectID uuid.UUID
	AppID     uuid.UUID
	App       string
	Bytes     int64
}

// ErrEgressUnmeasured is an install that does not measure egress: it has no
// metrics store (YACHT_METRICS_URL). Not zero bytes — nothing was counted —
// so a wrapper charging for egress leaves it off rather than charging none.
var ErrEgressUnmeasured = errors.New("egress is not measured on this install: it has no metrics store (YACHT_METRICS_URL)")

// ErrEgressWindowOpen is a window that ended less than a minute ago, which
// the metrics store may not hold all of yet. Asked again later, it does.
var ErrEgressWindowOpen = observe.ErrWindowOpen

// egressCounter is what Egress reads the bytes from: the metrics store, or a
// test's stand-in for one.
type egressCounter interface {
	Egress(ctx context.Context, from, to time.Time, services []observe.Service) (map[observe.Service]int64, error)
}

// Egress is the bytes each app sent to the public internet in the window
// from..to, for charging by: one row per app that sent any, ordered by owner
// and app name. Apps that sent nothing are left out.
//
// Public egress is the bodies of the responses the install's ingress
// (Traefik) served for an app, on its generated hostname and any custom
// domain, counted by the ingress itself: summed over every ingress replica,
// and indifferent to how many replicas the app runs. Traffic between apps on
// the private network never passes through it and is not counted, nor is the
// platform's own — the dashboard, the waker answering for a sleeping app, the
// buckets' S3 API. Not counted either: what an app sends out on its own
// account, what a TCP proxy carries, and what a bucket serves, which reaches
// the internet through one shared S3 address that nothing counts per bucket.
//
// The window is whole seconds; from and to are truncated to them. Windows
// that meet divide the counters' samples between them — one taken after from
// and at or before to is in the window — so they neither overlap nor leave a
// gap, and twenty-four hours read one at a time are the day read at once. A counter's restart is read
// as a restart, not as traffic gone. Reading the same window again gives the
// same answer, for the apps that still exist: an app deleted since is left
// out, as it is from everything else the meter reads.
//
// A window must have ended at least a minute ago, or ErrEgressWindowOpen. An
// install without a metrics store returns ErrEgressUnmeasured; a store that
// cannot be read, or answers with only part of its data, an error — never
// zero, so a meter tries the window again rather than charging too little.
func (e *Engine) Egress(ctx context.Context, from, to time.Time) ([]EgressUsage, error) {
	if e.egress == nil {
		return nil, ErrEgressUnmeasured
	}
	apps, err := e.Apps.AllNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	services := make([]observe.Service, 0, len(apps))
	for _, a := range apps {
		services = append(services, observe.Service{Name: a.Name, Namespace: a.Namespace})
	}
	sent, err := e.egress.Egress(ctx, from, to, services)
	if err != nil {
		return nil, err
	}
	var out []EgressUsage
	for _, a := range apps {
		n := sent[observe.Service{Name: a.Name, Namespace: a.Namespace}]
		if n <= 0 {
			continue
		}
		out = append(out, EgressUsage{OwnerID: a.OwnerID, ProjectID: a.ProjectID, AppID: a.ID, App: a.Name, Bytes: n})
	}
	// Already in this order from the database; sorted again so the order is
	// this function's promise and not the query's.
	slices.SortStableFunc(out, func(x, y EgressUsage) int {
		return cmp.Or(cmp.Compare(x.OwnerID, y.OwnerID), cmp.Compare(x.App, y.App))
	})
	return out, nil
}
