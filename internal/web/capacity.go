package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Capacity: whether the cluster has room for what its teams have committed.
//
// The question an operator of a shared install has to answer before a
// customer does, by finding their deploy stuck Pending. Quotas bound each team;
// the capacity policy bounds their sum, and this page puts the machines, the
// policy and the teams side by side.

// capacityWarnPercent is how much of the cluster may be committed before the
// page says to add a node, on an install with no capacity policy behind it.
// Not 100: a node takes minutes to join, a drain needs somewhere to move pods
// to, and a rollout briefly runs old and new pods together — all of which need
// room the committed figure does not show. The policy's default is the same.
const capacityWarnPercent = 80

// Capacity is the dashboard's view of how much of the install is sold.
//
// Its own interface rather than more of Quotas, because a wrapper may supply
// one without the other, and because every method here reads across teams.
type Capacity interface {
	// Capacity is the install's room now: the machines, the policy, and what
	// every team has committed together.
	Capacity(ctx context.Context) (app.CapacitySnapshot, error)

	// SetCapacityPolicy replaces the policy. It stops nothing running; it
	// refuses, from the next change on, what would sell past it.
	SetCapacityPolicy(ctx context.Context, p app.CapacityPolicy) error

	// CapacityRefusals is the changes refused for want of room since a
	// moment, newest first.
	CapacityRefusals(ctx context.Context, since time.Time, limit int32) ([]app.CapacityRefusal, error)
}

var _ Capacity = (*app.Service)(nil)

// CapacityData is the cluster's room beside every team's commitments.
type CapacityData struct {
	OK    bool
	Error string

	// Nodes is every machine; Schedulable the ones new pods can land on —
	// ready and not cordoned — which are the only ones whose room counts.
	Nodes       int
	Schedulable int

	CPUAllocatable int64
	MemAllocatable int64

	// CPUUsed and MemUsed are what the schedulable nodes report using now,
	// known only where metrics-server is installed.
	CPUUsed    int64
	MemUsed    int64
	UsageKnown bool

	// Committed is every team's committed use together, counted exactly as a
	// quota counts it.
	Committed app.Usage

	// Teams is the same, per team, largest share of the cluster first.
	Teams []app.TeamUsage

	// CanAddNode links the warning to the page that adds one, where that page
	// exists.
	CanAddNode bool

	// Snapshot is the install's capacity under its policy; nil where the
	// server has no Capacity, which leaves the page as it was before there
	// was a policy to show.
	Snapshot *app.CapacitySnapshot

	// Form is the policy as a form, and FormError why it was not saved.
	Form      CapacityPolicyForm
	FormError string

	// Refusals is the last week's changes refused for want of room, newest
	// first.
	Refusals []app.CapacityRefusal
}

// addNodes totals the room on the machines new pods can be placed on — see
// app.RoomOf, which admission reads the same way.
func (d *CapacityData) addNodes(nodes []orchestrator.NodeInfo) {
	d.addRoom(app.RoomOf(nodes))
}

func (d *CapacityData) addRoom(r app.Room) {
	d.Nodes, d.Schedulable = r.Nodes, r.Schedulable
	d.CPUAllocatable, d.MemAllocatable = r.CPUMillis, r.MemoryBytes
	d.CPUUsed, d.MemUsed, d.UsageKnown = r.CPUUsedMillis, r.MemoryUsedBytes, r.UsageKnown
}

// addSnapshot reads the machines from the snapshot rather than from the
// cluster, so the page and admission agree on the room they are looking at.
func (d *CapacityData) addSnapshot(s app.CapacitySnapshot) {
	d.Snapshot = &s
	d.addRoom(app.Room{
		Known: s.Known, Nodes: s.Nodes, Schedulable: s.Schedulable,
		CPUMillis: s.CPU.Allocatable, MemoryBytes: s.Memory.Allocatable,
		CPUUsedMillis: s.CPU.Used, MemoryUsedBytes: s.Memory.Used, UsageKnown: s.UsageKnown,
	})
}

// addTeams totals every team's commitments and orders them by how much of the
// cluster each holds, so the team to talk to is at the top.
func (d *CapacityData) addTeams(teams []app.TeamUsage) {
	for _, t := range teams {
		d.Committed.Apps += t.Usage.Apps
		d.Committed.CPUMillis += t.Usage.CPUMillis
		d.Committed.MemoryBytes += t.Usage.MemoryBytes
		d.Committed.StorageBytes += t.Usage.StorageBytes
	}
	d.Teams = slices.Clone(teams)
	slices.SortStableFunc(d.Teams, func(a, b app.TeamUsage) int {
		if c := cmp.Compare(d.share(b), d.share(a)); c != 0 {
			return c
		}
		return strings.Compare(strings.ToLower(a.TeamName), strings.ToLower(b.TeamName))
	})
}

// share is the larger of a team's CPU and memory fractions of the cluster —
// whichever it is closer to running out of. With no room known it falls back
// to raw CPU, so the order still means something on an install with no nodes.
func (d CapacityData) share(t app.TeamUsage) float64 {
	if d.CPUAllocatable == 0 || d.MemAllocatable == 0 {
		return float64(t.Usage.CPUMillis)
	}
	return math.Max(
		float64(t.Usage.CPUMillis)/float64(d.CPUAllocatable),
		float64(t.Usage.MemoryBytes)/float64(d.MemAllocatable))
}

func (d CapacityData) CPUCommittedPercent() int {
	return quotaPercent(d.Committed.CPUMillis, d.CPUAllocatable)
}

func (d CapacityData) MemCommittedPercent() int {
	return quotaPercent(d.Committed.MemoryBytes, d.MemAllocatable)
}

// WarnPercent is the policy's warning line, or the fixed one without a policy.
func (d CapacityData) WarnPercent() int {
	if d.Snapshot != nil {
		return d.Snapshot.WarnPercent
	}
	return capacityWarnPercent
}

// Pressures are the resources committed past the warning line, each with how
// far. Against what is sellable where there is a policy, and against the
// machines where there is not. CPU first: it is the one a node runs out of
// first on most installs, and the one the sentence is usually about.
func (d CapacityData) Pressures() []CapacityPressure {
	var out []CapacityPressure
	if s := d.Snapshot; s != nil {
		for _, r := range []struct {
			name string
			res  app.CapacityResource
		}{{"CPU", s.CPU}, {"memory", s.Memory}, {"storage", s.Storage}} {
			if r.res.Counted && r.res.Percent() >= s.WarnPercent {
				out = append(out, CapacityPressure{r.name, r.res.Percent()})
			}
		}
		return out
	}
	if d.CPUAllocatable > 0 && d.CPUCommittedPercent() >= capacityWarnPercent {
		out = append(out, CapacityPressure{"CPU", d.CPUCommittedPercent()})
	}
	if d.MemAllocatable > 0 && d.MemCommittedPercent() >= capacityWarnPercent {
		out = append(out, CapacityPressure{"memory", d.MemCommittedPercent()})
	}
	return out
}

// RefusedSince counts the refusals on the page since a moment.
func (d CapacityData) RefusedSince(t time.Time) int {
	n := 0
	for _, r := range d.Refusals {
		if !r.At.Before(t) {
			n++
		}
	}
	return n
}

// CapacityPressure is one resource the cluster is close to running out of.
type CapacityPressure struct {
	Resource string
	Percent  int
}

// capacityRefusalsShown is how far back, and how many, the page lists.
const (
	capacityRefusalWindow = 7 * 24 * time.Hour
	capacityRefusalsShown = 100
)

func (s *Server) adminCapacity(w http.ResponseWriter, r *http.Request) {
	s.renderCapacity(w, r, http.StatusOK, nil, "")
}

// renderCapacity draws the page, with a refused form kept as it was typed.
func (s *Server) renderCapacity(
	w http.ResponseWriter, r *http.Request, status int, form *CapacityPolicyForm, formErr string,
) {
	ctx := r.Context()
	d := CapacityData{OK: true, CanAddNode: s.joiner != nil, FormError: formErr}

	if s.capacity != nil {
		snap, err := s.capacity.Capacity(ctx)
		if err != nil {
			s.log.Error("read capacity", slog.String("error", err.Error()))
			d.OK, d.Error = false, "Could not read the install's capacity."
		} else {
			d.addSnapshot(snap)
			d.Form = capacityPolicyForm(snap.Policy)
		}
		refusals, err := s.capacity.CapacityRefusals(ctx, time.Now().Add(-capacityRefusalWindow), capacityRefusalsShown)
		if err != nil {
			s.log.Error("list capacity refusals", slog.String("error", err.Error()))
		}
		d.Refusals = refusals
	} else {
		nodes, err := s.orch.Nodes(ctx)
		if err != nil {
			// The teams' side is still worth showing: it comes from the
			// database, and "how much have they committed" does not need the
			// cluster.
			d.OK, d.Error = false, err.Error()
		}
		d.addNodes(nodes)
	}
	if form != nil {
		d.Form = *form
	}

	teams, err := s.quotas.TeamUsages(ctx)
	if err != nil {
		s.log.Error("list teams for capacity", slog.String("error", err.Error()))
		d.OK, d.Error = false, "Could not read what the teams on this install have committed."
	}
	d.addTeams(teams)

	s.renderStatus(w, r, status, AdminCapacity(d))
}

func (s *Server) adminCapacityPolicy(w http.ResponseWriter, r *http.Request) {
	form := CapacityPolicyForm{
		Enforce:  formChecked(r, "enforce"),
		CPURatio: r.FormValue("cpu_ratio"), MemoryRatio: r.FormValue("memory_ratio"),
		Reserve: r.FormValue("reserve"), Warn: r.FormValue("warn"), Storage: r.FormValue("storage"),
	}
	p, err := form.Parse()
	if err == nil {
		err = s.capacity.SetCapacityPolicy(r.Context(), p)
	}
	if err != nil {
		// Re-rendered rather than redirected, so what was typed is still in
		// the boxes beside the reason it was refused.
		s.log.Warn("set capacity policy", slog.String("error", err.Error()))
		s.renderCapacity(w, r, http.StatusUnprocessableEntity, &form, err.Error())
		return
	}
	msg := "Capacity policy saved. Nothing is refused for capacity while it is not enforced."
	if p.Enforce {
		msg = "Capacity policy saved. It applies to the next change any team makes; " +
			"nothing already running was stopped."
	}
	s.flashOK(w, r, msg)
	http.Redirect(w, r, "/admin/capacity", http.StatusSeeOther)
}

// CapacityPolicyForm is the policy in the units a person types: ratios as
// numbers, percentages as whole numbers, storage in GiB. Strings for the same
// reason QuotaForm's are.
type CapacityPolicyForm struct {
	Enforce     bool
	CPURatio    string
	MemoryRatio string
	Reserve     string
	Warn        string
	Storage     string
}

func capacityPolicyForm(p app.CapacityPolicy) CapacityPolicyForm {
	f := CapacityPolicyForm{
		Enforce:     p.Enforce,
		CPURatio:    strconv.FormatFloat(p.CPURatio, 'f', -1, 64),
		MemoryRatio: strconv.FormatFloat(p.MemoryRatio, 'f', -1, 64),
		Reserve:     strconv.Itoa(p.ReservePercent),
		Warn:        strconv.Itoa(p.WarnPercent),
	}
	if p.StorageBytes > 0 {
		f.Storage = strconv.FormatFloat(roundTo(float64(p.StorageBytes)/(1<<30), 3), 'f', -1, 64)
	}
	return f
}

// Parse reads the form back into a policy, naming the field that is wrong.
// Whether the values make sense together is the policy's own Validate.
func (f CapacityPolicyForm) Parse() (app.CapacityPolicy, error) {
	p := app.CapacityPolicy{Enforce: f.Enforce}
	number := func(s, field string, fallback float64) (float64, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return fallback, nil
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("%s must be a number — %q is not one", field, s)
		}
		return n, nil
	}
	whole := func(s, field string, fallback int) (int, error) {
		n, err := number(s, field, float64(fallback))
		if err == nil && n != math.Trunc(n) {
			err = fmt.Errorf("%s must be a whole number", field)
		}
		return int(n), err
	}
	d := app.DefaultCapacityPolicy()
	var err error
	if p.CPURatio, err = number(f.CPURatio, "The CPU commit ratio", d.CPURatio); err != nil {
		return p, err
	}
	if p.MemoryRatio, err = number(f.MemoryRatio, "The memory commit ratio", d.MemoryRatio); err != nil {
		return p, err
	}
	if p.ReservePercent, err = whole(f.Reserve, "The reserve", d.ReservePercent); err != nil {
		return p, err
	}
	if p.WarnPercent, err = whole(f.Warn, "The warning", d.WarnPercent); err != nil {
		return p, err
	}
	storage, err := quotaNumber(f.Storage, "Storage", 1<<20)
	if err != nil {
		return p, err
	}
	p.StorageBytes = int64(math.Round(storage * (1 << 30)))
	return p, p.Validate()
}

// headroom says how much is left, or how far past the cluster the teams have
// gone — which limits allow, because a limit is a ceiling and not a
// reservation, and which is worth saying rather than showing as a negative.
func headroom(committed, allocatable int64, amount func(int64) string) string {
	if committed <= allocatable {
		return amount(allocatable-committed) + " left"
	}
	return amount(committed-allocatable) + " more than the cluster has"
}

// ratioLabel writes a commit ratio the way the form takes it, "2×".
func ratioLabel(r float64) string {
	return strconv.FormatFloat(roundTo(r, 2), 'f', -1, 64) + "×"
}

// soldFuller is whichever of CPU and memory is closer to sold out.
func soldFuller(s app.CapacitySnapshot) (string, app.CapacityResource) {
	if s.Memory.Percent() > s.CPU.Percent() {
		return "memory", s.Memory
	}
	return "CPU", s.CPU
}

// refusalAmount writes a refusal's shortfall in its resource's unit.
func refusalAmount(r app.CapacityRefusal) string {
	if r.Resource == "cpu" {
		return cpuAmount(r.Shortfall)
	}
	return byteAmount(r.Shortfall) + " " + r.Resource
}

// ---------------------------------------------------------------- the alert

// capacityAlert is what the operator is told on every page, or nil when there
// is nothing to tell: committed past the warning line, anything refused in the
// last day, or a cluster that cannot be read while the install is enforcing —
// when nothing is being refused and it looks as if it were.
type capacityAlert struct {
	Snapshot   app.CapacitySnapshot
	CanAddNode bool
}

func (a capacityAlert) warranted() bool {
	s := a.Snapshot
	switch {
	case s.RefusedLast24h > 0:
		return true
	case !s.Known:
		return s.Enforced
	}
	return s.CPU.Percent() >= s.WarnPercent || s.Memory.Percent() >= s.WarnPercent
}

// Severe is the destructive style: past what is sellable, or demand turned
// away. Under 100% it is a warning, and so is a cluster nobody can read.
func (a capacityAlert) Severe() bool {
	s := a.Snapshot
	if s.RefusedLast24h > 0 {
		return true
	}
	return s.Known && (s.CPU.Percent() >= 100 || s.Memory.Percent() >= 100)
}

// Headline is the one sentence the alert leads with.
func (a capacityAlert) Headline() string {
	s := a.Snapshot
	if !s.Known {
		return "Capacity is unknown — nothing is being refused while the cluster cannot be read"
	}
	which, r := soldFuller(s)
	pct := r.Percent()
	if pct < s.WarnPercent {
		return refusedCount(s.RefusedLast24h) + " for want of room in the last day"
	}
	if pct >= 100 {
		return fmt.Sprintf("This install has sold all of its %s — %d%% of what is sellable is committed",
			which, pct)
	}
	return fmt.Sprintf("%d%% of this install's sellable %s is committed", pct, which)
}

// Detail is what follows it.
func (a capacityAlert) Detail() string {
	s := a.Snapshot
	switch {
	case !s.Known:
		return "The capacity policy is enforced, but without the cluster there is no room to hold changes to."
	case s.RefusedLast24h > 0 && (s.CPU.Percent() >= s.WarnPercent || s.Memory.Percent() >= s.WarnPercent):
		return refusedCount(s.RefusedLast24h) + " in the last day. Add a node, or raise the commit " +
			"ratio if the workloads leave room under their limits."
	case s.RefusedLast24h > 0:
		return "Customers asked for more than was free at the time. See what they asked for, " +
			"and add a node if it keeps happening."
	case s.Enforced:
		return "Past what is sellable, new apps and more replicas are refused. Add a node before customers find out."
	}
	return "Nothing is refused while the policy is not enforced — past 100%, pods start competing for the machines."
}

func refusedCount(n int) string {
	if n == 1 {
		return "1 change was refused"
	}
	return strconv.Itoa(n) + " changes were refused"
}

// capacityBannerFor is the alert for this request: an operator's, on a
// dashboard page other than the one it links to.
func (s *Server) capacityBannerFor(r *http.Request) templ.Component {
	// Without Quotas there is no Capacity page for the alert to lead to.
	if s.capacity == nil || s.quotas == nil || !SurfacesFromContext(r.Context()).Operator ||
		hasPrefix(r.URL.Path, "/admin/capacity") {
		return nil
	}
	snap, err := s.capacity.Capacity(r.Context())
	if err != nil {
		s.log.Error("read capacity for the banner", slog.String("error", err.Error()))
		return nil
	}
	a := capacityAlert{Snapshot: snap, CanAddNode: s.joiner != nil}
	if !a.warranted() {
		return nil
	}
	return capacityBanner(a)
}

// withCapacityBanner puts the operator's alert on a page's banner: under the
// acting notice when there is one, above whatever a SlotProvider put there.
//
// Composed here rather than drawn by the layout, so a wrapper's banner and the
// acting notice keep their places and the layout stays as it was. The acting
// notice stays first — being somebody else is the one thing that matters more.
func withCapacityBanner(alert, banner templ.Component) templ.Component {
	if alert == nil {
		return banner
	}
	if b, ok := banner.(actingBanner); ok {
		b.below = bannerStack(alert, b.below)
		return b
	}
	return bannerStack(alert, banner)
}

// ---------------------------------------------------------------- customers

// roomShort reports whether customers should be told that room is limited.
// A yes or no, and nothing more: the numbers behind it are every team's.
func (s *Server) roomShort(ctx context.Context) bool {
	if s.capacity == nil {
		return false
	}
	snap, err := s.capacity.Capacity(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.log.Error("read capacity for the room notice", slog.String("error", err.Error()))
		}
		return false
	}
	return snap.RoomShort
}
