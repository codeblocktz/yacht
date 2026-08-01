package web

import (
	"strings"
	"testing"

	"github.com/codeblocktz/yacht/internal/app"
)

func node(t *testing.T, d CanvasData, name string) CanvasNode {
	t.Helper()
	for _, n := range d.Nodes {
		if n.App.Name == name {
			return n
		}
	}
	t.Fatalf("no node for %s", name)
	return CanvasNode{}
}

// What an app depends on is drawn above it. That is the direction people read
// a stack in, and getting it upside down makes the picture actively misleading
// rather than merely ugly.
func TestDependenciesAreDrawnAboveTheirDependents(t *testing.T) {
	d := layout(
		[]app.App{{Name: "api"}, {Name: "db"}},
		[]app.Link{{From: "api", To: "db", Via: "DATABASE_URL"}},
	)

	if node(t, d, "db").Y >= node(t, d, "api").Y {
		t.Fatalf("db is at y=%d and api at y=%d — the database must be above what uses it",
			node(t, d, "db").Y, node(t, d, "api").Y)
	}
}

// A chain of three needs three rows, not two.
func TestAChainGetsARowEach(t *testing.T) {
	d := layout(
		[]app.App{{Name: "web"}, {Name: "api"}, {Name: "db"}},
		[]app.Link{
			{From: "web", To: "api", Via: "API_URL"},
			{From: "api", To: "db", Via: "DATABASE_URL"},
		},
	)

	ys := map[string]int{}
	for _, n := range d.Nodes {
		ys[n.App.Name] = n.Y
	}
	if !(ys["db"] < ys["api"] && ys["api"] < ys["web"]) {
		t.Fatalf("rows = %v, want db above api above web", ys)
	}
}

// Two services naming each other is a cycle. The layout must terminate and
// still draw something, rather than recursing until the stack gives out.
func TestACycleStillDraws(t *testing.T) {
	done := make(chan CanvasData, 1)
	go func() {
		done <- layout(
			[]app.App{{Name: "a"}, {Name: "b"}},
			[]app.Link{
				{From: "a", To: "b", Via: "B_URL"},
				{From: "b", To: "a", Via: "A_URL"},
			},
		)
	}()

	select {
	case d := <-done:
		if len(d.Nodes) != 2 {
			t.Fatalf("nodes = %d, want both apps drawn", len(d.Nodes))
		}
	default:
		// layout is synchronous and fast; if it has not finished by the time
		// the goroutine is scheduled, something is looping.
	}
	d := <-done
	if len(d.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(d.Nodes))
	}
}

// An app's storage is drawn attached to it, which is the whole point of
// showing it on the graph rather than only on a tab.
func TestAVolumeIsDrawnAttachedToItsApp(t *testing.T) {
	d := layout([]app.App{{
		Name:    "db",
		Volumes: []app.Volume{{Name: "data", SizeBytes: 2 << 30}},
	}}, nil)

	n := node(t, d, "db")
	if n.Volume == nil {
		t.Fatal("the volume is not on the node")
	}
	if n.Height <= cardH {
		t.Fatal("the card did not grow to hold its volume, so the edge would land wrong")
	}
}

// An edge to an app that is not on the canvas cannot be drawn. Skipping it is
// what keeps a stale link from producing a path to nowhere.
func TestEdgesToUnknownAppsAreSkipped(t *testing.T) {
	d := layout(
		[]app.App{{Name: "api"}},
		[]app.Link{{From: "api", To: "deleted", Via: "OLD_URL"}},
	)
	if len(d.Edges) != 0 {
		t.Fatalf("edges = %d, want none — the target is not on the canvas", len(d.Edges))
	}
}

func TestEdgePathIsWellFormed(t *testing.T) {
	d := layout(
		[]app.App{{Name: "api"}, {Name: "db"}},
		[]app.Link{{From: "api", To: "db", Via: "DATABASE_URL"}},
	)
	if len(d.Edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(d.Edges))
	}
	p := d.Edges[0].Path
	if !strings.HasPrefix(p, "M") || !strings.Contains(p, "V") {
		t.Fatalf("path %q is not an SVG path", p)
	}
	if d.Edges[0].Via != "DATABASE_URL" {
		t.Errorf("the edge does not say which variable made it: %q", d.Edges[0].Via)
	}
}

// A dragged card stays where it was put.
//
// The computed layout still runs first, so an app added later lands somewhere
// sensible rather than at the origin under whatever is already there.
func TestADraggedCardKeepsItsPosition(t *testing.T) {
	x, y := int32(700), int32(410)
	d := layout(
		[]app.App{{Name: "api", X: &x, Y: &y}, {Name: "db"}},
		[]app.Link{{From: "api", To: "db", Via: "DATABASE_URL"}},
	)

	api := node(t, d, "api")
	if api.X != 700 || api.Y != 410 {
		t.Fatalf("api is at (%d,%d), want the position it was dragged to", api.X, api.Y)
	}
	if !api.Pinned {
		t.Error("a card at a stored position is not marked as pinned")
	}
	if node(t, d, "db").Pinned {
		t.Error("a card nobody moved is marked as pinned")
	}
}

// The surface has to reach past the lowest card.
//
// Height used to come from the number of rows, which is right only while every
// card sits in one. A card dragged below them all would hang out of a canvas
// that stopped short, with no way to scroll to it.
func TestTheCanvasReachesPastTheLowestCard(t *testing.T) {
	x, y := int32(40), int32(2000)
	d := layout([]app.App{{Name: "api"}, {Name: "far", X: &x, Y: &y}}, nil)

	if d.Height <= 2000 {
		t.Fatalf("canvas is %dpx tall but a card sits at y=2000", d.Height)
	}
	if d.Width <= 40 {
		t.Fatalf("canvas is %dpx wide but a card sits at x=40", d.Width)
	}
}

// Edges carry the names of the cards they join, so the script can find the
// paths that need re-routing when one of them moves. Without them a drag moves
// the card and leaves its connections pointing at where it used to be.
func TestEdgesNameTheCardsTheyJoin(t *testing.T) {
	d := layout(
		[]app.App{{Name: "api"}, {Name: "db"}},
		[]app.Link{{From: "api", To: "db", Via: "DATABASE_URL"}},
	)

	if len(d.Edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(d.Edges))
	}
	if d.Edges[0].From != "api" || d.Edges[0].To != "db" {
		t.Fatalf("edge joins %q to %q, want api to db", d.Edges[0].From, d.Edges[0].To)
	}
}
