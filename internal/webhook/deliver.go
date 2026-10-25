package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Delivery is one event sent, or being sent, to one webhook.
type Delivery struct {
	ID       uuid.UUID
	Event    string
	Status   string
	Attempts int
	// LastStatus is the receiver's last HTTP status, nil when it never
	// answered; LastError says why not, and LastResponse is the start of
	// what it said back, with anything of ours struck out.
	LastStatus    *int
	LastError     string
	LastResponse  string
	LastDuration  time.Duration
	CreatedAt     time.Time
	NextAttemptAt time.Time
	FinishedAt    *time.Time
}

// Delivery statuses.
const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// backoff is how long after each failed attempt the next is made: soon at
// first, for a receiver that blinked, and then further apart, for one that is
// down — six attempts over about two and three-quarter hours.
var backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// maxResponse is how much of a receiver's reply the log keeps.
const maxResponse = 1024

// Emit records a delivery for every webhook listening for an event. It
// implements app.EventSink, so it is called after a deploy has finished and
// cannot change how it went; a failure here is logged, and the event is not
// sent.
func (s *Service) Emit(ctx context.Context, e app.Event) {
	if s == nil || e.ProjectID == uuid.Nil {
		return
	}
	hooks, err := s.q.WebhooksForEvent(ctx, dbgen.WebhooksForEventParams{
		OwnerID: e.OwnerID, ProjectID: e.ProjectID, Event: e.Kind,
	})
	if err != nil {
		s.log.Error("find webhooks for an event", slog.String("event", e.Kind), slog.String("error", err.Error()))
		return
	}
	if len(hooks) == 0 {
		return
	}
	p := s.payload(ctx, e)
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	for _, h := range hooks {
		if _, err := s.q.CreateWebhookDelivery(ctx, dbgen.CreateWebhookDeliveryParams{
			WebhookID: h.ID, OwnerID: e.OwnerID, Event: e.Kind, Payload: body,
		}); err != nil {
			s.log.Error("record a webhook delivery", slog.String("webhook", h.ID.String()),
				slog.String("error", err.Error()))
		}
	}
	s.wake()
}

// payload is the JSON event for one app event.
func (s *Service) payload(ctx context.Context, e app.Event) Payload {
	p := Payload{Type: e.Kind, ID: uuid.NewString(), Timestamp: e.At.UTC(), Message: e.Message}
	if p.Timestamp.IsZero() {
		p.Timestamp = time.Now().UTC()
	}
	base := strings.TrimRight(s.opts.BaseURL, "/")
	var slug, name string
	if err := s.pool.QueryRow(ctx, `SELECT slug, name FROM projects WHERE owner_id = $1 AND id = $2`,
		e.OwnerID, e.ProjectID).Scan(&slug, &name); err == nil {
		p.Project = &PayloadRef{ID: e.ProjectID.String(), Name: name}
		if base != "" {
			p.Project.URL = base + "/projects/" + url.PathEscape(slug)
		}
	}
	if e.AppName != "" {
		p.App = &PayloadRef{ID: e.AppID.String(), Name: e.AppName}
		if base != "" && e.Kind != app.EventDeployRemoved {
			p.App.URL = base + "/apps/" + url.PathEscape(e.AppName)
		}
	}
	if e.DeploymentID != uuid.Nil {
		status := map[string]string{
			app.EventDeploySucceeded: "succeeded", app.EventDeployFailed: "failed",
			app.EventDeployCrashed: "crashed", app.EventDeployRemoved: "removed",
		}[e.Kind]
		if status == "" {
			status = "active"
		}
		p.Deployment = &PayloadDeployment{ID: e.DeploymentID.String(), Status: status, Trigger: e.Trigger}
		if e.Kind == app.EventDeployFailed || e.Kind == app.EventDeployCrashed {
			p.Deployment.Error, p.Message = e.Message, ""
		}
	}
	return p
}

// wake tells the worker there is something to send.
func (s *Service) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Test sends a test event to one webhook now, and returns how it went: the
// Test Webhook button. Recorded like any delivery, so it is on the log, and
// retried like one if it fails.
func (s *Service) Test(ctx context.Context, ownerID string, id uuid.UUID) (Delivery, error) {
	w, err := s.Get(ctx, ownerID, id)
	if err != nil {
		return Delivery{}, err
	}
	p := Payload{
		Type: EventTest, ID: uuid.NewString(), Timestamp: time.Now().UTC(),
		Message: "This is a test from " + s.opts.Brand + ". Real events look like this one.",
	}
	var slug, name string
	if err := s.pool.QueryRow(ctx, `SELECT slug, name FROM projects WHERE owner_id = $1 AND id = $2`,
		ownerID, w.ProjectID).Scan(&slug, &name); err == nil {
		p.Project = &PayloadRef{ID: w.ProjectID.String(), Name: name}
	}
	body, _ := json.Marshal(p)
	row, err := s.q.CreateWebhookDelivery(ctx, dbgen.CreateWebhookDeliveryParams{
		WebhookID: w.ID, OwnerID: ownerID, Event: EventTest, Payload: body,
	})
	if err != nil {
		return Delivery{}, fmt.Errorf("webhook: record test: %w", err)
	}
	claimed, err := s.q.ClaimWebhookDelivery(ctx, row.ID)
	if err != nil {
		return Delivery{}, fmt.Errorf("webhook: claim test: %w", err)
	}
	s.send(ctx, claimed)
	return s.delivery(ctx, ownerID, row.ID)
}

func (s *Service) delivery(ctx context.Context, ownerID string, id uuid.UUID) (Delivery, error) {
	var d dbgen.WebhookDelivery
	err := s.pool.QueryRow(ctx, `SELECT id, event, status, attempts, last_status, last_error,
		last_response, last_duration_ms, created_at, next_attempt_at, finished_at
		FROM webhook_deliveries WHERE owner_id = $1 AND id = $2`, ownerID, id).Scan(
		&d.ID, &d.Event, &d.Status, &d.Attempts, &d.LastStatus, &d.LastError,
		&d.LastResponse, &d.LastDurationMs, &d.CreatedAt, &d.NextAttemptAt, &d.FinishedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Delivery{}, ErrNotFound
		}
		return Delivery{}, fmt.Errorf("webhook: read delivery: %w", err)
	}
	return toDelivery(d), nil
}

// Deliveries is a webhook's recent deliveries, newest first.
func (s *Service) Deliveries(ctx context.Context, ownerID string, webhookID uuid.UUID, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.q.ListWebhookDeliveries(ctx, dbgen.ListWebhookDeliveriesParams{
		OwnerID: ownerID, WebhookID: webhookID, Lim: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("webhook: list deliveries: %w", err)
	}
	out := make([]Delivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, toDelivery(r))
	}
	return out, nil
}

func toDelivery(r dbgen.WebhookDelivery) Delivery {
	d := Delivery{
		ID: r.ID, Event: r.Event, Status: r.Status, Attempts: int(r.Attempts),
		LastError: r.LastError, LastResponse: r.LastResponse,
		CreatedAt: r.CreatedAt, NextAttemptAt: r.NextAttemptAt,
	}
	if r.LastStatus != nil {
		code := int(*r.LastStatus)
		d.LastStatus = &code
	}
	if r.LastDurationMs != nil {
		d.LastDuration = time.Duration(*r.LastDurationMs) * time.Millisecond
	}
	if r.FinishedAt.Valid {
		at := r.FinishedAt.Time
		d.FinishedAt = &at
	}
	return d
}

// Run sends due deliveries until ctx ends, and prunes old ones. Every replica
// may run it: deliveries are claimed with a lease, so none is sent twice
// while its sender is alive, and one whose sender died is sent again.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(s.opts.Interval)
	defer tick.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			s.sending.Wait()
			return
		case <-tick.C:
		case <-s.kick:
		case <-prune.C:
			if err := s.q.PruneWebhookDeliveries(ctx, pgtype.Timestamptz{Time: time.Now().Add(-30 * 24 * time.Hour), Valid: true}); err != nil && ctx.Err() == nil {
				s.log.Warn("prune webhook deliveries", slog.String("error", err.Error()))
			}
		}
	}
}

// Drain sends whatever is due now and waits for it, for a test that cannot
// wait for the worker's tick.
func (s *Service) Drain(ctx context.Context) {
	s.drain(ctx)
	s.sending.Wait()
}

func (s *Service) drain(ctx context.Context) {
	rows, err := s.q.ClaimDueWebhookDeliveries(ctx, 20)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("claim webhook deliveries", slog.String("error", err.Error()))
		}
		return
	}
	for _, d := range rows {
		s.sending.Add(1)
		go func() {
			defer s.sending.Done()
			s.send(context.WithoutCancel(ctx), d)
		}()
	}
}

// send makes one attempt at one delivery and records how it went.
func (s *Service) send(ctx context.Context, d dbgen.WebhookDelivery) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	row, err := s.q.GetProjectWebhook(ctx, dbgen.GetProjectWebhookParams{OwnerID: d.OwnerID, ID: d.WebhookID})
	if err != nil {
		// Deleted while it was due: the cascade takes the delivery too, and
		// there is nothing to record against.
		return
	}
	res := s.attempt(ctx, row, d)
	attempts := int(d.Attempts) + 1
	status, next := StatusSucceeded, time.Now()
	if !res.ok {
		status = StatusFailed
		if res.retry && attempts <= len(backoff) {
			status, next = StatusPending, time.Now().Add(backoff[attempts-1])
		}
	}
	ms := int32(res.took.Milliseconds())
	if err := s.q.RecordWebhookAttempt(ctx, dbgen.RecordWebhookAttemptParams{
		Status: status, NextAttemptAt: next, LastStatus: res.code, LastError: res.err,
		LastResponse: res.response, LastDurationMs: &ms, ID: d.ID,
	}); err != nil {
		s.log.Error("record a webhook attempt", slog.String("delivery", d.ID.String()),
			slog.String("error", err.Error()))
	}
}

type attemptResult struct {
	ok, retry bool
	code      *int32
	err       string
	response  string
	took      time.Duration
}

// attempt sends one delivery and reads the answer.
func (s *Service) attempt(ctx context.Context, w dbgen.ProjectWebhook, d dbgen.WebhookDelivery) attemptResult {
	signing, err := s.keeper.Open(w.SecretSealed)
	if err != nil {
		return attemptResult{err: "the webhook's secret could not be opened with this install's key"}
	}
	var headers []Header
	if len(w.HeadersSealed) > 0 {
		raw, err := s.keeper.Open(w.HeadersSealed)
		if err != nil {
			return attemptResult{err: "the webhook's headers could not be opened with this install's key"}
		}
		_ = json.Unmarshal([]byte(raw), &headers)
	}
	// Everything of ours that a reply might echo back, to strike out of what
	// is kept of it.
	secrets := []string{signing}
	for _, h := range headers {
		secrets = append(secrets, h.Value)
	}

	var p Payload
	_ = json.Unmarshal(d.Payload, &p)
	body, err := render(kindOf(w.Url), s.opts.Brand, p)
	if err != nil {
		return attemptResult{err: "the event could not be written: " + err.Error()}
	}
	ts := time.Now()
	sig, err := sign(signing, d.ID.String(), ts, body)
	if err != nil {
		return attemptResult{err: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Url, bytes.NewReader(body))
	if err != nil {
		return attemptResult{err: "the URL cannot be requested"}
	}
	for _, h := range headers {
		req.Header.Set(h.Name, h.Value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.opts.Brand+"-Webhooks/1")
	req.Header.Set("webhook-id", d.ID.String())
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set("webhook-signature", sig)

	start := time.Now()
	resp, err := s.client.Do(req)
	took := time.Since(start)
	if err != nil {
		// An address refused for being private will be refused the next time
		// too; anything else — a timeout, a refused connection — may not be.
		msg := transportError(err)
		return attemptResult{retry: msg != errBlockedAddress.Error(), err: redact(msg, secrets), took: took}
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	code := int32(resp.StatusCode)
	res := attemptResult{code: &code, took: took, response: redact(printable(reply), secrets)}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.ok = true
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooEarly,
		resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		// The receiver is busy or broken; it may not be in a minute.
		res.retry, res.err = true, "answered "+strconv.Itoa(resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Not followed, deliberately — see guardedClient — and not going to
		// stop redirecting by being asked again.
		res.err = "answered " + strconv.Itoa(resp.StatusCode) + " — redirects are not followed; use the address it redirects to"
	default:
		// The receiver understood and said no: a wrong token, a URL that
		// is gone. Asking again would get the same answer.
		res.err = "answered " + strconv.Itoa(resp.StatusCode)
	}
	return res
}

// transportError says why a request got no answer, without the URL that
// net/http puts at the front of it: a Discord or Slack URL is itself the
// credential, and the log is not the place to repeat it.
func transportError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	switch {
	case errors.Is(err, errBlockedAddress):
		return errBlockedAddress.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer in time"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the host's name does not resolve"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, errBlockedAddress) || strings.Contains(opErr.Error(), errBlockedAddress.Error()) {
			return errBlockedAddress.Error()
		}
		return "could not connect: " + opErr.Err.Error()
	}
	return err.Error()
}

// redact strikes every one of our secrets out of text a receiver sent back.
// An echo service returns the request it was sent, Authorization header and
// all; the deliveries log must not become where that header is shown.
func redact(text string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
	}
	return text
}

// printable keeps a reply readable on a page: valid UTF-8, no control
// characters but newlines and tabs.
func printable(b []byte) string {
	s := strings.ToValidUTF8(string(b), "")
	var out strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			out.WriteRune(r)
		}
	}
	if !utf8.ValidString(out.String()) {
		return ""
	}
	return out.String()
}
