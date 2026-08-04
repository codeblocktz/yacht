package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// ProjectListData is the page listing a team's projects.
type ProjectListData struct {
	Projects []app.Project
	Error    string
}

// projectList shows the projects a team has.
func (s *Server) projectList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	projects, err := s.apps.Projects(ctx, owner.ID)
	if err != nil {
		s.log.Error("list projects", slog.String("error", err.Error()))
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}
	s.render(w, r, ProjectList(ProjectListData{Projects: projects}))
}

// projectCreate makes a project and opens its canvas.
func (s *Server) projectCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	p, err := s.apps.CreateProject(ctx, owner.ID, r.FormValue("name"))
	if err != nil {
		projects, listErr := s.apps.Projects(ctx, owner.ID)
		if listErr != nil {
			s.log.Error("list projects", slog.String("error", listErr.Error()))
		}
		// Re-rendered rather than redirected, so the message arrives with the
		// form that caused it instead of as a detail on another page.
		s.renderStatus(w, r, http.StatusUnprocessableEntity, ProjectList(ProjectListData{Projects: projects, Error: err.Error()}))
		return
	}
	http.Redirect(w, r, "/projects/"+p.Slug, http.StatusSeeOther)
}

// projectCanvas draws a project's apps and what connects them.
func (s *Server) projectCanvas(w http.ResponseWriter, r *http.Request) {
	s.renderCanvas(w, r, chi.URLParam(r, "slug"), "")
}

// canvasRedirect keeps the old standalone canvas URL working.
//
// It was a page of its own before projects existed. Somebody has it open in a
// tab, and a 404 is a worse answer than the same picture at its new address.
func (s *Server) canvasRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/projects/"+app.DefaultProjectSlug, http.StatusMovedPermanently)
}

// appDetail draws the app's project, with that app's panel already open.
//
// The app does not get a page away from the canvas. Everything it relates to —
// the database it reads, the worker that shares it — is on the canvas, and a
// detail view that hides all of it makes the relationships invisible at exactly
// the moment somebody is changing one.
func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	s.renderCanvas(w, r, "", chi.URLParam(r, "name"))
}

// renderCanvas draws one project, optionally with an app's panel open.
//
// Both entry points come through here so there is one description of what the
// canvas is: which project, which apps, which edges, and which panel. Opening
// an app resolves the project from the app rather than from the URL, so a link
// to an app cannot land on a canvas the app is not drawn on.
func (s *Server) renderCanvas(w http.ResponseWriter, r *http.Request, slug, appName string) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	if s.apps == nil {
		http.NotFound(w, r)
		return
	}

	var open *AppDetailData
	if appName != "" {
		a, err := s.apps.Get(ctx, owner.ID, appName)
		if err != nil {
			if errors.Is(err, app.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			s.log.Error("get app", slog.String("error", err.Error()))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		open = s.appPanel(ctx, a, detailTab(r))
	}

	// An app with no project yet resolves to the default one, which is where
	// Projects would have adopted it anyway.
	if open != nil && open.App.ProjectID != uuid.Nil {
		p, err := s.apps.ProjectByID(ctx, owner.ID, open.App.ProjectID)
		if err == nil {
			slug = p.Slug
		}
	}

	project, err := s.apps.Project(ctx, owner.ID, slug)
	if err != nil {
		if errors.Is(err, app.ErrProjectNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("get project", slog.String("error", err.Error()))
		http.Error(w, "could not load project", http.StatusInternalServerError)
		return
	}

	apps, err := s.apps.ListInProject(ctx, owner.ID, project.ID)
	if err != nil {
		s.log.Error("list apps for canvas", slog.String("error", err.Error()))
		http.Error(w, "could not load apps", http.StatusInternalServerError)
		return
	}

	links, err := s.apps.Links(ctx, owner.ID)
	if err != nil {
		// The graph degrades to a plain arrangement of cards rather than
		// failing: the apps are the point, and the edges are how they relate.
		s.log.Error("list links for canvas", slog.String("error", err.Error()))
	}

	data := layout(apps, links)
	data.Project = project
	data.Open = open
	if projects, err := s.apps.Projects(ctx, owner.ID); err == nil {
		data.Projects = projects
	}

	slots := s.slots.Slots(ctx, r)
	slots.Breadcrumb = append(slots.Breadcrumb, Crumb{Label: project.Name, Href: "/projects/" + project.Slug})
	if open != nil {
		slots.Breadcrumb = append(slots.Breadcrumb, Crumb{Label: open.App.Name})
	}
	s.renderWithSlots(w, r, slots, Canvas(data))
}

// appPanel gathers what the open panel shows about one app.
func (s *Server) appPanel(ctx context.Context, a app.App, tab string) *AppDetailData {
	d := &AppDetailData{App: a, Tab: tab}

	if pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{
		Namespace: a.Namespace, ManagedOnly: true,
	}); err == nil {
		d.Pods = pods
	}
	// Routing, for the Settings tab. Read here rather than on its own page,
	// because a panel nothing links to is a panel nobody finds — which is
	// exactly what happened the first time.
	if s.nets != nil {
		if n, err := s.nets.Networking(ctx, a.OwnerID, a.Name); err == nil {
			d.Net = n
		} else {
			s.log.Error("read networking", slog.String("error", err.Error()))
		}
		d.ResolverName = s.nets.ResolverName()
	}
	if deps, err := s.apps.Deployments(ctx, a.OwnerID, a.ID, 20); err == nil {
		d.Deployments = deps
	} else {
		s.log.Error("list deployments", slog.String("error", err.Error()))
	}
	s.attachSliders(ctx, d)
	return d
}

// attachSliders builds the resource tracks from the cluster's own shape.
//
// The ceiling is the largest node rather than the cluster total: a pod runs on
// one node, so the sum is a number no workload can reach, and a slider that
// offered it would be offering a limit that guarantees the app never schedules.
//
// A cluster that will not answer leaves the defaults in place rather than
// failing the page — the settings still work, they just lose the comparison.
func (s *Server) attachSliders(ctx context.Context, d *AppDetailData) {
	var cpuMax, memMax int64
	if nodes, err := s.orch.Nodes(ctx); err == nil {
		cpuMax, memMax = schedulableCPU(nodes), schedulableMemory(nodes)
	}

	d.CPULimit = cpuSlider("cpu_limit", "CPU", d.App.CPULimit, cpuMax)
	d.MemoryLimit = memSlider("memory_limit", "Memory", d.App.MemoryLimit, memMax)
	d.CPURequest = cpuSlider("cpu_request", "CPU", d.App.CPURequest, cpuMax)
	d.MemoryRequest = memSlider("memory_request", "Memory", d.App.MemoryRequest, memMax)
}

// deploymentsFragment is the polled half of the deployments tab.
//
// Its own endpoint rather than re-rendering the tab, so a request every three
// seconds does not replace the pods table and the log buttons under whoever is
// reading them.
func (s *Server) deploymentsFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")

	a, err := s.apps.Get(ctx, owner.ID, name)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("read app", slog.String("error", err.Error()))
		http.Error(w, "could not read the app", http.StatusInternalServerError)
		return
	}

	if err := appDeployments(*s.appPanel(ctx, a, "")).Render(ctx, w); err != nil {
		s.log.Error("render deployments fragment", slog.String("error", err.Error()))
	}
}

// canvasPosition records where a card was dragged to.
//
// A small write on its own endpoint rather than part of a form: dragging is not
// a submission, and a page reload in the middle of arranging things should keep
// what was already moved.
func (s *Server) canvasPosition(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	x, errX := strconv.Atoi(r.FormValue("x"))
	y, errY := strconv.Atoi(r.FormValue("y"))
	if errX != nil || errY != nil {
		http.Error(w, "x and y must be numbers", http.StatusBadRequest)
		return
	}

	// Clamped rather than rejected. A card dragged off the top-left is a
	// gesture that overshot, not a request to store a negative coordinate that
	// would draw it where nobody can reach it again.
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	if err := s.apps.SetPosition(ctx, owner.ID, chi.URLParam(r, "name"), int32(x), int32(y)); err != nil {
		if errors.Is(err, app.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("set position", slog.String("error", err.Error()))
		http.Error(w, "could not save position", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// canvasArrange forgets a project's positions so it lays out again.
func (s *Server) canvasArrange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)

	project, err := s.apps.Project(ctx, owner.ID, chi.URLParam(r, "slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.apps.ClearPositions(ctx, owner.ID, project.ID); err != nil {
		s.log.Error("clear positions", slog.String("error", err.Error()))
		http.Error(w, "could not rearrange", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+project.Slug, http.StatusSeeOther)
}
