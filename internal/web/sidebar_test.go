package web

import (
	"context"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/identity"
)

// render returns the HTML a slot component produces.
func render(t *testing.T, s Slots) string {
	t.Helper()
	if s.SidebarFooter == nil {
		return ""
	}
	var b strings.Builder
	if err := s.SidebarFooter.Render(context.Background(), &b); err != nil {
		t.Fatalf("render footer: %v", err)
	}
	return b.String()
}

// footerSlots builds the chrome for a request carrying owner and teams.
func footerSlots(owner *identity.Owner, teams []TeamChoice) Slots {
	ctx := context.Background()
	if owner != nil {
		ctx = identity.NewContext(ctx, *owner)
	}
	if teams != nil {
		ctx = context.WithValue(ctx, teamsKey{}, teamLister(func() []TeamChoice {
			return teams
		}))
	}
	return DefaultSlots{}.Slots(ctx, httptest.NewRequest("GET", "/", nil))
}

// TestSidebarFooterNeedsASession.
//
// The sign-in page renders the same chrome as every other page, with no owner
// on the context. A footer there would offer to sign out of a session that
// does not exist — and reading the owner with MustFromContext would panic the
// sign-in page outright, which is the failure this guards.
func TestSidebarFooterNeedsASession(t *testing.T) {
	if got := footerSlots(nil, nil).SidebarFooter; got != nil {
		t.Fatal("footer rendered for a request with no owner")
	}
}

// TestSidebarFooterShowsTheSignedInOwner.
func TestSidebarFooterShowsTheSignedInOwner(t *testing.T) {
	html := render(t, footerSlots(
		&identity.Owner{ID: "u1", DisplayName: "Ada Lovelace", Email: "ada@example.com"},
		[]TeamChoice{{ID: "t1", Name: "Local", Active: true}},
	))

	for _, want := range []string{
		"Ada Lovelace",
		"ada@example.com",
		"AL", // the initials avatar
		`action="/sign-out"`,
		"/sign-out-everywhere",
		`href="/settings"`,
		`name="sidebar-menu"`, // shares the switcher's exclusive group
		"switcher",            // the bordered control, not a bare nav row
	} {
		if !strings.Contains(html, want) {
			t.Errorf("footer is missing %q\n%s", want, html)
		}
	}
}

// TestSidebarFooterHidesSignOutWithoutAccounts.
//
// A token-authenticated install resolves an owner but routes no /sign-out at
// all, so the buttons would post into a 404. The owner is still worth showing;
// the actions are not.
func TestSidebarFooterHidesSignOutWithoutAccounts(t *testing.T) {
	html := render(t, footerSlots(
		&identity.Owner{ID: identity.DefaultOwnerID, DisplayName: "Local"},
		nil,
	))

	if !strings.Contains(html, "Local") {
		t.Errorf("footer does not name the owner\n%s", html)
	}
	if strings.Contains(html, "sign-out") {
		t.Errorf("footer offers sign-out on an install that does not route it\n%s", html)
	}
}

// TestInitials.
func TestInitials(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Ada Lovelace", "AL"},
		{"Local", "L"},
		{"ada@example.com", "A"},
		{"  spaced   out  ", "SO"},
		{"", "?"},
		// Runes, not bytes: slicing this one by byte yields half a character.
		{"Ötzi", "Ö"},
	} {
		if got := initials(c.in); got != c.want {
			t.Errorf("initials(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestOwnerLabelFallsBackToEmail.
//
// An identity provider may resolve an owner that has only ever proved an email
// address, and a blank row beside an avatar reads as a bug.
func TestOwnerLabelFallsBackToEmail(t *testing.T) {
	for _, c := range []struct {
		owner identity.Owner
		want  string
	}{
		{identity.Owner{DisplayName: "Ada", Email: "ada@example.com"}, "Ada"},
		{identity.Owner{Email: "ada@example.com"}, "ada@example.com"},
		{identity.Owner{}, "Account"},
	} {
		if got := ownerLabel(c.owner); got != c.want {
			t.Errorf("ownerLabel(%+v) = %q, want %q", c.owner, got, c.want)
		}
	}
}

// Below md the sidebar is a drawer, and the button that opens it is the only
// way to reach another page.
//
// This shipped as `hidden md:flex` on the aside and `hidden md:inline-flex` on
// the toggle, which on a phone is a dashboard you can navigate into and not out
// of. Both are asserted here because hiding either one alone recreates it.
func TestOnAPhoneThereIsStillAWayOut(t *testing.T) {
	page := renderToString(t, Layout(
		Slots{Title: "t", BrandName: "Yacht", BrandHref: "/", Nav: []NavGroup{{
			Items: []NavItem{{Label: "Projects", Href: "/projects"}},
		}}},
		templ.ComponentFunc(func(context.Context, io.Writer) error { return nil }),
	))

	aside := regexp.MustCompile(`<aside[^>]*>`).FindString(page)
	if aside == "" {
		t.Fatal("no sidebar in the layout")
	}
	if strings.Contains(aside, "hidden") {
		t.Errorf("the sidebar hides itself in markup, so no breakpoint can bring it back: %s", aside)
	}

	toggle := regexp.MustCompile(`<button[^>]*data-sidebar-toggle[^>]*>`).FindString(page)
	if toggle == "" {
		t.Fatal("no sidebar toggle in the layout")
	}
	if strings.Contains(toggle, "hidden") {
		t.Errorf("the toggle is hidden, so a phone has no way to open the drawer: %s", toggle)
	}

	// Tapping away is how a drawer is dismissed, so something has to be there
	// to tap.
	if !strings.Contains(page, "nav-backdrop") {
		t.Error("the drawer has no backdrop to dismiss it")
	}
}

// The drawer must not be remembered. Every link is a full page load, so a
// drawer that persisted would reopen itself over the page you just chose.
func TestTheDrawerIsNotPersisted(t *testing.T) {
	page := renderToString(t, Layout(Slots{Title: "t"},
		templ.ComponentFunc(func(context.Context, io.Writer) error { return nil })))

	script := regexp.MustCompile(`(?s)<script nonce="">.*?</script>`).FindAllString(page, -1)
	var sidebar string
	for _, s := range script {
		if strings.Contains(s, "yachtSidebarToggle") {
			sidebar = s
		}
	}
	if sidebar == "" {
		t.Fatal("no sidebar script in the layout")
	}
	// The rail's collapsed state is stored; the drawer's is not. If data-nav
	// ever reaches localStorage, that distinction has been lost.
	if regexp.MustCompile(`setItem\([^)]*data-nav`).MatchString(sidebar) ||
		strings.Contains(sidebar, `"yacht-nav"`) {
		t.Error("the drawer state is being stored, so it will reopen over the next page")
	}
	if !strings.Contains(sidebar, "yacht-sidebar") {
		t.Error("the desktop rail no longer remembers being collapsed")
	}
}

// Every nav link carries its own name.
//
// It used to come from the visible label, which the collapsed rail hides, and
// the tooltip beside it, which is display:none until hovered. In the rail that
// left every link with no accessible name at all — a screen reader read "link"
// and nothing else, on the only navigation the page has.
//
// The name is on the anchor because that is the one place true in both states.
// The tooltip is marked decorative: it is a visual affordance for the rail, and
// counted as a name it would say everything twice.
func TestEveryNavLinkIsNamedInBothStates(t *testing.T) {
	page := renderToString(t, Layout(
		Slots{Title: "t", BrandName: "Yacht", BrandHref: "/", Nav: []NavGroup{{
			Items: []NavItem{
				{Label: "Projects", Href: "/projects", Icon: "grid"},
				{Label: "Nodes", Href: "/cluster/nodes", Icon: "server"},
			},
		}}},
		templ.ComponentFunc(func(context.Context, io.Writer) error { return nil }),
	))

	links := regexp.MustCompile(`<a[^>]*class="nav-item[^"]*"[^>]*>`).FindAllString(page, -1)
	if len(links) != 2 {
		t.Fatalf("found %d nav links, want 2", len(links))
	}
	for _, l := range links {
		if !strings.Contains(l, "aria-label=") {
			t.Errorf("a nav link has no name of its own, so the collapsed rail leaves it unnamed: %s", l)
		}
	}

	// Nothing may be named by a tooltip that is hidden until hover.
	for _, tip := range regexp.MustCompile(`<span class="nav-tip"[^>]*>`).FindAllString(page, -1) {
		if !strings.Contains(tip, `aria-hidden="true"`) {
			t.Errorf("a tooltip is exposed as a name and will be read twice: %s", tip)
		}
	}
}
