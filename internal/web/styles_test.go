package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/app"
)

// TestTemplatesOnlyUseClassesThatExist.
//
// A template can name any class it likes and still compile. Naming one the
// stylesheet does not define produces a page with no padding, no borders and
// no alignment — which is what the team page shipped as, because it used
// "table" where the design system has "dtable".
//
// Utility classes come from Tailwind and are generated on demand, so only the
// design system's own names are checked: those are the ones with no generator
// behind them, and the ones a typo silently removes.
func TestTemplatesOnlyUseClassesThatExist(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("..", "..", "assets", "css", "input.css"))
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}

	// Every class named anywhere in a selector, not only those immediately
	// followed by a brace: a rule like ".panel:not(:empty)" or ".a > .b"
	// defines its classes just as much, and missing them makes this test
	// report real classes as typos.
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.([a-z][a-z0-9-]*)`).
		FindAllStringSubmatch(string(css), -1) {
		defined[m[1]] = true
	}
	if len(defined) < 20 {
		t.Fatalf("only %d classes found in the stylesheet — the scan is broken", len(defined))
	}

	// Names that look like design-system classes: a bare word, no colon, no
	// bracket, no slash. Anything with those is a Tailwind utility.
	looksLikeOurs := regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

	// Tailwind's own single-word utilities, which are not in our stylesheet
	// and are not typos.
	tailwind := map[string]bool{
		"flex": true, "grid": true, "block": true, "hidden": true, "inline": true,
		"grow": true, "shrink": true, "truncate": true, "relative": true,
		"absolute": true, "fixed": true, "sticky": true, "border": true,
		"rounded": true, "italic": true, "underline": true, "uppercase": true,
		"capitalize": true, "container": true, "transform": true, "overflow": true,
		"group": true, "peer": true, "sr": true, "antialiased": true,
		"tabular": true, "nums": true, "mono": true, "invisible": true, "visible": true,
		"inline-flex": true, "inline-block": true, "inline-grid": true,
	}

	files, err := filepath.Glob(filepath.Join(".", "*.templ"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}

	var unknown []string
	classAttr := regexp.MustCompile(`class="([^"{]*)"`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range classAttr.FindAllStringSubmatch(string(src), -1) {
			for _, name := range strings.Fields(m[1]) {
				if !looksLikeOurs.MatchString(name) || defined[name] || tailwind[name] {
					continue
				}
				// A Tailwind utility with a known prefix.
				if strings.ContainsAny(name, ":[]/") {
					continue
				}
				if prefixed(name) {
					continue
				}
				unknown = append(unknown, filepath.Base(f)+": "+name)
			}
		}
	}

	sort.Strings(unknown)
	for _, u := range unknown {
		t.Errorf("class is neither in the stylesheet nor a Tailwind utility: %s", u)
	}
}

// prefixed reports whether a name is a Tailwind utility by its prefix.
func prefixed(name string) bool {
	for _, p := range []string{
		"p-", "px-", "py-", "pt-", "pb-", "pl-", "pr-",
		"m-", "mx-", "my-", "mt-", "mb-", "ml-", "mr-",
		"w-", "h-", "min-", "max-", "gap-", "space-",
		"text-", "font-", "bg-", "border-", "rounded-", "shadow-",
		"flex-", "grid-", "col-", "row-", "items-", "justify-", "self-",
		"opacity-", "cursor-", "overflow-", "whitespace-", "break-",
		"top-", "bottom-", "left-", "right-", "z-", "order-", "leading-",
		"tracking-", "align-", "object-", "list-", "divide-", "ring-",
		"transition", "duration-", "ease-", "hover", "focus", "sm", "md", "lg", "xl",
		"tabular-", "shrink-", "size-", "from-", "to-", "via-", "gradient-", "aspect-",
		"inset-", "translate-", "scale-", "rotate-", "fill-", "stroke-",
		"placeholder-", "caret-", "accent-", "select-", "resize-", "appearance-",
		"pointer-", "outline-", "decoration-", "indent-", "content-", "backdrop-",
	} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// TestNoRemoteAssets.
//
// The binary is the whole deployment — that is the claim in assets.go and the
// reason the stylesheet is embedded. A script or stylesheet fetched from a CDN
// breaks it: the dashboard stops working on an air-gapped install, and the
// bytes a browser receives are whatever the CDN serves that day rather than
// the ones that were tested.
//
// htmx shipped that way for a while, loaded from unpkg on every page and used
// by nothing.
func TestNoRemoteAssets(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(".", "*.templ"))
	remote := regexp.MustCompile(`(src|href)=['"]?(https?:)?//`)

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if remote.MatchString(line) && !strings.Contains(line, "rel=\"noreferrer\"") {
				t.Errorf("%s loads an asset from off this server:\n  %s",
					filepath.Base(f), strings.TrimSpace(line))
			}
		}
	}
}

// And the file it now points at has to actually be there, or every page loads
// a 404 and the panel silently never opens.
// Every script the layout asks for must be a file this binary carries.
//
// The failure this catches renders perfectly: a <script src> for a URL nothing
// serves produces no error anywhere, just a component that never responds to a
// click. Checking one known filename would not have caught it — the tag that
// was wrong came from a vendored component, not from a template anyone here
// wrote.
func TestEveryScriptTheLayoutAsksForIsEmbedded(t *testing.T) {
	// Both shapes of the layout: the full-bleed one loads the canvas script and
	// the ordinary one does not, so rendering only the default would leave the
	// script that is hardest to notice missing entirely unchecked.
	var buf bytes.Buffer
	page := templ.ComponentFunc(func(context.Context, io.Writer) error { return nil })
	for _, slots := range []Slots{{Title: "t"}, {Title: "t", FullBleed: true}} {
		if err := Layout(slots, page).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render layout: %v", err)
		}
	}
	if !strings.Contains(buf.String(), "canvas.js") {
		t.Error("the full-bleed layout does not load the canvas script")
	}

	srcs := regexp.MustCompile(`<script[^>]+src="([^"]+)"`).FindAllStringSubmatch(buf.String(), -1)
	if len(srcs) == 0 {
		t.Fatal("the layout loads no scripts at all — this test would pass vacuously")
	}
	for _, m := range srcs {
		// The fingerprint is a query string, not part of the path.
		path := strings.TrimPrefix(strings.SplitN(m[1], "?", 2)[0], "/")
		if _, err := assetsFS.ReadFile(path); err != nil {
			t.Errorf("layout loads %s, which nothing serves: %v", m[1], err)
		}
	}
}

func TestVendoredHtmxIsWholeLibrary(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/js/htmx.min.js")
	if err != nil {
		t.Fatalf("htmx is not embedded: %v", err)
	}
	if len(b) < 10_000 {
		t.Fatalf("htmx is %d bytes — that is not the library", len(b))
	}
}

// Every icon name a template can ask for has a drawing.
//
// A name with no case renders an empty <svg>: correct markup, right size,
// nothing in it. That is how Postgres cards shipped with a blank square where
// their icon should be — sourceIcon returned "storage" and the switch had never
// heard of it.
func TestEveryIconNameHasADrawing(t *testing.T) {
	layout, err := os.ReadFile("layout.templ")
	if err != nil {
		t.Fatalf("read layout.templ: %v", err)
	}
	drawn := map[string]bool{}
	for _, m := range regexp.MustCompile(`case "([a-z-]+)":`).FindAllStringSubmatch(string(layout), -1) {
		drawn[m[1]] = true
	}
	if len(drawn) == 0 {
		t.Fatal("no icon cases found — this test would pass vacuously")
	}

	// Names reached through icon(...) in a template, and names returned by the
	// Go helpers that feed it. The second set is where the blank square came
	// from: nothing in a template mentions "storage".
	asked := map[string]string{}
	files, _ := filepath.Glob("*.templ")
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range regexp.MustCompile(`icon\("([a-z-]+)"\)`).FindAllStringSubmatch(string(src), -1) {
			asked[m[1]] = filepath.Base(f)
		}
	}
	for _, src := range []app.Source{app.SourceImage, app.SourcePostgres, "git", "template"} {
		asked[sourceIcon(src)] = "sourceIcon(" + string(src) + ")"
	}
	for _, group := range (DefaultSlots{}).Slots(context.Background(),
		httptest.NewRequest(http.MethodGet, "/", nil)).Nav {
		for _, item := range group.Items {
			if item.Icon != "" {
				asked[item.Icon] = "nav: " + item.Label
			}
		}
	}

	for name, where := range asked {
		if !drawn[name] {
			t.Errorf("%s asks for the %q icon, which draws nothing", where, name)
		}
	}
}

// The card's rendered height is the height the server laid out with.
//
// Every edge is drawn from cardH and volumeH. When the stylesheet sized cards
// from their content instead, the numbers disagreed by 23px and every arrow
// stopped short of the card it pointed at — which looked like the graph being
// disconnected rather than like a spacing bug.
func TestTheCardGeometryComesFromTheServer(t *testing.T) {
	style := canvasSize(CanvasData{Width: 10, Height: 10})
	for _, want := range []string{
		"--canvas-card-h:" + strconv.Itoa(cardH) + "px",
		"--canvas-volume-h:" + strconv.Itoa(volumeH) + "px",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("the canvas does not publish %s — it is %q", want, style)
		}
	}

	css, err := os.ReadFile(filepath.Join("..", "..", "assets", "css", "input.css"))
	if err != nil {
		t.Fatalf("read input.css: %v", err)
	}
	for _, want := range []string{
		"height: var(--canvas-card-h)",
		"height: var(--canvas-volume-h)",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("the stylesheet does not take %s from the server", want)
		}
	}
}
