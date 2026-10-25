package webhook

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store"
)

// receiver records what it is sent and answers with whatever the test says
// next — a 500, a 200 — so retries can be watched one attempt at a time.
type receiver struct {
	mu      sync.Mutex
	got     []received
	answers []int
	// echo answers with the request's headers, the way an echo service
	// does, to prove the log does not keep what it echoes.
	echo bool
	srv  *httptest.Server
}

type received struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T, answers ...int) *receiver {
	rc := &receiver{answers: answers}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.got = append(rc.got, received{header: r.Header.Clone(), body: body})
		code := http.StatusOK
		if len(rc.answers) > 0 {
			code, rc.answers = rc.answers[0], rc.answers[1:]
		}
		echo := rc.echo
		rc.mu.Unlock()
		w.WriteHeader(code)
		if echo {
			_ = json.NewEncoder(w).Encode(r.Header)
		}
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) received() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]received(nil), rc.got...)
}

type fixture struct {
	pool    *pgxpool.Pool
	svc     *Service
	keeper  *secret.Keeper
	team    string
	project uuid.UUID
	other   uuid.UUID
	apps    *app.Service
	orch    *failingOrchestrator
}

// failingOrchestrator is the in-memory one, with apps that can be made to
// read as failing — what a crash loop looks like from the engine.
type failingOrchestrator struct {
	*orchestrator.Noop
	mu      sync.Mutex
	failing map[string]bool
}

func (f *failingOrchestrator) AppStatus(ctx context.Context, ref orchestrator.Ref) (orchestrator.AppStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	failing, watched := f.failing[ref.Name]
	if !watched {
		return f.Noop.AppStatus(ctx, ref)
	}
	if failing {
		return orchestrator.AppStatus{Phase: orchestrator.PhaseFailing, Reason: "CrashLoopBackOff",
			Message: "the container exited 5 times"}, nil
	}
	return orchestrator.AppStatus{Phase: orchestrator.PhaseRunning, Desired: 1, Ready: 1}, nil
}

// digests pins every image to the same digest, which is all an app's first
// release needs here.
type digests struct{}

func (digests) ResolveDigest(context.Context, string) (string, error) {
	return "sha256:" + strings.Repeat("a", 64), nil
}

func newFixture(t *testing.T, team string, allowPrivate bool) *fixture {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run webhook tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	purge := func() { _, _ = pool.Exec(ctx, `DELETE FROM teams WHERE id LIKE 'hook-%'`) }
	purge()
	t.Cleanup(purge)

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	keeper, err := secret.NewKeeper(key)
	if err != nil {
		t.Fatalf("keeper: %v", err)
	}
	orch := &failingOrchestrator{Noop: orchestrator.NewNoop(), failing: map[string]bool{}}
	f := &fixture{pool: pool, keeper: keeper, team: team, orch: orch}
	f.svc = New(pool, keeper, orch, log, Options{BaseURL: "https://dash.test", Brand: "Datahuru", AllowPrivate: allowPrivate})
	f.apps = app.NewService(pool, orch, log, app.Options{Events: f.svc, Manifests: digests{}})
	if err := f.apps.EnsureOwner(ctx, team, team, ""); err != nil {
		t.Fatalf("EnsureOwner: %v", err)
	}
	for _, p := range []struct {
		slug string
		id   *uuid.UUID
	}{{"web", &f.project}, {"other", &f.other}} {
		if err := pool.QueryRow(ctx, `INSERT INTO projects (owner_id, name, slug) VALUES ($1, $2, $2) RETURNING id`,
			team, p.slug).Scan(p.id); err != nil {
			t.Fatalf("project: %v", err)
		}
	}
	return f
}

func (f *fixture) hook(t *testing.T, url string, events []string, headers ...Header) (Webhook, string) {
	t.Helper()
	w, secret, err := f.svc.Create(context.Background(), f.team, f.project, uuid.Nil, Input{URL: url, Events: events, Headers: headers})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return w, secret
}

func (f *fixture) emit(kind string) {
	f.svc.Emit(context.Background(), app.Event{
		Kind: kind, OwnerID: f.team, ProjectID: f.project, AppID: uuid.New(), AppName: "web",
		DeploymentID: uuid.New(), Trigger: "redeploy", Message: "CrashLoopBackOff: the container exited",
	})
}

func TestADeliveryIsSignedAndCarriesTheEvent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "hook-sign", true)
	rc := newReceiver(t)
	w, secret := f.hook(t, rc.srv.URL+"/hook", []string{app.EventDeploySucceeded}, Header{Name: "x-api-key", Value: "s3cret-value"})
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("secret %q is not in the Standard Webhooks form", secret)
	}
	if strings.Join(w.HeaderNames, ",") != "X-Api-Key" {
		t.Errorf("header names = %v", w.HeaderNames)
	}
	f.emit(app.EventDeploySucceeded)
	f.emit(app.EventDeployFailed) // not listened for
	f.svc.Drain(ctx)

	got := rc.received()
	if len(got) != 1 {
		t.Fatalf("received %d deliveries, want 1", len(got))
	}
	h := got[0].header
	if h.Get("X-Api-Key") != "s3cret-value" {
		t.Error("the custom header did not arrive")
	}
	if !Verify(secret, h.Get("webhook-id"), h.Get("webhook-timestamp"), got[0].body, h.Get("webhook-signature")) {
		t.Fatal("the signature does not verify with the webhook's secret")
	}
	if Verify(secret, h.Get("webhook-id"), h.Get("webhook-timestamp"), append(got[0].body, ' '), h.Get("webhook-signature")) {
		t.Fatal("a signature verified over a body that was changed")
	}
	var p Payload
	if err := json.Unmarshal(got[0].body, &p); err != nil {
		t.Fatalf("body: %v", err)
	}
	if p.Type != app.EventDeploySucceeded || p.App == nil || p.App.Name != "web" ||
		p.Project == nil || p.Project.Name != "web" || p.Deployment == nil || p.Deployment.Status != "succeeded" {
		t.Errorf("payload = %+v", p)
	}
	if p.App.URL != "https://dash.test/apps/web" {
		t.Errorf("app url = %q", p.App.URL)
	}

	// Sealed at rest: neither the header's value nor the secret is in the row.
	var row []byte
	if err := f.pool.QueryRow(ctx, `SELECT row_to_json(w)::text::bytea FROM project_webhooks w WHERE id = $1`, w.ID).Scan(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if bytes.Contains(row, []byte("s3cret-value")) || bytes.Contains(row, []byte(secret)) {
		t.Fatal("a header value or the signing secret is stored readable")
	}
	ds, _ := f.svc.Deliveries(ctx, f.team, w.ID, 10)
	if len(ds) != 1 || ds[0].Status != StatusSucceeded || ds[0].LastStatus == nil || *ds[0].LastStatus != 200 {
		t.Fatalf("deliveries log = %+v", ds)
	}
}

func TestAnotherProjectsEventsAreNotSent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "hook-scope", true)
	rc := newReceiver(t)
	f.hook(t, rc.srv.URL, []string{app.EventDeploySucceeded})
	f.svc.Emit(ctx, app.Event{Kind: app.EventDeploySucceeded, OwnerID: f.team, ProjectID: f.other, AppName: "x"})
	f.svc.Emit(ctx, app.Event{Kind: app.EventDeploySucceeded, OwnerID: "hook-nobody", ProjectID: f.project, AppName: "x"})
	f.svc.Drain(ctx)
	if n := len(rc.received()); n != 0 {
		t.Fatalf("received %d deliveries for events that were not this project's", n)
	}
}

func TestAFailedDeliveryIsRetriedWithBackoff(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "hook-retry", true)
	rc := newReceiver(t, http.StatusInternalServerError, http.StatusOK)
	w, _ := f.hook(t, rc.srv.URL, []string{app.EventDeployFailed})
	f.emit(app.EventDeployFailed)
	f.svc.Drain(ctx)

	ds, _ := f.svc.Deliveries(ctx, f.team, w.ID, 10)
	if len(ds) != 1 || ds[0].Status != StatusPending || ds[0].Attempts != 1 || *ds[0].LastStatus != 500 {
		t.Fatalf("after a 500 = %+v, want pending after one attempt", ds)
	}
	if wait := time.Until(ds[0].NextAttemptAt); wait < 20*time.Second || wait > time.Minute {
		t.Errorf("next attempt in %s, want about %s", wait, backoff[0])
	}
	f.svc.Drain(ctx)
	if n := len(rc.received()); n != 1 {
		t.Fatalf("a delivery not yet due was sent again (%d sends)", n)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = now() WHERE webhook_id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.Drain(ctx)
	ds, _ = f.svc.Deliveries(ctx, f.team, w.ID, 10)
	if ds[0].Status != StatusSucceeded || ds[0].Attempts != 2 {
		t.Fatalf("after the retry = %+v, want succeeded on the second attempt", ds[0])
	}
	// Discord-shaped bodies for a deploy that failed carry its reason.
	var p Payload
	_ = json.Unmarshal(rc.received()[1].body, &p)
	if p.Deployment == nil || p.Deployment.Error == "" {
		t.Errorf("a failed deploy's payload has no error: %+v", p)
	}
}

func TestARefusalIsNotRetried(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "hook-refused", true)
	rc := newReceiver(t, http.StatusUnauthorized)
	w, _ := f.hook(t, rc.srv.URL, []string{app.EventDeploySucceeded})
	d, err := f.svc.Test(ctx, f.team, w.ID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if d.Status != StatusFailed || d.Attempts != 1 || *d.LastStatus != 401 {
		t.Fatalf("a 401 = %+v, want failed without retrying", d)
	}
	var p Payload
	_ = json.Unmarshal(rc.received()[0].body, &p)
	if p.Type != EventTest {
		t.Errorf("Test sent %q", p.Type)
	}
}

func TestWhatAReceiverEchoesIsRedacted(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "hook-redact", true)
	rc := newReceiver(t)
	rc.echo = true
	w, secret := f.hook(t, rc.srv.URL, []string{app.EventDeploySucceeded},
		Header{Name: "Authorization", Value: "Bearer very-secret-token"})
	d, err := f.svc.Test(ctx, f.team, w.ID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if strings.Contains(d.LastResponse, "very-secret-token") || strings.Contains(d.LastResponse, secret) {
		t.Fatalf("the deliveries log keeps an echoed secret: %s", d.LastResponse)
	}
	if !strings.Contains(d.LastResponse, "[redacted]") {
		t.Errorf("expected the echoed header struck out, got %s", d.LastResponse)
	}
	var stored string
	_ = f.pool.QueryRow(ctx, `SELECT last_response || last_error || payload::text FROM webhook_deliveries WHERE webhook_id = $1`, w.ID).Scan(&stored)
	if strings.Contains(stored, "very-secret-token") {
		t.Fatal("a header value reached the delivery row")
	}
}

func TestAPrivateAddressIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "hook-private", false)
	rc := newReceiver(t)
	w, _ := f.hook(t, rc.srv.URL, []string{app.EventDeploySucceeded})
	d, err := f.svc.Test(ctx, f.team, w.ID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if len(rc.received()) != 0 {
		t.Fatal("a webhook reached a loopback address")
	}
	if d.Status != StatusFailed || !strings.Contains(d.LastError, "private") {
		t.Fatalf("delivery = %+v, want refused as private and not retried", d)
	}
	if strings.Contains(d.LastError, rc.srv.URL) {
		t.Error("the error repeats the URL")
	}
}

func TestInputIsChecked(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "hook-input", true)
	for _, in := range []Input{
		{URL: "ftp://x.test", Events: []string{app.EventDeploySucceeded}},
		{URL: "https://user:pass@x.test", Events: []string{app.EventDeploySucceeded}},
		{URL: "https://x.test", Events: nil},
		{URL: "https://x.test", Events: []string{"deploy.exploded"}},
		{URL: "https://x.test", Events: []string{app.EventDeploySucceeded}, Headers: []Header{{Name: "webhook-signature", Value: "x"}}},
		{URL: "https://x.test", Events: []string{app.EventDeploySucceeded}, Headers: []Header{{Name: "X-A", Value: "a\r\nb"}}},
		{URL: "https://x.test", Events: []string{app.EventDeploySucceeded}, Headers: []Header{{Name: "bad header", Value: "x"}}},
	} {
		if _, _, err := f.svc.Create(ctx, f.team, f.project, uuid.Nil, in); !errors.As(err, new(InputError)) {
			t.Errorf("Create(%+v) = %v, want an InputError", in, err)
		}
	}
}

func TestDiscordAndSlackAreSentTheirOwnFormat(t *testing.T) {
	p := Payload{
		Type: app.EventDeployFailed, Timestamp: time.Unix(1700000000, 0),
		Project: &PayloadRef{Name: "shop"}, App: &PayloadRef{Name: "api", URL: "https://dash.test/apps/api"},
		Deployment: &PayloadDeployment{Status: "failed", Error: "build failed <here>"},
	}
	if kindOf("https://discord.com/api/webhooks/1/abc") != "discord" || kindOf("https://hooks.slack.com/services/T/B/x") != "slack" ||
		kindOf("https://example.com/api/webhooks/1") != "json" {
		t.Fatal("kindOf misreads a URL")
	}
	raw, _ := render("discord", "Datahuru", p)
	var d struct {
		Username string `json:"username"`
		Embeds   []struct {
			Title, Description, URL string
			Color                   int
		} `json:"embeds"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || d.Username != "Datahuru" || len(d.Embeds) != 1 ||
		d.Embeds[0].Title != "Deploy of api failed" || d.Embeds[0].Color != 0xef4444 || d.Embeds[0].URL == "" ||
		!strings.Contains(d.Embeds[0].Description, "build failed") {
		t.Errorf("discord body = %s", raw)
	}
	raw, _ = render("slack", "Datahuru", p)
	var s struct{ Text string }
	if err := json.Unmarshal(raw, &s); err != nil || !strings.Contains(s.Text, "<https://dash.test/apps/api|Deploy of api failed>") ||
		!strings.Contains(s.Text, "&lt;here&gt;") {
		t.Errorf("slack body = %s", raw)
	}
	raw, _ = render("json", "Datahuru", p)
	if !strings.Contains(string(raw), `"type": "deploy.failed"`) && !strings.Contains(string(raw), `"type":"deploy.failed"`) {
		t.Errorf("json body = %s", raw)
	}
}

func TestAnAppStartingToCrashIsSaidOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "hook-crash", true)
	rc := newReceiver(t)
	f.hook(t, rc.srv.URL, []string{app.EventAppCrashed})
	a, err := f.apps.Create(ctx, f.team, app.CreateInput{Name: "worker", Image: "nginx:alpine", Replicas: 1, ProjectID: f.project})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.orch.mu.Lock()
	f.orch.failing[a.Name] = true
	f.orch.mu.Unlock()
	f.svc.CheckCrashes(ctx)
	f.svc.CheckCrashes(ctx)
	f.svc.Drain(ctx)
	got := rc.received()
	if len(got) != 1 {
		t.Fatalf("app.crashed sent %d times for one crash, want once", len(got))
	}
	var p Payload
	_ = json.Unmarshal(got[0].body, &p)
	if p.Type != app.EventAppCrashed || p.App == nil || p.App.Name != "worker" || !strings.Contains(p.Message, "CrashLoopBackOff") {
		t.Errorf("payload = %+v", p)
	}
	// Recovering and crashing again is a second crash.
	f.orch.mu.Lock()
	f.orch.failing[a.Name] = false
	f.orch.mu.Unlock()
	f.svc.CheckCrashes(ctx)
	f.orch.mu.Lock()
	f.orch.failing[a.Name] = true
	f.orch.mu.Unlock()
	f.svc.CheckCrashes(ctx)
	f.svc.Drain(ctx)
	if n := len(rc.received()); n != 2 {
		t.Fatalf("a second crash after recovering sent %d in all, want 2", n)
	}
}
