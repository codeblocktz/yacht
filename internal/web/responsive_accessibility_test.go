package web

import (
	"context"
	"io"
	"os"
	"path/filepath"
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
			`class="check-field"`,
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
		`.metrics > .metric:nth-child(even)`,
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
