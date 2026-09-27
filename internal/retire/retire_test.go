package retire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// cluster is an in-memory cluster that answers the retirer's questions the way
// the Kubernetes implementation does, and lets a test finish a rollout.
type cluster struct {
	*orchestrator.Noop

	node     string
	cordoned bool
	since    time.Time
	apps     []*app
	evict    []orchestrator.PodRef
	pinned   []orchestrator.PinnedPod
	room     orchestrator.RetireRoom

	// calls records every write, in order, so a test can assert what was done
	// and what was not.
	calls []string
}

type app struct {
	name      string
	podsHere  int
	token     string
	rolledOut bool
	stuck     string
}

func newCluster(apps ...string) *cluster {
	c := &cluster{Noop: orchestrator.NewNoop(), node: "old", room: roomy()}
	for _, name := range apps {
		c.apps = append(c.apps, &app{name: name, podsHere: 1, rolledOut: true})
	}
	return c
}

func roomy() orchestrator.RetireRoom {
	return orchestrator.RetireRoom{NeedCPUMillis: 500, FreeCPUMillis: 4000, NeedMemBytes: 1 << 30, FreeMemBytes: 8 << 30}
}

func (c *cluster) Nodes(context.Context) ([]orchestrator.NodeInfo, error) {
	return []orchestrator.NodeInfo{{Name: c.node, Ready: true, Unschedulable: c.cordoned, RetiringSince: c.since}}, nil
}

func (c *cluster) Cordon(_ context.Context, _ string, on bool) error {
	c.cordoned = on
	c.calls = append(c.calls, "cordon:"+boolWord(on))
	return nil
}

func (c *cluster) Drain(context.Context, string) (int, error) {
	c.calls = append(c.calls, "drain")
	return 0, nil
}

func (c *cluster) DeleteNode(context.Context, string) error { return nil }

func (c *cluster) RetirePlan(_ context.Context, node string) (orchestrator.RetirePlan, error) {
	plan := orchestrator.RetirePlan{
		Node: node, Since: c.since, Cordoned: c.cordoned,
		Evict: c.evict, Pinned: c.pinned, Room: c.room,
	}
	token := plan.Token()
	for _, a := range c.apps {
		restarted := token != "" && a.token == token
		if a.podsHere == 0 && !restarted {
			continue
		}
		plan.Apps = append(plan.Apps, orchestrator.RetireApp{
			Namespace: "yacht-x", Name: a.name, PodsHere: a.podsHere, Rolling: true,
			Restarted: restarted, RolledOut: a.rolledOut, Stuck: a.stuck,
		})
	}
	return plan, nil
}

func (c *cluster) MarkRetiring(_ context.Context, _ string, since time.Time) error {
	c.since = since
	c.calls = append(c.calls, "mark")
	return nil
}

func (c *cluster) RestartApp(_ context.Context, _, name, token string) error {
	for _, a := range c.apps {
		if a.name == name {
			// The same token is the same template: no new rollout.
			if a.token != token {
				a.token, a.rolledOut = token, false
			}
			c.calls = append(c.calls, "restart:"+name)
			return nil
		}
	}
	return orchestrator.ErrNotFound
}

func (c *cluster) EvictPod(_ context.Context, _, name string) error {
	c.calls = append(c.calls, "evict:"+name)
	c.evict = slices.DeleteFunc(c.evict, func(p orchestrator.PodRef) bool { return p.Name == name })
	return nil
}

// finish completes an app's rollout: its replacement is ready elsewhere and
// the original has gone.
func (c *cluster) finish(name string) {
	for _, a := range c.apps {
		if a.name == name {
			a.podsHere, a.rolledOut = 0, true
		}
	}
}

func (c *cluster) wrote(prefix string) []string {
	var out []string
	for _, call := range c.calls {
		if len(call) >= len(prefix) && call[:len(prefix)] == prefix {
			out = append(out, call)
		}
	}
	return out
}

func boolWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func retirer(t *testing.T, c *cluster) *Retirer {
	t.Helper()
	r, ok := For(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !ok {
		t.Fatal("the in-memory cluster should support retiring")
	}
	r.now = func() time.Time { return time.Unix(1_790_000_000, 0) }
	return r
}

// The central promise: apps move by rolling restart, one at a time, and are
// never evicted.
func TestAppsMoveOneAtATimeByRestartNotEviction(t *testing.T) {
	ctx := context.Background()
	c := newCluster("api", "web")
	r := retirer(t, c)

	if _, err := r.Begin(ctx, "old"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := c.wrote("restart:"); !slices.Equal(got, []string{"restart:api"}) {
		t.Fatalf("restarts after beginning = %v, want only the first app", got)
	}

	// While api's rollout is in flight, nothing else starts.
	if _, err := r.Step(ctx, "old"); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if got := c.wrote("restart:"); len(got) != 1 {
		t.Fatalf("restarts while one is in flight = %v, want still one", got)
	}

	c.finish("api")
	if _, err := r.Step(ctx, "old"); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if got := c.wrote("restart:"); !slices.Equal(got, []string{"restart:api", "restart:web"}) {
		t.Fatalf("restarts = %v, want web after api finished", got)
	}
	if got := c.wrote("evict:"); len(got) != 0 {
		t.Errorf("evictions = %v; an app is moved by restart, never evicted", got)
	}
	if got := c.wrote("drain"); len(got) != 0 {
		t.Error("a retirement fell back to draining")
	}
}

// Closed to work before anything is restarted, or the replacement could be
// scheduled straight back onto the machine being emptied.
func TestTheNodeIsCordonedBeforeAnythingMoves(t *testing.T) {
	c := newCluster("web")
	if _, err := retirer(t, c).Begin(context.Background(), "old"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if len(c.calls) < 3 || c.calls[0] != "cordon:on" || c.calls[1] != "mark" {
		t.Errorf("calls = %v, want cordon, then mark, then restarts", c.calls)
	}
}

// Refused with the shortfall, and nothing touched: a half-started retirement
// that ran out of room would leave apps on a node taking no work.
func TestNoRoomRefusesBeforeTouchingAnything(t *testing.T) {
	c := newCluster("web")
	c.room = orchestrator.RetireRoom{NeedCPUMillis: 2000, FreeCPUMillis: 500}

	_, err := retirer(t, c).Begin(context.Background(), "old")

	var noRoom *NoRoomError
	if !errors.As(err, &noRoom) {
		t.Fatalf("err = %v, want a NoRoomError", err)
	}
	if noRoom.Room.ShortCPUMillis() != 1500 {
		t.Errorf("shortfall = %dm, want 1500m", noRoom.Room.ShortCPUMillis())
	}
	if len(c.calls) != 0 {
		t.Errorf("calls = %v, want nothing done when there is no room", c.calls)
	}
}

// The process driving a retirement can restart at any point. A fresh Retirer
// reads where things are from the cluster and neither restarts what is in
// flight nor forgets what has moved.
func TestAResumedRetirementCarriesOnWhereItWas(t *testing.T) {
	ctx := context.Background()
	c := newCluster("api", "web")
	if _, err := retirer(t, c).Begin(ctx, "old"); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// The engine restarts mid-rollout.
	resumed := retirer(t, c)
	resumed.now = func() time.Time { return time.Unix(1_790_009_999, 0) }
	resumed.pass(ctx)
	if got := c.wrote("restart:"); len(got) != 1 {
		t.Fatalf("restarts after resuming = %v; the in-flight app was restarted again", got)
	}

	// Beginning again from a reloaded page is the same as a step, not a
	// second retirement with a new token.
	if _, err := resumed.Begin(ctx, "old"); err != nil {
		t.Fatalf("Begin again: %v", err)
	}
	if !c.since.Equal(time.Unix(1_790_000_000, 0)) {
		t.Errorf("since = %v; beginning again restarted the retirement", c.since)
	}

	c.finish("api")
	resumed.pass(ctx)
	plan, _ := resumed.Plan(ctx, "old")
	if plan.Moved() != 1 || len(plan.Apps) != 2 {
		t.Errorf("after resuming: %d of %d moved, want 1 of 2", plan.Moved(), len(plan.Apps))
	}
	if got := c.wrote("restart:"); !slices.Equal(got, []string{"restart:api", "restart:web"}) {
		t.Errorf("restarts = %v, want api once, then web", got)
	}
}

// What Yacht does not manage is evicted, and only once the apps have gone.
func TestTheRestIsEvictedOnlyAfterTheApps(t *testing.T) {
	ctx := context.Background()
	c := newCluster("web")
	c.evict = []orchestrator.PodRef{{Namespace: "kube-system", Name: "coredns-1"}}
	r := retirer(t, c)

	if _, err := r.Begin(ctx, "old"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := c.wrote("evict:"); len(got) != 0 {
		t.Fatalf("evicted %v while an app was still moving", got)
	}
	c.finish("web")
	if _, err := r.Step(ctx, "old"); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if got := c.wrote("evict:"); !slices.Equal(got, []string{"evict:coredns-1"}) {
		t.Errorf("evictions = %v, want the unmanaged pod", got)
	}
	plan, _ := r.Plan(ctx, "old")
	if !plan.Empty() {
		t.Error("a node with everything moved is not reported empty")
	}
}

// Pods held by storage are never touched, and do not stop the node from being
// reported as retired down to what stays.
func TestPinnedPodsAreLeftAlone(t *testing.T) {
	ctx := context.Background()
	c := newCluster()
	c.pinned = []orchestrator.PinnedPod{{Namespace: "yacht-x", Name: "db-0", App: "db", SizeBytes: 10 << 30}}
	r := retirer(t, c)

	plan, err := r.Begin(ctx, "old")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := append(c.wrote("restart:"), c.wrote("evict:")...); len(got) != 0 {
		t.Errorf("writes = %v, want a pinned pod neither restarted nor evicted", got)
	}
	if !plan.Empty() {
		t.Error("a node holding only a pinned pod is not offered for removal")
	}
}

// A rollout that gave up is not waited on forever. Its original keeps serving
// on this node — nothing stops before its replacement is ready — and the
// retirement carries on with the next app.
func TestAStuckAppDoesNotHoldUpTheOthers(t *testing.T) {
	ctx := context.Background()
	c := newCluster("api", "web")
	r := retirer(t, c)
	if _, err := r.Begin(ctx, "old"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	c.apps[0].stuck = "ReplicaSet \"api-2\" has timed out progressing."

	if _, err := r.Step(ctx, "old"); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if got := c.wrote("restart:"); !slices.Equal(got, []string{"restart:api", "restart:web"}) {
		t.Errorf("restarts = %v, want web started once api gave up", got)
	}
}

// Somebody reopened the node with kubectl. It is closed again before the next
// app moves, or the replacement could land right back here.
func TestAReopenedNodeIsClosedAgainBeforeMoving(t *testing.T) {
	ctx := context.Background()
	c := newCluster("web")
	c.since = time.Unix(1_790_000_000, 0)

	if _, err := retirer(t, c).Step(ctx, "old"); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(c.calls) < 2 || c.calls[0] != "cordon:on" || c.calls[1] != "restart:web" {
		t.Errorf("calls = %v, want cordon before the restart", c.calls)
	}
}

// Stopping reopens the node and clears the mark, so the background loop stops
// moving things.
func TestStoppingClearsTheMarkAndReopens(t *testing.T) {
	ctx := context.Background()
	c := newCluster("web")
	r := retirer(t, c)
	if _, err := r.Begin(ctx, "old"); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := r.Stop(ctx, "old"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !c.since.IsZero() || c.cordoned {
		t.Errorf("since = %v, cordoned = %v; want neither", c.since, c.cordoned)
	}
	before := len(c.calls)
	r.pass(ctx)
	if len(c.calls) != before {
		t.Errorf("the loop still acted on a stopped retirement: %v", c.calls[before:])
	}
}
