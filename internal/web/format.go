package web

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"regexp"
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
	case string(orchestrator.PhaseFailing):
		return "status-err"
	case string(orchestrator.PhaseScheduled):
		return "status-info"
	}
	return "status-neutral"
}

// cronStatus is a scheduled app's status in a few words: that it runs on a
// schedule, and when it next will — the question somebody looking at a card
// that is never "running" has.
func cronStatus(st orchestrator.AppStatus) string { return cronStatusAt(st, time.Now()) }

func cronStatusAt(st orchestrator.AppStatus, now time.Time) string {
	next, ok := orchestrator.NextRun(st.Schedule, now)
	if !ok {
		return "Cron"
	}
	return "Cron · next run " + untilLabel(next.Sub(now)) + afterFailure(st)
}

// afterFailure adds that the last run failed, when it did.
func afterFailure(st orchestrator.AppStatus) string {
	if st.Message != "" {
		return " · " + st.Message
	}
	return ""
}

// untilLabel is a wait in words, rounded the way a person says it.
func untilLabel(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return "in " + plural(int(d.Round(time.Minute).Minutes()), "minute")
	case d < 48*time.Hour:
		return "in " + plural(int(d.Round(time.Hour).Hours()), "hour")
	}
	return "in " + plural(int(d.Round(24*time.Hour).Hours()/24), "day")
}

// restartPolicies are the restart policy's choices, in Railway's order and
// words.
var restartPolicies = []struct {
	Value orchestrator.RestartPolicy
	Label string
}{
	{orchestrator.RestartOnFailure, "On failure"},
	{orchestrator.RestartAlways, "Always"},
	{orchestrator.RestartNever, "Never"},
}

// restartPolicyLabel is a policy in words.
func restartPolicyLabel(p orchestrator.RestartPolicy) string {
	for _, c := range restartPolicies {
		if c.Value == p {
			return c.Label
		}
	}
	return string(p)
}

// restartPolicyOf is an app's policy, the default when it has none recorded.
func restartPolicyOf(a app.App) orchestrator.RestartPolicy {
	return cmp.Or(a.RestartPolicy, orchestrator.RestartOnFailure)
}

// watchPathsPlaceholder shows both halves of the grammar: what to watch, and
// a ! line taking some of it back.
const watchPathsPlaceholder = "/backend/**\n!/backend/**/*.md"

// watchPathsText is watch paths as the field edits them, one per line.
func watchPathsText(patterns []string) string { return strings.Join(patterns, "\n") }

// optionalNumber shows zero — "the default" — as an empty field, so the
// placeholder says what the default is.
func optionalNumber(n int32) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(int(n))
}

// podPhaseClass maps a pod to a status style, treating a Running pod whose
// containers are not all ready as degraded rather than healthy.
func podPhaseClass(p orchestrator.PodInfo) string {
	switch p.Phase {
	case "Running":
		if p.Healthy() {
			return "status-ok"
		}
		if containerFailing(p.Reason) {
			return "status-err"
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
	if d.IsActive {
		return "border-l-success"
	}
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

func activeDeploymentOf(deployments []app.Deployment) (app.Deployment, bool) {
	for _, deployment := range deployments {
		if deployment.IsActive {
			return deployment, true
		}
	}
	return app.Deployment{}, false
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
	if image == app.BucketImage {
		return "S3-compatible bucket"
	}
	if image == app.PendingImage {
		return "building…"
	}
	return image
}

// appImageLabel is imageLabel for an app, which knows whether a missing
// image is a build under way or an empty service waiting for its source.
func appImageLabel(a app.App) string {
	if awaitsSource(a) {
		return "no source yet"
	}
	return imageLabel(a.Image)
}

// imageIsPending reports whether an app has no built image yet, so a template
// can style the placeholder as the absence it is rather than as a value.
func imageIsPending(image string) bool { return image == app.PendingImage }

// sourceEmpty is the picker's Empty Service. Not a source of the engine's:
// an empty service builds from source like a repository does, and is
// created as one with nothing connected yet.
const sourceEmpty app.Source = "empty"

// awaitsSource is an empty service before anything was given to it: no
// repository, and nothing built.
func awaitsSource(a app.App) bool {
	return a.Source == app.SourceGit && !a.Repo.Set() && a.Image == app.PendingImage
}

// sourcesCanBuild says the install builds from source, which an empty
// service needs as much as a repository does.
func sourcesCanBuild(sources []app.Blueprint) bool {
	for _, b := range sources {
		if b.Source == app.SourceGit {
			return b.Available
		}
	}
	return false
}

// sourceHref is where the picker sends each source.
//
// A template makes several apps and a project to hold them, so it cannot use
// the create form, which is a form for one app. Branching here rather than in
// the template keeps the picker a list of links.
func sourceHref(src app.Source, project string) string {
	if src == app.SourceTemplate {
		return "/templates"
	}
	return "/apps/new?source=" + string(src) + projectQuery("&", project)
}

// newAppHref is the deploy page, into a project when one is named.
func newAppHref(project string) string { return "/apps/new" + projectQuery("?", project) }

// githubDeployHref is the one-click GitHub deploy, into a project when one
// is named.
func githubDeployHref(project string) string { return "/apps/new/github" + projectQuery("?", project) }

func projectQuery(sep, project string) string {
	if project == "" {
		return ""
	}
	return sep + "project=" + url.QueryEscape(project)
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

// podsTitle counts the pods listed, and says how many of them the header's
// Pods figure leaves out and why, so the two numbers are seen to agree.
func podsTitle(pods []orchestrator.PodInfo) string {
	title := plural(len(pods), "pod")
	if off := len(pods) - podsOnNodes(pods); off > 0 {
		title += fmt.Sprintf(" · %d finished or waiting for a node, not counted above", off)
	}
	return title
}

// teamLabel names whose a pod or claim is in a list that crosses teams. One
// the engine did not make carries no owner and is the install's own: its
// database, its object store, Kubernetes itself.
func teamLabel(teams map[orchestrator.OwnerID]string, owner orchestrator.OwnerID) string {
	if owner == "" {
		return "Platform"
	}
	if name := teams[owner]; name != "" {
		return name
	}
	return string(owner)
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
	parts := make([]string, 0, 5)
	if n.Address != "" {
		parts = append(parts, n.Address)
	}
	if n.Version != "" {
		parts = append(parts, n.Version)
	}
	if n.OS != "" && n.Architecture != "" {
		parts = append(parts, n.OS+"/"+n.Architecture)
	}
	if n.Region != "" {
		parts = append(parts, "region: "+n.Region)
	}
	if n.Site != "" {
		parts = append(parts, "site: "+n.Site)
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
	case app.SourcePostgres, app.SourceMySQL, app.SourceRedis, app.SourceMongo:
		return "storage"
	case "git":
		return "external"
	case "template":
		return "layers"
	case app.SourceBucket:
		return "bucket"
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
		";height:" + strconv.Itoa(d.Height) + "px"
}

// canvasVars are the card geometry, on the canvas itself rather than the
// server's layer: the script moves the cards out of that layer into the
// drawing, and they keep their size only if what sizes them is still above
// them.
func canvasVars() string {
	return "--canvas-card-h:" + strconv.Itoa(cardH) + "px" +
		";--canvas-volume-h:" + strconv.Itoa(volumeH) + "px"
}

func nodeAt(n CanvasNode) string {
	return "left:" + strconv.Itoa(n.X) + "px;top:" + strconv.Itoa(n.Y) + "px;width:" +
		strconv.Itoa(cardW) + "px"
}

// ansiEscape matches the terminal escape sequences programs write into
// their output: colour and cursor control (CSI, "ESC [ … letter"), window
// titles and links (OSC, ended by BEL or "ESC \\"), and the two-byte rest.
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// plainLog takes the terminal escapes out of a log line for the page.
//
// The buildpacks lifecycle colours its phases and errors, and an app's own
// logger often does too. A browser shows the escape as "[36m" around every
// heading. Stripped rather than turned into colour: the words carry what the
// colour did ("ERROR:"), the page's own find marks what somebody is looking
// for, and a match has to be on the text people read — "ERROR: No" is not in
// "\x1b[31;1mERROR: \x1b[0mNo". What is stored is left alone, so the CLI,
// which writes to a terminal, still shows the colours.
func plainLog(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiEscape.ReplaceAllString(s, "")
}

// buildLines splits a stored build log for rendering.
//
// The log is appended to as it arrives, so the last chunk usually ends without
// a newline and a naive split leaves a trailing empty row that grows and
// shrinks as the build writes.
func buildLines(log string) []string {
	log = strings.TrimRight(plainLog(log), "\n")
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
		if strings.Contains(strings.ToLower(plainLog(l.Text)), needle) {
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

// canRollBackTo reports whether a history row can be rolled back to.
//
// Only a deployment that succeeded, of a release that is not the one running,
// with an image that exists. A failed one never ran, so there is nothing to go
// back to; and a row with no release predates releases being recorded, with
// nothing pinned to restore.
func canRollBackTo(a app.App, dep app.Deployment) bool {
	switch {
	case dep.ReleaseID == nil, dep.Status != app.DeploySucceeded, imageIsPending(dep.Image):
		return false
	case a.ActiveReleaseID != nil && *a.ActiveReleaseID == *dep.ReleaseID:
		return false
	}
	return true
}

// sourceDisconnectForm is the id of the form the source card's Disconnect
// button submits: see appRuntimeForm.
const sourceDisconnectForm = "source-disconnect"

// podStatusLabel is a pod's state as somebody reading the table means it. A
// pod is Running as soon as it is scheduled, and stays Running while its
// container fails to start, which is the moment the word misleads most: the
// container's own reason is the state then.
func podStatusLabel(p orchestrator.PodInfo) string {
	if p.Phase == "Running" && !p.Healthy() && p.Reason != "" {
		return p.Reason
	}
	return p.Phase
}

// containerFailing reports whether a container's waiting reason means it
// cannot run as it is, rather than that it is on its way.
func containerFailing(reason string) bool {
	switch reason {
	case "CrashLoopBackOff", "RunContainerError", "CreateContainerError", "CreateContainerConfigError",
		"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "OOMKilled", "Error":
		return true
	}
	return false
}

// deploymentImageLabel is what a deployment row shows where its image goes.
// A deploy that builds records its image once the build has made one, so
// one without is building, or its build failed, or it was given up before
// the build finished. One refused for a reference it could not read never
// started its build.
func deploymentImageLabel(dep app.Deployment) string {
	if !imageIsPending(dep.Image) {
		return dep.Image
	}
	switch dep.Status {
	case app.DeployFailed:
		if app.ReferenceFailure(dep.Message) {
			return "invalid reference"
		}
		return "build failed"
	case app.DeployRunning, "pending":
		return "building…"
	}
	return "not built"
}

// deploymentSource is where a deployment came from, as a person would say
// it. An app's first deploy is recorded as "initial", which reads as an
// image even for one built from GitHub.
//
// A source app with no repository has only ever had an image from `up`, so
// a deploy on that image — a variable changed, a redeploy — came from the
// CLI as much as the upload's own row did.
func deploymentSource(a app.App, dep app.Deployment) string {
	src := dep.Source()
	if src == "image" && a.Source == app.SourceGit {
		switch {
		case a.Repo.GitHub.Set() || strings.Contains(a.Repo.URL, "github.com"):
			return "GitHub"
		case !a.Repo.Set():
			return "CLI"
		}
		return "Git"
	}
	return src
}

// failureReason is a failed deployment's message as a person reads it: the
// package prefixes the service wraps it in are for logs, not for them.
func failureReason(message string) string {
	message = strings.TrimSpace(message)
	for _, prefix := range []string{"app: ", "rollout failed: "} {
		message = strings.TrimPrefix(message, prefix)
	}
	return message
}

// failureLines is failureReason a line at a time. Most reasons are one line;
// a pre-deploy command's carries the end of what it printed, and a stack
// trace run together into one paragraph is a stack trace nobody can read.
func failureLines(message string) []string {
	return strings.Split(failureReason(message), "\n")
}

// lastDeployFailed reports whether the most recent deployment failed.
func lastDeployFailed(deps []app.Deployment) bool {
	return len(deps) > 0 && deps[0].Status == app.DeployFailed
}

// settingsTab is which tab is lit for a tab name: Domains and Storage are
// sections of Settings now, so their addresses light Settings.
func settingsTab(active string) string {
	if active == "domains" || active == "storage" {
		return "settings"
	}
	return active
}

// deploymentTitle is what a deployment row is called, in Railway's place
// for the commit message: what caused it, said the way a person would.
func deploymentTitle(dep app.Deployment) string {
	rev := dep.Revision
	switch {
	case rev == "" || rev == "initial":
		return "Initial deploy"
	case rev == "redeploy":
		return "Redeploy"
	case rev == app.RollbackTrigger:
		return "Rollback"
	case strings.HasPrefix(rev, app.PushTriggerPrefix):
		sha := strings.TrimPrefix(rev, app.PushTriggerPrefix)
		if len(sha) > 7 {
			sha = sha[:7]
		}
		return "Push " + sha
	case strings.HasPrefix(rev, "changes:"):
		// Staged changes deployed together, as Railway's "N changes".
		keys := strings.Split(strings.TrimPrefix(rev, "changes:"), ",")
		if len(keys) == 1 {
			return "Deployed " + keys[0]
		}
		return fmt.Sprintf("Deployed %d changes", len(keys))
	case strings.HasPrefix(rev, "variable-delete:"):
		return "Removed " + strings.TrimPrefix(rev, "variable-delete:")
	case strings.HasPrefix(rev, "variable:"):
		return "Set " + strings.TrimPrefix(rev, "variable:")
	case strings.HasPrefix(rev, "git:"):
		return "Build " + strings.TrimPrefix(rev, "git:")
	case strings.HasPrefix(rev, app.UploadTriggerPrefix):
		// Railway's words for `railway up`, for the CLI's `up`.
		return "Deploy via CLI"
	}
	// Settings changes are recorded as what changed ("port:8080 cpu:…");
	// the first part is enough for a title, and the rest is in the logs.
	first, _, _ := strings.Cut(rev, " ")
	return "Changed " + strings.ReplaceAll(first, ":", " → ")
}

// deploymentBadge is the word Railway puts in a deployment's badge — ACTIVE
// for the one serving, REMOVED for one it replaced — with its colour.
func deploymentBadge(dep app.Deployment) (label, class string) {
	switch {
	case dep.IsActive:
		return "ACTIVE", "badge-ok"
	case dep.Status == app.DeployFailed:
		return "FAILED", "badge-err"
	case dep.Status == app.DeployRunning || dep.Status == "pending":
		if imageIsPending(dep.Image) {
			return "BUILDING", "badge-busy"
		}
		return "DEPLOYING", "badge-busy"
	case dep.Status == "cancelled":
		return "CANCELLED", "badge-idle"
	}
	return "REMOVED", "badge-idle"
}

// hasSleepActions reports whether the panel has a Sleep now or Wake now to
// show, so an empty actions bar is not drawn when it has neither.
func hasSleepActions(d AppDetailData) bool {
	return d.Sleep != nil && (d.App.Sleep.Asleep() || d.Sleep.Unavailable == nil)
}

// envDot colours an environment's dot: production green, as the one serving
// people, and every other environment the colour of work in progress.
func envDot(slug string) string {
	if slug == "" || slug == app.DefaultEnvironment {
		return "status-ok"
	}
	return "status-info"
}

// deploymentsToken is a fingerprint of what the deployments tab shows: each
// deployment's state, the pods' and the app's. The tab polls with it, and an
// answer for the same fingerprint is "nothing changed" — no body, no swap —
// so a panel left open costs a request, not a re-render.
func deploymentsToken(d AppDetailData) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%v|%s\n", d.App.Status.Phase, d.App.StatusKnown, d.App.Host)
	for _, dep := range d.Deployments {
		fmt.Fprintf(h, "%s|%s|%s|%v|%s\n", dep.ID, dep.Status, dep.Image, dep.IsActive, dep.Message)
	}
	for _, p := range d.Pods {
		fmt.Fprintf(h, "%s|%s|%v|%d|%s\n", p.Name, p.Phase, p.Healthy(), p.Restarts, p.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// deploymentsPoll is how often an open deployments tab asks: often while a
// deploy is under way, and still now and then, because a deploy is as often
// started by a push as by a click here.
func deploymentsPoll(d AppDetailData) string {
	if deploymentsInFlight(d) {
		return "every 3s"
	}
	return "every 10s"
}
