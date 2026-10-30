package web

import (
	"regexp"
	"strings"
	"testing"
)

// The canvas had no heading at all, the service panel only its H2, the panel's
// dialog no name, and its tabs marked the open one by a class alone, so a
// screen reader could not say which was open. The page now has one H1, the
// project's; the dialog is named by the service's title; and the open tab
// carries aria-current, as the project rail's links already did.
func TestTheCanvasAndPanelAreNavigable(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})

	for path, current := range map[string]string{
		"/apps/web":           "/apps/web",
		"/apps/web/variables": "/apps/web/variables",
		"/apps/web/settings":  "/apps/web/settings",
	} {
		body := get(t, h, path).Body.String()

		if h1 := regexp.MustCompile(`<h1[^>]*>([^<]*)</h1>`).FindAllStringSubmatch(body, -1); len(h1) != 1 || h1[0][1] != fakeProject.Name {
			t.Errorf("%s: headings level one = %v, want one, the project's name %q", path, h1, fakeProject.Name)
		}

		// The open service's sheet, not the page's other dialogs: the
		// confirmation dialog is named, and would pass for it.
		var panel string
		for _, tag := range regexp.MustCompile(`<dialog[^>]*>`).FindAllString(body, -1) {
			if strings.Contains(tag, `class="canvas-sheet `) && strings.Contains(tag, `data-tui-dialog-open="true"`) {
				panel = tag
			}
		}
		label := regexp.MustCompile(`aria-labelledby="([^"]+)"`).FindStringSubmatch(panel)
		if panel == "" || label == nil {
			t.Errorf("%s: the service's panel has no accessible name: %.200s", path, panel)
		} else if !regexp.MustCompile(`<h2 id="` + label[1] + `"[^>]*>`).MatchString(body) {
			t.Errorf("%s: the panel is labelled by %q, which is not the service's title", path, label[1])
		}

		if !strings.Contains(body, `<nav class="tabs canvas-sheet-tabs" aria-label="Service">`) {
			t.Errorf("%s: the panel's tab strip has no name", path)
		}
		marked := regexp.MustCompile(`<a href="([^"]+)" class="tab[^"]*" aria-current="page">`).FindAllStringSubmatch(body, -1)
		if len(marked) != 1 || marked[0][1] != current {
			t.Errorf("%s: tabs marked current = %v, want only %s", path, marked, current)
		}
	}
}
