package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// The team's projects and apps, for the chrome.
//
// The sidebar's project tree, the Deployments badge, the command palette's
// index and the getting-started checklist all read the same thing, and on a
// full page render several of them are drawn at once. So it is read once per
// request, lazily, and shared — the same arrangement withTeams uses for the
// switcher, for the same reason: most requests through the authenticated group
// are POSTs that redirect and never draw any of it.

// workspaceKey addresses the loader on the request. Unexported, so nothing
// outside this package can plant a workspace for the chrome to draw.
type workspaceKey struct{}

// workspaceLoader returns the request's workspace, reading it on first call.
// The bool is false where there is nothing to read — no app service, no owner,
// or a query that failed — and the chrome then draws none of it.
type workspaceLoader func() (app.Workspace, bool)

// workspaceFromContext returns the request's workspace, if it has one.
func workspaceFromContext(ctx context.Context) (app.Workspace, bool) {
	load, _ := ctx.Value(workspaceKey{}).(workspaceLoader)
	if load == nil {
		return app.Workspace{}, false
	}
	return load()
}

// withWorkspace makes the workspace available to whatever renders the chrome.
func (s *Server) withWorkspace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			once sync.Once
			ws   app.Workspace
			ok   bool
		)
		load := workspaceLoader(func() (app.Workspace, bool) {
			once.Do(func() { ws, ok = s.workspaceFor(r) })
			return ws, ok
		})
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), workspaceKey{}, load)))
	})
}

func (s *Server) workspaceFor(r *http.Request) (app.Workspace, bool) {
	if s.apps == nil {
		return app.Workspace{}, false
	}
	owner, found := identity.FromContext(r.Context())
	if !found {
		return app.Workspace{}, false
	}
	ws, err := s.apps.Workspace(r.Context(), owner.ID)
	if err != nil {
		// Chrome, so it disappears rather than failing the page: a dashboard
		// that 500s because the sidebar could not list projects is a worse
		// answer than one with no project list in it.
		s.log.Error("read workspace for the chrome", slog.String("error", err.Error()))
		return app.Workspace{}, false
	}
	return ws, true
}

// ---------------------------------------------------------------- sidebar tree

// sidebarProjectLimit is how many projects the sidebar lists before it offers
// "All projects" instead. Enough for most teams to never see the link, few
// enough that the tree does not push everything below it off the screen.
const sidebarProjectLimit = 6

// sidebarProjects builds the sidebar's Projects section from a workspace and
// the path being drawn.
//
// The project being looked at is always in the list, even when it is not
// among the first few: a tree that loses its own highlight the moment you open
// the seventh project answers "where am I" with nothing.
func sidebarProjects(ws app.Workspace, path string) *SidebarProjects {
	activeSlug, activeApp := activeInTree(path)

	all := make([]SidebarProject, 0, len(ws.Projects))
	activeIndex := -1
	for _, p := range ws.Projects {
		entry := SidebarProject{
			Key:    p.ID.String(),
			Name:   p.Name,
			Href:   "/projects/" + p.Slug,
			Active: p.Slug == activeSlug && activeApp == "",
		}
		for _, a := range p.Apps {
			current := a.Name == activeApp
			entry.Apps = append(entry.Apps, SidebarApp{
				Name: a.Name, Href: "/apps/" + a.Name, State: string(a.State), Active: current,
			})
			if current {
				entry.Open = true
			}
		}
		if p.Slug == activeSlug {
			entry.Open = true
		}
		if entry.Active || entry.Open {
			activeIndex = len(all)
		}
		all = append(all, entry)
	}

	shown := all
	if len(all) > sidebarProjectLimit {
		shown = append([]SidebarProject(nil), all[:sidebarProjectLimit]...)
		if activeIndex >= sidebarProjectLimit {
			shown[sidebarProjectLimit-1] = all[activeIndex]
		}
	}
	return &SidebarProjects{
		Items:   shown,
		More:    len(all) - len(shown),
		AllHref: "/projects",
		NewHref: "/projects?new=1",
	}
}

// activeInTree reads which project or app a path is about.
//
// /apps/new is a form, not an app called "new", and /apps itself is the list.
func activeInTree(path string) (project, appName string) {
	switch {
	case hasPrefix(path, "/projects/"):
		project, _, _ = strings.Cut(strings.TrimPrefix(path, "/projects/"), "/")
	case hasPrefix(path, "/apps/") && !hasPrefix(path, "/apps/new"):
		appName, _, _ = strings.Cut(strings.TrimPrefix(path, "/apps/"), "/")
	}
	return project, appName
}

// appStateClass colours an app's recorded state with the status palette the
// rest of the dashboard uses, so a dot in the sidebar and a word on a row mean
// the same thing.
func appStateClass(state string) string {
	switch app.AppState(state) {
	case app.AppServing:
		return "status-ok"
	case app.AppDeployFailed:
		return "status-warn"
	case app.AppFailed:
		return "status-err"
	case app.AppDeploying:
		return "status-info status-live"
	}
	return "status-neutral"
}

// appStateLabel is the state in words, for the dot's tooltip and for a screen
// reader, which cannot see a colour.
//
// Worded as what the records say rather than as a health check, because that
// is what it is: "Serving" means a release is in service, not that its pods
// are answering. The canvas is where live health is shown.
func appStateLabel(state string) string {
	switch app.AppState(state) {
	case app.AppServing:
		return "Serving"
	case app.AppDeployFailed:
		return "Last deploy failed — the previous version is still serving"
	case app.AppFailed:
		return "Deploy failed"
	case app.AppDeploying:
		return "Deploying"
	case app.AppStopped:
		return "Stopped"
	case app.AppIdle:
		return "Not deployed yet"
	}
	return "Unknown"
}

// ---------------------------------------------------------------- search index

// searchEntry is one thing the command palette can go to.
type searchEntry struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Href   string `json:"href"`
	Status string `json:"status,omitempty"`
	// Detail is the muted text beside the name: an app's project, a
	// project's size.
	Detail string `json:"detail,omitempty"`
}

type searchIndex struct {
	Items []searchEntry `json:"items"`
	// Domains says whether apps have a domains tab to send "Add a custom
	// domain" to. The palette leaves the action out when they do not.
	Domains bool `json:"domains"`
}

// searchIndexPath is where the palette reads the team's projects and apps.
const searchIndexPath = "/search/index"

// searchIndexHandler lists the team's projects and apps for the palette.
//
// Behind the member gate with every other read, and scoped by the owner the
// request resolved to — never by anything in the request — so the palette can
// only ever offer what the sidebar could. Built from the same per-request
// workspace the chrome draws, and not cached past the request: a list of
// somebody's apps is not a thing to leave in a shared cache.
func (s *Server) searchIndexHandler(w http.ResponseWriter, r *http.Request) {
	out := searchIndex{Items: []searchEntry{}, Domains: s.nets != nil}
	if ws, ok := workspaceFromContext(r.Context()); ok {
		out.Items = searchEntries(ws)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// searchEntries flattens a workspace into palette entries: every project,
// then every app with the project it is on.
func searchEntries(ws app.Workspace) []searchEntry {
	var out []searchEntry
	for _, p := range ws.Projects {
		out = append(out, searchEntry{
			Name: p.Name, Kind: "project", Href: "/projects/" + p.Slug,
			Detail: plural(len(p.Apps), "app"),
		})
	}
	for _, p := range ws.Projects {
		for _, a := range p.Apps {
			out = append(out, searchEntry{
				Name: a.Name, Kind: "app", Href: "/apps/" + a.Name,
				Status: string(a.State), Detail: p.Name,
			})
		}
	}
	for _, a := range ws.Unplaced {
		out = append(out, searchEntry{
			Name: a.Name, Kind: "app", Href: "/apps/" + a.Name, Status: string(a.State),
		})
	}
	if out == nil {
		out = []searchEntry{}
	}
	return out
}

// ---------------------------------------------------------------- checklist

// ChecklistStep is one step of getting started.
type ChecklistStep struct {
	Title  string
	Detail string
	Href   string
	// Action is the label on the link to go and do it.
	Action string
	Done   bool
}

// Checklist is getting started, computed from what the team has.
type Checklist struct {
	Steps []ChecklistStep
}

// Done is how many steps are.
func (c Checklist) Done() int {
	n := 0
	for _, s := range c.Steps {
		if s.Done {
			n++
		}
	}
	return n
}

// Next is the first step not yet done, which the checklist offers as the
// primary action. -1 when all are.
func (c Checklist) Next() int {
	for i, s := range c.Steps {
		if !s.Done {
			return i
		}
	}
	return -1
}

// checklistFacts is everything the checklist is computed from.
type checklistFacts struct {
	Onboarding app.Onboarding
	// Members is how many people are in the team; zero where the install has
	// no accounts and there is nobody to invite.
	Members int
	// Accounts and Domains are whether the install can invite anybody or
	// route a custom domain at all. A step for something the install cannot
	// do would sit undone forever.
	Accounts bool
	Domains  bool
	// FirstApp is where "Add a custom domain" goes: an app's domains tab.
	FirstApp string
}

// gettingStarted computes the checklist, or nil when there is nothing to show:
// dismissed, or every step done.
//
// Every step is a fact about the team's own data rather than a box somebody
// ticked, so the checklist cannot claim a step that was not taken — and a team
// that did the steps some other way, through the API or before this existed,
// finds them already done.
func gettingStarted(f checklistFacts) *Checklist {
	if f.Onboarding.Dismissed {
		return nil
	}
	c := Checklist{Steps: []ChecklistStep{
		{
			Title:  "Deploy your first app",
			Detail: "Connect a repository to build from, or run an image you already have.",
			Href:   "/apps/new", Action: "Deploy an app",
			Done: f.Onboarding.HasApps,
		},
		{
			Title:  "See a deploy succeed",
			Detail: "Watch the first release go live, with its build and logs as it happens.",
			Href:   "/deployments", Action: "View deployments",
			Done: f.Onboarding.HasSucceededDeploy,
		},
	}}
	if f.Domains {
		href := "/apps/new"
		if f.FirstApp != "" {
			href = "/apps/" + f.FirstApp + "/domains"
		}
		c.Steps = append(c.Steps, ChecklistStep{
			Title:  "Add a custom domain",
			Detail: "Point a hostname you own at an app. Yacht verifies it and issues the certificate.",
			Href:   href, Action: "Add a domain",
			Done: f.Onboarding.HasCustomDomain,
		})
	}
	if f.Accounts {
		c.Steps = append(c.Steps, ChecklistStep{
			Title:  "Invite a teammate",
			Detail: "Give somebody else a way in, with a role that fits what they need to do.",
			Href:   "/team", Action: "Invite someone",
			Done: f.Members > 1,
		})
	}
	if c.Next() < 0 {
		return nil
	}
	return &c
}

// firstAppName is the first app in a workspace, for a step that needs one.
func firstAppName(ws app.Workspace) string {
	for _, p := range ws.Projects {
		if len(p.Apps) > 0 {
			return p.Apps[0].Name
		}
	}
	if len(ws.Unplaced) > 0 {
		return ws.Unplaced[0].Name
	}
	return ""
}

// checklistFor computes the request's checklist. Nil wherever there is no
// workspace to compute it from.
func (s *Server) checklistFor(r *http.Request) *Checklist {
	ws, ok := workspaceFromContext(r.Context())
	if !ok {
		return nil
	}
	f := checklistFacts{
		Onboarding: ws.Onboarding,
		Domains:    s.nets != nil,
		Accounts:   s.accounts != nil,
		FirstApp:   firstAppName(ws),
	}
	if f.Onboarding.Dismissed {
		return nil
	}
	if s.accounts != nil {
		owner := identity.MustFromContext(r.Context())
		members, err := s.accounts.ListMembers(r.Context(), owner.ID)
		if err != nil {
			// Unknown is not "alone". Leaving the step out is better than
			// telling a team of ten to invite somebody.
			s.log.Error("count members for the checklist", slog.String("error", err.Error()))
			f.Accounts = false
		}
		f.Members = len(members)
	}
	return gettingStarted(f)
}

// onboardingDismiss puts the checklist away for the team.
func (s *Server) onboardingDismiss(w http.ResponseWriter, r *http.Request) {
	if s.apps == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	owner := identity.MustFromContext(r.Context())
	if err := s.apps.DismissOnboarding(r.Context(), owner.ID); err != nil {
		s.log.Error("dismiss onboarding", slog.String("error", err.Error()))
		s.flashErr(w, r, "Could not hide the checklist. Try again.")
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
