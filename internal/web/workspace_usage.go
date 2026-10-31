package web

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// WorkspaceUsageData is the workspace's Usage page: the month's total, as a
// wrapping application's meter sums it, and what each project has cost.
type WorkspaceUsageData struct {
	// Summary is the month as a whole: nil when the meter prices projects
	// but does not total them, which leaves the page at the projects alone.
	Summary *UsageSummary

	// Month is the month shown, its first instant in UTC, and Prev and Next
	// the months either side as ?month= values; Next is empty for this month.
	Month      time.Time
	Prev, Next string

	Projects []ProjectUsageRow
	// Note is the meter's fine print, once for the page.
	Note string
}

// ProjectUsageRow is one project's usage.
type ProjectUsageRow struct {
	Project app.Project
	Usage   ProjectUsage
}

// UsageSummarizer is a meter that also totals the workspace's month: what it
// has used, what the plan includes, the bill it comes to, and the line items
// behind it. Optional: a UsageMeter that implements it gives /usage the
// summary card above the projects. As with ProjectUsage, every amount
// arrives already written in the wrapper's money.
type UsageSummarizer interface {
	WorkspaceUsage(ctx context.Context, req WorkspaceUsageRequest) (UsageSummary, error)
}

// WorkspaceUsageRequest asks for one month of a workspace.
type WorkspaceUsageRequest struct {
	OwnerID string
	// Month is the first instant of the month asked for, in UTC; Now is the
	// moment of asking, which is within Month when it is this one.
	Month, Now time.Time
}

// UsageSummary is the month as the Usage page's summary card draws it.
type UsageSummary struct {
	// Period names the month, "1 – 30 Nov".
	Period string

	// Current is what the month's usage has come to, Included what the plan
	// includes of it, and Bill what the month is charged, or is estimated to
	// be by its end: BillLabel says which ("Estimated bill", "Bill").
	Current, Included, Bill, BillLabel string

	// Limit is a spending limit and how much of it is used; nil for none.
	Limit *UsageLimit

	// Breakdown is the month's usage by resource, then Totals the lines that
	// take it to the bill: the subtotal, what the plan includes, its fee.
	Breakdown []UsageCharge
	Totals    []UsageTotal

	// Plan is the plan line under the card; nil draws none.
	Plan *UsagePlan

	// Past is a month already over: its projects are not priced again, so
	// the page shows the summary alone.
	Past bool

	// Note is the summary's fine print.
	Note string
}

// UsageLimit is a cap on what a month may be charged.
type UsageLimit struct {
	// Label is "Spending limit"; Used and Of are its two amounts.
	Label, Used, Of string
	Percent         int
	// Href is where to set it, labelled ActionLabel. Empty draws no button.
	Href, ActionLabel string
}

// UsageCharge is one resource in the breakdown.
type UsageCharge struct {
	Name, Quantity, UnitPrice, Cost string
}

// UsageTotal is one line under the breakdown. Strong is the bill itself.
type UsageTotal struct {
	Label, Amount string
	Strong        bool
}

// UsagePlan is the plan the workspace is on, and where to change it.
type UsagePlan struct {
	Name, Detail, Includes string
	Href, HrefLabel        string
}

// workspaceUsage totals the month, where the meter can, and prices every
// project. Project amounts arrive written in the wrapping application's
// money, so they are listed rather than added up: the engine has no
// arithmetic for a currency it does not know, and the total is the meter's.
func (s *Server) workspaceUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	now := time.Now().UTC()
	this := monthStart(now)
	d := WorkspaceUsageData{Month: this}
	if m, err := time.Parse("2006-01", r.URL.Query().Get("month")); err == nil && m.Before(this) {
		d.Month = m.UTC()
	}
	d.Prev = d.Month.AddDate(0, -1, 0).Format("2006-01")
	if d.Month.Before(this) {
		d.Next = d.Month.AddDate(0, 1, 0).Format("2006-01")
	}
	if sum, ok := s.usage.(UsageSummarizer); ok {
		u, err := sum.WorkspaceUsage(ctx, WorkspaceUsageRequest{OwnerID: owner.ID, Month: d.Month, Now: now})
		if err != nil {
			s.log.Error("total the workspace's usage", slog.String("error", err.Error()))
		} else {
			d.Summary = &u
		}
	}
	if d.Month.Before(this) {
		// Projects are priced for this month only; an earlier one is the
		// meter's summary or nothing.
		s.render(w, r, WorkspaceUsage(d))
		return
	}
	projects, err := s.apps.Projects(ctx, owner.ID)
	if err != nil {
		s.log.Error("list projects for usage", slog.String("error", err.Error()))
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}
	for _, p := range projects {
		apps, err := s.apps.ListInProject(ctx, owner.ID, p.ID)
		if err != nil {
			s.log.Warn("list the project's apps", slog.String("project", p.Slug), slog.String("error", err.Error()))
			continue
		}
		u := s.projectUsage(ctx, owner.ID, p, apps)
		if u == nil {
			continue
		}
		if d.Note == "" {
			d.Note = u.Note
		}
		d.Projects = append(d.Projects, ProjectUsageRow{Project: p, Usage: *u})
	}
	s.render(w, r, WorkspaceUsage(d))
}
