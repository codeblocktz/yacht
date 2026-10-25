// Package webhook tells the outside world what happened to a project's apps:
// Railway's Project Settings › Webhooks.
//
// An event — a deploy finished, an app started crashing — becomes one
// delivery row per webhook listening for it, written when it happens. A
// worker on every replica claims due rows, sends them, and records what came
// back; a failure is tried again with backoff, and every attempt is on the
// deliveries log with its status code. Rows rather than goroutines, so a
// delivery survives the process that would have sent it.
//
// Each request is signed the Standard Webhooks way — webhook-id,
// webhook-timestamp and webhook-signature headers over the body — so a
// receiver can tell it came from here. A Discord or Slack URL is sent the
// message those expect instead of the JSON event, and both ignore the
// signature.
//
// The custom headers are usually a credential, so they are sealed with the
// install's key and never shown again, only their names; and because a
// receiver's reply can echo what it was sent, every value of theirs and the
// signing secret are struck out of anything the log keeps.
package webhook

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// EventTest is what the Test Webhook button sends: an event like the others,
// that nothing but the button produces.
const EventTest = "webhook.test"

// Events are what a webhook can listen for, in the order the form offers
// them, with what each is called there.
var Events = []EventInfo{
	{app.EventDeploySucceeded, "Deploy succeeded", "A new version came up and is serving."},
	{app.EventDeployFailed, "Deploy failed", "A build, pre-deploy command or rollout failed."},
	{app.EventDeployCrashed, "Deploy crashed", "A new version crashed as it started."},
	{app.EventDeployRemoved, "Deploy removed", "A deployment went because its app was deleted."},
	{app.EventAppCrashed, "App crashed", "An app that was serving started crashing."},
}

// EventInfo is one event a form offers.
type EventInfo struct {
	Name, Label, Description string
}

// ValidEvent reports whether name is one a webhook can listen for.
func ValidEvent(name string) bool {
	for _, e := range Events {
		if e.Name == name {
			return true
		}
	}
	return false
}

var (
	// ErrNoKey means the install has no key to seal headers and the signing
	// secret with, so it cannot keep webhooks at all.
	ErrNoKey = errors.New("webhook: this install has no YACHT_SECRET_KEY to seal a webhook's secret with")
	// ErrNotFound is a webhook that is not the team's.
	ErrNotFound = errors.New("webhook: no such webhook")
)

// InputError is a webhook that cannot be saved, with the reason in words.
type InputError struct{ Reason string }

func (e InputError) Error() string { return "webhook: " + e.Reason }

// Header is one custom header, as typed on the form.
type Header struct{ Name, Value string }

// Input is a webhook to make.
type Input struct {
	URL     string
	Events  []string
	Headers []Header
}

// Webhook is one as the page lists it: everything but the secrets.
type Webhook struct {
	ID          uuid.UUID
	OwnerID     string
	ProjectID   uuid.UUID
	URL         string
	Events      []string
	HeaderNames []string
	CreatedAt   time.Time

	// Kind is how its payload is shaped: "discord", "slack" or "json".
	Kind string
}

// Options are what a Service needs beyond its dependencies.
type Options struct {
	// BaseURL is the dashboard's, for links back to an app in a message.
	BaseURL string
	// Brand names the platform in a Discord or Slack message.
	Brand string
	// AllowPrivate lets a webhook reach loopback and private addresses. Off
	// in production, where a webhook URL is somebody's input and the engine
	// sits beside the cluster's API and every app's private port. On only
	// for tests whose receiver is on 127.0.0.1.
	AllowPrivate bool
	// Client sends the requests. Nil is one with the address guard above
	// and a ten-second timeout.
	Client *http.Client
	// Interval is how often the worker looks for due deliveries without
	// being told; zero is five seconds.
	Interval time.Duration
}

// Service keeps webhooks, records their deliveries, and sends them.
type Service struct {
	pool   *pgxpool.Pool
	q      *dbgen.Queries
	keeper *secret.Keeper
	orch   orchestrator.Orchestrator
	log    *slog.Logger
	opts   Options
	client *http.Client

	// kick wakes the worker when a delivery is written, so an event is sent
	// in a moment rather than at the next tick.
	kick chan struct{}
	// sending lets a test wait for deliveries started in the background.
	sending sync.WaitGroup
}

// New wires the store into the service. keeper must be configured: the
// service refuses to create webhooks without one.
func New(pool *pgxpool.Pool, keeper *secret.Keeper, orch orchestrator.Orchestrator, log *slog.Logger, opts Options) *Service {
	if log == nil {
		log = slog.Default()
	}
	if opts.Brand == "" {
		opts.Brand = "Yacht"
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	client := opts.Client
	if client == nil {
		client = guardedClient(opts.AllowPrivate)
	}
	return &Service{
		pool: pool, q: dbgen.New(pool), keeper: keeper, orch: orch, log: log,
		opts: opts, client: client, kick: make(chan struct{}, 1),
	}
}

// Configured reports whether webhooks can be kept on this install.
func (s *Service) Configured() bool { return s != nil && s.keeper.Configured() }

// maxHeaders bounds the custom headers on one webhook.
const maxHeaders = 10

// reservedHeaders are set by the delivery itself, and a custom header of the
// same name would either be overwritten or forge the signature.
var reservedHeaders = []string{
	"host", "content-length", "content-type", "user-agent", "connection",
	"transfer-encoding", "webhook-id", "webhook-timestamp", "webhook-signature",
}

// Create makes a webhook for a project and returns its signing secret, once.
func (s *Service) Create(
	ctx context.Context, ownerID string, projectID uuid.UUID, actor uuid.UUID, in Input,
) (Webhook, string, error) {
	if !s.Configured() {
		return Webhook{}, "", ErrNoKey
	}
	target, err := checkURL(in.URL)
	if err != nil {
		return Webhook{}, "", err
	}
	var events []string
	for _, e := range in.Events {
		if !ValidEvent(e) {
			return Webhook{}, "", InputError{fmt.Sprintf("%q is not an event a webhook can listen for", e)}
		}
		if !slices.Contains(events, e) {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		return Webhook{}, "", InputError{"choose at least one event to send"}
	}
	headers, names, err := checkHeaders(in.Headers)
	if err != nil {
		return Webhook{}, "", err
	}
	var sealedHeaders []byte
	if len(headers) > 0 {
		raw, _ := json.Marshal(headers)
		if sealedHeaders, err = s.keeper.Seal(string(raw)); err != nil {
			return Webhook{}, "", fmt.Errorf("webhook: seal headers: %w", err)
		}
	}
	signing, err := newSecret()
	if err != nil {
		return Webhook{}, "", err
	}
	sealedSecret, err := s.keeper.Seal(signing)
	if err != nil {
		return Webhook{}, "", fmt.Errorf("webhook: seal secret: %w", err)
	}
	by := pgtype.UUID{}
	if actor != uuid.Nil {
		by = pgtype.UUID{Bytes: actor, Valid: true}
	}
	row, err := s.q.CreateProjectWebhook(ctx, dbgen.CreateProjectWebhookParams{
		OwnerID: ownerID, ProjectID: projectID, Url: target, Events: events,
		HeaderNames: names, HeadersSealed: sealedHeaders, SecretSealed: sealedSecret, CreatedBy: by,
	})
	if err != nil {
		return Webhook{}, "", fmt.Errorf("webhook: create: %w", err)
	}
	return fromRow(row), signing, nil
}

// List is a project's webhooks.
func (s *Service) List(ctx context.Context, ownerID string, projectID uuid.UUID) ([]Webhook, error) {
	rows, err := s.q.ListProjectWebhooks(ctx, dbgen.ListProjectWebhooksParams{
		OwnerID: ownerID, ProjectID: projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("webhook: list: %w", err)
	}
	out := make([]Webhook, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// Get is one of the team's webhooks.
func (s *Service) Get(ctx context.Context, ownerID string, id uuid.UUID) (Webhook, error) {
	row, err := s.q.GetProjectWebhook(ctx, dbgen.GetProjectWebhookParams{OwnerID: ownerID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Webhook{}, ErrNotFound
		}
		return Webhook{}, fmt.Errorf("webhook: read: %w", err)
	}
	return fromRow(row), nil
}

// Delete removes a webhook and its deliveries.
func (s *Service) Delete(ctx context.Context, ownerID string, id uuid.UUID) (Webhook, error) {
	w, err := s.Get(ctx, ownerID, id)
	if err != nil {
		return Webhook{}, err
	}
	if _, err := s.q.DeleteProjectWebhook(ctx, dbgen.DeleteProjectWebhookParams{OwnerID: ownerID, ID: id}); err != nil {
		return Webhook{}, fmt.Errorf("webhook: delete: %w", err)
	}
	return w, nil
}

func fromRow(r dbgen.ProjectWebhook) Webhook {
	return Webhook{
		ID: r.ID, OwnerID: r.OwnerID, ProjectID: r.ProjectID, URL: r.Url,
		Events: r.Events, HeaderNames: r.HeaderNames, CreatedAt: r.CreatedAt,
		Kind: kindOf(r.Url),
	}
}

// checkURL refuses what cannot be a webhook: not http or https, carrying a
// username and password, or with no host. Whether the host is somewhere the
// engine may send to is decided when it is dialled, against the address it
// resolves to then; deciding here would be undone by a DNS change tomorrow.
func checkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", InputError{"a webhook needs a URL"}
	}
	if len(raw) > 2048 {
		return "", InputError{"the URL is longer than 2048 characters"}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", InputError{"the URL must be an http:// or https:// address"}
	}
	if u.User != nil {
		return "", InputError{"put credentials in a header rather than in the URL — a URL is shown on this page"}
	}
	u.Fragment = ""
	return u.String(), nil
}

// checkHeaders validates the custom headers, returning them and their names.
func checkHeaders(in []Header) ([]Header, []string, error) {
	var out []Header
	names := []string{}
	for _, h := range in {
		name := strings.TrimSpace(h.Name)
		value := strings.TrimSpace(h.Value)
		if name == "" && value == "" {
			continue
		}
		if !validHeaderName(name) {
			return nil, nil, InputError{fmt.Sprintf("%q is not a header name", name)}
		}
		if slices.Contains(reservedHeaders, strings.ToLower(name)) {
			return nil, nil, InputError{fmt.Sprintf("%s is set by the delivery itself", name)}
		}
		if strings.ContainsAny(value, "\r\n") || len(value) > 4096 {
			return nil, nil, InputError{fmt.Sprintf("the value of %s must be one line, at most 4096 characters", name)}
		}
		if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, name) }) {
			return nil, nil, InputError{fmt.Sprintf("%s is given twice", name)}
		}
		out = append(out, Header{Name: http.CanonicalHeaderKey(name), Value: value})
		names = append(names, http.CanonicalHeaderKey(name))
	}
	if len(out) > maxHeaders {
		return nil, nil, InputError{fmt.Sprintf("at most %d custom headers", maxHeaders)}
	}
	return out, names, nil
}

// validHeaderName is RFC 9110's token: what a header's name may be made of.
func validHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
}

// newSecret is a signing secret in the Standard Webhooks form: whsec_ and
// base64 of the key, which is what their libraries take as it is.
func newSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("webhook: generate secret: %w", err)
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(b), nil
}
