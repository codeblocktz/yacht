package web

import (
	"cmp"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Capacity: whether the cluster has room for what its teams have committed.
//
// The question an operator of a shared install has to answer before a
// customer does, by finding their deploy stuck Pending. Quotas bound each team;
// nothing bounds their sum except the machines, and this page puts the two side
// by side.

// capacityWarnPercent is how much of the cluster may be committed before the
// page says to add a node. Not 100: a node takes minutes to join, a drain
// needs somewhere to move pods to, and a rollout briefly runs old and new pods
// together — all of which need room the committed figure does not show.
const capacityWarnPercent = 80

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
}

// addNodes totals the room on the machines new pods can be placed on.
//
// A cordoned or not-ready node is left out of the room but still counted as
// a node: its pods are running and its capacity is real, but nothing new will
// be placed there, and "is there room for the next deploy" is the question.
func (d *CapacityData) addNodes(nodes []orchestrator.NodeInfo) {
	for _, n := range nodes {
		d.Nodes++
		if !n.Ready || n.Unschedulable {
			continue
		}
		d.Schedulable++
		d.CPUAllocatable += cmp.Or(n.CPUAllocatableMillis, n.CPUCapacityMillis)
		d.MemAllocatable += cmp.Or(n.MemAllocatableBytes, n.MemCapacityBytes)
		if n.UsageKnown {
			d.UsageKnown = true
			d.CPUUsed += n.CPUUsedMillis
			d.MemUsed += n.MemUsedBytes
		}
	}
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

// Pressures are the resources committed past capacityWarnPercent, each with
// how far. CPU first: it is the one a node runs out of first on most installs,
// and the one the sentence is usually about.
func (d CapacityData) Pressures() []CapacityPressure {
	var out []CapacityPressure
	if d.CPUAllocatable > 0 && d.CPUCommittedPercent() >= capacityWarnPercent {
		out = append(out, CapacityPressure{"CPU", d.CPUCommittedPercent()})
	}
	if d.MemAllocatable > 0 && d.MemCommittedPercent() >= capacityWarnPercent {
		out = append(out, CapacityPressure{"memory", d.MemCommittedPercent()})
	}
	return out
}

// CapacityPressure is one resource the cluster is close to running out of.
type CapacityPressure struct {
	Resource string
	Percent  int
}

func (s *Server) adminCapacity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := CapacityData{OK: true, CanAddNode: s.joiner != nil}

	nodes, err := s.orch.Nodes(ctx)
	if err != nil {
		// The teams' side is still worth showing: it comes from the database,
		// and "how much have they committed" does not need the cluster.
		d.OK, d.Error = false, err.Error()
	}
	d.addNodes(nodes)

	teams, err := s.quotas.TeamUsages(ctx)
	if err != nil {
		s.log.Error("list teams for capacity", slog.String("error", err.Error()))
		d.OK, d.Error = false, "Could not read what the teams on this install have committed."
	}
	d.addTeams(teams)

	s.render(w, r, AdminCapacity(d))
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
