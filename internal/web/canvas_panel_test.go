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
// the canvas-panel.js the dashboard serves on the cards /projects/default
// renders, answering its request with what /apps/web serves, and checks
// that the click stays on the page, takes only the sheet, leaves the canvas
// element alone, and that back closes the panel again.
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
	const logs = "/apps/api/deployments/00000000-0000-0000-0000-000000000001/logs"
	type scenario struct {
		Click    string   `json:"click"`
		Cards    []string `json:"cards,omitempty"`
		Modifier string   `json:"modifier,omitempty"`
		Fail     bool     `json:"fail,omitempty"`
		Redirect string   `json:"redirect,omitempty"`
	}
	page, err := json.Marshal(map[string]any{
		"at":     "/projects/default",
		"cards":  cards,
		"panels": map[string]string{"/apps/web": get(t, h, "/apps/web").Body.String()},
		"scenarios": map[string]scenario{
			"click":      {Click: "/apps/web"},
			"modified":   {Click: "/apps/web", Modifier: "metaKey"},
			"logs":       {Click: logs, Cards: []string{"/apps/web", logs}},
			"failed":     {Click: "/apps/web", Fail: true},
			"redirected": {Click: "/apps/web", Redirect: "/login"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pageFile := filepath.Join(t.TempDir(), "page.json")
	if err := os.WriteFile(pageFile, page, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "testdata/canvas_panel_harness.js", "assets/js/canvas-panel.js", pageFile).CombinedOutput()
	if err != nil {
		t.Fatalf("the harness failed: %v\n%s", err, out)
	}
	type report struct {
		Followed       bool
		Fetched        []string
		Pushed         []string
		Assigned       []string
		Processed      int
		KeepView       *string
		Sheet          *string
		SheetAfterLogs bool
		Open           []string
		Title          string
		CanvasTouched  int
		Back           *struct {
			Closed int
			Open   int
			Title  string
		}
	}
	var got map[string]report
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	click := got["click"]
	if click.Followed {
		t.Error("a card click loaded a new page: the canvas is drawn again under it")
	}
	if !reflect.DeepEqual(click.Fetched, []string{"/apps/web"}) || !reflect.DeepEqual(click.Pushed, []string{"/apps/web"}) {
		t.Errorf("fetched %v and pushed %v, want the panel's address for both", click.Fetched, click.Pushed)
	}
	if len(click.Assigned) != 0 {
		t.Errorf("fell back to a page load: %v", click.Assigned)
	}
	if click.Sheet == nil || *click.Sheet != "response" || !click.SheetAfterLogs {
		t.Errorf("the response's sheet is not on the page where the server renders it (sheet=%v, after the logs sheet=%v)", click.Sheet, click.SheetAfterLogs)
	}
	if click.Processed != 1 {
		t.Errorf("htmx processed the new sheet %d times, want once: its deployments poll would not run", click.Processed)
	}
	if !reflect.DeepEqual(click.Open, []string{"/apps/web"}) {
		t.Errorf("cards marked open: %v, want /apps/web", click.Open)
	}
	if !strings.HasPrefix(click.Title, "web ·") {
		t.Errorf("title %q, want the panel's", click.Title)
	}
	if click.CanvasTouched != 0 {
		t.Errorf("the canvas element was changed %d times", click.CanvasTouched)
	}
	if b := click.Back; b == nil || b.Closed != 1 || b.Open != 0 || b.Title != "Default · Yacht" {
		t.Errorf("back to the canvas: %+v, want the panel closed, no card open and the canvas's title", b)
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
