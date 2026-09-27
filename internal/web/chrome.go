package web

import (
	"strings"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
)

// Helpers for the chrome's templates. In Go rather than in layout.templ for
// the reason format.go gives: they decide what somebody reads, and that is
// worth a test.

// navSections splits the navigation the way the sidebar lays it out.
//
// lead is the unheaded groups at the start — the primary navigation — which
// the Projects section follows. admin is the operator's group, wherever it
// arrived, which the sidebar draws apart at the bottom. rest is everything
// else, in the order given.
//
// Keyed on the heading because that is the one thing a wrapping application
// that reorders or extends the groups still shares with the engine: it finds
// the Admin group by AdminNavHeading to add to it, and the sidebar finds it
// the same way.
func navSections(groups []NavGroup) (lead, rest, admin []NavGroup) {
	leading := true
	for _, g := range groups {
		switch {
		case g.Heading == AdminNavHeading:
			admin = append(admin, g)
		case leading && g.Heading == "":
			lead = append(lead, g)
		default:
			leading = false
			rest = append(rest, g)
		}
	}
	return lead, rest, admin
}

// navItemName is a nav link's accessible name.
//
// The name is on the anchor because the visible label is hidden in the
// collapsed rail (see TestEveryNavLinkIsNamedInBothStates), which means the
// badge has to be in it too: otherwise a screen reader hears "Deployments" and
// never that three are running.
func navItemName(item NavItem) string {
	switch {
	case item.Badge == "":
		return item.Label
	case item.Live:
		return item.Label + ", " + item.Badge + " in progress"
	}
	return item.Label + ", " + item.Badge
}

// treeParentHref is the link that leads to the project tree, when the tree
// is showing the page being looked at; empty otherwise. See navGroup.
func treeParentHref(p *SidebarProjects) string {
	if p == nil {
		return ""
	}
	for _, pr := range p.Items {
		if pr.Active {
			return p.AllHref
		}
		for _, a := range pr.Apps {
			if a.Active {
				return p.AllHref
			}
		}
	}
	return ""
}

// projectDeploying is whether any app in a project has a deploy in flight.
func projectDeploying(p SidebarProject) bool {
	for _, a := range p.Apps {
		if app.AppState(a.State) == app.AppDeploying {
			return true
		}
	}
	return false
}

// ariaBool is a boolean as an ARIA state value. Not boolAttr, which renders
// false as empty for data attributes: aria-expanded="" is not false.
func ariaBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// treeTabIndex gives the project tree its single tab stop: the project being
// looked at, or the first one. A tree with a stop on every row turns getting
// past it into a dozen presses of Tab.
func treeTabIndex(items []SidebarProject, i int) string {
	stop := 0
	for j, p := range items {
		if p.Active || p.Open {
			stop = j
			break
		}
	}
	if i == stop {
		return "0"
	}
	return "-1"
}

// activeTeamRole is the viewer's role in the team the switcher is showing,
// by the same rule activeTeamName picks the team.
func activeTeamRole(teams []TeamChoice) account.Role {
	for _, t := range teams {
		if t.Active {
			return t.Role
		}
	}
	if len(teams) > 0 {
		return teams[0].Role
	}
	return ""
}

// roleLabel is a role as a person reads it: "Owner", not "owner".
func roleLabel(r account.Role) string {
	s := string(r)
	if s == "" {
		return "Member"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// paletteIcons are the glyphs the command palette draws beside the entries it
// builds itself — projects, apps and actions. Pages carry their own, copied
// from the sidebar link they came from.
var paletteIcons = []string{
	"boxes", "box", "rocket", "plus", "globe", "sun-moon", "arrow-right", "corner-down-left",
}
