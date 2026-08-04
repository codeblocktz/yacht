package web

import (
	"fmt"
	"maps"
	"slices"
	"sort"
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
	case app.DeployActive, "succeeded":
		return "status-ok"
	case app.DeployRunning, "pending":
		return "status-info"
	case app.DeployFailed:
		return "status-err"
	case "cancelled":
		return "status-warn"
	}
	// Superseded lands here, and neutral is right: it is history, not a
	// problem. Colouring a replaced deployment like a fault would make a
	// healthy app look like it had been failing all week.
	return "status-neutral"
}

// activeDeploymentBorder tints the live deployment panel by its health.
func activeDeploymentBorder(d app.Deployment) string {
	switch d.Status {
	case app.DeployActive, "succeeded":
		return "border-l-success"
	case app.DeployFailed:
		return "border-l-destructive"
	case app.DeployRunning, "pending":
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

// preciseAgo renders "how long ago" down to the second.
//
// relativeTime deliberately stops at "just now" below a minute, which is right
// for a deployment list nobody is watching tick. It is wrong for anything polled
// every few seconds: a line that reads "just now" for a full minute cannot be
// told from one that has silently stopped updating, and "is this still checking"
// is the exact question the domain panel exists to answer.
func preciseAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	}
	return relativeTime(t)
}

// untilTime renders how long until something is due.
//
// Shown next to preciseAgo so a person waiting on a background check can see
// that one is coming and roughly when, rather than deciding nothing is happening
// and reaching for the manual button.
func untilTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Until(t)
	switch {
	case d <= 0:
		return "due now"
	case d < time.Minute:
		return "in " + strconv.Itoa(int(d.Seconds())+1) + "s"
	case d < time.Hour:
		return "in " + plural(int(d.Minutes())+1, "minute")
	case d < 24*time.Hour:
		return "in " + plural(int(d.Hours())+1, "hour")
	}
	return "in " + plural(int(d.Hours()/24), "day")
}

// absoluteTime is what goes in a title attribute beside a relative one.
//
// Every "4 minutes ago" on a page is a number somebody eventually needs to
// correlate with a log line or an incident, and re-deriving it from the clock in
// their head is where the mistake gets made. Local rather than UTC: the reader
// is looking at their own machine's other timestamps.
func absoluteTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

// imageLabel is what to show where an app's image goes.
//
// A git app has no image until its first build finishes, and until then the
// column held the literal string yacht.invalid/not-built-yet:pending. That
// constant exists so the cluster has something syntactically valid to reject;
// its own comment anticipates it reaching a cluster, not a person's screen.
func imageLabel(image string) string {
	if image == app.PendingImage {
		return "building…"
	}
	return image
}

// imageIsPending reports whether an app has no built image yet, so a template
// can style the placeholder as the absence it is rather than as a value.
func imageIsPending(image string) bool { return image == app.PendingImage }

// sourceHref is where the picker sends each source.
//
// A template makes several apps and a project to hold them, so it cannot use
// the create form, which is a form for one app. Branching here rather than in
// the template keeps the picker a list of links.
func sourceHref(src app.Source) string {
	if src == app.SourceTemplate {
		return "/templates"
	}
	return "/apps/new?source=" + string(src)
}

// totalApps counts the apps across every project.
func totalApps(projects []app.Project) int64 {
	var n int64
	for _, p := range projects {
		n += p.Apps
	}
	return n
}

// boolAttr renders a data attribute as "true" or empty.
//
// Empty rather than "false" because [data-x] matches an attribute whose value
// is "false" just as readily as one whose value is "true" — a selector written
// the obvious way would be wrong for exactly half its inputs.
func boolAttr(b bool) string {
	if b {
		return "true"
	}
	return ""
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

// canvasSize and nodeAt are inline styles because the values are computed per
// render. A class cannot carry a coordinate.
// canvasSize carries the surface's size and the card geometry the server laid
// it out with.
//
// The card heights go out as custom properties rather than being written in the
// stylesheet, because the server draws every edge from them. A card whose CSS
// height disagreed with cardH would still render — it would just have arrows
// ending in the air a few pixels past it, which is how this shipped the first
// time.
func canvasSize(d CanvasData) string {
	return "width:" + strconv.Itoa(d.Width) + "px" +
		";height:" + strconv.Itoa(d.Height) + "px" +
		";--canvas-card-h:" + strconv.Itoa(cardH) + "px" +
		";--canvas-volume-h:" + strconv.Itoa(volumeH) + "px"
}

func nodeAt(n CanvasNode) string {
	return "left:" + strconv.Itoa(n.X) + "px;top:" + strconv.Itoa(n.Y) + "px;width:" +
		strconv.Itoa(cardW) + "px"
}

// buildLines splits a stored build log for rendering.
//
// The log is appended to as it arrives, so the last chunk usually ends without
// a newline and a naive split leaves a trailing empty row that grows and
// shrinks as the build writes.
func buildLines(log string) []string {
	log = strings.TrimRight(log, "\n")
	if log == "" {
		return nil
	}
	return strings.Split(log, "\n")
}

// shortSHA is the prefix people actually quote.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// joinHosts lists an app's hostnames for a heading.
func joinHosts(hosts []string) string { return strings.Join(hosts, ", ") }

// statusClass colours an HTTP status by class, because scanning a log for the
// one request that failed is the whole reason to open it.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "status-err"
	case code >= 400:
		return "status-warn"
	default:
		return "status-ok"
	}
}

// requestTiming renders how long a request took and how much it returned.
func requestTiming(l orchestrator.HTTPLogLine) string {
	out := l.Duration.Round(time.Millisecond).String()
	if l.Bytes > 0 {
		out += " · " + strconv.FormatInt(l.Bytes, 10) + " B"
	}
	return out
}

// shortTime is the clock time an event was last seen.
//
// Just the time, not the date. Kubernetes expires events after about an hour,
// so every row on this page happened today — a date on each one would be the
// same date repeated down the column.
func shortTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("15:04:05")
}

// matchSpan is one piece of a line, and whether the search found it.
type matchSpan struct {
	Text  string
	Match bool
}

// splitMatches cuts a line around every occurrence of a search term.
//
// Returned as spans rather than as marked-up HTML, so the template escapes the
// log's own text. A log line is arbitrary output from somebody's container —
// building a string with <mark> in it and trusting it is how a crash message
// becomes script running in the dashboard.
func splitMatches(line, needle string) []matchSpan {
	if needle == "" || line == "" {
		return []matchSpan{{Text: line}}
	}

	lowLine, lowNeedle := strings.ToLower(line), strings.ToLower(needle)
	var out []matchSpan
	for {
		i := strings.Index(lowLine, lowNeedle)
		if i < 0 {
			if line != "" {
				out = append(out, matchSpan{Text: line})
			}
			return out
		}
		if i > 0 {
			out = append(out, matchSpan{Text: line[:i]})
		}
		out = append(out, matchSpan{Text: line[i : i+len(needle)], Match: true})
		line, lowLine = line[i+len(needle):], lowLine[i+len(needle):]
	}
}

// filterLines keeps the log lines that match a search.
func filterLines(lines []orchestrator.LogLine, query string) []orchestrator.LogLine {
	if query == "" {
		return lines
	}
	needle := strings.ToLower(query)
	kept := make([]orchestrator.LogLine, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l.Text), needle) {
			kept = append(kept, l)
		}
	}
	return kept
}

// filterBuildLog keeps the build-log lines that match a search.
func filterBuildLog(lines []string, query string) []string {
	if query == "" {
		return lines
	}
	needle := strings.ToLower(query)
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), needle) {
			kept = append(kept, l)
		}
	}
	return kept
}

// methodsIn lists the HTTP methods present, for a selector that only offers
// what the data actually contains.
//
// A fixed list of every verb would offer PATCH on an app that has never seen
// one, and a filter that can only ever return nothing is a worse answer than
// not offering it.
func methodsIn(lines []orchestrator.HTTPLogLine) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lines {
		if l.Method != "" && !seen[l.Method] {
			seen[l.Method] = true
			out = append(out, l.Method)
		}
	}
	sort.Strings(out)
	return out
}
