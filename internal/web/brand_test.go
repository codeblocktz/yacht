package web

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderToString(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// The wordmark has to be drawn in currentColor.
//
// Without it the letters take the SVG default, which is black — invisible on
// the dark theme. Nothing errors and the mark beside it still draws, so the
// sidebar looks like it has lost its name rather than like it has a bug.
func TestTheWordmarkFollowsTheTheme(t *testing.T) {
	word := renderToString(t, brandWordmark("x"))

	if !strings.Contains(word, `fill="currentColor"`) {
		t.Error("the wordmark is not drawn in currentColor — it will be black on the dark theme")
	}
	// Every path either carries the mark's own teal or takes currentColor. One
	// with neither renders black wherever it lands.
	for _, m := range regexp.MustCompile(`<path[^>]*>`).FindAllString(word, -1) {
		if strings.Contains(m, "currentColor") || strings.Contains(m, "fill:rgb(") {
			continue
		}
		t.Errorf("path with no colour of its own and no currentColor: %.90s", m)
	}
}

// The mark keeps its own two teals.
//
// It is a logo, not an icon: recolouring it with the text beside it would make
// it a different mark on the dark theme than on the light one. The wordmark is
// the opposite case, which is why they are separate components.
func TestTheMarkKeepsItsOwnColours(t *testing.T) {
	mark := renderToString(t, brandMark("x"))
	for _, want := range []string{"fill:rgb(33,149,132)", "fill:rgb(74,168,154)"} {
		if !strings.Contains(mark, want) {
			t.Errorf("the mark lost %s", want)
		}
	}
	if strings.Contains(mark, "currentColor") {
		t.Error("the mark takes the surrounding text colour — it is a logo, not an icon")
	}
}

// The favicon is a real file this binary carries, not the blank placeholder.
func TestTheFaviconIsTheMark(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/brand/icon.svg")
	if err != nil {
		t.Fatalf("the icon is not embedded: %v", err)
	}
	if !bytes.Contains(b, []byte("rgb(33,149,132)")) {
		t.Error("the embedded icon is not the brand mark")
	}
	// A favicon with no intrinsic size is one a browser has to guess at.
	if !bytes.Contains(b, []byte(`width="215"`)) || !bytes.Contains(b, []byte(`viewBox=`)) {
		t.Error("the icon has no intrinsic size for a browser to draw a tab from")
	}

	page := renderToString(t, Layout(Slots{Title: "t"},
		templ.ComponentFunc(func(context.Context, io.Writer) error { return nil })))
	if strings.Contains(page, `href="data:,"`) {
		t.Error("the layout still ships the blank placeholder favicon")
	}
	if !strings.Contains(page, "/assets/brand/icon.svg") {
		t.Error("the layout does not point at the brand icon")
	}
}

// The link home keeps a name a screen reader can read.
//
// It used to be named by the text it held. Replacing that text with a drawing
// took the name with it — the mark and the wordmark are both marked decorative,
// which is correct for images of a name and useless for the link around them.
func TestTheBrandLinkIsStillNamed(t *testing.T) {
	page := renderToString(t, Layout(Slots{Title: "t", BrandName: "Yacht", BrandHref: "/"},
		templ.ComponentFunc(func(context.Context, io.Writer) error { return nil })))

	brand := regexp.MustCompile(`<a[^>]*class="brand[^"]*"[^>]*>`).FindString(page)
	if brand == "" {
		t.Fatal("no brand link in the layout")
	}
	if !strings.Contains(brand, `aria-label="Yacht"`) {
		t.Errorf("the brand link has no accessible name: %s", brand)
	}
}
