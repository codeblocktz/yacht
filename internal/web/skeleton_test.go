package web

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/codeblocktz/yacht/internal/app"
)

// Whatever arrives after the page shows its shape while it is on its way:
// skeletons, not a line of "Reading…" text in an empty box or nothing at all,
// so the section neither jumps nor sits blank when the answer lands. Each
// loader says in words what is loading, for a screen reader, and is marked
// busy.
func TestLazySectionsRenderASkeleton(t *testing.T) {
	render := func(c templ.Component) string {
		var b bytes.Buffer
		if err := c.Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	skeletons := regexp.MustCompile(`data-slot="skeleton"`)

	for _, c := range []struct {
		name, html, words string
	}{
		{"metrics charts", render(appMetricsLoader("web")), "Reading metrics…"},
		{"bucket files", render(bucketFiles(AppDetailData{App: sampleApp("owner-1", "files")})), "Reading the bucket…"},
		{"database version", render(databaseReleaseSlot("db")), "Reading the database&#39;s version…"},
		{"repository picker", render(githubRepoPicker(app.GitHubRef{}, "")), "Reading the team&#39;s repositories…"},
		{"deploy logs", render(logSheetSkeleton()), "Loading the logs…"},
	} {
		if n := len(skeletons.FindAllString(c.html, -1)); n == 0 {
			t.Errorf("%s: no skeleton while it loads:\n%s", c.name, c.html)
		}
		if !strings.Contains(c.html, `aria-busy="true"`) {
			t.Errorf("%s: not marked busy while it loads", c.name)
		}
		if !strings.Contains(c.html, `<span class="sr-only">`+c.words+`</span>`) {
			t.Errorf("%s: a screen reader is not told %q:\n%s", c.name, c.words, c.html)
		}
	}

	// A repository already chosen is shown as itself, not in outline.
	if got := render(githubRepoPicker(app.GitHubRef{InstallationID: 7, RepoID: 501}, "acme/web")); skeletons.MatchString(got) || !strings.Contains(got, "acme/web") {
		t.Errorf("a chosen repository is drawn in outline instead of by name:\n%s", got)
	}

	// shadcn's skeleton, and still under reduced motion.
	if got := render(Skeleton("h-4 w-10")); !strings.Contains(got, `class="animate-pulse rounded-md bg-accent motion-reduce:animate-none h-4 w-10"`) {
		t.Errorf("Skeleton renders %s", got)
	}
}

// The canvas carries the service panel's outline for canvas-panel.js to open
// on a click, inert in a <template>, and the deploy logs sheet carries the
// logs' outline, which htmx shows while the logs load: every button that
// fills the sheet names it as its indicator.
func TestTheCanvasCarriesThePanelsOutline(t *testing.T) {
	h := testServer(t, Options{Apps: newFakeApps(sampleApp("owner-1", "web"))})
	body := get(t, h, "/projects/default").Body.String()

	tpl := regexp.MustCompile(`(?s)<template id="canvas-sheet-skeleton">(.*?)</template>`).FindStringSubmatch(body)
	if tpl == nil {
		t.Fatal("the canvas renders no panel outline")
	}
	for _, want := range []string{`data-tui-dialog-content`, `class="canvas-sheet-head`, `canvas-sheet-tabs`, `class="canvas-sheet-body"`, `data-slot="skeleton"`, `aria-labelledby="` + canvasSheetSkeletonTitle + `"`} {
		if !strings.Contains(tpl[1], want) {
			t.Errorf("the panel outline has no %s", want)
		}
	}
	if strings.Contains(tpl[1], `id="canvas-sheet"`) || strings.Contains(tpl[1], `id="`+canvasSheetTitle+`"`) {
		t.Error("the outline reuses the panel's ids, which are the open panel's on a page that has one")
	}
	if !strings.Contains(body, `id="deploy-log-loading"`) {
		t.Error("the deploy logs sheet has no outline for while its logs load")
	}

	panel := get(t, h, "/apps/web").Body.String()
	buttons := regexp.MustCompile(`(?s)<button[^>]*hx-target="#deploy-log-panel"[^>]*>`).FindAllString(panel, -1)
	for _, b := range buttons {
		if !strings.Contains(b, `hx-indicator="#deploy-log-loading"`) {
			t.Errorf("a button that fills the logs sheet shows no outline while it does: %s", b)
		}
	}
}

// How a sheet moves is CSS, and the dialog script times the close by it, so
// the compiled stylesheet is what is checked: shadcn's 200ms fade and 2.5rem
// slide, the overlay's 150ms fade, and no slide under reduced motion — by a
// rule as specific as the slides, since one less specific lost to them and
// the panel slid anyway.
func TestSheetsMoveAsShadcnsDo(t *testing.T) {
	css, err := os.ReadFile("assets/css/app.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	for _, want := range []string{
		`[data-tui-sheet-content]{transition-property:opacity,translate!important;transition-duration:.2s!important;transition-timing-function:cubic-bezier(.4,0,.2,1)!important}`,
		`[data-tui-sheet-content]::backdrop{transition-duration:.15s!important}`,
		`[data-tui-sheet-content][data-tui-dialog-open=false]{opacity:0!important}`,
		`[data-tui-sheet-content][data-tui-sheet-side=right][data-tui-dialog-open=false]{translate:2.5rem!important}`,
		`@media (prefers-reduced-motion:reduce){[data-tui-sheet-content][data-tui-sheet-side][data-tui-dialog-open=false]{translate:0!important}}`,
		`.sheet-loading.htmx-request~#deploy-log-panel{display:none}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.css has no %s", want)
		}
	}
}
