# Accounts Foundation (Sub-project B) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Yacht users, teams, memberships, sessions and invitations, plus a Mailer seam — the foundation the sign-in surface (sub-project C) sits on.

**Architecture:** `owners` is renamed to `teams`, so every existing `owner_id` foreign key follows automatically and no query in apps, deployments or domains changes. A new `internal/account` package owns people and membership and provides a third `identity.Provider`. A new `internal/notify` package is seam 4: one `Mailer` interface with SMTP, Resend and a logging fallback.

**Tech Stack:** Go 1.26, pgx/v5, sqlc, goose, `crypto/rand` + `crypto/sha256` for tokens, `net/smtp`, Resend HTTP API.

**Spec:** `docs/superpowers/specs/2026-07-31-accounts-teams-email-design.md`

## Global Constraints

- Module path `github.com/codeblocktz/yacht`. Go directive `go 1.26.0`.
- **Use Go 1.26.** Bare `go` may resolve to a ServBay shim at 1.20.5. Check `go version`; if wrong, `export PATH=/usr/local/go/bin:$PATH`.
- **Store tests need Postgres:** `export YACHT_TEST_DATABASE_URL="postgres://ericmaro@localhost:5432/yacht_test?sslmode=disable"`. Without it they silently skip.
- **No Kubernetes types cross the `orchestrator` boundary.**
- **Secrets are stored as SHA-256 hashes, never as tokens** — sessions, magic links and invitations alike. A database dump must not be a set of working credentials.
- **`make boundary` / the CI boundary check** fails on commercial concepts (wallet, billing, invoice, subscription, metering) in non-test engine code.
- `.env.example` must document every `YACHT_*` variable `config.go` reads. `TestEveryVariableReadIsDocumented` scans for them and fails otherwise.
- Run `go vet ./...` and `go test ./... -race -count=1` before every commit.
- Comments explain **why**, not what. Match surrounding density.
- This plan does **not** add HTTP routes, handlers or templates. That is sub-project C.

---

## File Structure

**Create:**
- `internal/store/migrations/00003_accounts.sql` — rename + four new tables
- `internal/store/queries/accounts.sql` — users, memberships, sessions, invitations
- `internal/notify/notify.go` — `Mailer`, `Message`, the `Log` fallback
- `internal/notify/smtp.go`, `internal/notify/resend.go`
- `internal/notify/notify_test.go`, `internal/notify/smtp_test.go`, `internal/notify/resend_test.go`
- `internal/account/token.go` — generation and hashing, shared by all three token kinds
- `internal/account/account.go` — `Service`, users, teams, memberships, roles
- `internal/account/session.go` — sessions and the `identity.Provider`
- `internal/account/magiclink.go`, `internal/account/invitation.go`
- matching `_test.go` files

**Modify:**
- `internal/store/store_test.go` — extend the owner-scope exemptions with reasons
- `internal/identity/identity.go` — rewrite the single-owner claim; seam count
- `internal/orchestrator/orchestrator.go`, `internal/web/slots.go` — seam count `of 3` → `of 4`
- `internal/config/config.go`, `.env.example`

---

### Task 1: Migration — rename owners to teams, add the account tables

**Files:**
- Create: `internal/store/migrations/00003_accounts.sql`
- Modify: `internal/store/store_test.go`

**Interfaces:**
- Produces: tables `teams`, `users`, `memberships`, `sessions`, `invitations`.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
// The rename must carry every foreign key with it. If it does not, apps
// silently lose their parent and the owner-scoping invariant is gone.
func TestOwnersRenamedToTeamsKeepsForeignKeys(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.constraint_column_usage ccu
		  ON tc.constraint_name = ccu.constraint_name
		WHERE tc.constraint_type = 'FOREIGN KEY' AND ccu.table_name = 'teams'
	`).Scan(&n); err != nil {
		t.Fatalf("query foreign keys: %v", err)
	}
	// apps, deployments, domains, memberships, invitations all reference it.
	if n < 3 {
		t.Fatalf("foreign keys referencing teams = %d, want at least 3 (apps, deployments, domains)", n)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.owners') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check owners: %v", err)
	}
	if exists {
		t.Fatal("owners still exists — the rename did not happen")
	}
}

func TestAccountTablesExist(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	for _, table := range []string{"users", "memberships", "sessions", "invitations"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s does not exist", table)
		}
	}
}

// A person may hold one role in a team, not two.
func TestMembershipIsUniquePerUserAndTeam(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	const teamID = "test-membership-team"
	seedOwners(t, pool, teamID)

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"membership@example.test").Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (user_id, owner_id, role) VALUES ($1, $2, 'owner')`,
		userID, teamID); err != nil {
		t.Fatalf("first membership: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (user_id, owner_id, role) VALUES ($1, $2, 'member')`,
		userID, teamID); err == nil {
		t.Fatal("a second membership for the same user and team was accepted")
	}
}
```

Update the exemption list in `TestEveryTableIsOwnerScoped`. Replace the `exempt` map and its comment with:

```go
	// Tables that legitimately carry no owner_id, each for a stated reason.
	// Extending this map is how the invariant gets quietly lost, so every
	// entry says why it is here.
	exempt := map[string]string{
		"teams":             "the principal table itself — its own id is the owner",
		"goose_db_version":  "migration bookkeeping",
		"users":             "a person exists before belonging to any team",
		"sessions":          "a session belongs to a person, not to a team",
	}
```

and change the membership check from `if exempt[name]` to `if _, ok := exempt[name]; ok`. `memberships` and `invitations` are deliberately absent — they are team-scoped and stay under the invariant.

- [ ] **Step 2: Run test to verify it fails**

```bash
export PATH=/usr/local/go/bin:$PATH
export YACHT_TEST_DATABASE_URL="postgres://ericmaro@localhost:5432/yacht_test?sslmode=disable"
go test ./internal/store/ -run 'Teams|AccountTables|Membership' -v
```

Expected: FAIL — `owners still exists`, and the account tables do not exist.

- [ ] **Step 3: Write the migration**

Create `internal/store/migrations/00003_accounts.sql`:

```sql
-- +goose Up
-- +goose StatementBegin

-- A team IS an owner. Renaming rather than adding a table is what makes this
-- change cheap: Postgres carries foreign keys, indexes and constraints through
-- a rename, so every existing owner_id reference follows automatically and no
-- query in apps, deployments or domains changes at all.
--
-- This is the return on putting owner_id on every table in the first place.
-- Had ownership been a join through two other tables, this would be a rewrite.
ALTER TABLE owners RENAME TO teams;

-- Column names stay owner_id. Renaming them to team_id would touch every query
-- and generated struct for no behavioural gain.

-- A person, independent of any team.
CREATE TABLE users (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email        text        NOT NULL,
    display_name text        NOT NULL DEFAULT '',

    -- Carried now, unused until 2FA lands. Adding a column later is a
    -- migration across live data; two nullable ones cost nothing today.
    totp_secret    text,
    totp_confirmed boolean   NOT NULL DEFAULT false,

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Addresses are compared case-insensitively: Alice@ and alice@ are one person,
-- and treating them as two is how an invitation goes to an account nobody
-- can sign in to.
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

-- Where roles live. A person may hold one role in a team, not two.
CREATE TABLE memberships (
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    owner_id   text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    role       text        NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, owner_id)
);

CREATE INDEX memberships_owner_id_idx ON memberships (owner_id);

-- Sessions store a hash, never a token. A database dump must not be a set of
-- working credentials.
CREATE TABLE sessions (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash     bytea       NOT NULL,
    active_team_id text        REFERENCES teams (id) ON DELETE SET NULL,
    user_agent     text        NOT NULL DEFAULT '',
    ip             text        NOT NULL DEFAULT '',
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX sessions_token_hash_key ON sessions (token_hash);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- Invitations are team-scoped, so they carry owner_id and stay under the
-- owner-scoping invariant.
CREATE TABLE invitations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    text        NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    email       text        NOT NULL,
    role        text        NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    token_hash  bytea       NOT NULL,
    invited_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX invitations_token_hash_key ON invitations (token_hash);
CREATE INDEX invitations_owner_id_idx ON invitations (owner_id);

-- One outstanding invitation per address per team. Without this, resending
-- creates a second live token and revoking the first appears to do nothing.
CREATE UNIQUE INDEX invitations_pending_key
    ON invitations (owner_id, lower(email)) WHERE accepted_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS users;
ALTER TABLE teams RENAME TO owners;
-- +goose StatementEnd
```

- [ ] **Step 4: Follow the rename through everything that names the table**

The rename is one SQL statement, but `owners` is named by hand in six places that Postgres cannot fix for you. All of them break at this step. Do not skip any — a missed one fails at `sqlc generate` or at test time, not at migration time.

**`internal/store/queries/apps.sql`** — two queries reference the table by name:

```sql
-- name: CreateTeamRow :one
INSERT INTO teams (id, display_name, email)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        email        = EXCLUDED.email,
        updated_at   = now()
RETURNING *;

-- name: GetTeamRow :one
SELECT * FROM teams WHERE id = $1;
```

Rename `CreateOwner` → `CreateTeamRow` and `GetOwner` → `GetTeamRow`. Task 6 adds a richer `CreateTeam`; these two keep the narrow shape `app.Service` already uses, and the distinct names stop the two colliding in `dbgen`.

**`internal/app/service.go:150`** — `EnsureOwner` calls `q.CreateOwner`. Point it at `q.CreateTeamRow`. Keep the method name `EnsureOwner`: its callers pass an owner id, and `owner_id` still means "the owning team".

**Three test files use raw SQL against the old table.** Change `owners` to `teams` in each:

- `internal/store/store_test.go:64,72` — inside `seedOwners`
- `internal/app/service_test.go:68`
- `internal/domain/store_test.go:51`

Then regenerate:

```bash
go tool sqlc generate
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test ./... -race -count=1
```

Expected: PASS everywhere, including `TestEveryTableIsOwnerScoped`, `TestMigrateIsIdempotent`, and the three new tests.

**Every pre-existing test must still pass.** If `internal/app` or `internal/domain` fails here, the rename broke a foreign key and that is the priority over everything else in this plan.

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations/00003_accounts.sql internal/store/store_test.go
git commit -m "Rename owners to teams, and add users, memberships, sessions and invitations

A team IS an owner, so this is a rename rather than a new table: Postgres
carries the foreign keys, and no query in apps, deployments or domains
changes at all. That is the return on putting owner_id on every table.

Sessions and invitations store a hash, never a token — a database dump
must not be a set of working credentials.

The owner-scoping exemption list now records a reason per entry. users and
sessions genuinely have no owning team; memberships and invitations do and
stay under the invariant. Extending that map silently is how the check
gets lost."
```

---

### Task 2: The notify seam and its logging fallback

**Files:**
- Create: `internal/notify/notify.go`, `internal/notify/notify_test.go`

**Interfaces:**
- Produces:
  - `type Message struct { To, Subject, TextBody string }`
  - `type Mailer interface { Send(ctx context.Context, msg Message) error }`
  - `func NewLog(log *slog.Logger) Mailer`

- [ ] **Step 1: Write the failing test**

Create `internal/notify/notify_test.go`:

```go
package notify

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLogMailerWritesTheMessage(t *testing.T) {
	var buf bytes.Buffer
	m := NewLog(slog.New(slog.NewTextHandler(&buf, nil)))

	err := m.Send(context.Background(), Message{
		To:       "someone@example.test",
		Subject:  "Sign in to Yacht",
		TextBody: "https://yacht.example.test/auth/abc123",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	out := buf.String()
	// The body has to be logged in full. This is the break-glass path when no
	// mail transport is configured — a truncated link is no way back in.
	for _, want := range []string{
		"someone@example.test", "Sign in to Yacht",
		"https://yacht.example.test/auth/abc123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\ngot: %s", want, out)
		}
	}
}

func TestMessageValidation(t *testing.T) {
	m := NewLog(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	for name, msg := range map[string]Message{
		"no recipient": {Subject: "s", TextBody: "b"},
		"no subject":   {To: "a@b.test", TextBody: "b"},
		"no body":      {To: "a@b.test", Subject: "s"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.Send(context.Background(), msg); err == nil {
				t.Fatal("Send accepted an incomplete message")
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/notify/ -v
```

Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

Create `internal/notify/notify.go`:

```go
// Package notify delivers messages to people.
//
// SEAM 4 of 4. The engine sends a small, fixed set of messages — a sign-in
// link, an invitation — and does not care how they arrive. An application
// wrapping the engine supplies Slack, Discord, or a queue by implementing one
// method, without touching anything that composes a message.
package notify

import (
	"context"
	"errors"
	"log/slog"
)

// Message is what the engine wants delivered.
//
// Text only. A control plane's mail is short and transactional, and an HTML
// body is a rendering surface with nothing to gain from it.
type Message struct {
	To       string
	Subject  string
	TextBody string
}

// Validate reports whether the message is deliverable.
func (m Message) Validate() error {
	switch {
	case m.To == "":
		return errors.New("notify: message has no recipient")
	case m.Subject == "":
		return errors.New("notify: message has no subject")
	case m.TextBody == "":
		return errors.New("notify: message has no body")
	}
	return nil
}

// Mailer delivers a message.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// NewLog returns a Mailer that writes messages to the log.
//
// This is the zero-configuration default, and deliberately not an error. It is
// the break-glass path: if accounts are switched on and mail delivery then
// breaks, the sign-in link in the log is the only way back into the dashboard.
// That is safe only because reading the log already implies host access, and it
// is the reason the whole body is logged rather than a summary.
func NewLog(log *slog.Logger) Mailer {
	if log == nil {
		log = slog.Default()
	}
	return &logMailer{log: log}
}

type logMailer struct{ log *slog.Logger }

func (m *logMailer) Send(_ context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	m.log.Info("no mail transport configured — message written to the log instead",
		slog.String("to", msg.To),
		slog.String("subject", msg.Subject),
		slog.String("body", msg.TextBody),
	)
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/notify/ -v
go vet ./internal/notify/
```

Expected: PASS.

- [ ] **Step 5: Update the seam count in the other three packages**

The seam comments read `SEAM n of 3`. There are four now.

```bash
grep -rn "of 3" internal/orchestrator/orchestrator.go internal/identity/identity.go internal/web/slots.go
```

Change each to `of 4`. Do not change anything else in those files yet — the `identity.go` rewrite is Task 9.

- [ ] **Step 6: Commit**

```bash
git add internal/notify/ internal/orchestrator/orchestrator.go internal/identity/identity.go internal/web/slots.go
git commit -m "Add the notification seam, with a logging fallback

Seam 4. One Send method, so a wrapping application supplies Slack or a
queue without touching anything that composes a message.

The logging mailer is the zero-configuration default rather than an
error, and it logs the whole body on purpose: if accounts are switched on
and mail delivery then breaks, the link in the log is the only way back
into the dashboard."
```

---

### Task 3: SMTP mailer

**Files:**
- Create: `internal/notify/smtp.go`, `internal/notify/smtp_test.go`

**Interfaces:**
- Consumes: `Message`, `Mailer` from Task 2.
- Produces: `type SMTPConfig struct { Addr, Username, Password, From string }`, `func NewSMTP(cfg SMTPConfig) (Mailer, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/notify/smtp_test.go`:

```go
package notify

import "testing"

func TestNewSMTPRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]SMTPConfig{
		"no address":   {From: "yacht@example.test"},
		"no from":      {Addr: "smtp.example.test:587"},
		"host only":    {Addr: "smtp.example.test", From: "yacht@example.test"},
		"password only": {
			Addr: "smtp.example.test:587", From: "yacht@example.test",
			Password: "secret",
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSMTP(cfg); err == nil {
				t.Fatalf("NewSMTP accepted %+v", cfg)
			}
		})
	}
}

func TestNewSMTPAcceptsCompleteConfig(t *testing.T) {
	for name, cfg := range map[string]SMTPConfig{
		"anonymous": {Addr: "smtp.example.test:587", From: "yacht@example.test"},
		"authenticated": {
			Addr: "smtp.example.test:587", From: "yacht@example.test",
			Username: "user", Password: "secret",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSMTP(cfg); err != nil {
				t.Fatalf("NewSMTP: %v", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/notify/ -run SMTP -v
```

Expected: FAIL — `undefined: SMTPConfig`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/notify/smtp.go`:

```go
package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// SMTPConfig describes an SMTP relay.
type SMTPConfig struct {
	Addr     string // host:port
	Username string
	Password string
	From     string
}

// NewSMTP returns a Mailer that sends through an SMTP relay.
//
// Configuration is validated here rather than at first send, because the first
// send is a sign-in attempt — and a relay misconfiguration discovered then
// looks to the operator like sign-in being broken.
func NewSMTP(cfg SMTPConfig) (Mailer, error) {
	var errs []error
	if cfg.Addr == "" {
		errs = append(errs, errors.New("notify: SMTP address is required"))
	} else if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		errs = append(errs, fmt.Errorf("notify: SMTP address must be host:port: %w", err))
	}
	if cfg.From == "" {
		errs = append(errs, errors.New("notify: SMTP from address is required"))
	}
	// A password with no username is a configuration slip that would otherwise
	// silently send unauthenticated.
	if cfg.Password != "" && cfg.Username == "" {
		errs = append(errs, errors.New("notify: SMTP password set without a username"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &smtpMailer{cfg: cfg}, nil
}

type smtpMailer struct{ cfg SMTPConfig }

func (m *smtpMailer) Send(_ context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}

	host, _, err := net.SplitHostPort(m.cfg.Addr)
	if err != nil {
		return fmt.Errorf("notify: smtp address: %w", err)
	}

	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, host)
	}

	if err := smtp.SendMail(m.cfg.Addr, auth, m.cfg.From,
		[]string{msg.To}, m.wire(msg)); err != nil {
		return fmt.Errorf("notify: smtp send: %w", err)
	}
	return nil
}

// wire builds the RFC 5322 message.
//
// Headers are written explicitly rather than with a template so that a newline
// in a subject cannot inject one: header injection through a display name is
// the classic mail bug.
func (m *smtpMailer) wire(msg Message) []byte {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(m.cfg.From) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(msg.To) + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(msg.Subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.TextBody)
	return []byte(b.String())
}

func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/notify/ -v
go vet ./internal/notify/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/notify/smtp.go internal/notify/smtp_test.go
git commit -m "Add the SMTP mailer

Config is validated at construction rather than at first send, because
the first send is a sign-in attempt — and a relay misconfiguration
discovered then looks like sign-in being broken.

Headers are written explicitly and stripped of CR/LF: a newline in a
subject is how header injection gets in."
```

---

### Task 4: Resend mailer

**Files:**
- Create: `internal/notify/resend.go`, `internal/notify/resend_test.go`

**Interfaces:**
- Produces: `func NewResend(apiKey, from string) (Mailer, error)`, and an unexported `endpoint` field so tests can point at a stub server.

- [ ] **Step 1: Write the failing test**

Create `internal/notify/resend_test.go`:

```go
package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewResendRejectsIncompleteConfig(t *testing.T) {
	if _, err := NewResend("", "yacht@example.test"); err == nil {
		t.Error("NewResend accepted an empty API key")
	}
	if _, err := NewResend("re_123", ""); err == nil {
		t.Error("NewResend accepted an empty from address")
	}
}

func TestResendPostsTheMessage(t *testing.T) {
	var gotAuth string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"abc"}`))
	}))
	defer srv.Close()

	m, err := NewResend("re_123", "yacht@example.test")
	if err != nil {
		t.Fatalf("NewResend: %v", err)
	}
	m.(*resendMailer).endpoint = srv.URL

	if err := m.Send(context.Background(), Message{
		To: "someone@example.test", Subject: "Sign in", TextBody: "link",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotAuth != "Bearer re_123" {
		t.Errorf("Authorization = %q, want Bearer re_123", gotAuth)
	}
	if body["subject"] != "Sign in" {
		t.Errorf("subject = %v", body["subject"])
	}
}

// A failed send must say so. Swallowing it means a sign-in link that never
// arrives and nothing anywhere explaining why.
func TestResendReportsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"invalid from address"}`))
	}))
	defer srv.Close()

	m, _ := NewResend("re_123", "yacht@example.test")
	m.(*resendMailer).endpoint = srv.URL

	err := m.Send(context.Background(), Message{
		To: "someone@example.test", Subject: "Sign in", TextBody: "link",
	})
	if err == nil {
		t.Fatal("Send reported success on a 422")
	}
	if !strings.Contains(err.Error(), "invalid from address") {
		t.Errorf("error should carry the API's message, got: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/notify/ -run Resend -v
```

Expected: FAIL — `undefined: NewResend`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/notify/resend.go`:

```go
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const resendEndpoint = "https://api.resend.com/emails"

// NewResend returns a Mailer that sends through Resend's HTTP API.
//
// Chosen alongside SMTP because a self-hoster with no relay still needs mail,
// and an API key is less to get wrong than a relay. The HTTP call is written
// directly rather than pulling in an SDK: it is one POST, and a dependency that
// exists to save fifteen lines is a dependency to keep updated forever.
func NewResend(apiKey, from string) (Mailer, error) {
	var errs []error
	if apiKey == "" {
		errs = append(errs, errors.New("notify: Resend API key is required"))
	}
	if from == "" {
		errs = append(errs, errors.New("notify: Resend from address is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &resendMailer{
		apiKey:   apiKey,
		from:     from,
		endpoint: resendEndpoint,
		client:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

type resendMailer struct {
	apiKey   string
	from     string
	endpoint string
	client   *http.Client
}

func (m *resendMailer) Send(ctx context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}

	payload, err := json.Marshal(map[string]any{
		"from":    m.from,
		"to":      []string{msg.To},
		"subject": msg.Subject,
		"text":    msg.TextBody,
	})
	if err != nil {
		return fmt.Errorf("notify: resend encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint,
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notify: resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: resend send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		// Carry the API's own message: "422" alone gives an operator nothing
		// to act on, and this is the error they will actually see.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Message != "" {
			return fmt.Errorf("notify: resend rejected the message: %s", apiErr.Message)
		}
		return fmt.Errorf("notify: resend returned %s", resp.Status)
	}
	return nil
}
```

- [ ] **Step 4: Run tests and commit**

```bash
go test ./internal/notify/ -v
go vet ./...
git add internal/notify/resend.go internal/notify/resend_test.go
git commit -m "Add the Resend mailer

Written as one POST rather than pulling in an SDK: a dependency that
exists to save fifteen lines is one to keep updated forever.

API errors carry Resend's own message, because \"422\" alone gives an
operator nothing to act on and this is the error they will actually see."
```

---

### Task 5: Token generation and hashing

**Files:**
- Create: `internal/account/token.go`, `internal/account/token_test.go`

**Interfaces:**
- Produces: `func NewToken() (raw string, hash []byte, err error)`, `func HashToken(raw string) []byte`

Shared by sessions, magic links and invitations — one implementation so all three get the same properties.

- [ ] **Step 1: Write the failing test**

Create `internal/account/token_test.go`:

```go
package account

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestNewTokenIsUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		raw, hash, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[raw] {
			t.Fatal("NewToken returned a duplicate")
		}
		seen[raw] = true

		// Enough entropy that guessing is not a strategy.
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			t.Fatalf("token is not URL-safe base64: %v", err)
		}
		if len(decoded) < 32 {
			t.Fatalf("token entropy = %d bytes, want at least 32", len(decoded))
		}
		if len(hash) != 32 {
			t.Fatalf("hash = %d bytes, want 32 (SHA-256)", len(hash))
		}
	}
}

func TestHashTokenMatchesNewToken(t *testing.T) {
	raw, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !bytes.Equal(HashToken(raw), hash) {
		t.Fatal("HashToken does not reproduce the hash NewToken returned")
	}
}

// The raw token must never be derivable from what is stored.
func TestHashIsNotTheToken(t *testing.T) {
	raw, hash, _ := NewToken()
	if bytes.Contains(hash, []byte(raw)) {
		t.Fatal("the stored hash contains the token")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/account/ -v
```

Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

Create `internal/account/token.go`:

```go
// Package account owns people: users, the teams they belong to, the role they
// hold in each, and the sessions and invitations that get them there.
package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// tokenBytes is the entropy behind every credential this package issues.
// 32 bytes is beyond brute force and costs nothing.
const tokenBytes = 32

// NewToken returns a fresh credential and the hash to store for it.
//
// One implementation shared by sessions, magic links and invitations, so all
// three get the same entropy and the same storage rule rather than three
// chances to get it wrong.
//
// The caller sends raw to the person and stores hash. Never the reverse: a
// database dump must not be a set of working credentials.
func NewToken() (raw string, hash []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("account: generate token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashToken(raw), nil
}

// HashToken returns the stored form of a token.
//
// Plain SHA-256, deliberately: unlike a password this is full-entropy random,
// so there is nothing to brute-force and a slow KDF would only add latency to
// every request that carries a session cookie.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
```

- [ ] **Step 4: Run tests and commit**

```bash
go test ./internal/account/ -v
go vet ./internal/account/
git add internal/account/token.go internal/account/token_test.go
git commit -m "Add token generation and hashing

One implementation shared by sessions, magic links and invitations, so
all three get the same entropy and the same storage rule rather than
three chances to get it wrong.

Plain SHA-256 rather than a KDF: these are full-entropy random values,
so there is nothing to brute-force, and a slow hash would add latency to
every request carrying a session cookie."
```

---

### Task 6: Account queries and the users/teams/memberships service

**Files:**
- Create: `internal/store/queries/accounts.sql`, `internal/account/account.go`, `internal/account/account_test.go`
- Regenerates: `internal/store/dbgen/*`

**Interfaces:**
- Consumes: the schema from Task 1.
- Produces:
  - `type Role string` with `RoleOwner`, `RoleAdmin`, `RoleMember`
  - `func (r Role) CanAdminister() bool`, `func (r Role) CanOwn() bool`, `func (r Role) AtLeast(other Role) bool`
  - `type Service struct{...}`, `func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service`
  - `func (s *Service) EnsureUser(ctx, email, displayName string) (User, error)`
  - `func (s *Service) CreateTeam(ctx, id, name string, ownerUserID uuid.UUID) (Team, error)`
  - `func (s *Service) TeamsFor(ctx, userID uuid.UUID) ([]Membership, error)`
  - `func (s *Service) RoleIn(ctx, userID uuid.UUID, teamID string) (Role, error)`
  - `func (s *Service) SetRole(ctx, actor uuid.UUID, teamID string, target uuid.UUID, role Role) error`
  - `func (s *Service) RemoveMember(ctx, actor uuid.UUID, teamID string, target uuid.UUID) error`
  - `var ErrLastOwner = errors.New("account: a team must keep at least one owner")`
  - `var ErrNotAMember = errors.New("account: not a member of this team")`

- [ ] **Step 1: Write the failing test**

Create `internal/account/account_test.go` with a `testService` helper following `internal/domain/store_test.go`'s pattern (migrate, connect, purge before and via `t.Cleanup`), and these tests:

```go
func TestRoleOrdering(t *testing.T) {
	if !RoleOwner.AtLeast(RoleAdmin) || !RoleOwner.AtLeast(RoleMember) {
		t.Error("owner must outrank admin and member")
	}
	if !RoleAdmin.AtLeast(RoleMember) {
		t.Error("admin must outrank member")
	}
	if RoleMember.AtLeast(RoleAdmin) {
		t.Error("member must not outrank admin")
	}
	if !RoleAdmin.CanAdminister() || RoleMember.CanAdminister() {
		t.Error("CanAdminister: admin yes, member no")
	}
	if !RoleOwner.CanOwn() || RoleAdmin.CanOwn() {
		t.Error("CanOwn: owner only")
	}
}

// Addresses are one identity regardless of case. Treating Alice@ and alice@ as
// two people sends an invitation to an account nobody can sign in to.
func TestEnsureUserIsCaseInsensitive(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	a, err := s.EnsureUser(ctx, "Alice@Example.Test", "Alice")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	b, err := s.EnsureUser(ctx, "alice@example.test", "")
	if err != nil {
		t.Fatalf("EnsureUser again: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("two users created for one address: %s and %s", a.ID, b.ID)
	}
}

func TestCreateTeamMakesTheCreatorAnOwner(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "owner@example.test", "Owner")
	if _, err := s.CreateTeam(ctx, "team-create", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	role, err := s.RoleIn(ctx, u.ID, "team-create")
	if err != nil {
		t.Fatalf("RoleIn: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("role = %q, want owner", role)
	}
}

// A team with no owner cannot be administered by anyone ever again.
func TestLastOwnerCannotBeDemotedOrRemoved(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "solo@example.test", "Solo")
	if _, err := s.CreateTeam(ctx, "team-last-owner", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	if err := s.SetRole(ctx, u.ID, "team-last-owner", u.ID, RoleMember); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the last owner: want ErrLastOwner, got %v", err)
	}
	if err := s.RemoveMember(ctx, u.ID, "team-last-owner", u.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the last owner: want ErrLastOwner, got %v", err)
	}
}

func TestOnlyAnOwnerCanManageAdmins(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	owner, _ := s.EnsureUser(ctx, "o@example.test", "O")
	admin, _ := s.EnsureUser(ctx, "a@example.test", "A")
	if _, err := s.CreateTeam(ctx, "team-roles", "Team", owner.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := s.SetRole(ctx, owner.ID, "team-roles", admin.ID, RoleAdmin); err != nil {
		t.Fatalf("owner promoting to admin: %v", err)
	}

	// An admin must not be able to make another admin — that is ownership.
	member, _ := s.EnsureUser(ctx, "m@example.test", "M")
	if err := s.SetRole(ctx, admin.ID, "team-roles", member.ID, RoleAdmin); err == nil {
		t.Fatal("an admin promoted someone to admin")
	}
	if err := s.SetRole(ctx, admin.ID, "team-roles", member.ID, RoleMember); err != nil {
		t.Fatalf("an admin must be able to add a member: %v", err)
	}
}

func TestRoleInReportsNonMembers(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "stranger@example.test", "S")
	if _, err := s.RoleIn(ctx, u.ID, "team-does-not-exist"); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("want ErrNotAMember, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/account/ -v
```

Expected: FAIL to compile — `Role`, `Service`, `EnsureUser` undefined.

- [ ] **Step 3: Write the queries**

Create `internal/store/queries/accounts.sql`:

```sql
-- name: UpsertUser :one
INSERT INTO users (email, display_name)
VALUES (lower(@email), @display_name)
ON CONFLICT (lower(email)) DO UPDATE
SET display_name = CASE
        WHEN excluded.display_name <> '' THEN excluded.display_name
        ELSE users.display_name
    END,
    updated_at = now()
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower(@email);

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id;

-- name: CreateTeam :one
INSERT INTO teams (id, display_name)
VALUES (@id, @display_name)
ON CONFLICT (id) DO UPDATE SET display_name = excluded.display_name, updated_at = now()
RETURNING *;

-- name: GetTeam :one
SELECT * FROM teams WHERE id = @id;

-- name: UpsertMembership :one
INSERT INTO memberships (user_id, owner_id, role)
VALUES (@user_id, @owner_id, @role)
ON CONFLICT (user_id, owner_id) DO UPDATE SET role = excluded.role
RETURNING *;

-- name: GetMembership :one
SELECT * FROM memberships WHERE user_id = @user_id AND owner_id = @owner_id;

-- name: ListMembershipsForUser :many
SELECT m.*, t.display_name AS team_name
FROM memberships m
JOIN teams t ON t.id = m.owner_id
WHERE m.user_id = @user_id
ORDER BY t.display_name;

-- name: ListMembersOfTeam :many
SELECT m.*, u.email, u.display_name AS user_name
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.owner_id = @owner_id
ORDER BY u.email;

-- name: DeleteMembership :exec
DELETE FROM memberships WHERE user_id = @user_id AND owner_id = @owner_id;

-- name: CountOwnersOfTeam :one
SELECT count(*) FROM memberships WHERE owner_id = @owner_id AND role = 'owner';
```

Run `go tool sqlc generate`.

- [ ] **Step 4: Write the implementation**

Create `internal/account/account.go` with `Role`, its comparison helpers, `Service`, and the methods listed under Interfaces. Key behaviours the tests pin down:

- `EnsureUser` lowercases the address before writing and returns the existing row on conflict.
- `CreateTeam` creates the team and the creator's `owner` membership **in one transaction** — a team whose creator is not a member is unadministrable.
- `SetRole` and `RemoveMember` check the actor's role first: `RoleAdmin` may set `RoleMember` only; `RoleOwner` may set any role.
- Both refuse when the change would leave the team with zero owners, using `CountOwnersOfTeam` **inside the transaction** so two concurrent demotions cannot both pass the check.
- `RoleIn` returns `ErrNotAMember` rather than an empty role, so a caller cannot mistake "no membership" for "least privilege".

Write `Role` as:

```go
// Role is what a person may do in a team.
//
// The split is at the line where an action stops being undoable by
// redeploying: a member deploys, scales and redeploys; an admin can also
// delete apps, manage domains and storage, and invite people; an owner can
// also manage admins and the team itself.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

var rank = map[Role]int{RoleMember: 1, RoleAdmin: 2, RoleOwner: 3}

// AtLeast reports whether r carries at least the authority of other.
func (r Role) AtLeast(other Role) bool { return rank[r] >= rank[other] && rank[r] > 0 }

// CanAdminister covers deleting apps, managing domains and storage, and
// inviting people.
func (r Role) CanAdminister() bool { return r.AtLeast(RoleAdmin) }

// CanOwn covers managing admins, transferring ownership and deleting the team.
func (r Role) CanOwn() bool { return r == RoleOwner }

// Valid reports whether r is a role the system knows.
func (r Role) Valid() bool { _, ok := rank[r]; return ok }
```

- [ ] **Step 5: Run tests and commit**

```bash
go test ./internal/account/ -v
go test ./... -race -count=1
go vet ./...
git add internal/store/queries/accounts.sql internal/store/dbgen/ internal/account/
git commit -m "Add users, teams, memberships and roles

The role split is at the line where an action stops being undoable by
redeploying: a member deploys, scales and redeploys; an admin can also
delete apps and invite people; an owner can also manage admins.

A team can never reach zero owners — the check runs inside the
transaction, so two concurrent demotions cannot both pass it. A team with
no owner cannot be administered by anyone, ever again.

RoleIn returns ErrNotAMember rather than an empty role, so a caller
cannot mistake absence of membership for least privilege."
```

---

### Task 7: Sessions and the identity provider

**Files:**
- Create: `internal/account/session.go`, `internal/account/session_test.go`
- Modify: `internal/store/queries/accounts.sql`

**Interfaces:**
- Consumes: `NewToken`/`HashToken` (Task 5), `Service` (Task 6).
- Produces:
  - `func (s *Service) CreateSession(ctx, userID uuid.UUID, teamID, userAgent, ip string, ttl time.Duration) (raw string, err error)`
  - `func (s *Service) ResolveSession(ctx, raw string) (Session, error)`
  - `func (s *Service) SwitchTeam(ctx, sessionID uuid.UUID, teamID string) error`
  - `func (s *Service) RevokeSession(ctx, raw string) error`
  - `func (s *Service) RevokeAllSessions(ctx, userID uuid.UUID) error`
  - `type Sessions struct{...}` implementing `identity.Provider`, `func (s *Service) Provider(cookieName string) *Sessions`
  - `var ErrSessionInvalid = errors.New("account: session is not valid")`

- [ ] **Step 1: Write the failing test**

Add to a new `internal/account/session_test.go`:

```go
func TestSessionRoundTrip(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "session@example.test", "S")
	if _, err := s.CreateTeam(ctx, "team-session", "Team", u.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	raw, err := s.CreateSession(ctx, u.ID, "team-session", "test-agent", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.ResolveSession(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if got.UserID != u.ID || got.ActiveTeamID != "team-session" {
		t.Fatalf("session = %+v, want user %s in team-session", got, u.ID)
	}
}

// The database must not hold anything that can be replayed.
func TestSessionTokenIsStoredAsAHash(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "hash@example.test", "H")
	_, _ = s.CreateTeam(ctx, "team-hash", "Team", u.ID)
	raw, err := s.CreateSession(ctx, u.ID, "team-hash", "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE encode(token_hash, 'escape') = $1`,
		raw).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatal("the raw session token is present in the database")
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "expired@example.test", "E")
	_, _ = s.CreateTeam(ctx, "team-expired", "Team", u.ID)
	raw, _ := s.CreateSession(ctx, u.ID, "team-expired", "", "", -time.Minute)

	if _, err := s.ResolveSession(ctx, raw); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("want ErrSessionInvalid for an expired session, got %v", err)
	}
}

func TestUnknownSessionIsRejected(t *testing.T) {
	s := testService(t)
	if _, err := s.ResolveSession(context.Background(), "not-a-real-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("want ErrSessionInvalid, got %v", err)
	}
}

// Without this a leaked session cannot be revoked.
func TestRevokeAllSessionsSignsOutEverywhere(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "everywhere@example.test", "E")
	_, _ = s.CreateTeam(ctx, "team-everywhere", "Team", u.ID)

	first, _ := s.CreateSession(ctx, u.ID, "team-everywhere", "", "", time.Hour)
	second, _ := s.CreateSession(ctx, u.ID, "team-everywhere", "", "", time.Hour)

	if err := s.RevokeAllSessions(ctx, u.ID); err != nil {
		t.Fatalf("RevokeAllSessions: %v", err)
	}
	for name, raw := range map[string]string{"first": first, "second": second} {
		if _, err := s.ResolveSession(ctx, raw); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("%s session still resolves after sign-out everywhere", name)
		}
	}
}

// The provider is what lets every existing handler stay untouched.
func TestSessionsImplementsIdentityProvider(t *testing.T) {
	s := testService(t)
	ctx := context.Background()

	u, _ := s.EnsureUser(ctx, "provider@example.test", "P")
	_, _ = s.CreateTeam(ctx, "team-provider", "Team", u.ID)
	raw, _ := s.CreateSession(ctx, u.ID, "team-provider", "", "", time.Hour)

	var p identity.Provider = s.Provider("yacht_session")

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "yacht_session", Value: raw})

	owner, err := p.Resolve(ctx, r)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The owner is the active TEAM, not the user. Everything downstream is
	// scoped by owner_id, and owner_id means team.
	if owner.ID != "team-provider" {
		t.Fatalf("owner.ID = %q, want the active team", owner.ID)
	}

	bare := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := p.Resolve(ctx, bare); err == nil {
		t.Fatal("Resolve accepted a request with no cookie")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/account/ -run Session -v
```

Expected: FAIL to compile — `CreateSession` undefined.

- [ ] **Step 3: Add the queries**

Append to `internal/store/queries/accounts.sql`, then `go tool sqlc generate`:

```sql
-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, active_team_id, user_agent, ip, expires_at)
VALUES (@user_id, @token_hash, @active_team_id, @user_agent, @ip, @expires_at)
RETURNING *;

-- Expiry is filtered in SQL rather than in Go so that an expired row can never
-- be treated as valid by a caller that forgets to check.
-- name: GetSessionByHash :one
SELECT * FROM sessions
WHERE token_hash = @token_hash AND expires_at > now();

-- name: SetSessionTeam :exec
UPDATE sessions SET active_team_id = @active_team_id WHERE id = @id;

-- name: DeleteSessionByHash :exec
DELETE FROM sessions WHERE token_hash = @token_hash;

-- name: DeleteSessionsForUser :exec
DELETE FROM sessions WHERE user_id = @user_id;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at <= now();
```

- [ ] **Step 4: Write the implementation**

Create `internal/account/session.go`. The provider:

```go
// Sessions resolves the owner of a request from a session cookie.
//
// The third identity.Provider the engine ships, alongside SingleOwner and
// StaticToken. It exists as a Provider rather than as a replacement for the
// seam so that every handler, the app service, the store and the orchestrator
// stay untouched: they already take an OwnerID and never ask where it came
// from, and a wrapping application still swaps the whole provider out.
type Sessions struct {
	svc    *Service
	cookie string
}

// Resolve returns the ACTIVE TEAM as the owner, not the user.
//
// Everything downstream is scoped by owner_id, and owner_id means team. A
// session that resolved to the person would silently widen every query to
// every team they belong to.
func (p *Sessions) Resolve(ctx context.Context, r *http.Request) (identity.Owner, error) {
	c, err := r.Cookie(p.cookie)
	if err != nil || c.Value == "" {
		return identity.Owner{}, identity.ErrUnauthenticated
	}

	sess, err := p.svc.ResolveSession(ctx, c.Value)
	if err != nil {
		return identity.Owner{}, identity.ErrUnauthenticated
	}
	if sess.ActiveTeamID == "" {
		return identity.Owner{}, identity.ErrUnauthenticated
	}
	...
}
```

`ResolveSession` hashes the presented token and looks up by hash — never by comparing raw values — and returns `ErrSessionInvalid` uniformly for unknown, expired and malformed tokens so the failures are indistinguishable.

`SwitchTeam` must verify the user is a member of the target team before writing, or a crafted request switches into someone else's team.

- [ ] **Step 5: Run tests and commit**

```bash
go test ./internal/account/ -v
go test ./... -race -count=1
git add internal/account/session.go internal/account/session_test.go internal/store/queries/accounts.sql internal/store/dbgen/
git commit -m "Add sessions, and the third identity provider

Sessions resolve to the active TEAM, not to the person. Everything
downstream is scoped by owner_id and owner_id means team; resolving to
the user would silently widen every query to every team they belong to.

Implemented as an identity.Provider rather than as a replacement for the
seam, so every handler, the store and the orchestrator stay untouched.

Unknown, expired and malformed tokens all return the same error, and
expiry is filtered in SQL so an expired row cannot be treated as valid by
a caller that forgets to check."
```

---

### Task 8: Magic links and invitations

**Files:**
- Create: `internal/account/magiclink.go`, `internal/account/invitation.go` and their tests
- Modify: `internal/store/queries/accounts.sql`

**Interfaces:**
- Produces:
  - `func (s *Service) IssueMagicLink(ctx, email string, ttl time.Duration) (raw string, user User, existed bool, err error)`
  - `func (s *Service) ConsumeMagicLink(ctx, raw string) (User, error)`
  - `func (s *Service) Invite(ctx, actor uuid.UUID, teamID, email string, role Role, ttl time.Duration) (raw string, err error)`
  - `func (s *Service) AcceptInvitation(ctx, raw string, userID uuid.UUID) (teamID string, role Role, err error)`
  - `func (s *Service) RevokeInvitation(ctx, actor uuid.UUID, teamID string, id uuid.UUID) error`
  - `var ErrTokenInvalid = errors.New("account: token is not valid")`

**Add a `magic_links` table** in a new migration `internal/store/migrations/00004_magic_links.sql`:

```sql
-- +goose Up
-- +goose StatementBegin

-- Kept separate from invitations rather than folded into them. The two mean
-- different things — one proves an address, the other grants a role — expire
-- on different timescales, and are consumed by different flows. One table
-- serving both would need a discriminator column and a partial index per
-- meaning, which is the same separation with more ways to get it wrong.
CREATE TABLE magic_links (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  bytea       NOT NULL,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX magic_links_token_hash_key ON magic_links (token_hash);
CREATE INDEX magic_links_user_id_idx ON magic_links (user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS magic_links;
-- +goose StatementEnd
```

`magic_links` belongs to a user, not a team, so add it to the `TestEveryTableIsOwnerScoped` exemption map from Task 1 with the reason `"a sign-in link proves an address, before any team is chosen"`.

- [ ] **Step 1: Write the failing tests**

Cover, at minimum:

```go
// Issuing must not reveal whether an address is registered. The caller decides
// what to send; this reports `existed` so it can send nothing without the
// response differing.
func TestIssueMagicLinkDoesNotRevealExistence(t *testing.T)

// A link in a mail archive is a permanent key otherwise.
func TestMagicLinkIsSingleUse(t *testing.T)
func TestExpiredMagicLinkIsRejected(t *testing.T)
func TestMagicLinkTokenIsStoredAsAHash(t *testing.T)

// Only an admin or owner may invite.
func TestMemberCannotInvite(t *testing.T)
// One outstanding invitation per address per team.
func TestReinvitingReplacesThePendingInvitation(t *testing.T)
func TestAcceptedInvitationCreatesTheMembership(t *testing.T)
func TestAcceptedInvitationCannotBeReused(t *testing.T)
func TestRevokedInvitationCannotBeAccepted(t *testing.T)
// An admin must not be able to invite someone as an owner.
func TestAdminCannotInviteAnOwner(t *testing.T)
```

Write each with real bodies following the patterns in Tasks 6 and 7.

- [ ] **Step 2: Run to verify they fail, 3: implement, 4: run to verify they pass**

Consumption must be **atomic**: `UPDATE magic_links SET consumed_at = now() WHERE token_hash = $1 AND consumed_at IS NULL AND expires_at > now() RETURNING user_id`. Checking then updating leaves a window where one link signs in twice.

The same applies to `AcceptInvitation`.

- [ ] **Step 5: Commit**

```bash
git commit -m "Add magic links and invitations

Both are consumed with a single conditional UPDATE ... RETURNING rather
than a check followed by a write: checking first leaves a window where
one link is redeemed twice.

Issuing a magic link reports whether the address existed rather than
acting on it, so the caller can send nothing without the response
differing — a sign-in form that reveals which addresses are registered is
a user-list disclosure."
```

---

### Task 9: Configuration, wiring, and the honest seam comment

**Files:**
- Modify: `internal/config/config.go`, `.env.example`, `internal/identity/identity.go`, `cmd/yacht/main.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.BaseURL`, `Config.SMTP*`, `Config.ResendAPIKey`, `Config.SessionTTL`, `Config.MagicLinkTTL`, `func (c Config) AccountsEnabled() bool`

- [ ] **Step 1: Write the failing test**

```go
// Turning sign-in on with no way to deliver a link locks the operator out of
// their own dashboard permanently. Accounts stay off unless a link can be
// delivered and a URL exists to put in it.
func TestAccountsRequireABaseURL(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_SMTP_ADDR": "smtp.example.test:587"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AccountsEnabled() {
		t.Fatal("accounts enabled with no YACHT_BASE_URL — the link would point nowhere")
	}
}

func TestAccountsEnabledWithBaseURL(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_BASE_URL": "https://yacht.example.test"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.AccountsEnabled() {
		t.Fatal("accounts should be enabled once a base URL is set")
	}
}

func TestSMTPAndResendTogetherFail(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_BASE_URL":       "https://yacht.example.test",
		"YACHT_SMTP_ADDR":      "smtp.example.test:587",
		"YACHT_SMTP_FROM":      "yacht@example.test",
		"YACHT_RESEND_API_KEY": "re_123",
	})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted two mail transports at once")
	}
}

func TestSMTPWithoutFromFails(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_BASE_URL":  "https://yacht.example.test",
		"YACHT_SMTP_ADDR": "smtp.example.test:587",
	})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted SMTP with no from address")
	}
}

func TestMalformedBaseURLFails(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_BASE_URL": "not a url"})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a malformed base URL")
	}
}
```

- [ ] **Step 2–4: implement, document, verify**

`AccountsEnabled` returns `c.BaseURL != ""`. The mail transport is independent: with a base URL and no transport, the logging mailer is used and the link goes to the log — the documented break-glass path.

Document all seven variables in `.env.example`. `TestEveryVariableReadIsDocumented` will fail otherwise.

- [ ] **Step 5: Rewrite the identity seam comment**

`internal/identity/identity.go:7-13` currently says the engine is single-owner. That stops being true. Replace that paragraph with an honest one: the engine now ships a session-backed provider and hosts several teams, and the seam survives because `account.Sessions` is *an implementation of* `Provider` rather than a replacement for it — a wrapping application still supplies its own.

Do not delete the reasoning. Rewrite it to match what is now true.

- [ ] **Step 6: Wire it in `cmd/yacht/main.go`**

`newIdentity` gains a branch: when `cfg.AccountsEnabled()`, build the account service and return its provider; otherwise fall back to the existing `StaticToken`/`SingleOwner` behaviour unchanged. Log which one is active — an operator must be able to tell from the log whether the dashboard has accounts on.

- [ ] **Step 7: Full verification and commit**

```bash
go vet ./...
CGO_ENABLED=0 go build ./...
go test ./... -race -count=1
git commit -m "Enable accounts only when a sign-in link can actually arrive

Accounts stay off unless YACHT_BASE_URL is set, because turning sign-in
on with no way to deliver a link locks the operator out of their own
dashboard with no recovery path.

Rewrote identity.go's claim that the engine is single-owner. It hosts
several teams now. The seam survives because account.Sessions is an
implementation of Provider rather than a replacement for it — which is
why no handler, no store query and no orchestrator call changed."
```

---

## Final verification

- [ ] **Run the full standard**

```bash
export PATH=/usr/local/go/bin:$PATH
export YACHT_TEST_DATABASE_URL="postgres://ericmaro@localhost:5432/yacht_test?sslmode=disable"

go vet ./...
CGO_ENABLED=0 go build ./...
go test ./... -race -count=1
go test ./internal/store -v -count=1 | grep -c SKIP   # must be 0
make generate && make css
git diff --exit-code -- '*_templ.go' internal/web/assets/css/app.css
git status --short
git check-ignore -v $(git ls-files --others --exclude-standard) 2>/dev/null
```

- [ ] **Confirm the rename did not break sub-project A**

```bash
go test ./internal/app/ ./internal/domain/ -v -count=1
```

Every hostname test must still pass. If any fails, the rename broke a foreign key and that is the priority over everything else.

- [ ] **Confirm no HTTP surface was added.** This plan is the foundation only.

```bash
git diff --stat cb9989c..HEAD -- internal/web/
```

Expected: only the seam-count comment change in `slots.go`.

---

## Out of scope

- **Sub-project C** — sign-in page, session cookies over HTTP, invitation accept flow, team switcher, team management page, `requireRole` route gates. Next plan.
- **TOTP enrolment** — columns carried, no flow.
- **Volumes** — separate sub-project.
