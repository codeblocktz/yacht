package web

import (
	"strings"

	"github.com/codeblocktz/yacht/internal/app"
)

// ProjectBar is the chrome of a project page: which project, which
// environment, the team's other projects to switch to, and the project's
// sections down the rail.
type ProjectBar struct {
	Name string
	Slug string

	// Environment is the one being looked at. There is one until
	// environments exist; it is named anyway, so the bar reads the way it
	// will when there are more.
	Environment string

	// Projects are the team's, for the switcher.
	Projects []ProjectChoice

	// Section is which of the rail's entries is showing.
	Section string
}

// ProjectChoice is one project in the switcher.
type ProjectChoice struct {
	Name   string
	Href   string
	Apps   int
	Active bool
}

// The rail's sections.
const (
	SectionArchitecture = "architecture"
	SectionSettings     = "settings"
)

// projectBarFor works out the project a path is about: a canvas, a
// project's settings, or an app — which belongs to the project it is drawn
// on. Nil for anything else, and for a request with no workspace to read.
func projectBarFor(ws app.Workspace, ok bool, path string) *ProjectBar {
	if !ok || !isCanvasPath(path) {
		return nil
	}
	slug, appName := activeInTree(path)
	section := SectionArchitecture
	if rest, found := strings.CutPrefix(path, "/projects/"+slug+"/"); found && slug != "" {
		if strings.HasPrefix(rest, "settings") {
			section = SectionSettings
		}
	}

	bar := &ProjectBar{Environment: app.DefaultEnvironment, Section: section}
	for _, p := range ws.Projects {
		current := p.Slug == slug
		if appName != "" {
			for _, a := range p.Apps {
				current = current || a.Name == appName
			}
		}
		if current && bar.Slug == "" {
			bar.Name, bar.Slug = p.Name, p.Slug
		}
		bar.Projects = append(bar.Projects, ProjectChoice{
			Name: p.Name, Href: "/projects/" + p.Slug, Apps: len(p.Apps), Active: current,
		})
	}
	if bar.Slug == "" {
		// An app with no project yet, or a project the workspace has not
		// caught up with: the dashboard's chrome is the honest answer.
		return nil
	}
	return bar
}

// projectHref is a project's own address, for the rail.
func projectHref(b ProjectBar, section string) string {
	if section == SectionSettings {
		return "/projects/" + b.Slug + "/settings"
	}
	return "/projects/" + b.Slug
}
