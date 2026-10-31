package web

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// A wrapper's badge is drawn beside its name, as the same chip in the
// sidebar and above the signed-out card, and its note where the dashboard
// introduces the product. Unset, neither leaves a trace.
func TestTheBrandBadgeAndNoteFollowTheSlots(t *testing.T) {
	page := templ.ComponentFunc(func(context.Context, io.Writer) error { return nil })
	render := func(s Slots, c templ.Component) string {
		var b strings.Builder
		if err := Layout(s, c).Render(context.Background(), &b); err != nil {
			t.Fatalf("render: %v", err)
		}
		return b.String()
	}
	chip := regexp.MustCompile(`<span class="brand-badge[^"]*">Preview</span>`)
	base := Slots{BrandName: "acme", BrandHref: "/", BrandMark: templ.NopComponent}

	with := base
	with.BrandBadge, with.ProductNote = "Preview", "Acme is new: tell us what you need."
	sidebar := regexp.MustCompile(`(?s)<a[^>]*class="brand".*?</a>`).FindString(render(with, page))
	if !chip.MatchString(sidebar) {
		t.Errorf("the sidebar's brand has no badge: %s", sidebar)
	}
	bare := with
	bare.Bare = true
	signedOut := render(bare, page)
	if lockup := regexp.MustCompile(`(?s)<a[^>]*class="auth-brand".*?</a>`).FindString(signedOut); !chip.MatchString(lockup) {
		t.Errorf("the signed-out brand has no badge: %s", lockup)
	}
	if !strings.Contains(signedOut, `<p class="product-note">Acme is new: tell us what you need.</p>`) {
		t.Error("the signed-out page does not say the product's note")
	}
	var home strings.Builder
	if err := WorkspaceHome(WorkspaceHomeData{Sort: sortRecent, View: viewGrid, Note: with.ProductNote}).
		Render(context.Background(), &home); err != nil {
		t.Fatalf("render the workspace home: %v", err)
	}
	if !strings.Contains(home.String(), "Acme is new: tell us what you need.") {
		t.Error("a new team's empty projects page does not say the product's note")
	}

	without := base
	without.Bare = true
	for _, html := range []string{render(base, page), render(without, page)} {
		if strings.Contains(html, "brand-badge") || strings.Contains(html, "product-note") {
			t.Error("a badge or note is drawn though the wrapper set none")
		}
	}
}
