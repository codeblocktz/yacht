package web

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/codeblocktz/yacht/internal/account"
)

// allImpersonations is the account service's one read across teams.
// Asserted rather than added to Accounts, so a wrapping application's own
// accounts need not answer it.
type allImpersonations interface {
	AllImpersonationEvents(ctx context.Context, limit int32) ([]account.ImpersonationEvent, error)
}

// Impersonation is one operator's visit to a team: when it began, why, and
// when it ended.
type Impersonation struct {
	TeamID, TeamName string
	Operator         string
	Reason           string
	Started          time.Time
	// Ended is zero while it has no stop: still going.
	Ended time.Time
}

// ImpersonationsData is the Admin console's list of them.
type ImpersonationsData struct {
	Items []Impersonation
	Error string
}

// adminImpersonations lists every operator's acting as a team, across the
// install: the operators' own record, beside each team's on its page and in
// its audit log.
//
// Read from the account service's record rather than the audit log: every
// way an impersonation ends — Stop, signing out, switching team, losing the
// right to act — writes its stop there, and only the first is a request the
// audit log sees.
func (s *Server) adminImpersonations(w http.ResponseWriter, r *http.Request) {
	l, ok := s.accounts.(allImpersonations)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var d ImpersonationsData
	events, err := l.AllImpersonationEvents(r.Context(), 1000)
	if err != nil {
		s.log.Error("list impersonations", slog.String("error", err.Error()))
		d.Error = "Could not read the impersonations."
	}
	d.Items = pairImpersonations(events)
	s.render(w, r, AdminImpersonations(d))
}

// pairImpersonations matches each start with the stop that ended it: the
// next stop by the same operator in the same team.
func pairImpersonations(events []account.ImpersonationEvent) []Impersonation {
	sorted := append([]account.ImpersonationEvent(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	type key struct{ team, operator string }
	open := map[key]int{}
	var out []Impersonation
	for _, e := range sorted {
		k := key{e.TeamID, e.OperatorEmail}
		switch e.Action {
		case account.ImpersonationStart:
			out = append(out, Impersonation{
				TeamID: e.TeamID, TeamName: cmp.Or(e.TeamName, e.TeamID), Operator: e.OperatorEmail,
				Reason: e.Reason, Started: e.At,
			})
			open[k] = len(out) - 1
		case account.ImpersonationStop:
			if i, ok := open[k]; ok {
				out[i].Ended = e.At
				delete(open, k)
			}
		}
	}
	// Newest first, as every log on the dashboard reads.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// impersonationLength is how long a visit lasted, in words.
func impersonationLength(i Impersonation) string {
	if i.Ended.IsZero() {
		return "ongoing"
	}
	d := i.Ended.Sub(i.Started).Round(time.Minute)
	if d < time.Minute {
		return "under a minute"
	}
	return d.String()
}
