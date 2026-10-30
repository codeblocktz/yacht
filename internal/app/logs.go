package app

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// LogRequest asks for one app's container output.
type LogRequest struct {
	// Pod is optional. Empty reads the app's first pod, which is what somebody
	// looking at a single-replica service wants and is the common case.
	Pod string

	// Previous reads the container that died rather than the one running.
	Previous bool

	Tail int64
}

// Deployment returns one of an app's deployments.
//
// Separate from DeploymentLogs because the sheet has views that show no
// container output at all and still need the deployment's own detail for their
// heading. Reading the cluster to fill a pane that discards the result would
// cost an API call per tab and, worse, make a log fetch happen where the page
// says none did.
func (s *Service) Deployment(
	ctx context.Context, ownerID, appName string, deployID uuid.UUID,
) (Deployment, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return Deployment{}, err
	}

	row, err := s.q.GetDeployment(ctx, dbgen.GetDeploymentParams{OwnerID: ownerID, ID: deployID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Deployment{}, ErrNotFound
		}
		return Deployment{}, fmt.Errorf("app: read deployment: %w", err)
	}
	// The deployment has to belong to the app in the URL, or a deployment id is
	// a way to read across apps within a team.
	if row.AppID != a.ID {
		return Deployment{}, ErrNotFound
	}

	d := toDeployment(row)
	if a.ActiveReleaseID != nil {
		d.IsActive = sameRelease(d.ReleaseID, pgUUID(*a.ActiveReleaseID))
	}
	return d, nil
}

// DeployLogs is what one deployment's log view can honestly show.
type DeployLogs struct {
	Deployment Deployment
	Logs       Logs

	// Live is true when this deployment is the one currently running, which is
	// the only case where there are containers left to read.
	Live bool
}

// DeploymentLogs reads the output of one deployment.
//
// Only the running deployment has any. Kubernetes keeps a container's log with
// the container, so the pods that served a superseded deployment took their
// output with them when they were replaced — retaining it would need a log
// store shipped off the cluster, which this install does not have.
//
// Saying that is the point. Reading the current pods and captioning them with
// an old deployment's revision would look like history and be the live log,
// which is worse than an empty pane: somebody would draw conclusions about a
// deploy from output that postdates it.
func (s *Service) DeploymentLogs(
	ctx context.Context, ownerID, appName string, deployID uuid.UUID, req LogRequest,
) (DeployLogs, error) {
	d, err := s.Deployment(ctx, ownerID, appName, deployID)
	if err != nil {
		return DeployLogs{}, err
	}

	out := DeployLogs{Deployment: d}
	// The latest deploy's containers are the ones there, failed or not: one
	// that could not start is still being retried, and what it printed
	// before dying is the reason somebody opened this.
	out.Live = d.IsActive || (d.Status == DeployFailed && s.isLatestDeployment(ctx, ownerID, d))
	if !out.Live {
		out.Logs.Note = "This deployment has been replaced. Its containers are gone, and " +
			"their output went with them — nothing here stores logs off the cluster."
		if d.Status == DeployFailed {
			out.Logs.Note = "A later deploy replaced this one, and its containers' output went with them."
		}
		return out, nil
	}

	if out.Logs, err = s.Logs(ctx, ownerID, appName, req); err != nil {
		return DeployLogs{}, err
	}
	return out, nil
}

// isLatestDeployment reports whether nothing has been deployed since d.
func (s *Service) isLatestDeployment(ctx context.Context, ownerID string, d Deployment) bool {
	// A few rather than one, so skipped pushes since — which deployed
	// nothing — do not make a failed deploy stop being the latest.
	rows, err := s.q.ListDeployments(ctx, dbgen.ListDeploymentsParams{OwnerID: ownerID, AppID: d.AppID, Limit: 10})
	if err != nil {
		return false
	}
	for _, row := range rows {
		if row.Status != DeploySkipped {
			return row.ID == d.ID
		}
	}
	return false
}

// Logs is an app's container output, and which pods it could have come from.
type Logs struct {
	Pod      string
	Pods     []string
	Lines    []orchestrator.LogLine
	Previous bool

	// Note explains an empty result that is not a failure — a pod that has
	// never restarted has no previous container, and saying so beats an empty
	// pane that looks like a bug.
	Note string
}

// Logs reads an app's container output.
//
// The app is resolved by owner first and the namespace comes from that row, so
// the only pods reachable are the ones belonging to an app the caller owns. A
// pod name is enough to read any tenant's output, so it is never trusted from
// the request: the name is checked against the pods this app actually has.
func (s *Service) Logs(ctx context.Context, ownerID, appName string, req LogRequest) (Logs, error) {
	namespace, out, err := s.resolveLogTarget(ctx, ownerID, appName, req)
	if err != nil {
		return Logs{}, err
	}
	if out.Pod == "" {
		return out, nil
	}

	lines, err := s.orch.Logs(ctx, orchestrator.LogOptions{
		Namespace: namespace, Pod: out.Pod, Tail: req.Tail, Previous: req.Previous,
	})
	if errors.Is(err, orchestrator.ErrNotStarted) {
		// Not a failure to report as one. The container has no log because it
		// never ran, and why it never ran is on the pod, which the page shows
		// directly above this.
		out.Note = "This container has not started, so it has written nothing. " +
			"The status above says why it could not start."
		return out, nil
	}
	if err != nil {
		return Logs{}, fmt.Errorf("app: read logs: %w", err)
	}
	out.Lines = lines

	if len(lines) == 0 {
		if req.Previous {
			out.Note = "This container has not restarted, so there is no earlier run to show."
		} else {
			out.Note = "The container has not written anything yet."
		}
	}
	return out, nil
}

// resolveLogTarget resolves an app to the namespace and pod a log read may
// touch, and is the only place that decides it.
//
// Shared by the batch read and the stream rather than written twice. A pod name
// is enough to read any tenant's output, so the check that it belongs to this
// app is the whole security of both paths — and a guard implemented once per
// caller is a guard that will eventually be applied to all but one of them.
//
// An empty Pod on the returned Logs means there is nothing to read, with Note
// already saying why.
func (s *Service) resolveLogTarget(
	ctx context.Context, ownerID, appName string, req LogRequest,
) (namespace string, out Logs, err error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return "", Logs{}, err
	}

	pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{
		Namespace: a.Namespace, ManagedOnly: true, Owner: orchestrator.OwnerID(ownerID),
	})
	if err != nil {
		return "", Logs{}, fmt.Errorf("app: list pods for logs: %w", err)
	}

	out = Logs{Previous: req.Previous}
	for _, p := range pods {
		out.Pods = append(out.Pods, p.Name)
	}
	if len(out.Pods) == 0 {
		out.Note = "This app has no running pods, so there is no output to read."
		return a.Namespace, out, nil
	}

	// A pod named by the request is honoured only if it is one of this app's.
	// Otherwise the name is a way to read somebody else's container.
	out.Pod = out.Pods[0]
	if req.Pod != "" {
		var ok bool
		for _, name := range out.Pods {
			if name == req.Pod {
				out.Pod, ok = name, true
				break
			}
		}
		if !ok {
			return "", Logs{}, ErrNotFound
		}
	}
	return a.Namespace, out, nil
}

// LogStream follows an app's container output until ctx is done.
//
// Scoped exactly as Logs is, through the same resolver. Previous runs are not
// offered: a container that has already died writes nothing more, so following
// it would hold a connection open on a stream that can never produce a line.
func (s *Service) LogStream(
	ctx context.Context, ownerID, appName string, req LogRequest,
) (iter.Seq2[orchestrator.LogLine, error], error) {
	namespace, target, err := s.resolveLogTarget(ctx, ownerID, appName, req)
	if err != nil {
		return nil, err
	}
	if target.Pod == "" {
		// Nothing to follow, and an empty iterator says so without the caller
		// needing a second way to ask.
		return func(func(orchestrator.LogLine, error) bool) {}, nil
	}

	return s.orch.LogStream(ctx, orchestrator.LogOptions{
		Namespace: namespace, Pod: target.Pod, Tail: req.Tail,
	})
}

// HTTPLogs is what the ingress controller recorded for one app.
type HTTPLogs struct {
	Lines []orchestrator.HTTPLogLine

	// Hosts are the names these requests could have arrived on, shown so an
	// empty result can be told from one for an app with no hostname at all.
	Hosts []string

	// Note explains an empty result, and Hint is the configuration that would
	// fix it when the answer is a setting somebody else has to apply.
	Note string
	Hint string

	// Since is when Lines begin being whole: every request to the app from
	// then on is in them, and none from before. Coverage says the same for a
	// person, and is shown whatever the count, because a count with no
	// beginning reads as everything (QA OBSERVE-1).
	Since    time.Time
	Coverage string
}

// RequestLog is the log store's copy of the ingress controller's access log:
// every request, kept for the store's retention rather than until the
// controller's log is next rotated. observe.LogStore is the engine's.
type RequestLog interface {
	Requests(ctx context.Context, hosts []string, start, end time.Time, limit int) (
		lines []orchestrator.HTTPLogLine, more bool, err error)
}

// requestLogSpan is how far back an app's HTTP log reads from the store, at
// most: a day of requests is what the page is for, and the store's retention
// is often longer than anybody wants to scan for one app.
const requestLogSpan = 24 * time.Hour

// requestLogLimit is the most requests the page is given, the same as the
// pod log reader's tail.
const requestLogLimit = 5000

// UseRequestLog reads apps' HTTP logs from the log store, which keeps them for
// retention, rather than from the controller's pod.
func (s *Service) UseRequestLog(l RequestLog, retention time.Duration) {
	s.requestLog, s.requestLogRetention = l, retention
}

// canApplyHTTPLogs reports whether Yacht can configure this cluster's ingress
// controller at all.
//
// Asked by attempting nothing: a dry run against the cluster is the only
// honest answer, and there is no dry run for this, so the question is narrowed
// to the one thing that decides it — whether k3s installed the controller.
//
// It no longer gates an offer, because nothing is offered any more. It decides
// which of two true things the panel says while the log is off: that Yacht is
// switching it on, or that this controller is somebody else's to configure.
func (s *Service) canApplyHTTPLogs(ctx context.Context, l orchestrator.HTTPLogger) bool {
	type prober interface{ CanConfigureIngress(context.Context) bool }
	p, ok := l.(prober)
	return ok && p.CanConfigureIngress(ctx)
}

// DeploymentHTTPLogs reads the requests made to an app.
//
// The app is resolved by owner first and the hostnames come from that app's own
// rows, which is the entire tenancy boundary: one ingress controller logs every
// tenant's traffic into one stream, so what makes this one app's log is that
// the filter was built from an owner-scoped query and nothing else.
//
// An app with no hostname gets nothing, not everything. That is the failure
// worth naming, because an empty filter over a shared log is the difference
// between this feature and a way to read the whole cluster's traffic.
func (s *Service) DeploymentHTTPLogs(
	ctx context.Context, ownerID, appName string,
) (HTTPLogs, error) {
	a, err := s.Get(ctx, ownerID, appName)
	if err != nil {
		return HTTPLogs{}, err
	}

	logger, ok := s.orch.(orchestrator.HTTPLogger)
	if !ok {
		return HTTPLogs{Note: "This install cannot read the ingress controller's log."}, nil
	}

	hosts, err := domain.RoutableHosts(ctx, s.q, a.ID)
	if err != nil {
		return HTTPLogs{}, err
	}
	out := HTTPLogs{Hosts: hosts}
	if len(hosts) == 0 {
		out.Note = "This app has no hostname, so nothing reaches it through the " +
			"ingress controller and there is nothing to record."
		return out, nil
	}

	if s.requestLog != nil {
		// The pod log is the fallback, not the answer: a store that cannot
		// be read now is better covered by what the controller still has
		// than by nothing.
		stored, err := s.storedHTTPLogs(ctx, hosts, out)
		if err == nil {
			return stored, nil
		}
		s.log.Warn("read http logs from the log store; reading the ingress controller's instead",
			slog.String("error", err.Error()))
	}

	got, err := logger.HTTPLogs(ctx, orchestrator.HTTPLogOptions{Hosts: hosts})
	if err != nil {
		return HTTPLogs{}, fmt.Errorf("app: read http logs: %w", err)
	}
	out.Lines = got.Lines
	if !got.From.IsZero() {
		out.Since = got.From
		out.Coverage = "Since " + coverageTime(got.From, time.Now()) + ", when the ingress " +
			"controller's log begins. Requests before then are not kept, and every app's " +
			"traffic shares that one log, so a busy cluster moves this forward quickly."
	}

	switch got.Reason {
	case orchestrator.HTTPLogNotEnabled:
		// Two different situations that look identical from here, and only one
		// of them is anybody's to act on. Where Yacht owns the controller this
		// is a restart in progress and the answer is to wait; where it does
		// not, the setting lives wherever the controller was installed from and
		// nothing Yacht does will change it.
		if s.canApplyHTTPLogs(ctx, logger) {
			out.Note = "The ingress controller is not writing an access log yet. " +
				"It is switched on at startup and the controller restarts to " +
				"pick that up, so requests should start appearing within a minute."
			break
		}
		out.Note = "The ingress controller is running but is not writing an access " +
			"log, so no requests are being recorded — for this app or any other."
		out.Hint = logger.HTTPLogHint()
	case orchestrator.HTTPLogNoController:
		out.Note = "No ingress controller was found, so nothing in this cluster is " +
			"recording requests."
	default:
		if len(out.Lines) == 0 {
			out.Note = "No requests to this app appear in the part of the " +
				"controller's log that is still available. Every app's traffic " +
				"shares that one log, so a busy cluster overwrites it quickly."
		}
	}
	return out, nil
}

// storedHTTPLogs reads an app's requests from the log store.
func (s *Service) storedHTTPLogs(ctx context.Context, hosts []string, out HTTPLogs) (HTTPLogs, error) {
	now := time.Now()
	span := requestLogSpan
	if s.requestLogRetention > 0 && s.requestLogRetention < span {
		span = s.requestLogRetention
	}
	lines, more, err := s.requestLog.Requests(ctx, hosts, now.Add(-span), now, requestLogLimit)
	if err != nil {
		return HTTPLogs{}, err
	}
	out.Lines = lines
	out.Since = now.Add(-span)
	if more && len(lines) > 0 {
		out.Since = lines[0].At
		out.Coverage = fmt.Sprintf("The newest %d requests, since %s. Older ones are kept, "+
			"and are past what one page holds.", len(lines), coverageTime(out.Since, now))
	} else {
		out.Coverage = "Every request in the last " + spanText(span) + "."
	}
	if len(lines) == 0 {
		out.Note = "No requests to this app in the last " + spanText(span) + "."
	}
	return out, nil
}

// coverageTime names a moment the way the rows beside it do: a time of day
// in UTC, with the day when it is not today's.
func coverageTime(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04:05") + " UTC"
	}
	return t.Format("Jan 2, 15:04:05") + " UTC"
}

// spanText is a whole number of hours or days, as a person says it.
func spanText(d time.Duration) string {
	switch {
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d == 24*time.Hour:
		return "24 hours"
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	case d >= time.Hour:
		return "hour"
	}
	return d.String()
}

// EnsureHTTPLogs turns the ingress controller's access log on at startup.
//
// Request logging is not a feature somebody switches on. An app whose requests
// are not being recorded is an app nobody can debug, and a platform that made
// that the default and then put the fix behind a button was asking every user
// to discover, understand, and accept a cluster-wide restart before they could
// see their own traffic. So Yacht does it once, here, on its own.
//
// It is still cluster-wide — there is one controller and one log, and Traefik
// has no per-router or per-host way to switch access logging off — which is
// exactly why this is not reached from an app's page. Nothing about it is one
// app's decision, so nothing about it is presented as one.
//
// Idempotent, and quiet when there is nothing to do: a controller already
// writing an access log is left alone, so this costs a restart once per
// install rather than once per boot.
func (s *Service) EnsureHTTPLogs(ctx context.Context) {
	logger, ok := s.orch.(orchestrator.HTTPLogger)
	if !ok {
		return
	}
	if logger.HTTPLogsEnabled(ctx) {
		return
	}

	if err := logger.EnableHTTPLogs(ctx); err != nil {
		if errors.Is(err, orchestrator.ErrNotSupported) {
			// This cluster's controller belongs to whoever installed it. Said
			// once at startup rather than swallowed, because it is the reason
			// the HTTP tab will keep handing over a configuration file.
			s.log.Info("request logging is off and this cluster's ingress controller "+
				"is not one Yacht can configure — apply its access log setting "+
				"wherever the controller was installed from",
				slog.String("reason", err.Error()))
			return
		}
		s.log.Error("could not turn request logging on",
			slog.String("error", err.Error()))
		return
	}
	s.log.Info("request logging was off and has been turned on — the ingress " +
		"controller is restarting, so every app in this cluster will drop " +
		"connections briefly")
}

// HTTPBucket is one column of the request timeline.
//
// Counted by status class rather than kept as raw requests, because the chart
// answers "when did this app get traffic, and when did it go wrong" — and a
// thousand identical 200s answer that no better than a number does.
type HTTPBucket struct {
	At time.Time `json:"at"`

	// To is where this column ends. Sent rather than inferred, because a bar
	// on a time axis has to be told how wide it is: without an interval the
	// chart spreads whatever bars exist across the whole width, so two busy
	// minutes drew as two bars half a chart wide each.
	To time.Time `json:"to"`

	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Err  int `json:"err"`
}

// httpBuckets is how many columns the timeline is drawn with.
//
// Fixed rather than derived from the data, so the chart has the same shape
// whether an app served six requests or six thousand. A bar count that tracked
// the request count would be a histogram of nothing.
const httpBuckets = 48

// Buckets groups requests into a timeline.
//
// The range comes from the requests themselves rather than a fixed window,
// because the controller's log is a moving window of unknown span: asking for
// the last hour when the log only reaches back four minutes draws fifty-six
// empty columns and squeezes everything real into the last four.
func (h HTTPLogs) Buckets() []HTTPBucket {
	if len(h.Lines) == 0 {
		return nil
	}

	var first, last time.Time
	for _, l := range h.Lines {
		if l.At.IsZero() {
			continue
		}
		if first.IsZero() || l.At.Before(first) {
			first = l.At
		}
		if l.At.After(last) {
			last = l.At
		}
	}
	if first.IsZero() {
		return nil
	}
	if !last.After(first) {
		// Everything in one instant. One column wide is the honest picture of
		// that; spreading it over an invented range would draw a trend that is
		// not in the data.
		last = first.Add(time.Second)
	}

	width := last.Sub(first) / httpBuckets
	if width <= 0 {
		width = time.Millisecond
	}

	out := make([]HTTPBucket, httpBuckets)
	for i := range out {
		out[i].At = first.Add(time.Duration(i) * width)
		out[i].To = out[i].At.Add(width)
	}
	for _, l := range h.Lines {
		if l.At.IsZero() {
			continue
		}
		i := int(l.At.Sub(first) / width)
		if i < 0 {
			i = 0
		}
		if i >= httpBuckets {
			i = httpBuckets - 1
		}
		switch {
		case l.Status >= 500:
			out[i].Err++
		case l.Status >= 400:
			out[i].Warn++
		default:
			out[i].OK++
		}
	}
	return out
}
