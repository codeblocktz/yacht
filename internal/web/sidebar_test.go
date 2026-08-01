package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

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
