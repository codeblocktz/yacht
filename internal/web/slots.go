// Package web serves the engine's dashboard.
//
// SEAM 3 of 4, and the one that is easiest to get wrong. The other three seams
// are Go interfaces, which compose naturally. A rendered HTML page does not:
// you cannot wrap a template in middleware. If the layout hard-codes its own
// sidebar and header, an application built on top of the engine has exactly two
// options — fork the templates, or rewrite the dashboard — and forking breaks
// the rule that the engine must stay runnable on its own.
//
// The fix is to make the chrome data rather than markup. Layout renders slots;
// a SlotProvider fills them. The engine ships a provider with single-owner
// defaults. A wrapping application supplies its own, adding an organisation
// switcher, a balance indicator, or extra navigation, without editing a single
// file in this package.
package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/identity"
)

// NavItem is one entry in the sidebar.
type NavItem struct {
	Label string
	Href  string
	// Icon names one of the inline glyphs in layout.templ. Unknown names
	// render nothing rather than a broken image.
	Icon   string
	Badge  string
	Active bool
}

// Crumb is one segment of the breadcrumb in the top bar.
// An empty Href renders as the current, non-navigable segment.
type Crumb struct {
	Label string
	Href  string
}

// NavGroup is a labelled block of navigation. An empty Heading renders the
// items without a header, which is how the primary group is expressed.
type NavGroup struct {
	Heading string
	Items   []NavItem
}

// Slots is the chrome around a page.
//
// Every field here is an extension point. Adding a field is safe; changing the
// meaning of one is a breaking change for anything wrapping the engine, so
// treat this struct as public API.
type Slots struct {
	// Title is the document title.
	Title string

	// Breadcrumb is the trail in the top bar. A wrapping application prepends
	// its own segments — an account or environment, say — without the engine
	// needing a concept for them.
	Breadcrumb []Crumb

	// BrandName and BrandHref label the top-left corner. A wrapping
	// application overrides these to present its own product.
	BrandName string
	BrandHref string

	// Nav is the sidebar navigation. A wrapping application may append its
	// own groups, or replace the set entirely.
	Nav []NavGroup

	// HeaderTools renders at the right of the top bar. This is where a
	// commercial layer would put a balance indicator or usage meter.
	HeaderTools templ.Component

	// SidebarTop renders above the navigation — the natural home for an
	// organisation or project switcher.
	SidebarTop templ.Component

	// SidebarFooter renders at the bottom of the sidebar.
	SidebarFooter templ.Component

	// FullBleed drops the centred, padded content wrapper so the page fills
	// the window and manages its own scrolling.
	//
	// A chrome decision rather than a page one, which is why it lives here: a
	// wrapping application that replaces the layout decides for itself which
	// of its pages want the whole window, and the engine's own answer is not
	// binding on it.
	FullBleed bool

	// Banner renders above the page content, full width. Intended for
	// account-level notices such as a low balance or a pending suspension.
	Banner templ.Component

	// Flash is the outcome of whatever the last request did, shown once.
	//
	// Filled by the server immediately before rendering rather than by a
	// SlotProvider: reading it also clears it, and a provider is handed the
	// request without the response it would need to do that. A wrapping
	// application therefore inherits toasts without implementing anything.
	Flash *Flash
}

// SlotProvider builds the chrome for a request.
//
// Taking the request, rather than a fixed Slots value, is what allows the
// chrome to vary per principal — which is precisely what a multi-tenant
// wrapper needs and a single-owner engine does not.
type SlotProvider interface {
	Slots(ctx context.Context, r *http.Request) Slots
}

// SlotProviderFunc adapts a function to SlotProvider.
type SlotProviderFunc func(ctx context.Context, r *http.Request) Slots

func (f SlotProviderFunc) Slots(ctx context.Context, r *http.Request) Slots {
	return f(ctx, r)
}

// DefaultSlots is the engine's own chrome: the navigation a single-owner
// self-hosted install needs, and nothing else.
type DefaultSlots struct{}

var _ SlotProvider = DefaultSlots{}

func (DefaultSlots) Slots(ctx context.Context, r *http.Request) Slots {
	path := r.URL.Path

	// Read once and shared: the switcher offers this list, and the footer asks
	// the same question of it — whether this install has accounts at all.
	teams := TeamsFromContext(ctx)

	// The switcher goes in SidebarTop — the slot documented as the home for an
	// organisation switcher — rather than into the layout directly. A wrapping
	// application that fills this slot with its own control replaces the
	// engine's, which is the point of the seam: the chrome stays data.
	//
	// Nothing is drawn on an install with no accounts, where the person belongs
	// to no teams and there is nothing to switch between.
	var switcher templ.Component
	if len(teams) > 0 {
		switcher = TeamSwitcher(teams)
	}

	// The footer fills the slot documented as the bottom of the sidebar, for
	// the same reason the switcher fills SidebarTop: a wrapping application
	// that wants its own account control replaces this one by filling the
	// slot, rather than by editing the layout.
	//
	// Nothing is drawn where no owner was resolved. The sign-in page renders
	// this same chrome with no session, and a footer offering to sign out of
	// nothing is worse than an empty corner — which is also why this reads the
	// owner with FromContext rather than MustFromContext.
	var footer templ.Component
	if owner, ok := identity.FromContext(ctx); ok {
		// Sign-out is routed only where accounts are on, and that is the same
		// condition that gives a session teams to switch between.
		footer = UserMenu(owner, len(teams) > 0)
	}

	return Slots{
		Title: "Yacht",
		// The canvas is a workspace rather than a document: a graph inside a
		// 1240px column with the window's scrollbar beside it reads as a
		// picture of a canvas rather than one.
		FullBleed:     isCanvasPath(path),
		Breadcrumb:    breadcrumbFor(path),
		BrandName:     DefaultBrandName,
		BrandHref:     "/",
		SidebarTop:    switcher,
		SidebarFooter: footer,
		Nav: []NavGroup{
			{Items: []NavItem{
				{Label: "Overview", Href: "/", Icon: "grid", Active: path == "/"},
				// No Apps entry. An app is drawn on a project's canvas and
				// reached from it, so a second door into a flat list of every
				// app in the team would show the same things with the one fact
				// that matters — what they are connected to — taken out.
				{Label: "Projects", Href: "/projects", Icon: "boxes",
					Active: hasPrefix(path, "/projects") || hasPrefix(path, "/apps") ||
						hasPrefix(path, "/canvas")},
				{Label: "Deployments", Href: "/deployments", Icon: "rocket",
					Active: hasPrefix(path, "/deployments")},
			}},
			{Heading: "Infrastructure", Items: append([]NavItem{
				{Label: "Nodes", Href: "/cluster/nodes", Icon: "server",
					Active: path == "/cluster" || hasPrefix(path, "/cluster/nodes")},
				{Label: "Pods", Href: "/cluster/pods", Icon: "layers",
					Active: hasPrefix(path, "/cluster/pods")},
				{Label: "Volumes", Href: "/cluster/volumes", Icon: "disk",
					Active: hasPrefix(path, "/cluster/volumes")},
				{Label: "Events", Href: "/cluster/events", Icon: "activity",
					Active: hasPrefix(path, "/cluster/events")},
			}, infraNav(ctx, path)...)},
			{Heading: "System", Items: append(teamNav(ctx, path), NavItem{
				Label: "Settings", Href: "/settings", Icon: "settings",
				Active: hasPrefix(path, "/settings"),
			})},
		},
	}
}

func breadcrumbFor(path string) []Crumb {
	switch {
	case path == "/":
		return []Crumb{{Label: "Overview"}}
	case hasPrefix(path, "/apps/new"):
		return []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "New"}}
	case hasPrefix(path, "/apps/"):
		// The app name is appended by the handler, which knows it.
		return []Crumb{{Label: "Apps", Href: "/apps"}}
	case hasPrefix(path, "/apps"):
		return []Crumb{{Label: "Apps"}}
	case hasPrefix(path, "/deployments"):
		return []Crumb{{Label: "Deployments"}}
	case hasPrefix(path, "/cluster/volumes"):
		return []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "Volumes"}}
	case hasPrefix(path, "/cluster/events"):
		return []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "Events"}}
	case hasPrefix(path, "/cluster/dns"):
		return []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "DNS"}}
	case hasPrefix(path, "/cluster/registry"):
		return []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "Registry"}}
	case hasPrefix(path, "/cluster"):
		return []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "Cluster"}}
	case hasPrefix(path, "/settings"):
		return []Crumb{{Label: "Settings"}}
	}
	return nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// ownerLabel is what the sidebar calls the signed-in owner.
//
// An identity provider is free to resolve an owner with no display name — the
// token provider does, and so does a fresh account that has only ever proved
// an email address. Falling back to the address keeps the corner labelled
// instead of leaving a blank row beside an avatar.
func ownerLabel(o identity.Owner) string {
	if o.DisplayName != "" {
		return o.DisplayName
	}
	if o.Email != "" {
		return o.Email
	}
	return "Account"
}

// initials abbreviates a label for the sidebar's square avatars.
//
// Two letters from the first two words, one from a single word. Runes rather
// than bytes: a team named "Ötzi" abbreviated by byte would render half a
// character.
func initials(label string) string {
	fields := strings.Fields(label)
	if len(fields) == 0 {
		return "?"
	}
	first := []rune(fields[0])
	out := string(first[:1])
	if len(fields) > 1 {
		second := []rune(fields[1])
		out += string(second[:1])
	}
	return strings.ToUpper(out)
}

// slot renders c, tolerating nil so every slot is genuinely optional.
func slot(c templ.Component) templ.Component {
	if c == nil {
		return templ.NopComponent
	}
	return c
}

// teamNav offers team management only where there is a team to manage.
//
// An install resolved by a shared token has one owner and no memberships, so
// the page would show a list of one person nobody can change. The switcher
// already answers "is this install multi-team?", and this asks it the same way.
func teamNav(ctx context.Context, path string) []NavItem {
	if len(TeamsFromContext(ctx)) == 0 {
		return nil
	}
	return []NavItem{{
		Label: "Team", Href: "/team", Icon: "users",
		Active: hasPrefix(path, "/team"),
	}}
}

// infraNav lists the install-wide settings pages this install actually has.
//
// Conditional for the reason the Team entry is: an entry whose route was never
// mounted is a link to a 404, which reads as a broken feature rather than an
// absent one. DNS had exactly that problem from the day it was written — a
// page with no way in, and a custom-domain panel telling people to go to it.
func infraNav(ctx context.Context, path string) []NavItem {
	var items []NavItem
	s := SurfacesFromContext(ctx)
	if s.DNS {
		items = append(items, NavItem{
			Label: "DNS", Href: "/cluster/dns", Icon: "globe",
			Active: hasPrefix(path, "/cluster/dns"),
		})
	}
	if s.Registry {
		items = append(items, NavItem{
			Label: "Registry", Href: "/cluster/registry", Icon: "package",
			Active: hasPrefix(path, "/cluster/registry"),
		})
	}
	return items
}

// isCanvasPath reports whether a path renders the canvas.
//
// The canvas fills the window, so it opts out of the centred, padded wrapper
// every other page sits in. An app is on this list because its detail is a
// panel over its canvas rather than a page of its own — but /apps and
// /apps/new are ordinary pages, and a prefix test alone would swallow both.
func isCanvasPath(path string) bool {
	switch {
	case path == "/projects", path == "/projects/":
		// The list of projects is an ordinary page. Only a project itself is a
		// canvas, and letting the prefix cover both took the padding off the
		// list — which then sat flush against the chrome while every other
		// index page was inset.
		return false
	case hasPrefix(path, "/projects/"), hasPrefix(path, "/canvas"):
		return true
	case path == "/apps", path == "/apps/", hasPrefix(path, "/apps/new"):
		return false
	default:
		return hasPrefix(path, "/apps/")
	}
}

// DefaultBrandName is this engine's own name.
//
// Compared against rather than only assigned: the wordmark is this one word
// drawn, so it is shown when the name is still this one and replaced by plain
// text the moment a wrapping application sets its own.
const DefaultBrandName = "Yacht"
