package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// A card opens its service's panel over the canvas without leaving it. As a
// link it loaded /apps/<name> as a new page: the drawing was built again,
// and canvas.js, finding a card open, slid the view to it, so the canvas
// jumped under the click (612px on a pan the QA run measured). This runs
// the canvas-panel.js and templUI dialog script the dashboard serves, on the
// cards and sheets /projects/default renders, answering its requests with
// what /apps/web, /apps/api and /apps/api/variables serve, and checks that
// the click stays on the page and leaves the canvas element alone.
//
// And that it moves as a sheet should. The sheet opens at the click, with
// the panel in outline, starting from closed so it slides in rather than
// appearing; the panel takes the outline's place in the same open sheet when
// it arrives; another service or a tab changes what the sheet holds without
// closing it, in outline only once the answer is slow; Close runs the exit
// and only then closes the dialog, which is what takes it out of the tab
// order (#65); and back closes it the same way.
//
// That the view itself does not move is canvas.js's, which needs JointJS
// and a layout engine; that was measured in a browser (see the PR).
func TestACardOpensItsPanelWithoutLeavingTheCanvas(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not on PATH, and CI must run the canvas panel script")
		}
		t.Skip("node is not on PATH")
	}
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"), sampleApp("owner-1", "api"))})

	doc, err := html.Parse(strings.NewReader(get(t, h, "/projects/default").Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	cards := cardHrefs(doc)
	if len(cards) != 2 {
		t.Fatalf("the canvas renders %d cards, want 2: %v", len(cards), cards)
	}
	var after []*dialogNode
	for _, id := range []string{"deploy-logs-sheet", "canvas-sheet-skeleton"} {
		n := elementByID(doc, id)
		if n == nil {
			t.Fatalf("the canvas renders no #%s", id)
		}
		after = append(after, elementTree(n))
	}
	type panel struct {
		Title string      `json:"title"`
		Sheet *dialogNode `json:"sheet"`
	}
	panels := map[string]panel{}
	for _, path := range []string{"/apps/web", "/apps/api", "/apps/api/variables"} {
		d, err := html.Parse(strings.NewReader(get(t, h, path).Body.String()))
		if err != nil {
			t.Fatal(err)
		}
		sheet := elementByID(d, "canvas-sheet")
		if sheet == nil {
			t.Fatalf("%s renders no #canvas-sheet", path)
		}
		panels[path] = panel{Title: textOf(elementByTag(d, "title")), Sheet: elementTree(sheet)}
	}
	page, err := json.Marshal(map[string]any{
		"at":     "/projects/default",
		"title":  textOf(elementByTag(doc, "title")),
		"cards":  cards,
		"after":  after,
		"panels": panels,
	})
	if err != nil {
		t.Fatal(err)
	}
	pageFile := filepath.Join(t.TempDir(), "page.json")
	if err := os.WriteFile(pageFile, page, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "testdata/canvas_panel_harness.js",
		"assets/js/templui/dialog.min.js", "assets/js/canvas-panel.js", pageFile).CombinedOutput()
	if err != nil {
		t.Fatalf("the harness failed: %v\n%s", err, out)
	}
	type step struct {
		Sheet, SheetAfterLogs, Open, FocusInSheet, SameDialog bool
		DataOpen, DataOpenAtShow, Closing, Busy, Label        *string
		Panel, ActiveTab, FocusOnTab                          *string
		Skeletons, BodySkeletons, HeadSkeletons               int
		CardsOpen                                             []string
		Title, Path                                           string
		CanvasTouched                                         int
		Delays                                                []int
	}
	type report struct {
		Error                                  string
		Followed, TabFollowed, MetricsFollowed bool
		Fetched, Pushed, Assigned              []string
		Processed                              int
		KeepView, MetricsKeepView              *string
		Steps                                  map[string]step
	}
	var got map[string]report
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for name, r := range got {
		if r.Error != "" {
			t.Fatalf("%s: the harness failed: %s", name, r.Error)
		}
	}
	str := func(p *string) string {
		if p == nil {
			return "<none>"
		}
		return *p
	}

	open := got["open"]
	if open.Followed {
		t.Error("a card click loaded a new page: the canvas is drawn again under it")
	}

	// The click: the sheet is open before anything has come back.
	s := open.Steps["clicked"]
	if !s.Sheet || !s.SheetAfterLogs {
		t.Errorf("clicked: no sheet where the server renders the panel (sheet=%v, after the logs sheet=%v)", s.Sheet, s.SheetAfterLogs)
	}
	if !s.Open || str(s.DataOpen) != "true" {
		t.Errorf("clicked: the sheet is not open while the panel loads (open=%v, data-tui-dialog-open=%s)", s.Open, str(s.DataOpen))
	}
	if str(s.DataOpenAtShow) != "false" {
		t.Errorf("clicked: the sheet was shown at data-tui-dialog-open=%s: it has no closed state to slide in from", str(s.DataOpenAtShow))
	}
	if s.HeadSkeletons == 0 || s.BodySkeletons == 0 || str(s.Busy) != "true" {
		t.Errorf("clicked: the panel is not in outline while it loads (header %d, body %d skeletons, aria-busy=%s)", s.HeadSkeletons, s.BodySkeletons, str(s.Busy))
	}
	if !s.FocusInSheet {
		t.Error("clicked: focus is not in the open sheet")
	}
	if !reflect.DeepEqual(s.CardsOpen, []string{"/apps/web"}) {
		t.Errorf("clicked: cards marked open %v, want /apps/web", s.CardsOpen)
	}
	if len(open.Pushed) == 0 {
		t.Error("nothing was pushed: the address never became the panel's")
	} else if open.Pushed[0] != "/apps/web" {
		t.Errorf("pushed %v first, want /apps/web", open.Pushed)
	}

	// The answer: the panel in the same sheet, still open.
	s = open.Steps["answered"]
	if !s.SameDialog || !s.Open || s.Skeletons != 0 || str(s.Panel) != "/apps/web" {
		t.Errorf("answered: the panel is not in the open sheet (same dialog=%v, open=%v, skeletons=%d, panel=%s)", s.SameDialog, s.Open, s.Skeletons, str(s.Panel))
	}
	if s.Busy != nil || str(s.Label) != canvasSheetTitle {
		t.Errorf("answered: aria-busy=%s, aria-labelledby=%s; want no aria-busy and the panel's title", str(s.Busy), str(s.Label))
	}
	if !s.FocusInSheet {
		t.Error("answered: focus left the sheet when the outline went")
	}
	if !strings.HasPrefix(s.Title, "web ·") {
		t.Errorf("answered: title %q, want the panel's", s.Title)
	}
	if open.Processed < 1 {
		t.Error("htmx never processed the panel: its deployments poll would not run")
	}

	// Another service: the sheet stays open throughout.
	if s := open.Steps["switching"]; !s.Open || s.Skeletons != 0 || str(s.Panel) != "/apps/web" {
		t.Errorf("switching: before the answer is slow the sheet should keep what it shows (open=%v, skeletons=%d, panel=%s)", s.Open, s.Skeletons, str(s.Panel))
	}
	if s := open.Steps["switchSlow"]; !s.Open || s.HeadSkeletons == 0 || !reflect.DeepEqual(s.Delays, []int{150}) {
		t.Errorf("switchSlow: a slow answer should put the panel in outline in the open sheet (open=%v, header skeletons=%d, timers %v)", s.Open, s.HeadSkeletons, s.Delays)
	}
	if s := open.Steps["switched"]; !s.SameDialog || !s.Open || str(s.Panel) != "/apps/api" || s.Skeletons != 0 {
		t.Errorf("switched: same dialog=%v, open=%v, panel=%s, skeletons=%d; want /apps/api in the sheet that was open", s.SameDialog, s.Open, str(s.Panel), s.Skeletons)
	}

	// A tab.
	if open.TabFollowed {
		t.Error("the Variables tab loaded a new page")
	}
	if s := open.Steps["tabClicked"]; str(s.ActiveTab) != "/apps/api/variables" {
		t.Errorf("tabClicked: the current tab is %s, want the one clicked", str(s.ActiveTab))
	}
	if s := open.Steps["tabSlow"]; s.HeadSkeletons != 0 || s.BodySkeletons == 0 || !s.Open {
		t.Errorf("tabSlow: want only the body in outline under the panel's header (header %d, body %d skeletons, open=%v)", s.HeadSkeletons, s.BodySkeletons, s.Open)
	}
	if s := open.Steps["tabbed"]; !s.SameDialog || !s.Open || s.Skeletons != 0 || str(s.ActiveTab) != "/apps/api/variables" || s.Path != "/apps/api/variables" {
		t.Errorf("tabbed: same dialog=%v, open=%v, skeletons=%d, tab=%s, address=%s", s.SameDialog, s.Open, s.Skeletons, str(s.ActiveTab), s.Path)
	} else if str(s.FocusOnTab) != "/apps/api/variables" {
		t.Errorf("tabbed: focus is on %s, want the tab that was clicked", str(s.FocusOnTab))
	}
	if !open.MetricsFollowed || str(open.MetricsKeepView) != "1" {
		t.Errorf("Metrics: followed=%v, keepView=%s; its scripts start on page load, so it should load the page and keep the view", open.MetricsFollowed, str(open.MetricsKeepView))
	}

	// Close: the exit, then closed.
	if s := open.Steps["closing"]; !s.Open || str(s.Closing) != "true" || str(s.DataOpen) != "false" {
		t.Errorf("closing: open=%v, closing=%s, data-tui-dialog-open=%s; the sheet should still be drawn, sliding out", s.Open, str(s.Closing), str(s.DataOpen))
	}
	if s := open.Steps["closed"]; s.Open || s.Closing != nil || !reflect.DeepEqual(s.Delays, []int{200}) {
		t.Errorf("closed: open=%v, closing=%s, timers %v; want the dialog closed once its 200ms transition is over", s.Open, str(s.Closing), s.Delays)
	} else if len(s.CardsOpen) != 0 || s.Path != "/projects/default" || s.Title != "Default · Yacht" {
		t.Errorf("closed: cards open %v at %s titled %q; want none, at the canvas", s.CardsOpen, s.Path, s.Title)
	}
	if open.Steps["closed"].CanvasTouched != 0 {
		t.Errorf("the canvas element was changed %d times", open.Steps["closed"].CanvasTouched)
	}
	if len(open.Assigned) != 0 {
		t.Errorf("fell back to a page load: %v", open.Assigned)
	}

	back := got["back"]
	if s := back.Steps["closing"]; !s.Open || str(s.Closing) != "true" {
		t.Errorf("back: the sheet closed without its exit (open=%v, closing=%s)", s.Open, str(s.Closing))
	}
	if s := back.Steps["closed"]; s.Open || len(s.CardsOpen) != 0 || s.Title != "Default · Yacht" {
		t.Errorf("back: open=%v, cards open %v, title %q; want the panel closed at the canvas", s.Open, s.CardsOpen, s.Title)
	}
	if !reflect.DeepEqual(back.Pushed, []string{"/apps/web"}) {
		t.Errorf("back: pushed %v; going back is not a new entry", back.Pushed)
	}

	if s := got["instant"].Steps["closed"]; !reflect.DeepEqual(s.Delays, []int{0}) {
		t.Errorf("instant: with no transition the close waited %v", s.Delays)
	}

	for _, name := range []string{"modified", "logs"} {
		if r := got[name]; !r.Followed || len(r.Fetched) != 0 {
			t.Errorf("%s: the click was taken over (followed=%v, fetched=%v); it should be left to the browser", name, r.Followed, r.Fetched)
		}
	}
	// The logs card loads a page after all; canvas.js is told to keep the view.
	if r := got["logs"]; r.KeepView == nil || *r.KeepView != "1" {
		t.Errorf("logs: canvas.js is not told to keep the view on the page it loads")
	}
	for _, name := range []string{"failed", "redirected"} {
		if r := got[name]; !reflect.DeepEqual(r.Assigned, []string{"/apps/web"}) || len(r.Pushed) != 0 {
			t.Errorf("%s: assigned %v and pushed %v; it should load the panel's page instead", name, r.Assigned, r.Pushed)
		}
	}
}

// elementTree is n and every element under it, attributes and all, without
// text: enough for the harness to build the page's own markup.
func elementTree(n *html.Node) *dialogNode {
	el := &dialogNode{Tag: n.Data, Attrs: map[string]string{}}
	for _, a := range n.Attr {
		el.Attrs[a.Key] = a.Val
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			el.Children = append(el.Children, elementTree(c))
		}
	}
	return el
}

func elementByID(n *html.Node, id string) *html.Node {
	return findElement(n, func(n *html.Node) bool {
		for _, a := range n.Attr {
			if a.Key == "id" && a.Val == id {
				return true
			}
		}
		return false
	})
}

func elementByTag(n *html.Node, tag string) *html.Node {
	return findElement(n, func(n *html.Node) bool { return n.Data == tag })
}

func findElement(n *html.Node, ok func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && ok(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findElement(c, ok); f != nil {
			return f
		}
	}
	return nil
}

func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// cardHrefs is where each card on a rendered canvas links.
func cardHrefs(n *html.Node) []string {
	var out []string
	if n.Type == html.ElementNode && n.Data == "a" {
		var href string
		card := false
		for _, a := range n.Attr {
			switch a.Key {
			case "href":
				href = a.Val
			case "class":
				card = strings.Contains(" "+a.Val+" ", " canvas-card ")
			}
		}
		if card {
			out = append(out, href)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, cardHrefs(c)...)
	}
	return out
}
