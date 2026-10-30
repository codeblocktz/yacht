package web

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func TestLayoutProvidesSkipNavigationAndDrawerSemantics(t *testing.T) {
	html := renderToString(t, Layout(
		Slots{Title: "Yacht", BrandName: "Yacht", BrandHref: "/"},
		templ.ComponentFunc(func(context.Context, io.Writer) error { return nil }),
	))

	for _, want := range []string{
		`href="#main-content"`,
		`id="main-content"`,
		`aria-label="Primary navigation"`,
		`data-app-shell`,
		`data-nav-backdrop`,
		`data-confirm-dialog`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("layout is missing %q", want)
		}
	}

	for _, behavior := range []string{"previousFocus", "setDrawerInert", "focusDrawer", "trapFocus"} {
		if !strings.Contains(html, behavior) {
			t.Errorf("layout script is missing drawer behavior %q", behavior)
		}
	}
}

func TestResponsiveSurfacesUseSharedStructure(t *testing.T) {
	checks := map[string][]string{
		"team.templ": {
			`class="table-scroll"`,
			`class="flex flex-col gap-4 sm:flex-row sm:items-end"`,
		},
		"pages.templ": {
			`class="table-scroll"`,
			`data-app-switcher`,
			// Was `class="check-field"`, a local class carrying the touch
			// target. The control is templUI's checkbox now and checkField is
			// what pairs it with a label and that minimum height — so the thing
			// worth asserting is still "the shared one, not markup written
			// here", which is what this names.
			`@checkField(`,
		},
	}
	for file, wants := range checks {
		body, err := os.ReadFile(filepath.Join(".", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s is missing %q", file, want)
			}
		}
	}
}

func TestMetricStripDefinesAnIntentionalMobileGrid(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "assets", "css", "input.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(body)
	for _, want := range []string{
		`.metrics > .metric:last-child:nth-child(odd)`,
		`grid-column: 1 / -1`,
		`grid-template-columns: repeat(2, minmax(0, 1fr))`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("mobile metric layout is missing %q", want)
		}
	}
}

func TestDestructiveActionsDoNotUseNativeConfirm(t *testing.T) {
	files, err := filepath.Glob("*.templ")
	if err != nil {
		t.Fatal(err)
	}
	var native []string
	var triggers int
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(body)
		if strings.Contains(src, "confirm('") {
			native = append(native, file)
		}
		triggers += strings.Count(src, "data-confirm=")
	}
	if len(native) > 0 {
		t.Errorf("native confirmation remains in %v", native)
	}
	if triggers < 6 {
		t.Errorf("found %d shared confirmation triggers, want at least 6", triggers)
	}
}

// Shut, the phone drawer and a closed sheet take no focus. Both sat just off
// screen with every link and button in them still in the tab order, so Tab
// walked through things nobody could see.
func TestClosedPanelsAreOutOfTheTabOrder(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "assets", "css", "input.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(src)

	// The drawer: hidden while shut, shown while open, below md.
	from := strings.Index(css, "fixed bottom-0 left-0 top-0 z-50 w-[284px]")
	to := strings.Index(css, ".nav-backdrop {")
	if from < 0 || to < from {
		t.Fatal("no phone drawer rules in input.css")
	}
	phone := css[from:to]
	for _, want := range []string{
		"visibility: hidden;",
		"transition-property: transform, translate, visibility;",
		`:root[data-nav="open"] .sidebar { @apply translate-x-0; visibility: visible; }`,
	} {
		if !strings.Contains(phone, want) {
			t.Errorf("the phone drawer lacks %q", want)
		}
	}

	// The sheet: no display of its own. The rule is outside any layer, so a
	// display there beats the dialog's hidden-while-closed utility. Checked
	// in the compiled stylesheet too, which is what a browser reads.
	sheet := regexp.MustCompile(`(?s)\.canvas-sheet \{(.*?)\}`).FindStringSubmatch(css)
	if sheet == nil {
		t.Fatal("no .canvas-sheet rule in input.css")
	}
	for _, word := range strings.Fields(sheet[1]) {
		if word == "flex" || word == "block" || word == "grid" || strings.HasPrefix(word, "display") {
			t.Errorf(".canvas-sheet sets a display (%s): a closed sheet stays drawn off screen", word)
		}
	}
	built, err := os.ReadFile(filepath.Join("assets", "css", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range regexp.MustCompile(`\.canvas-sheet\{[^}]*\}`).FindAllString(string(built), -1) {
		if strings.Contains(rule, "display:") {
			t.Errorf("the built stylesheet gives .canvas-sheet a display: %s", rule)
		}
	}
}
