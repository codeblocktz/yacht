package web

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
)

// UsageMeter prices a project: what it has cost this month so far, and what
// the month will come to at the rate it is running now.
//
// The engine has no prices and no money — those are a wrapping application's
// (make boundary-check holds it to that) — so it hands over what it knows,
// which apps and how much each sent to the internet, and draws whatever
// comes back. Every amount arrives already written in the wrapper's own
// currency and style.
type UsageMeter interface {
	ProjectUsage(ctx context.Context, req UsageRequest) (ProjectUsage, error)
}

// UsageRequest is one project's apps, for the meter to price.
type UsageRequest struct {
	OwnerID string
	Project app.Project
	Apps    []UsageApp

	// Since and Now are the period: the start of this calendar month, in UTC,
	// and the moment of asking.
	Since, Now time.Time

	// EgressKnown is whether the install measures egress at all, which needs
	// the metrics store; without it every app's EgressBytes is zero and means
	// "not measured" rather than "none".
	EgressKnown bool
}

// UsageApp is one app in a project, as the meter is told of it.
type UsageApp struct {
	ID   uuid.UUID
	Name string

	// EgressBytes is what the app sent to the internet through the ingress
	// since the period began: its responses, counted by Traefik.
	EgressBytes int64
}

// ProjectUsage is a project's cost, written out.
type ProjectUsage struct {
	// Period names the span the current amounts cover, such as "1–25 Oct".
	Period string

	// Current is the cost so far this period, and Estimated what the whole
	// period comes to at the current rate.
	Current, Estimated string

	// Resources split it by what was used: memory, CPU, egress, volumes.
	Resources []UsageLine

	// Services split it by app, where the meter can.
	Services []UsageLine

	// Note is the fine print: what the amounts include and leave out.
	Note string
}

// UsageLine is one row of a usage table.
type UsageLine struct {
	Name string
	// Detail is the quantity behind the amount, such as "2.0 GiB average".
	Detail             string
	Current, Estimated string
}

// monthStart is the first instant of t's calendar month, in UTC.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// projectUsage asks the meter about a project, measuring each app's egress
// first where there is a metrics store to measure it with.
func (s *Server) projectUsage(ctx context.Context, ownerID string, p app.Project, apps []app.App) *ProjectUsage {
	if s.usage == nil {
		return nil
	}
	now := time.Now().UTC()
	req := UsageRequest{OwnerID: ownerID, Project: p, Since: monthStart(now), Now: now}
	var egress map[string]int64
	if s.metricStore != nil {
		if scope, err := scopeFor(ownerID, apps); err == nil && !scope.Empty() {
			e, err := s.metricStore.PublicEgress(ctx, scope, req.Since, now)
			if err != nil {
				s.log.Warn("measure egress", slog.String("error", err.Error()))
			} else {
				egress, req.EgressKnown = e, true
			}
		}
	}
	for _, a := range apps {
		req.Apps = append(req.Apps, UsageApp{ID: a.ID, Name: a.Name, EgressBytes: egress[a.Name]})
	}
	u, err := s.usage.ProjectUsage(ctx, req)
	if err != nil {
		s.log.Error("price the project", slog.String("project", p.Slug), slog.String("error", err.Error()))
		return nil
	}
	return &u
}
