package web

import (
	"net/http"
	"sort"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Canvas geometry. Fixed rather than fluid: the layout is computed on the
// server, and a card whose size the server does not know is a card it cannot
// draw an edge to.
const (
	cardW   = 260
	cardH   = 96
	volumeH = 40
	gapX    = 120
	gapY    = 110
	padX    = 40
	padY    = 40
)

// CanvasNode is one app as the graph draws it.
type CanvasNode struct {
	App    app.App
	X, Y   int
	Height int

	// Volume is the storage drawn attached beneath the card. Only the first is
	// shown: a second would need the card to grow, and no app in this engine
	// has one yet.
	Volume *app.Volume
}

// CanvasEdge is one app's dependency on another.
type CanvasEdge struct {
	// Path is an SVG path: out of the bottom of the dependent, up into the
	// bottom of the dependency. Orthogonal rather than curved, because a
	// straight line through three other cards is harder to follow than a
	// corner.
	Path string
	Via  string
}

// CanvasData is the whole graph.
type CanvasData struct {
	Nodes  []CanvasNode
	Edges  []CanvasEdge
	Width  int
	Height int

	// Empty distinguishes "no apps" from "apps that do not refer to each
	// other" — the second is a normal install, the first needs a prompt.
	Empty bool
}

// canvas draws the apps and what connects them.
func (s *Server) canvas(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	apps, err := s.apps.List(ctx, owner.ID)
	if err != nil {
		s.log.Error("list apps for canvas", "error", err)
		http.Error(w, "could not load apps", http.StatusInternalServerError)
		return
	}

	links, err := s.apps.Links(ctx, owner.ID)
	if err != nil {
		// The graph degrades to a plain arrangement of cards rather than
		// failing: the apps are the point, and the edges are how they relate.
		s.log.Error("list links for canvas", "error", err)
	}

	s.render(w, r, Canvas(layout(apps, links)))
}

// layout places the cards and routes the edges.
//
// Dependencies sit above the things that need them, which is the direction
// people read a stack in: a database at the top, the services that use it
// below. Depth is the longest path to something that depends on nothing, so a
// chain of three lands in three rows rather than two.
func layout(apps []app.App, links []app.Link) CanvasData {
	if len(apps) == 0 {
		return CanvasData{Empty: true, Width: 640, Height: 240}
	}

	byName := make(map[string]app.App, len(apps))
	for _, a := range apps {
		byName[a.Name] = a
	}

	out := make(map[string][]string, len(apps))
	for _, l := range links {
		if _, ok := byName[l.From]; !ok {
			continue
		}
		if _, ok := byName[l.To]; !ok {
			continue
		}
		out[l.From] = append(out[l.From], l.To)
	}

	depth := make(map[string]int, len(apps))
	var resolve func(name string, seen map[string]bool) int
	resolve = func(name string, seen map[string]bool) int {
		if d, ok := depth[name]; ok {
			return d
		}
		// A cycle is possible — two services can name each other — and a
		// recursive depth on one would not terminate. Treating a revisit as
		// depth zero breaks it, and the drawing is still readable.
		if seen[name] {
			return 0
		}
		seen[name] = true

		best := 0
		for _, to := range out[name] {
			if d := resolve(to, seen) + 1; d > best {
				best = d
			}
		}
		delete(seen, name)
		depth[name] = best
		return best
	}

	maxDepth := 0
	for _, a := range apps {
		if d := resolve(a.Name, map[string]bool{}); d > maxDepth {
			maxDepth = d
		}
	}

	// Depth zero depends on nothing, so it goes at the top and whatever needs
	// it is drawn below. Row is the depth itself; inverting it puts the
	// database under the service that uses it, which reads as the opposite
	// relationship.
	rows := make([][]app.App, maxDepth+1)
	for _, a := range apps {
		rows[depth[a.Name]] = append(rows[depth[a.Name]], a)
	}
	for _, r := range rows {
		sort.Slice(r, func(i, j int) bool { return r[i].Name < r[j].Name })
	}

	nodes := make([]CanvasNode, 0, len(apps))
	at := make(map[string]CanvasNode, len(apps))
	width := 0
	for y, row := range rows {
		for x, a := range row {
			n := CanvasNode{
				App:    a,
				X:      padX + x*(cardW+gapX),
				Y:      padY + y*(cardH+volumeH+gapY),
				Height: cardH,
			}
			if len(a.Volumes) > 0 {
				v := a.Volumes[0]
				n.Volume = &v
				n.Height = cardH + volumeH
			}
			nodes = append(nodes, n)
			at[a.Name] = n
			if right := n.X + cardW + padX; right > width {
				width = right
			}
		}
	}

	edges := make([]CanvasEdge, 0, len(links))
	for _, l := range links {
		from, okF := at[l.From]
		to, okT := at[l.To]
		if !okF || !okT || l.From == l.To {
			continue
		}
		edges = append(edges, CanvasEdge{Path: route(from, to), Via: l.Via})
	}

	height := padY
	if len(rows) > 0 {
		height = padY + len(rows)*(cardH+volumeH+gapY)
	}
	return CanvasData{Nodes: nodes, Edges: edges, Width: width, Height: height}
}

// route draws an orthogonal path from the dependent up to the dependency.
//
// Out of the top of the one that needs something, into the bottom of the one
// that provides it — so an arrowhead points at the thing being depended on,
// which is the way the relationship reads aloud.
func route(from, to CanvasNode) string {
	x1 := from.X + cardW/2
	y1 := from.Y
	x2 := to.X + cardW/2
	y2 := to.Y + to.Height

	mid := (y1 + y2) / 2
	return "M" + itoa(x1) + " " + itoa(y1) +
		" V" + itoa(mid) +
		" H" + itoa(x2) +
		" V" + itoa(y2)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
