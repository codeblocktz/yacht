package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// inspectorStub returns fixed cluster inspection data, so page behaviour can be
// tested without a cluster and without the Noop's "there is nothing here"
// answers.
type inspectorStub struct {
	*orchestrator.Noop
	events  []orchestrator.EventInfo
	volumes []orchestrator.VolumeInfo
	err     error
}

func (s inspectorStub) Events(context.Context, int) ([]orchestrator.EventInfo, error) {
	return s.events, s.err
}

func (s inspectorStub) Volumes(context.Context, orchestrator.OwnerID) ([]orchestrator.VolumeInfo, error) {
	return s.volumes, s.err
}

func newInspector() inspectorStub {
	now := time.Now()
	return inspectorStub{
		Noop: orchestrator.NewNoop(),
		events: []orchestrator.EventInfo{
			{
				Namespace: "yacht-a1b2", Type: "Warning", Reason: "FailedScheduling",
				Message: "0/3 nodes are available: insufficient cpu",
				Object:  "Pod/api-5c8b7d94f6-hq2mz",
				Count:   7, LastSeen: now.Add(-4 * time.Minute),
			},
			{
				Namespace: "yacht-c3d4", Type: "Normal", Reason: "Pulled",
				Message: `Successfully pulled image "nginx:alpine"`,
				Object:  "Pod/web-7d9f4b6c85-2xk9p",
				Count:   1, LastSeen: now.Add(-90 * time.Second),
			},
		},
		volumes: []orchestrator.VolumeInfo{
			{
				Name: "data-web-0", Namespace: "yacht-a1b2", Phase: "Bound",
				StorageClass: "local-path", CapacityBytes: 8 << 30,
				RequestBytes: 8 << 30, AccessModes: []string{"RWO"},
				App: "web", CreatedAt: now.Add(-48 * time.Hour),
			},
			{
				Name: "data-api-0", Namespace: "yacht-c3d4", Phase: "Pending",
				StorageClass: "longhorn", RequestBytes: 20 << 30,
				AccessModes: []string{"RWX"}, App: "api",
				CreatedAt: now.Add(-2 * time.Minute),
			},
		},
	}
}

func TestEventsPage(t *testing.T) {
	h := testServer(t, Options{Orchestrator: newInspector()})
	rec := get(t, h, "/cluster/events")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The message is the whole point — "Pending" is not actionable,
	// "insufficient cpu" is.
	for _, want := range []string{
		"FailedScheduling", "insufficient cpu",
		"Pod/api-5c8b7d94f6-hq2mz", "yacht-a1b2",
		// How many times it happened. A count column rather than a "×7" tag
		// since the list became a table, but the number still has to be there:
		// one FailedScheduling is a scheduling delay and seven is a cluster
		// that cannot place the pod.
		">7<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("events page missing %q", want)
		}
	}

	// The warning must be styled as one, and appear before routine chatter.
	if !strings.Contains(body, "status-warn") {
		t.Error("warning event should carry the warning style")
	}
	warnAt := strings.Index(body, "FailedScheduling")
	normalAt := strings.Index(body, "Successfully pulled")
	if warnAt < 0 || normalAt < 0 || warnAt > normalAt {
		t.Error("warnings must be ordered above Normal events")
	}
}

func TestEventsPageEmptyStateExplainsExpiry(t *testing.T) {
	h := testServer(t, Options{})
	body := get(t, h, "/cluster/events").Body.String()

	if !strings.Contains(body, "No events") {
		t.Error("expected an empty state")
	}
	// A blank events page reads as broken unless it says why it might be blank.
	if !strings.Contains(body, "expires events") {
		t.Error("empty state should mention that Kubernetes expires events")
	}
}

func TestVolumesPage(t *testing.T) {
	h := testServer(t, Options{Orchestrator: newInspector()})
	rec := get(t, h, "/cluster/volumes")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"data-web-0", "local-path", "RWO", "Bound",
		"data-api-0", "longhorn", "RWX", "Pending", "8.0 GiB",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("volumes page missing %q", want)
		}
	}
}

// A Pending claim has no capacity yet, so the request is shown and marked as
// such. Reporting the request as capacity would state a size the cluster has
// not actually provided.
func TestVolumeSizePrefersCapacity(t *testing.T) {
	bound := orchestrator.VolumeInfo{CapacityBytes: 8 << 30, RequestBytes: 4 << 30}
	if got := volumeSize(bound); got != "8.0 GiB" {
		t.Errorf("bound size = %q, want 8.0 GiB", got)
	}

	pending := orchestrator.VolumeInfo{RequestBytes: 20 << 30}
	if got := volumeSize(pending); got != "20.0 GiB req" {
		t.Errorf("pending size = %q, want '20.0 GiB req'", got)
	}

	unknown := orchestrator.VolumeInfo{}
	if got := volumeSize(unknown); got != "—" {
		t.Errorf("unknown size = %q, want a dash", got)
	}
}

func TestVolumeClass(t *testing.T) {
	cases := map[string]string{
		"Bound":   "status-ok",
		"Pending": "status-warn",
		"Lost":    "status-err",
		"":        "status-neutral",
	}
	for phase, want := range cases {
		got := volumeClass(orchestrator.VolumeInfo{Phase: phase})
		if got != want {
			t.Errorf("volumeClass(%q) = %q, want %q", phase, got, want)
		}
	}
}

// An unreachable cluster must degrade these pages, not error them out.
func TestInspectionPagesToleratePartialFailure(t *testing.T) {
	broken := inspectorStub{Noop: orchestrator.NewNoop(), err: errors.New("i/o timeout")}
	h := testServer(t, Options{Orchestrator: broken})

	for _, path := range []string{"/cluster/events", "/cluster/volumes"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 with a degraded banner", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "i/o timeout") {
			t.Errorf("GET %s should surface the underlying error", path)
		}
	}
}

func TestActivityPage(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"), sampleApp("owner-1", "api"))
	h := testServer(t, Options{Apps: apps})

	rec := get(t, h, "/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{"web", "api", "3 minutes ago", `href="/apps/web"`} {
		if !strings.Contains(body, want) {
			t.Errorf("activity page missing %q", want)
		}
	}
}

// The cross-app feed is the easiest place to leak another owner's data,
// because it queries by owner rather than resolving a named resource first.
func TestActivityIsScopedToOwner(t *testing.T) {
	apps := newFakeApps(
		sampleApp("owner-1", "mine"),
		sampleApp("owner-2", "theirs"),
	)
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/deployments").Body.String()
	if !strings.Contains(body, "mine") {
		t.Error("own deployment missing")
	}
	if strings.Contains(body, "theirs") {
		t.Error("another owner's deployment leaked into the activity feed")
	}
}

func TestActivityEmptyState(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps()})
	if !strings.Contains(get(t, h, "/deployments").Body.String(), "No deployments yet") {
		t.Error("expected an empty state")
	}
}

// Navigation must reach every page it advertises. A nav item pointing at a
// route that does not exist is a 404 the operator finds, not the developer.
func TestEveryNavTargetResolves(t *testing.T) {
	apps := newFakeApps(sampleApp("owner-1", "web"))
	h := testServer(t, Options{Apps: apps, Orchestrator: newInspector()})

	slots := DefaultSlots{}.Slots(context.Background(),
		httptest.NewRequest(http.MethodGet, "/", nil))

	var checked int
	for _, group := range slots.Nav {
		for _, item := range group.Items {
			if code := get(t, h, item.Href).Code; code != http.StatusOK {
				t.Errorf("nav item %q -> GET %s = %d, want 200",
					item.Label, item.Href, code)
			}
			checked++
		}
	}
	if checked < 7 {
		t.Errorf("only checked %d nav targets; expected the full menu", checked)
	}
}
