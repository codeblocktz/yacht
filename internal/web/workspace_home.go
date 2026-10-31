package web

import (
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// WorkspaceHomeData is the workspace's home: its projects as Railway draws
// them, each a small canvas of its services with how many are up.
type WorkspaceHomeData struct {
	Cards []ProjectCard
	// Total is every project the team has, whatever the search left out.
	Total int

	// Query, Sort and View are the page's controls as they were set.
	Query string
	Sort  string
	View  string

	// Error is why a project could not be made, shown with the form that
	// made it; Focus opens that form.
	Error string
	Focus bool

	// Checklist is getting started, for a team that has not finished it.
	Checklist *Checklist

	// Templates offers starting from one, where the install has them.
	Templates bool

	// Note is the chrome's ProductNote, said in the empty page a new team
	// first lands on.
	Note string
}

// ProjectCard is one project on the home page.
type ProjectCard struct {
	Project app.Project
	// Services are production's, as many as the card draws; More is how
	// many it leaves out.
	Services []app.App
	More     int
	// Online and Count are production's services that are up, of all of them.
	Online, Count int
	// Active is the last time anything in the project changed.
	Active time.Time
}

// projectCardServices is how many services a card draws before "+N".
const projectCardServices = 7

// Sorts and views the home page offers.
const (
	sortRecent = "recent"
	sortName   = "name"
	viewGrid   = "grid"
	viewList   = "list"
	viewCookie = "yacht_projects_view"
)

// workspaceHome is the workspace's home page, and its Projects page: one
// list, so the sidebar's Projects and the brand lead to the same place.
func (s *Server) workspaceHome(w http.ResponseWriter, r *http.Request) {
	s.renderWorkspaceHome(w, r, http.StatusOK, "")
}

func (s *Server) renderWorkspaceHome(w http.ResponseWriter, r *http.Request, status int, fail string) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	q := r.URL.Query()
	// Asked once, for the page's note and for the chrome around it.
	slots := s.slots.Slots(ctx, r)
	d := WorkspaceHomeData{
		Note:  slots.ProductNote,
		Query: strings.TrimSpace(q.Get("q")),
		Sort:  sortRecent,
		View:  viewGrid,
		Error: fail,
		Focus: fail != "" || q.Get("new") != "",

		Checklist: s.checklistFor(r),
		Templates: s.stacks != nil,
	}
	if q.Get("sort") == sortName {
		d.Sort = sortName
	}
	// The view is remembered, as Railway's is: it is how somebody likes to
	// look at their projects, not a question about this visit.
	switch v := q.Get("view"); v {
	case viewGrid, viewList:
		d.View = v
		http.SetCookie(w, &http.Cookie{Name: viewCookie, Value: v, Path: "/", MaxAge: 365 * 24 * 3600,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: requestIsTLS(r)})
	default:
		if c, err := r.Cookie(viewCookie); err == nil && c.Value == viewList {
			d.View = viewList
		}
	}

	// An install with no app service has no projects: the empty page, not
	// a nil dereference.
	if s.apps == nil {
		s.renderWithSlotsStatus(w, r, slots, status, WorkspaceHome(d))
		return
	}
	projects, err := s.apps.Projects(ctx, owner.ID)
	if err != nil {
		s.log.Error("list projects", slog.String("error", err.Error()))
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}
	apps, err := s.apps.List(ctx, owner.ID)
	if err != nil {
		// The projects are still worth showing without what is in them.
		s.log.Error("list apps for the projects", slog.String("error", err.Error()))
	}
	d.Total = len(projects)
	d.Cards = projectCards(projects, apps, d.Query, d.Sort)
	s.renderWithSlotsStatus(w, r, slots, status, WorkspaceHome(d))
}

// projectCards draws each project from its production services, filtered
// by a search and sorted.
func projectCards(projects []app.Project, apps []app.App, query, by string) []ProjectCard {
	byProject := map[string][]app.App{}
	active := map[string]time.Time{}
	for _, a := range apps {
		key := a.ProjectID.String()
		if a.UpdatedAt.After(active[key]) {
			active[key] = a.UpdatedAt
		}
		if a.Environment() != app.DefaultEnvironment {
			continue
		}
		byProject[key] = append(byProject[key], a)
	}
	needle := strings.ToLower(query)
	cards := make([]ProjectCard, 0, len(projects))
	for _, p := range projects {
		if needle != "" && !strings.Contains(strings.ToLower(p.Name), needle) &&
			!strings.Contains(p.Slug, needle) {
			continue
		}
		services := byProject[p.ID.String()]
		sort.Slice(services, func(i, j int) bool { return services[i].ServiceName() < services[j].ServiceName() })
		c := ProjectCard{Project: p, Count: len(services), Active: active[p.ID.String()]}
		for _, a := range services {
			// Up as its card says: a bucket has nothing running to be
			// online, and is up whenever it is there.
			if _, class := cardStatus(a); class == "card-status-ok" {
				c.Online++
			}
		}
		c.Services = services
		if len(services) > projectCardServices {
			c.Services, c.More = services[:projectCardServices-1], len(services)-(projectCardServices-1)
		}
		cards = append(cards, c)
	}
	sort.SliceStable(cards, func(i, j int) bool {
		if by == sortRecent && !cards[i].Active.Equal(cards[j].Active) {
			return cards[i].Active.After(cards[j].Active)
		}
		return strings.ToLower(cards[i].Project.Name) < strings.ToLower(cards[j].Project.Name)
	})
	return cards
}

// projectsSummary is the line under the cards: "production · 2/2 services
// online", or that there is nothing in it.
func projectsSummary(c ProjectCard) string {
	if c.Count == 0 {
		return "No services"
	}
	return app.DefaultEnvironment + " · " + itoa(c.Online) + "/" + itoa(c.Count) + " services online"
}

// projectsDot colours a card's dot: green when everything is up, amber when
// some of it is, grey when there is nothing or nothing is.
func projectsDot(c ProjectCard) string {
	switch {
	case c.Count > 0 && c.Online == c.Count:
		return "text-success"
	case c.Online > 0:
		return "text-warning"
	}
	return "text-muted-foreground"
}

// serviceMark picks the mark a project card draws for a service: a
// database's logo, GitHub's for a repository there, a package for an image.
func serviceMarkIcon(a app.App) string {
	switch {
	case a.Source == app.SourceGit && (a.Repo.GitHub.Set() || strings.Contains(a.Repo.URL, "github.com")):
		return "github"
	case a.Source == app.SourceGit:
		return "box"
	}
	return sourceIcon(a.Source)
}

// homeHref is the home page with one of its controls changed and the rest
// kept.
func homeHref(d WorkspaceHomeData, key, value string) string {
	v := map[string]string{"q": d.Query, "sort": d.Sort, "view": d.View}
	v[key] = value
	var parts []string
	for _, k := range []string{"q", "sort", "view"} {
		if v[k] != "" {
			parts = append(parts, k+"="+url.QueryEscape(v[k]))
		}
	}
	return "/?" + strings.Join(parts, "&")
}
