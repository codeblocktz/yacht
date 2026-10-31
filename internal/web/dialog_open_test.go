package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The service's panel is rendered open on /apps/<name> and its tabs, and
// templUI's dialog script is what opens it: its setup marked the dialog
// initialized before it looked for one rendered open, so it never called
// showModal (DEPLOY-15). A display: flex on .canvas-sheet had kept the
// unopened panel drawn anyway, which hid this until #65 took the display out
// so a closed sheet would stop taking focus. Markup alone cannot catch that:
// this runs the script the dashboard serves against the dialogs the page
// renders, on a page load and as an htmx swap, and clicks Close after.
func TestTheServicePanelOpensAndCloses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		// GitHub's runners have node; a laptop without it skips.
		if os.Getenv("CI") != "" {
			t.Fatal("node is not on PATH, and CI must run the dialog script")
		}
		t.Skip("node is not on PATH")
	}
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})

	for _, path := range []string{"/apps/web", "/apps/web/variables", "/apps/web/settings"} {
		doc, err := html.Parse(strings.NewReader(get(t, h, path).Body.String()))
		if err != nil {
			t.Fatal(err)
		}
		tree, err := json.Marshal(dialogTree(doc))
		if err != nil {
			t.Fatal(err)
		}
		treeFile := filepath.Join(t.TempDir(), "dialogs.json")
		if err := os.WriteFile(treeFile, tree, 0o600); err != nil {
			t.Fatal(err)
		}

		for _, mode := range []string{"load", "swap"} {
			out, err := exec.Command(node, "testdata/dialog_harness.js",
				"assets/js/templui/dialog.min.js", treeFile, mode).CombinedOutput()
			if err != nil {
				t.Fatalf("%s (%s): the harness failed: %v\n%s", path, mode, err, out)
			}
			type state struct {
				Open     bool   `json:"open"`
				DataOpen string `json:"dataOpen"`
			}
			var got struct{ Opened, Closed map[string]state }
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("%s (%s): %v: %s", path, mode, err, out)
			}

			if s, ok := got.Opened["canvas-sheet"]; !ok {
				t.Errorf("%s (%s): no canvas-sheet dialog on the page: %s", path, mode, out)
			} else if !s.Open || s.DataOpen != "true" {
				t.Errorf("%s (%s): the service's panel is not open (open=%v, data-tui-dialog-open=%q)", path, mode, s.Open, s.DataOpen)
			}
			// Opening every dialog would pass the check above too.
			if s := got.Opened["deploy-logs-sheet"]; s.Open {
				t.Errorf("%s (%s): the deploy logs sheet opened on its own", path, mode)
			}
			for id, s := range got.Closed {
				if s.Open || s.DataOpen == "true" {
					t.Errorf("%s (%s): %s is still open after its Close button (open=%v, data-tui-dialog-open=%q)", path, mode, id, s.Open, s.DataOpen)
				}
			}
		}
	}
}

// dialogNode is an element in dialogTree's cut-down page.
type dialogNode struct {
	Tag      string            `json:"tag"`
	Attrs    map[string]string `json:"attrs"`
	Children []*dialogNode     `json:"children,omitempty"`
}

// dialogTree keeps only the elements templUI's dialog script looks at, those
// with a data-tui-dialog* attribute, each under the nearest such ancestor, so
// the harness gets the page's own dialogs, triggers and Close buttons without
// a whole DOM to build.
func dialogTree(n *html.Node) []*dialogNode {
	var out []*dialogNode
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		kids := dialogTree(c)
		if c.Type != html.ElementNode || !hasDialogAttr(c) {
			out = append(out, kids...)
			continue
		}
		el := &dialogNode{Tag: c.Data, Attrs: map[string]string{}, Children: kids}
		for _, a := range c.Attr {
			el.Attrs[a.Key] = a.Val
		}
		out = append(out, el)
	}
	return out
}

func hasDialogAttr(n *html.Node) bool {
	for _, a := range n.Attr {
		if strings.HasPrefix(a.Key, "data-tui-dialog") {
			return true
		}
	}
	return false
}
