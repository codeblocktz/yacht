package web

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Presentation helpers.
//
// Kept in Go rather than in templates because they are worth testing: a
// mislabelled status or a meter that renders past 100% is a bug a reader will
// trust, and the template is the wrong place to hide that logic.

func clampPercent(p int) int {
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	}
	return p
}

// Class names returned below are declared in assets/css/input.css under
// @layer components, not composed from utilities here. A class name assembled
// in Go is invisible to Tailwind's scanner and would be stripped from the
// build; declaring them in CSS is what keeps them alive.

// meterClass colours a meter by pressure.
func meterClass(percent int) string {
	switch {
	case percent >= 90:
		return "meter-err"
	case percent >= 75:
		return "meter-warn"
	}
	return "meter-ok"
}

// phaseClass maps a workload phase to a status style.
func phaseClass(phase string) string {
	switch phase {
	case string(orchestrator.PhaseRunning):
		return "status-ok"
	case string(orchestrator.PhaseDegraded):
		return "status-warn"
	case string(orchestrator.PhasePending):
		return "status-info"
	}
	return "status-neutral"
}

// podPhaseClass maps a pod to a status style, treating a Running pod whose
// containers are not all ready as degraded rather than healthy.
func podPhaseClass(p orchestrator.PodInfo) string {
	switch p.Phase {
	case "Running":
		if p.Healthy() {
			return "status-ok"
		}
		return "status-warn"
	case "Pending", "ContainerCreating":
		return "status-info"
	case "Failed", "CrashLoopBackOff", "Unknown":
		return "status-err"
	}
	return "status-neutral"
}

// deploymentClass maps a deployment status to a status style.
func deploymentClass(status string) string {
	switch status {
	case "running", "succeeded":
		return "status-ok"
	case "pending":
		return "status-info"
	case "failed":
		return "status-err"
	case "cancelled":
		return "status-warn"
	}
	return "status-neutral"
}

// activeDeploymentBorder tints the live deployment panel by its health.
func activeDeploymentBorder(d app.Deployment) string {
	switch d.Status {
	case "running", "succeeded":
		return "border-l-success"
	case "failed":
		return "border-l-destructive"
	case "pending":
		return "border-l-info"
	}
	return "border-l-border"
}

// volumeClass maps a claim's phase to a status style.
func volumeClass(v orchestrator.VolumeInfo) string {
	switch v.Phase {
	case "Bound":
		return "status-ok"
	case "Pending":
		return "status-warn"
	case "Lost":
		return "status-err"
	}
	return "status-neutral"
}

// volumeSize prefers actual capacity over the request.
//
// They differ in practice: a provisioner may round up, and local-path ignores
// the request entirely. Showing the request when capacity is known would
// report a number the cluster does not agree with.
func volumeSize(v orchestrator.VolumeInfo) string {
	if v.CapacityBytes > 0 {
		return formatBytes(v.CapacityBytes)
	}
	if v.RequestBytes > 0 {
		return formatBytes(v.RequestBytes) + " req"
	}
	return "—"
}

// appTabHref builds the URL for one of an app's detail tabs.
func appTabHref(name, slug string) string {
	if slug == "" {
		return "/apps/" + name
	}
	return "/apps/" + name + "/" + slug
}

// relativeTime renders a coarse "how long ago", which is what an operator
// scanning a deployment list actually reads. An exact timestamp is available
// on the detail rows where precision matters.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/(24*30)), "month") + " ago"
	}
	return plural(int(d.Hours()/(24*365)), "year") + " ago"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// clusterCPUDetail and clusterMemDetail give the absolute figures under a
// percentage, so a number like "43%" can be checked against reality.
func clusterCPUDetail(s orchestrator.ClusterSummary) string {
	if !s.UsageKnown || s.CPUCapacityMillis == 0 {
		return ""
	}
	return fmt.Sprintf("%s / %s cores",
		formatMillicores(s.CPUUsedMillis), formatMillicores(s.CPUCapacityMillis))
}

func clusterMemDetail(s orchestrator.ClusterSummary) string {
	if !s.UsageKnown || s.MemCapacityBytes == 0 {
		return ""
	}
	return fmt.Sprintf("%s / %s",
		formatBytes(s.MemUsedBytes), formatBytes(s.MemCapacityBytes))
}

func replicaLabel(a app.App) string {
	if !a.StatusKnown {
		return fmt.Sprintf("%d desired", a.Replicas)
	}
	return fmt.Sprintf("%d/%d", a.Status.Ready, a.Replicas)
}

func readyLabel(a app.App) string {
	if !a.StatusKnown {
		return "—"
	}
	return fmt.Sprintf("%d", a.Status.Ready)
}

func portLabel(a app.App) string {
	if a.Port == 0 {
		return "—"
	}
	return fmt.Sprintf("%d", a.Port)
}

func podCapacityLabel(s orchestrator.ClusterSummary) string {
	if s.PodCapacity == 0 {
		return ""
	}
	return fmt.Sprintf("of %d capacity", s.PodCapacity)
}

func nodeSubtitle(n orchestrator.NodeInfo) string {
	parts := make([]string, 0, 4)
	if n.Address != "" {
		parts = append(parts, n.Address)
	}
	if n.Version != "" {
		parts = append(parts, n.Version)
	}
	if n.OS != "" && n.Architecture != "" {
		parts = append(parts, n.OS+"/"+n.Architecture)
	}
	if n.Pool != "" {
		parts = append(parts, "pool: "+n.Pool)
	}
	return strings.Join(parts, " · ")
}

func cpuLabel(n orchestrator.NodeInfo) string {
	if !n.UsageKnown {
		return fmt.Sprintf("%s cores", formatMillicores(n.CPUCapacityMillis))
	}
	return fmt.Sprintf("%s / %s cores (%d%%)",
		formatMillicores(n.CPUUsedMillis),
		formatMillicores(n.CPUCapacityMillis),
		n.CPUPercent())
}

func memLabel(n orchestrator.NodeInfo) string {
	if !n.UsageKnown {
		return formatBytes(n.MemCapacityBytes)
	}
	return fmt.Sprintf("%s / %s (%d%%)",
		formatBytes(n.MemUsedBytes),
		formatBytes(n.MemCapacityBytes),
		n.MemoryPercent())
}

// formatMillicores renders CPU as whole cores with one decimal, which is how
// operators think about capacity — "1.5 cores", not "1500m".
func formatMillicores(m int64) string {
	if m == 0 {
		return "0"
	}
	cores := float64(m) / 1000
	if cores >= 10 {
		return fmt.Sprintf("%.0f", cores)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", cores), ".0")
}

// formatBytes renders a byte count in binary units.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	value := float64(b) / float64(div)
	suffix := [...]string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, suffix)
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// sortedKeys gives templates a deterministic iteration order. Ranging over a
// map directly would reorder the page on every render.
func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// gigabytes renders a byte count as whole gigabytes.
//
// The engine stores bytes because comparing them is what enforces grow-only,
// but nobody types 2147483648 into a form.
func gigabytes(b int64) string {
	return strconv.FormatInt(b/(1<<30), 10)
}

// sourceIcon maps a source to an icon that already exists.
func sourceIcon(src app.Source) string {
	switch src {
	case app.SourcePostgres:
		return "storage"
	case "git":
		return "external"
	case "template":
		return "layers"
	}
	return "box"
}

// roleTone colours a role the way the rest of the dashboard colours state:
// an owner is the one that matters, a member is unremarkable.
func roleTone(r account.Role) string {
	switch r {
	case account.RoleOwner:
		return "status-info"
	case account.RoleAdmin:
		return "status-warn"
	}
	return "status-neutral"
}
