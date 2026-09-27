package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The Admin area: everything about running the install, for the people who
// run it. The teams on it and what each may take, whether the cluster has room
// for what they have taken, and — under their old /cluster URLs — the machines
// and install-wide settings underneath.
//
// It is also where an application wrapping the engine adds its own
// install-wide pages: it mounts them with ExtraRoutes.Operator, behind the same
// gate, and appends them to the sidebar group headed AdminNavHeading.

// AdminNavHeading is the sidebar group the Admin pages sit in. Named so a
// wrapping application's SlotProvider can find the group and append to it
// rather than draw a second one beside it.
const AdminNavHeading = "Admin"

// Quotas is the dashboard's view of what each team may commit.
type Quotas interface {
	// TeamUsages is every team on the install, with what each has committed
	// and what it may.
	TeamUsages(ctx context.Context) ([]app.TeamUsage, error)
	TeamUsage(ctx context.Context, teamID string) (app.TeamUsage, error)

	// SetQuota replaces a team's quota. It stops nothing already running;
	// it refuses what would raise use past it from then on.
	SetQuota(ctx context.Context, teamID string, q app.Quota) error
}

var _ Quotas = (*app.Service)(nil)

// AdminTeamsData is the list of every team.
type AdminTeamsData struct {
	Teams []app.TeamUsage

	// Accounts is whether there are people to count. Without an account
	// service nobody is a member of anything, and a column of zeros would
	// read as a team nobody is in.
	Accounts bool

	Error string
}

// Total is the whole install's committed use, for the strip above the list.
func (d AdminTeamsData) Total() app.Usage {
	var u app.Usage
	for _, t := range d.Teams {
		u.Apps += t.Usage.Apps
		u.CPUMillis += t.Usage.CPUMillis
		u.MemoryBytes += t.Usage.MemoryBytes
		u.StorageBytes += t.Usage.StorageBytes
	}
	return u
}

// AdminTeamData is one team's quota, as a form.
type AdminTeamData struct {
	Team     app.TeamUsage
	Accounts bool
	Form     QuotaForm
	Error    string
}

// QuotaForm is a quota in the units a person types: apps, vCPU, GiB. Strings,
// so a value that was refused is shown back exactly as it was typed rather
// than as whatever it failed to parse to.
type QuotaForm struct {
	Apps    string
	CPU     string
	Memory  string
	Storage string
}

// quotaForm renders a stored quota for editing. Zero is left empty rather than
// written as 0: the field's placeholder says "No limit", which is what zero
// means, and a 0 in the box reads as "none allowed".
func quotaForm(q app.Quota) QuotaForm {
	f := QuotaForm{}
	if q.Apps > 0 {
		f.Apps = strconv.Itoa(int(q.Apps))
	}
	if q.CPUMillis > 0 {
		f.CPU = strconv.FormatFloat(float64(q.CPUMillis)/1000, 'f', -1, 64)
	}
	if q.MemoryBytes > 0 {
		f.Memory = strconv.FormatFloat(roundTo(float64(q.MemoryBytes)/(1<<30), 3), 'f', -1, 64)
	}
	if q.StorageBytes > 0 {
		f.Storage = strconv.FormatFloat(roundTo(float64(q.StorageBytes)/(1<<30), 3), 'f', -1, 64)
	}
	return f
}

// Parse reads the form back into a quota, naming the field that is wrong.
//
// Each has a ceiling well past any real cluster. Not a policy — a guard, so
// that a mistyped row of nines is refused here rather than overflowing into a
// negative number of bytes on its way to the database.
func (f QuotaForm) Parse() (app.Quota, error) {
	var q app.Quota
	apps, err := quotaNumber(f.Apps, "Apps", 100_000)
	if err != nil {
		return q, err
	}
	if apps != math.Trunc(apps) {
		return q, errors.New("Apps must be a whole number")
	}
	cpu, err := quotaNumber(f.CPU, "CPU", 100_000)
	if err != nil {
		return q, err
	}
	mem, err := quotaNumber(f.Memory, "Memory", 1<<20)
	if err != nil {
		return q, err
	}
	storage, err := quotaNumber(f.Storage, "Storage", 1<<20)
	if err != nil {
		return q, err
	}
	q.Apps = int32(apps)
	q.CPUMillis = int64(math.Round(cpu * 1000))
	q.MemoryBytes = int64(math.Round(mem * (1 << 30)))
	q.StorageBytes = int64(math.Round(storage * (1 << 30)))
	return q, nil
}

func quotaNumber(s, field string, ceiling float64) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseFloat(s, 64)
	switch {
	case err != nil || math.IsNaN(n) || math.IsInf(n, 0):
		return 0, fmt.Errorf("%s must be a number — %q is not one", field, s)
	case n < 0:
		return 0, fmt.Errorf("%s cannot be negative — leave it empty for no limit", field)
	case n > ceiling:
		return 0, fmt.Errorf("%s is larger than any cluster this could be — %s at most", field,
			strconv.FormatFloat(ceiling, 'f', -1, 64))
	}
	return n, nil
}

func roundTo(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func (s *Server) adminTeams(w http.ResponseWriter, r *http.Request) {
	d := AdminTeamsData{Accounts: s.accounts != nil}
	teams, err := s.quotas.TeamUsages(r.Context())
	if err != nil {
		s.log.Error("list teams for admin", slog.String("error", err.Error()))
		d.Error = "Could not read the teams on this install."
	}
	d.Teams = teams
	s.render(w, r, AdminTeams(d))
}

func (s *Server) adminTeam(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminTeamOr404(w, r)
	if !ok {
		return
	}
	s.renderWithCrumb(w, r, AdminTeam(AdminTeamData{
		Team: t, Accounts: s.accounts != nil, Form: quotaForm(t.Quota),
	}), t.TeamName)
}

func (s *Server) adminTeamQuota(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminTeamOr404(w, r)
	if !ok {
		return
	}
	form := QuotaForm{
		Apps: r.FormValue("apps"), CPU: r.FormValue("cpu"),
		Memory: r.FormValue("memory"), Storage: r.FormValue("storage"),
	}
	refuse := func(msg string) {
		// Re-rendered rather than redirected, so what was typed is still in
		// the boxes beside the reason it was refused.
		slots := s.slots.Slots(r.Context(), r)
		slots.Breadcrumb = append(slots.Breadcrumb, Crumb{Label: t.TeamName})
		s.renderWithSlotsStatus(w, r, slots, http.StatusUnprocessableEntity, AdminTeam(AdminTeamData{
			Team: t, Accounts: s.accounts != nil, Form: form, Error: msg,
		}))
	}

	q, err := form.Parse()
	if err != nil {
		refuse(err.Error())
		return
	}
	if err := s.quotas.SetQuota(r.Context(), t.TeamID, q); err != nil {
		s.log.Warn("set team quota", slog.String("team", t.TeamID), slog.String("error", err.Error()))
		refuse(err.Error())
		return
	}
	s.flashOK(w, r, "Quota saved for "+t.TeamName+". It applies to the next change the team makes; "+
		"nothing already running was stopped.")
	http.Redirect(w, r, adminTeamHref(t.TeamID), http.StatusSeeOther)
}

// adminTeamOr404 reads the team a URL names. A team that does not exist is a
// 404: the operator may see every team, so saying which do not exist tells
// them nothing they could not read from the list.
func (s *Server) adminTeamOr404(w http.ResponseWriter, r *http.Request) (app.TeamUsage, bool) {
	t, err := s.quotas.TeamUsage(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, app.ErrNoSuchTeam) {
			http.NotFound(w, r)
			return t, false
		}
		s.log.Error("read team for admin", slog.String("error", err.Error()))
		http.Error(w, "could not read the team", http.StatusInternalServerError)
		return t, false
	}
	return t, true
}

func adminTeamHref(id string) string {
	return "/admin/teams/" + url.PathEscape(id)
}

// Presentation, kept in Go for the reason format.go gives: a meter past 100%
// or a limit labelled as use is a bug somebody will believe.

// quotaPercent is how much of a limit is committed. Zero when there is no
// limit, which the templates check first and draw as "no limit" rather than
// as an empty bar.
func quotaPercent(used, limit int64) int {
	if limit <= 0 {
		return 0
	}
	return int(math.Round(float64(used) * 100 / float64(limit)))
}

// cpuAmount and byteAmount write amounts in the units the quota form takes —
// vCPU and GiB — to two places at most. Finer than formatMillicores on
// purpose: 960m rounded to "1 vCPU" beside a quota of 1 reads as full when it
// is not, and this is the page where that difference is the point.
func cpuAmount(millis int64) string {
	return strconv.FormatFloat(roundTo(float64(millis)/1000, 2), 'f', -1, 64) + " vCPU"
}

// byteAmount says MiB below one GiB, where "0.12 GiB" is a number nobody
// recognises as the 128 MiB default it is.
func byteAmount(b int64) string {
	switch {
	case b == 0:
		return "0"
	case b < 1<<30:
		return strconv.FormatFloat(math.Round(float64(b)/(1<<20)), 'f', -1, 64) + " MiB"
	}
	return strconv.FormatFloat(roundTo(float64(b)/(1<<30), 2), 'f', -1, 64) + " GiB"
}

// usedOf writes "used / limit" with the unit once when the two share it —
// "2.4 / 3 vCPU" — which is what keeps a table cell to one line.
func usedOf(used, limit string) (string, string) {
	if i := strings.LastIndexByte(used, ' '); i >= 0 && strings.HasSuffix(limit, used[i:]) {
		return used[:i], limit
	}
	return used, limit
}

func appAmount(n int64) string {
	return plural(int(n), "app")
}

// defaultLimitsNote says what an app with no limit is counted at, from the
// same defaults the namespace is given, so the page cannot drift from them.
func defaultLimitsNote() string {
	d := orchestrator.ResourceLimits{}.OrDefaults()
	return cpuAmount(parseCPUMillis(d.DefaultCPU)) + " and " + byteAmount(parseMemMiB(d.DefaultMemory)<<20)
}

// overQuota reports a team committed past any of its limits — which happens
// only when an operator lowers one below what is running, and which the page
// says rather than leaving to a red bar somewhere down the form.
func overQuota(t app.TeamUsage) bool {
	q, u := t.Quota, t.Usage
	return (q.Apps > 0 && u.Apps > int64(q.Apps)) ||
		(q.CPUMillis > 0 && u.CPUMillis > q.CPUMillis) ||
		(q.MemoryBytes > 0 && u.MemoryBytes > q.MemoryBytes) ||
		(q.StorageBytes > 0 && u.StorageBytes > q.StorageBytes)
}

func committedNow(used int64, label string) string {
	if used == 0 {
		return "Nothing committed now"
	}
	return label + " committed now"
}

func membersLabel(t app.TeamUsage, accounts bool) string {
	if !accounts {
		return "—"
	}
	return strconv.FormatInt(t.Members, 10)
}
