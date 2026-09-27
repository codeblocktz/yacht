package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/identity"
)

// A wrapper's chrome, drawn by the engine's sidebar.
//
// Shaped like the hosted product that wraps this engine: it starts from the
// engine's own slots, renames the brand and draws it as a wordmark alone,
// adds an Account group, adds a page to the Admin group by its heading,
// reorders the groups so its customers' come first and Admin last, and puts
// a balance in the header and a notice in the banner. Every one of those has
// to survive a redesign of the sidebar, and none of it needs a database.
func TestAWrappersChromeRendersInTheEngineSidebar(t *testing.T) {
	ctx := identity.NewContext(context.Background(),
		Owner{ID: "org-1", DisplayName: "Customer Co", Email: "ops@customer.test"})
	r := httptest.NewRequest(http.MethodGet, "/billing", nil)

	s := DefaultSlots{}.Slots(ctx, r)
	s.BrandName = "kilicore"
	s.BrandMark = templ.NopComponent
	s.Nav = append(s.Nav, NavGroup{Heading: "Account", Items: []NavItem{
		{Label: "Billing", Href: "/billing", Icon: "key", Active: true},
	}})
	// Not an operator here, so the engine drew no Admin group; the wrapper
	// makes one the way it does when the engine has none.
	s.Nav = append(s.Nav, NavGroup{Heading: AdminNavHeading, Items: []NavItem{
		{Label: "Plans", Href: "/admin/plans", Icon: "key"},
	}})
	s.Nav = customerFirst(s.Nav)
	s.HeaderTools = templ.Raw(`<a class="status status-ok" href="/billing" id="balance">TZS 12,500</a>`)
	s.Banner = templ.Raw(`<div class="callout" id="suspended">This organisation is suspended.</div>`)

	var b strings.Builder
	page := templ.ComponentFunc(func(context.Context, io.Writer) error { return nil })
	if err := Layout(s, page).Render(ctx, &b); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := b.String()

	for _, want := range []string{
		`aria-label="kilicore"`, // the brand link, named for the wrapper
		`>kilicore</span>`,      // drawn as text: no wordmark of the engine's
		`>Account</div>`,        // the wrapper's group, under its own heading
		`href="/billing"`,
		`data-nav-label="Billing"`, // and so a page in the palette
		`data-nav-label="Plans"`,
		`id="balance"`,
		`id="suspended"`,
		`id="palette"`,
		"Customer Co", // the person, at the foot
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the wrapper's chrome is missing %q", want)
		}
	}

	// No Yacht mark where the wrapper asked for none.
	aside := regexp.MustCompile(`(?s)<aside.*?</aside>`).FindString(html)
	brand := regexp.MustCompile(`(?s)<a[^>]*class="brand".*?</a>`).FindString(aside)
	if strings.Contains(brand, "<svg") {
		t.Errorf("the brand draws a mark the wrapper replaced with nothing: %s", brand)
	}

	// Billing is the page being looked at, so it is the active link.
	if !regexp.MustCompile(`<a[^>]*class="nav-item nav-active"[^>]*aria-current="page"[^>]*data-nav-label="Billing"`).MatchString(aside) {
		t.Error("the wrapper's active page is not marked active")
	}

	// Its groups in the order it put them — the engine's primary links, then
	// Account, then System — and Admin last, apart.
	order := []string{`data-nav-label="Overview"`, `>Account</div>`, `>System</div>`, `class="nav-admin"`, `data-nav-label="Plans"`}
	last := -1
	for _, marker := range order {
		i := strings.Index(aside, marker)
		if i < 0 || i < last {
			t.Fatalf("the sidebar is out of order at %s (%d after %d)", marker, i, last)
		}
		last = i
	}
}

// A wrapper that fills SidebarTop replaces the engine's switcher rather than
// drawing beside it: the slot is the seam.
func TestAWrappersSidebarTopReplacesTheSwitcher(t *testing.T) {
	ctx := identity.NewContext(context.Background(), Owner{ID: "org-1", DisplayName: "Customer Co"})
	s := DefaultSlots{}.Slots(ctx, httptest.NewRequest(http.MethodGet, "/", nil))
	s.SidebarTop = templ.Raw(`<div id="org-switcher">Organisations</div>`)

	var b strings.Builder
	if err := Layout(s, templ.NopComponent).Render(ctx, &b); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := b.String()
	if !strings.Contains(html, `id="org-switcher"`) {
		t.Error("the wrapper's switcher is not drawn")
	}
	if strings.Contains(html, "switcher-static") {
		t.Error("the engine's own switcher is drawn beside the wrapper's")
	}
}

// customerFirst is the hosted product's ordering, restated: customers' groups
// first, then the install's.
func customerFirst(nav []NavGroup) []NavGroup {
	var main, account, system, admin, rest []NavGroup
	for _, g := range nav {
		switch g.Heading {
		case "":
			main = append(main, g)
		case "Account":
			account = append(account, g)
		case "System":
			system = append(system, g)
		case AdminNavHeading:
			admin = append(admin, g)
		default:
			rest = append(rest, g)
		}
	}
	out := make([]NavGroup, 0, len(nav))
	for _, part := range [][]NavGroup{main, account, rest, system, admin} {
		out = append(out, part...)
	}
	return out
}
