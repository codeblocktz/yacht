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
	"cmp"
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/identity"
)

// NavItem is one entry in the sidebar.
//
// Every item the sidebar draws is also a page in the command palette, found by
// its Label — a wrapping application's entries included, with nothing to
// register.
type NavItem struct {
	Label string
	Href  string
	// Icon names one of the inline glyphs in layout.templ. Unknown names
	// render nothing rather than a broken image.
	Icon  string
	Badge string
	// Live marks the badge as counting something in progress — deploys in
	// flight, say — which draws it in the in-flight colour with a pulse,
	// and as a dot on the icon in the collapsed rail.
	Live   bool
	Active bool
}

// SidebarProjects is the sidebar's Projects section: the team's projects,
// each opening to the apps on its canvas.
type SidebarProjects struct {
	// Items are the projects listed, in order.
	Items []SidebarProject
	// More is how many projects are not listed, which puts an "All projects"
	// link to AllHref under the list.
	More    int
	AllHref string
	// NewHref is where "New project" goes. Empty leaves it off.
	NewHref string
}

// SidebarProject is one project in the tree.
type SidebarProject struct {
	// Key identifies the project in the browser's memory of which projects
	// were left open. Unique across teams, so opening "Default" in one team
	// does not open it in the next.
	Key    string
	Name   string
	Href   string
	Active bool
	// Open draws the project expanded: it is the one being looked at, or holds
	// the app that is. A project somebody opened by hand stays open too, but
	// that is remembered by the browser rather than decided here.
	Open bool
	Apps []SidebarApp
}

// SidebarApp is one app under its project.
type SidebarApp struct {
	Name string
	Href string
	// State is an app.AppState: what the app's records say, drawn as a status
	// dot. See appStateClass.
	State  string
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
	// Admin says this is the Admin console's chrome rather than a workspace's:
	// the install's pages, for its operators. A wrapping application adds its
	// operator pages to the sidebar when it is set, and its workspace pages —
	// billing, say — when it is not.
	Admin bool

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

	// BrandMark is drawn beside BrandName, and alone in the collapsed rail.
	// Nil is Yacht's mark. A wrapping application passes its own — sized by
	// its own markup, around 19px tall to sit with the name — or
	// templ.NopComponent for a wordmark with no mark at all.
	//
	// Separate from BrandName because a product renamed but still drawn with
	// this engine's mark is presenting somebody else's logo as its own.
	BrandMark templ.Component

	// Stylesheets are a wrapping application's own, linked after the
	// engine's so they can restate its colour tokens: a product's own
	// primary colour, say. Paths on the dashboard's own origin; the content
	// security policy refuses any other. Nil adds nothing.
	Stylesheets []string

	// Nav is the sidebar navigation. A wrapping application may append its
	// own groups, or replace the set entirely.
	Nav []NavGroup

	// HeaderTools renders at the right of the top bar. This is where a
	// commercial layer would put a balance indicator or usage meter.
	HeaderTools templ.Component

	// SidebarTop renders under the brand, above search and the navigation:
	// the answer to "where am I". The engine fills it with the team switcher
	// (or the owner's name on an install without teams). A wrapping
	// application that sets its own replaces the engine's switcher entirely —
	// the natural place for an organisation switcher of its own. Sized for a
	// 40px row; in the collapsed rail anything inside marked rail-hide is
	// hidden.
	SidebarTop templ.Component

	// SidebarFooter renders at the bottom of the sidebar: the engine puts the
	// signed-in person's menu here — account, theme, docs, sign out.
	SidebarFooter templ.Component

	// Projects is the sidebar's Projects section, drawn after the primary
	// (unheaded) navigation. Nil draws no section.
	Projects *SidebarProjects

	// SearchIndex is where the command palette reads the team's projects and
	// apps from. Empty still gives a palette — of the sidebar's pages and the
	// built-in actions — just without the team's own things in it.
	SearchIndex string

	// FullBleed drops the centred, padded content wrapper so the page fills
	// the window and manages its own scrolling.
	//
	// A chrome decision rather than a page one, which is why it lives here: a
	// wrapping application that replaces the layout decides for itself which
	// of its pages want the whole window, and the engine's own answer is not
	// binding on it.
	FullBleed bool

	// Project is the project a full-bleed page belongs to. When set, the page
	// is drawn the way Railway draws a project — a bar naming the project and
	// its environment, and a rail of the project's own sections — instead of
	// under the dashboard's sidebar. Nil keeps the dashboard's chrome.
	Project *ProjectBar

	// Banner renders above the page content, full width. Intended for
	// account-level notices such as a low balance or a pending suspension.
	//
	// While an operator is acting as a team the layout draws the acting
	// notice above whatever is here; see Surfaces.ActingAs. It can be
	// composed with but not replaced.
	Banner templ.Component

	// Bare drops the sidebar and the top bar, leaving the page the whole
	// window.
	//
	// For the pages somebody meets before they are let in. Emptying the
	// individual slots is not the same thing: the navigation goes, but the
	// sidebar column and the top bar stay, so a sign-in form sits beside an
	// empty rail and under an empty header — chrome for an application the
	// visitor has no access to yet.
	//
	// A chrome decision rather than a page one, which is why it lives here
	// beside FullBleed. A wrapping application that replaces the layout decides
	// for itself what its signed-out pages look like.
	Bare bool

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
type DefaultSlots struct {
	// DocsHref is where the account menu's Docs leads. Empty is the engine's
	// own README; a wrapping application points it at its product's docs,
	// since its customers have no use for the engine's.
	DocsHref string

	// Plan names the team's plan under its name in the workspace switcher,
	// as Railway's HOBBY does. Nil, or an empty answer, shows the person's
	// role there instead. Asked only for a team that is the person's own.
	Plan func(ctx context.Context, teamID string) string
}

// DefaultDocsHref is the engine's own documentation.
const DefaultDocsHref = "https://github.com/codeblocktz/yacht#readme"

var _ SlotProvider = DefaultSlots{}

func (d DefaultSlots) Slots(ctx context.Context, r *http.Request) Slots {
	path := r.URL.Path

	// Read once and shared: the switcher offers this list, and the footer asks
	// the same question of it — whether this install has accounts at all.
	teams := TeamsFromContext(ctx)

	owner, signedIn := identity.FromContext(ctx)
	surfaces := SurfacesFromContext(ctx)

	// The switcher goes in SidebarTop — the slot documented as the home for an
	// organisation switcher — rather than into the layout directly. A wrapping
	// application that fills this slot with its own control replaces the
	// engine's, which is the point of the seam: the chrome stays data.
	//
	// It answers "where am I", so it says so even where there is nothing to
	// switch to: an operator acting as a team sees that team, and an install
	// with no accounts sees its one owner — named, with no menu to open.
	var switcher templ.Component
	switch {
	case surfaces.ActingAs != "":
		// Not a menu: switching team while acting as another would leave the
		// session in two places at once. The banner is where acting stops.
		switcher = TeamIdentity(surfaces.ActingAs, "Support access", true)
	case len(teams) > 0:
		var plan string
		if d.Plan != nil {
			if t, ok := activeTeam(teams); ok {
				plan = d.Plan(ctx, t.ID)
			}
		}
		switcher = TeamSwitcher(teams, plan)
	case signedIn:
		switcher = TeamIdentity(ownerLabel(owner), "Owner", false)
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
	if signedIn {
		// Sign-out is routed only where accounts are on, and that is the same
		// condition that gives a session teams to switch between.
		footer = UserMenu(owner, len(teams) > 0, cmp.Or(d.DocsHref, DefaultDocsHref))
	}

	// The acting notice is the engine's own banner, and it takes the slot: no
	// other notice the engine draws matters more than being told that what
	// you are about to change is somebody else's.
	var banner templ.Component
	if acting := surfaces.ActingAs; acting != "" {
		banner = ActingBanner(acting)
	}

	ws, haveWorkspace := workspaceFromContext(ctx)
	var searchIndex string
	if signedIn {
		searchIndex = searchIndexPath
	}

	slots := Slots{
		Title:  "Yacht",
		Banner: banner,
		// The canvas is a workspace rather than a document: a graph inside a
		// 1240px column with the window's scrollbar beside it reads as a
		// picture of a canvas rather than one.
		FullBleed:     isCanvasPath(path),
		Project:       projectBarFor(ws, haveWorkspace, path),
		Breadcrumb:    breadcrumbFor(path),
		BrandName:     DefaultBrandName,
		BrandHref:     "/",
		SidebarTop:    switcher,
		SidebarFooter: footer,
		SearchIndex:   searchIndex,
	}

	// Two chromes, as Railway has: the workspace, which is everybody's home
	// — the team's projects and what belongs to the team — and the Admin
	// console, which is the install's and only its operators'. One sidebar
	// holding both read as one product with the operator's tools bolted on,
	// and showed an operator two lists of the same teams.
	if surfaces.Operator && IsAdminPath(path) {
		slots.Admin = true
		slots.BrandHref = "/admin"
		slots.Project = nil
		slots.SidebarTop = AdminConsoleTop(signedIn)
		slots.Nav = adminConsoleNav(ctx, path)
		return slots
	}
	slots.Nav = workspaceNav(ctx, path, surfaces, len(teams) > 0)
	if haveWorkspace && ws.LiveDeploys > 0 {
		// Deploys in flight are counted on Projects, where they are: "3"
		// says whether the deploy you started is the only one, which a dot
		// cannot.
		slots.Nav[0].Items[0].Badge = strconv.Itoa(ws.LiveDeploys)
		slots.Nav[0].Items[0].Live = true
	}
	return slots
}

// The Admin console's groups. A wrapping application adds its own pages to
// these by heading — its customers to AdminCustomersHeading, say.
const (
	AdminCustomersHeading = "Customers"
	AdminClusterHeading   = "Cluster"
	AdminInstallHeading   = "Install"
)

// IsAdminPath reports whether a path is one of the Admin console's: the
// install's pages rather than a team's.
func IsAdminPath(path string) bool {
	return path == "/admin" || hasPrefix(path, "/admin/") ||
		path == "/cluster" || hasPrefix(path, "/cluster/") ||
		path == "/settings"
}

// workspaceNav is the workspace's sidebar, Railway's: the team's projects
// and templates, what it costs and who is in it, and its settings. An
// operator also has the way into the Admin console, apart at the bottom.
func workspaceNav(ctx context.Context, path string, s Surfaces, accounts bool) []NavGroup {
	main := []NavItem{
		// No Apps entry. An app is drawn on a project's canvas and reached
		// from it, so a second door into a flat list of every app in the team
		// would show the same things with the one fact that matters — what
		// they are connected to — taken out.
		{Label: "Projects", Href: "/", Icon: "boxes",
			Active: path == "/" || hasPrefix(path, "/projects") || hasPrefix(path, "/apps") ||
				hasPrefix(path, "/canvas")},
	}
	if s.Templates {
		main = append(main, NavItem{Label: "Templates", Href: "/templates", Icon: "layers", Active: hasPrefix(path, "/templates")})
	}
	if s.Usage {
		main = append(main, NavItem{Label: "Usage", Href: "/usage", Icon: "gauge", Active: hasPrefix(path, "/usage")})
	}
	if accounts {
		main = append(main, NavItem{Label: "People", Href: "/team", Icon: "users", Active: path == "/team"})
	}
	nav := []NavGroup{{Items: main}}

	var settings []NavItem
	if accounts && s.Tokens {
		settings = append(settings, NavItem{Label: "API tokens", Href: "/team/tokens", Icon: "key",
			Active: hasPrefix(path, "/team/tokens")})
	}
	if s.Audit {
		settings = append(settings, NavItem{Label: "Audit log", Href: "/team/audit", Icon: "activity",
			Active: hasPrefix(path, "/team/audit")})
	}
	if accounts {
		settings = append(settings, NavItem{Label: "Account", Href: "/account", Icon: "user",
			Active: hasPrefix(path, "/account")})
	}
	if len(settings) > 0 {
		nav = append(nav, NavGroup{Heading: WorkspaceSettingsHeading, Items: settings})
	}
	if s.Operator {
		nav = append(nav, NavGroup{Heading: AdminNavHeading, Items: []NavItem{
			{Label: "Admin console", Href: "/admin", Icon: "server"},
		}})
	}
	return nav
}

// WorkspaceSettingsHeading is the workspace's settings group, where a
// wrapping application puts its own — billing, say.
const WorkspaceSettingsHeading = "Settings"

// adminConsoleNav is the Admin console's sidebar: its overview, the teams on
// the install, the machines they run on, and the install's own settings.
func adminConsoleNav(ctx context.Context, path string) []NavGroup {
	s := SurfacesFromContext(ctx)
	customers := []NavItem{}
	if s.Quotas {
		customers = append(customers,
			NavItem{Label: "Teams", Href: "/admin/teams", Icon: "building",
				Active: hasPrefix(path, "/admin/teams")},
			NavItem{Label: "Capacity", Href: "/admin/capacity", Icon: "gauge",
				Active: hasPrefix(path, "/admin/capacity")},
		)
	}
	if s.Impersonation {
		customers = append(customers, NavItem{Label: "Impersonations", Href: "/admin/impersonations", Icon: "eye",
			Active: hasPrefix(path, "/admin/impersonations")})
	}
	if s.Regions {
		customers = append(customers, NavItem{Label: "Regions", Href: "/admin/regions", Icon: "map-pin",
			Active: hasPrefix(path, "/admin/regions")})
	}
	nav := []NavGroup{{Items: []NavItem{{Label: "Overview", Href: "/admin", Icon: "grid", Active: path == "/admin"}}}}
	if len(customers) > 0 {
		nav = append(nav, NavGroup{Heading: AdminCustomersHeading, Items: customers})
	}
	nav = append(nav,
		NavGroup{Heading: AdminClusterHeading, Items: adminNav(ctx, path)},
		NavGroup{Heading: AdminInstallHeading, Items: []NavItem{
			{Label: "Settings", Href: "/settings", Icon: "settings", Active: path == "/settings"},
		}},
	)
	return nav
}

func breadcrumbFor(path string) []Crumb {
	switch {
	case path == "/" || path == "/projects":
		return []Crumb{{Label: "Projects"}}
	case path == "/usage":
		return []Crumb{{Label: "Usage"}}
	case path == "/admin":
		return []Crumb{{Label: AdminNavHeading}, {Label: "Overview"}}
	case hasPrefix(path, "/admin/impersonations"):
		return []Crumb{{Label: AdminNavHeading, Href: "/admin"}, {Label: "Impersonations"}}
	case hasPrefix(path, "/apps/new"):
		return []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "New"}}
	case hasPrefix(path, "/apps/"):
		// The app name is appended by the handler, which knows it.
		return []Crumb{{Label: "Apps", Href: "/apps"}}
	case hasPrefix(path, "/apps"):
		return []Crumb{{Label: "Apps"}}
	case hasPrefix(path, "/deployments"):
		return []Crumb{{Label: "Deployments"}}
	// The cluster's pages keep their /cluster URLs — bookmarks and the
	// install docs point at them — and sit under Admin in the trail as they
	// do in the sidebar. Admin links to Nodes from here because Nodes exists
	// on every install; the teams pages need an app service behind them.
	case hasPrefix(path, "/cluster/volumes"):
		return []Crumb{{Label: AdminNavHeading, Href: "/cluster/nodes"}, {Label: "Volumes"}}
	case hasPrefix(path, "/cluster/events"):
		return []Crumb{{Label: AdminNavHeading, Href: "/cluster/nodes"}, {Label: "Events"}}
	case hasPrefix(path, "/cluster/dns"):
		return []Crumb{{Label: AdminNavHeading, Href: "/cluster/nodes"}, {Label: "DNS"}}
	case hasPrefix(path, "/cluster/registry"):
		return []Crumb{{Label: AdminNavHeading, Href: "/cluster/nodes"}, {Label: "Registry"}}
	case hasPrefix(path, "/cluster"):
		return []Crumb{{Label: AdminNavHeading, Href: "/cluster/nodes"}, {Label: "Cluster"}}
	case hasPrefix(path, "/settings"):
		return []Crumb{{Label: "Settings"}}
	case path == "/admin/teams":
		return []Crumb{{Label: AdminNavHeading, Href: "/admin/teams"}, {Label: "Teams"}}
	case hasPrefix(path, "/admin/teams/"):
		// The team's name is appended by the handler, which knows it.
		return []Crumb{{Label: AdminNavHeading, Href: "/admin/teams"}, {Label: "Teams", Href: "/admin/teams"}}
	case hasPrefix(path, "/admin/capacity"):
		return []Crumb{{Label: AdminNavHeading, Href: "/admin/teams"}, {Label: "Capacity"}}
	case path == "/admin/regions":
		return []Crumb{{Label: AdminNavHeading, Href: "/admin/teams"}, {Label: "Regions"}}
	case hasPrefix(path, "/admin/regions/"):
		// The region's name is appended by the handler, which knows it.
		return []Crumb{{Label: AdminNavHeading, Href: "/admin/teams"}, {Label: "Regions", Href: "/admin/regions"}}
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

// adminNav is the Admin console's Cluster group: the machines, and the
// registry and DNS where this install has them.
//
// Conditional: an entry whose route was never mounted is a link to a 404,
// which reads as a broken feature rather than an absent one. DNS had exactly
// that problem from the day it was written — a page with no way in, and a
// custom-domain panel telling people to go to it.
func adminNav(ctx context.Context, path string) []NavItem {
	var items []NavItem
	s := SurfacesFromContext(ctx)
	items = append(items,
		NavItem{Label: "Nodes", Href: "/cluster/nodes", Icon: "server",
			Active: path == "/cluster" || hasPrefix(path, "/cluster/nodes")},
		NavItem{Label: "Pods", Href: "/cluster/pods", Icon: "layers",
			Active: hasPrefix(path, "/cluster/pods")},
		NavItem{Label: "Volumes", Href: "/cluster/volumes", Icon: "disk",
			Active: hasPrefix(path, "/cluster/volumes")},
		NavItem{Label: "Events", Href: "/cluster/events", Icon: "activity",
			Active: hasPrefix(path, "/cluster/events")},
	)
	if s.Registry {
		items = append(items, NavItem{
			Label: "Registry", Href: "/cluster/registry", Icon: "package",
			Active: hasPrefix(path, "/cluster/registry"),
		})
	}
	if s.DNS {
		items = append(items, NavItem{
			Label: "DNS", Href: "/cluster/dns", Icon: "globe",
			Active: hasPrefix(path, "/cluster/dns"),
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
