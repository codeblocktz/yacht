# Per-App Hostnames and Shared Wildcard TLS Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every Yacht app a working hostname at create time, served over TLS from a single shared wildcard certificate, so a deployed app is actually reachable.

**Architecture:** A new `internal/domain` package holds pure hostname policy (issue, reserved-suffix) plus thin store helpers over the existing `domains` table. `AppSpec` gains `Hosts` and `TLS`; the Kubernetes backend turns those into an Ingress alongside the existing Deployment and Service, and prunes it when they go away. No Kubernetes types cross the orchestrator seam.

**Tech Stack:** Go 1.26, chi, pgx/v5, sqlc, goose, templ, client-go server-side apply, `k8s.io/client-go/kubernetes/fake` for tests, Postgres 18 for store tests.

**Spec:** `docs/superpowers/specs/2026-07-31-per-app-hostnames-design.md`

## Global Constraints

- Module path is `github.com/codeblocktz/yacht`. Go directive is `go 1.26.0`.
- **Use Go 1.26.** `go` on PATH may resolve to a ServBay shim at 1.20.5. Verify with `go version` before starting; if wrong, `export PATH=/usr/local/go/bin:$PATH`.
- **Store tests need Postgres.** `export YACHT_TEST_DATABASE_URL="postgres://ericmaro@localhost:5432/yacht_test?sslmode=disable"`. Without it they silently skip.
- **No Kubernetes types may cross the `orchestrator` package boundary.** Callers must never import `client-go`.
- **Every resource table carries `owner_id`; unique constraints are scoped by it** — except `domains.host`, which is correctly global.
- Templ output and the compiled stylesheet are committed. CI fails on drift. Run `make generate` and `make css` after touching `.templ` or CSS.
- `.env.example` must document every `YACHT_*` variable `config.go` reads. A test enforces this.
- Run `go test ./... -race -count=1` before each commit. Run `go vet ./...` too.
- Comments explain *why*, not *what*. Match the surrounding density — this codebase comments decisions, not mechanics.

---

## File Structure

**Create:**
- `internal/domain/policy.go` — pure hostname policy. No DB, no Kubernetes.
- `internal/domain/policy_test.go`
- `internal/domain/store.go` — helpers over `domains` rows, taking `*dbgen.Queries` so they compose with an open transaction.
- `internal/domain/store_test.go`
- `internal/store/migrations/00002_domains_managed.sql`
- `internal/store/queries/domains.sql`
- `internal/orchestrator/k8s/ingress.go` — `applyIngress` and `deleteIngress`.
- `internal/orchestrator/k8s/ingress_test.go`

**Modify:**
- `internal/orchestrator/orchestrator.go` — `AppSpec.Hosts`, `AppSpec.TLS`, validation.
- `internal/orchestrator/k8s/app.go` — call `applyIngress`, prune Service and Ingress, delete Ingress in `DeleteApp`.
- `internal/config/config.go` — three new variables.
- `internal/app/service.go` — `Options`, issue on create, reissue and pass hosts on apply.
- `cmd/yacht/main.go` — wire config through.
- `internal/web/server.go`, `pages.templ` — render the URL, show TLS posture.
- `.env.example`

**Why `internal/domain` is a package and not more of `internal/app`:** `service.go` is already 526 lines and owns the app lifecycle. The reserved-suffix rule is the one piece here that is unsafe to get subtly wrong, so it belongs somewhere pure and directly testable.

---

### Task 1: Pure hostname policy

**Files:**
- Create: `internal/domain/policy.go`
- Test: `internal/domain/policy_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func Issue(appName, appDomain string) (string, error)`
  - `func Reserved(host, appDomain string, extra []string) bool`
  - `var ErrNoAppDomain = errors.New("domain: no app domain configured")`

- [ ] **Step 1: Write the failing test**

Create `internal/domain/policy_test.go`:

```go
package domain

import (
	"errors"
	"testing"
)

func TestIssue(t *testing.T) {
	cases := []struct {
		name      string
		appName   string
		appDomain string
		want      string
		wantErr   bool
	}{
		{"simple", "web", "apps.example.com", "web.apps.example.com", false},
		{"uppercase is normalised", "web", "Apps.Example.COM", "web.apps.example.com", false},
		{"trailing dot is trimmed", "web", "apps.example.com.", "web.apps.example.com", false},
		{"dashes are fine", "my-api", "apps.example.com", "my-api.apps.example.com", false},
		{"no app domain", "web", "", "", true},
		{"app domain must have a dot", "web", "localhost", "", true},
		{"app name with a dot is rejected", "a.b", "apps.example.com", "", true},
		{"app name with underscore is rejected", "a_b", "apps.example.com", "", true},
		{"empty app name", "", "apps.example.com", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Issue(tc.appName, tc.appDomain)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Issue(%q, %q) = %q, want error", tc.appName, tc.appDomain, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Issue(%q, %q): %v", tc.appName, tc.appDomain, err)
			}
			if got != tc.want {
				t.Fatalf("Issue(%q, %q) = %q, want %q", tc.appName, tc.appDomain, got, tc.want)
			}
		})
	}
}

func TestIssueWithoutAppDomainIsDistinguishable(t *testing.T) {
	// Callers treat "no app domain configured" as the feature being off, not
	// as a failure, so it must be tellable apart from a malformed domain.
	if _, err := Issue("web", ""); !errors.Is(err, ErrNoAppDomain) {
		t.Fatalf("want ErrNoAppDomain, got %v", err)
	}
	if _, err := Issue("web", "localhost"); errors.Is(err, ErrNoAppDomain) {
		t.Fatal("a malformed domain must not report as ErrNoAppDomain")
	}
}

// TestReservedMatchesOnLabelBoundary is the most important test in this
// package. Suffix matching on raw strings says evilapps.example.com is inside
// apps.example.com, which would let a tenant claim a host under the platform
// domain — the exact hole the reserved rule exists to close.
func TestReservedMatchesOnLabelBoundary(t *testing.T) {
	const appDomain = "apps.example.com"

	cases := []struct {
		host string
		want bool
	}{
		{"apps.example.com", true},
		{"web.apps.example.com", true},
		{"a.b.apps.example.com", true},
		{"WEB.APPS.EXAMPLE.COM", true},
		{"web.apps.example.com.", true},

		{"evilapps.example.com", false},
		{"notapps.example.com", false},
		{"apps.example.com.evil.test", false},
		{"example.com", false},
		{"other.test", false},
	}

	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := Reserved(tc.host, appDomain, nil); got != tc.want {
				t.Fatalf("Reserved(%q, %q) = %v, want %v", tc.host, appDomain, got, tc.want)
			}
		})
	}
}

func TestReservedHonoursExtraDomains(t *testing.T) {
	extra := []string{"internal.example.com", "admin.example.com"}

	if !Reserved("thing.internal.example.com", "apps.example.com", extra) {
		t.Fatal("a host under an extra reserved domain must be reserved")
	}
	if Reserved("evilinternal.example.com", "apps.example.com", extra) {
		t.Fatal("extra domains must also match on the label boundary")
	}
	if Reserved("customer.test", "apps.example.com", extra) {
		t.Fatal("an unrelated host must not be reserved")
	}
}

func TestReservedWithNoAppDomainStillHonoursExtras(t *testing.T) {
	// The feature can be off while an operator still reserves names.
	if !Reserved("thing.internal.example.com", "", []string{"internal.example.com"}) {
		t.Fatal("extras must apply even with no app domain")
	}
	if Reserved("anything.test", "", nil) {
		t.Fatal("nothing is reserved when neither is configured")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
export PATH=/usr/local/go/bin:$PATH
go test ./internal/domain/ -run 'TestIssue|TestReserved' -v
```

Expected: FAIL — the package does not exist yet (`no Go files in .../internal/domain`).

- [ ] **Step 3: Write minimal implementation**

Create `internal/domain/policy.go`:

```go
// Package domain owns hostnames: the one the platform issues for an app, and
// the rule about which names a tenant may never claim.
//
// The policy in this file is deliberately pure — no database, no Kubernetes.
// The reserved-suffix rule is the piece of this subsystem that is unsafe to
// get subtly wrong, so it is kept somewhere it can be exhaustively tested on
// its own.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrNoAppDomain means no platform domain is configured. Callers treat this as
// the feature being switched off rather than as a failure, so it is a sentinel
// rather than a generic error.
var ErrNoAppDomain = errors.New("domain: no app domain configured")

// A single DNS label: what an app name must be to become a hostname.
var labelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// A dotted DNS name of at least two labels. Requiring a dot rejects bare names
// like "localhost", which would produce hostnames that cannot be delegated.
var domainRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

// maxHostname is the DNS limit on a fully qualified name.
const maxHostname = 253

// Issue returns the platform hostname for an app: <app name>.<app domain>.
//
// It returns ErrNoAppDomain when no domain is configured, which is not a
// failure — it is how an install with the feature switched off is detected.
func Issue(appName, appDomain string) (string, error) {
	d := normalize(appDomain)
	if d == "" {
		return "", ErrNoAppDomain
	}
	if !domainRE.MatchString(d) {
		return "", fmt.Errorf("domain: %q is not a valid app domain", appDomain)
	}

	n := normalize(appName)
	if !labelRE.MatchString(n) {
		return "", fmt.Errorf("domain: %q is not a valid hostname label", appName)
	}

	host := n + "." + d
	if len(host) > maxHostname {
		return "", fmt.Errorf("domain: hostname %q exceeds %d characters", host, maxHostname)
	}
	return host, nil
}

// Reserved reports whether host falls under the platform domain or any
// additionally reserved domain.
//
// The platform suffix is reserved whether or not an operator remembered to
// list it, because otherwise a tenant could claim a name under it in the
// window before the platform issued that name itself.
//
// Matching is on the label boundary, never on the raw string:
// evilapps.example.com ends with apps.example.com as a substring but is a
// different domain entirely, and treating it as reserved-by-suffix is the
// classic form of this bug.
func Reserved(host, appDomain string, extra []string) bool {
	h := normalize(host)
	if h == "" {
		return false
	}

	for _, d := range append([]string{appDomain}, extra...) {
		d = normalize(d)
		if d == "" {
			continue
		}
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// normalize lowercases, trims spaces, and drops a trailing root dot, so that
// "Apps.Example.COM." and "apps.example.com" compare equal. DNS is
// case-insensitive and the root dot is optional; comparing raw input would
// make both of those into security-relevant differences.
func normalize(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/domain/ -v
go vet ./internal/domain/
```

Expected: PASS, all subtests. Vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/policy.go internal/domain/policy_test.go
git commit -m "Add hostname policy: issue a platform host, reserve the platform suffix

Reserved matches on the label boundary rather than by string suffix,
because evilapps.example.com ends with apps.example.com without being
inside it. That is the whole reason this rule is a tested function and
not an inline strings.HasSuffix at the call site.

Reserved has no caller yet — the custom-domain claim path it guards is a
later sub-project. It is written now because it is thirty lines and is
the piece that is unsafe to retrofit."
```

---

### Task 2: Migration and domain queries

**Files:**
- Create: `internal/store/migrations/00002_domains_managed.sql`
- Create: `internal/store/queries/domains.sql`
- Modify: `internal/store/store_test.go` (add one test)
- Regenerates: `internal/store/dbgen/*`

**Interfaces:**
- Consumes: nothing.
- Produces (generated by sqlc):
  - `dbgen.Domain` struct gaining a `Managed bool` field
  - `func (q *Queries) UpsertManagedDomain(ctx, arg UpsertManagedDomainParams) (Domain, error)` with params `OwnerID string`, `AppID uuid.UUID`, `Host string`, `Tls bool`
  - `func (q *Queries) ListDomainsByApp(ctx, appID uuid.UUID) ([]Domain, error)`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
// TestManagedDomainIsUniquePerApp guards the partial index. An app has exactly
// one platform-issued hostname, but any number of custom domains — so the
// constraint has to be partial, and a plain unique index on app_id would
// silently forbid the custom domains this table exists for.
func TestManagedDomainIsUniquePerApp(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	q := dbgen.New(pool)
	ownerID := "owner-" + t.Name()

	if _, err := q.CreateOwner(ctx, dbgen.CreateOwnerParams{
		ID: ownerID, DisplayName: "Test", Email: "",
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}

	appRow, err := q.CreateApp(ctx, dbgen.CreateAppParams{
		OwnerID:   ownerID,
		Name:      "web",
		Namespace: "yacht-" + strings.ToLower(t.Name()),
		Image:     "nginx:alpine",
		Replicas:  1,
		Port:      8080,
		Env:       []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Two managed rows for one app must collide.
	if _, err := q.UpsertManagedDomain(ctx, dbgen.UpsertManagedDomainParams{
		OwnerID: ownerID, AppID: appRow.ID, Host: "web.apps.example.com", Tls: true,
	}); err != nil {
		t.Fatalf("first managed domain: %v", err)
	}

	// The upsert must move the host rather than insert a second managed row.
	if _, err := q.UpsertManagedDomain(ctx, dbgen.UpsertManagedDomainParams{
		OwnerID: ownerID, AppID: appRow.ID, Host: "web.apps.acme.com", Tls: true,
	}); err != nil {
		t.Fatalf("reissue managed domain: %v", err)
	}

	rows, err := q.ListDomainsByApp(ctx, appRow.ID)
	if err != nil {
		t.Fatalf("list domains: %v", err)
	}

	managed := 0
	for _, r := range rows {
		if r.Managed {
			managed++
			if r.Host != "web.apps.acme.com" {
				t.Fatalf("managed host = %q, want it reissued to web.apps.acme.com", r.Host)
			}
			if !r.Verified {
				t.Fatal("a platform-issued host needs no proof of ownership; want verified")
			}
		}
	}
	if managed != 1 {
		t.Fatalf("managed rows = %d, want exactly 1", managed)
	}
}
```

Check the top of `internal/store/store_test.go` for the existing helper that returns a pool (it is the one the other tests use to skip without a database) and use that name in place of `testPool` if it differs. Add `"strings"` to the imports if absent.

- [ ] **Step 2: Run test to verify it fails**

```bash
export PATH=/usr/local/go/bin:$PATH
export YACHT_TEST_DATABASE_URL="postgres://ericmaro@localhost:5432/yacht_test?sslmode=disable"
go test ./internal/store/ -run TestManagedDomainIsUniquePerApp -v
```

Expected: FAIL to compile — `UpsertManagedDomain` and `Domain.Managed` do not exist.

- [ ] **Step 3: Write the migration**

Create `internal/store/migrations/00002_domains_managed.sql`:

```sql
-- +goose Up
-- +goose StatementBegin

-- A platform-issued hostname is marked by a column rather than recognised by
-- matching its suffix against the current app domain.
--
-- The app domain is configuration and can change. Suffix matching would give a
-- different answer after that change: rows issued under the old domain would
-- stop being recognised as platform-issued, and a customer's own domain could
-- start being recognised as one. A column records what was true at issue time
-- and does not move when configuration does.
ALTER TABLE domains ADD COLUMN managed boolean NOT NULL DEFAULT false;

-- An app has at most one platform-issued hostname. Partial, because the same
-- app may hold any number of custom domains, and a plain unique index on
-- app_id would forbid exactly what this table exists for.
CREATE UNIQUE INDEX domains_app_managed_key ON domains (app_id) WHERE managed;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS domains_app_managed_key;
ALTER TABLE domains DROP COLUMN IF EXISTS managed;
-- +goose StatementEnd
```

- [ ] **Step 4: Write the queries**

Create `internal/store/queries/domains.sql`:

```sql
-- Platform-issued hostnames. Custom domains get their own queries when that
-- sub-project lands; keeping them in one file rather than in apps.sql is so
-- that apps.sql stays about apps.

-- Issues or reissues the platform hostname for an app.
--
-- The conflict target is the partial index, so a second call for the same app
-- moves its hostname rather than adding one. A collision on the global host
-- index is a different conflict and is deliberately left to raise, because two
-- apps claiming one hostname is a real error, not something to paper over.
--
-- verified is true because a name the platform issued needs no proof of
-- ownership; requiring proof would mean inventing a verification step for a
-- name we already control.
-- name: UpsertManagedDomain :one
INSERT INTO domains (owner_id, app_id, host, tls, verified, managed)
VALUES (@owner_id, @app_id, lower(@host), @tls, true, true)
ON CONFLICT (app_id) WHERE managed
DO UPDATE SET host = lower(@host), tls = @tls
RETURNING *;

-- name: ListDomainsByApp :many
SELECT * FROM domains
WHERE app_id = @app_id
ORDER BY managed DESC, host;

-- name: GetManagedDomain :one
SELECT * FROM domains
WHERE app_id = @app_id AND managed
LIMIT 1;
```

- [ ] **Step 5: Regenerate and run the test**

```bash
go tool sqlc generate
go test ./internal/store/ -run TestManagedDomainIsUniquePerApp -v
go test ./internal/store/ -v
```

Expected: sqlc generates without error; the new test PASSes; `TestMigrateIsIdempotent` and `TestEveryTableIsOwnerScoped` still PASS.

If sqlc rejects `ON CONFLICT (app_id) WHERE managed`, express the conflict target as `ON CONFLICT ON CONSTRAINT` is not available for a partial index — instead keep the query and confirm the generated code compiles; sqlc parses this form. If it genuinely cannot, split into `GetManagedDomain` + explicit insert/update in `internal/domain/store.go` and note the change in the commit message.

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrations/00002_domains_managed.sql \
        internal/store/queries/domains.sql \
        internal/store/dbgen/ internal/store/store_test.go
git commit -m "Add a managed column to domains, and the queries to issue one

managed is a column rather than a suffix match against the current app
domain, because that domain is configuration and can change — which would
silently reclassify rows on both sides.

The unique index is partial: one platform hostname per app, any number of
custom domains."
```

---

### Task 3: Domain store helpers

**Files:**
- Create: `internal/domain/store.go`
- Test: `internal/domain/store_test.go`

**Interfaces:**
- Consumes: `dbgen.Queries` from Task 2, `Issue` from Task 1.
- Produces:
  - `func EnsureManaged(ctx context.Context, q *dbgen.Queries, in ManagedInput) (string, error)`
  - `type ManagedInput struct { OwnerID string; AppID uuid.UUID; AppName, AppDomain string; TLS bool }`
  - `func HostsForApp(ctx context.Context, q *dbgen.Queries, appID uuid.UUID) ([]string, error)`
  - `var ErrHostTaken = errors.New("domain: hostname already taken")`

These are package-level functions taking `*dbgen.Queries` rather than methods on a struct, so a caller inside an open transaction can pass `q.WithTx(tx)` and get the write in the same transaction as the app insert.

- [ ] **Step 1: Write the failing test**

Create `internal/domain/store_test.go`:

```go
package domain

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

func testQueries(t *testing.T) *dbgen.Queries {
	t.Helper()
	url := os.Getenv("YACHT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("YACHT_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return dbgen.New(pool)
}

func seedApp(t *testing.T, q *dbgen.Queries, ownerID, name, ns string) dbgen.App {
	t.Helper()
	ctx := context.Background()
	if _, err := q.CreateOwner(ctx, dbgen.CreateOwnerParams{
		ID: ownerID, DisplayName: "Test", Email: "",
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	row, err := q.CreateApp(ctx, dbgen.CreateAppParams{
		OwnerID: ownerID, Name: name, Namespace: ns,
		Image: "nginx:alpine", Replicas: 1, Port: 8080, Env: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	return row
}

func TestEnsureManagedIssuesAndReissues(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()
	a := seedApp(t, q, "owner-ensure", "web", "yacht-ensure")

	host, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.example.com", TLS: true,
	})
	if err != nil {
		t.Fatalf("EnsureManaged: %v", err)
	}
	if host != "web.apps.example.com" {
		t.Fatalf("host = %q, want web.apps.example.com", host)
	}

	// The app domain changed. The next call must move the hostname.
	host, err = EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.acme.com", TLS: true,
	})
	if err != nil {
		t.Fatalf("EnsureManaged reissue: %v", err)
	}
	if host != "web.apps.acme.com" {
		t.Fatalf("reissued host = %q, want web.apps.acme.com", host)
	}

	hosts, err := HostsForApp(ctx, q, a.ID)
	if err != nil {
		t.Fatalf("HostsForApp: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "web.apps.acme.com" {
		t.Fatalf("hosts = %v, want exactly [web.apps.acme.com]", hosts)
	}
}

func TestEnsureManagedWithNoAppDomainIsNotAnError(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()
	a := seedApp(t, q, "owner-nodomain", "web", "yacht-nodomain")

	host, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name, AppDomain: "",
	})
	if err != nil {
		t.Fatalf("with no app domain configured this must succeed quietly: %v", err)
	}
	if host != "" {
		t.Fatalf("host = %q, want empty", host)
	}

	hosts, err := HostsForApp(ctx, q, a.ID)
	if err != nil {
		t.Fatalf("HostsForApp: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("hosts = %v, want none", hosts)
	}
}

func TestEnsureManagedReportsACollisionClearly(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()

	a := seedApp(t, q, "owner-collide-a", "web", "yacht-collide-a")
	b := seedApp(t, q, "owner-collide-b", "web", "yacht-collide-b")

	if _, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: "apps.example.com",
	}); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// Two owners, both with an app called "web", resolve to one hostname. The
	// engine is single-owner so this cannot happen there, but a multi-tenant
	// wrapper can hit it — and it must read as a taken name, not a raw
	// constraint violation.
	_, err := EnsureManaged(ctx, q, ManagedInput{
		OwnerID: b.OwnerID, AppID: b.ID, AppName: b.Name,
		AppDomain: "apps.example.com",
	})
	if !errors.Is(err, ErrHostTaken) {
		t.Fatalf("want ErrHostTaken, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
export PATH=/usr/local/go/bin:$PATH
export YACHT_TEST_DATABASE_URL="postgres://ericmaro@localhost:5432/yacht_test?sslmode=disable"
go test ./internal/domain/ -run TestEnsureManaged -v
```

Expected: FAIL to compile — `EnsureManaged`, `ManagedInput`, `HostsForApp`, `ErrHostTaken` are undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/domain/store.go`:

```go
package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// ErrHostTaken means another app already holds the hostname.
//
// Hostnames are globally unique because DNS has one owner per name. The engine
// is single-owner so it cannot collide with itself, but a multi-tenant wrapper
// can — two tenants each naming an app "web" resolve to the same host. This
// exists so that reads as a taken name rather than as a constraint violation.
var ErrHostTaken = errors.New("domain: hostname already taken")

// ManagedInput describes the platform hostname to issue for an app.
type ManagedInput struct {
	OwnerID   string
	AppID     uuid.UUID
	AppName   string
	AppDomain string
	TLS       bool
}

// EnsureManaged issues the app's platform hostname, or moves it if the app
// domain has changed since it was issued.
//
// Takes a *dbgen.Queries rather than holding its own, so a caller inside a
// transaction passes q.WithTx(tx) and the hostname is written in the same
// transaction as the app itself. An app cannot then exist without its URL.
//
// With no app domain configured it returns ("", nil): the feature is off, and
// that is a state to pass through quietly rather than an error to handle at
// every call site.
func EnsureManaged(ctx context.Context, q *dbgen.Queries, in ManagedInput) (string, error) {
	host, err := Issue(in.AppName, in.AppDomain)
	if errors.Is(err, ErrNoAppDomain) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	if _, err := q.UpsertManagedDomain(ctx, dbgen.UpsertManagedDomainParams{
		OwnerID: in.OwnerID,
		AppID:   in.AppID,
		Host:    host,
		Tls:     in.TLS,
	}); err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: %s", ErrHostTaken, host)
		}
		return "", fmt.Errorf("domain: issue %s: %w", host, err)
	}
	return host, nil
}

// HostsForApp returns every hostname routed to an app, managed first.
func HostsForApp(ctx context.Context, q *dbgen.Queries, appID uuid.UUID) ([]string, error) {
	rows, err := q.ListDomainsByApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("domain: list for app: %w", err)
	}
	hosts := make([]string, 0, len(rows))
	for _, r := range rows {
		hosts = append(hosts, r.Host)
	}
	return hosts, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/domain/ -v
go vet ./internal/domain/
```

Expected: PASS. If the collision test fails because the upsert updated instead of colliding, confirm the conflict target in `domains.sql` is `(app_id) WHERE managed` and not `(host)`.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/store.go internal/domain/store_test.go
git commit -m "Issue and reissue an app's platform hostname

EnsureManaged takes a *dbgen.Queries rather than owning one, so a caller
inside a transaction writes the hostname in the same transaction as the
app row — an app cannot exist without its URL.

No app domain configured returns (\"\", nil) rather than an error: the
feature being off is a state to pass through, not one to handle at every
call site."
```

---

### Task 4: AppSpec gains Hosts and TLS

**Files:**
- Modify: `internal/orchestrator/orchestrator.go` (`AppSpec` struct around line 144, `Validate` around line 173)
- Test: `internal/orchestrator/appspec_test.go` (create — the package has no test file yet)

**Interfaces:**
- Consumes: nothing.
- Produces: `AppSpec.Hosts []string`, `AppSpec.TLS bool`, both validated.

- [ ] **Step 1: Write the failing test**

Create `internal/orchestrator/appspec_test.go`:

```go
package orchestrator

import "testing"

func validSpec() AppSpec {
	return AppSpec{
		Ref:      Ref{Owner: "owner-1", Namespace: "yacht-demo", Name: "web"},
		Image:    "nginx:alpine",
		Replicas: 1,
		Port:     8080,
	}
}

func TestAppSpecAcceptsHosts(t *testing.T) {
	s := validSpec()
	s.Hosts = []string{"web.apps.example.com", "www.customer.test"}
	s.TLS = true
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestAppSpecRejectsMalformedHosts(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"space":         "web .example.com",
		"uppercase":     "WEB.example.com",
		"scheme":        "https://web.example.com",
		"path":          "web.example.com/app",
		"port":          "web.example.com:8080",
		"leading dot":   ".web.example.com",
		"double dot":    "web..example.com",
		"underscore":    "web_1.example.com",
		"trailing dash": "web-.example.com",
	}

	for name, host := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.Hosts = []string{host}
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted malformed host %q", host)
			}
		})
	}
}

// A spec that takes no traffic cannot be routed to, so declaring hosts for it
// is a wiring mistake worth catching here rather than producing an Ingress
// pointing at a Service that was never created.
func TestAppSpecRejectsHostsWithoutAPort(t *testing.T) {
	s := validSpec()
	s.Port = 0
	s.Hosts = []string{"web.apps.example.com"}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted hosts on a spec with no port")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/orchestrator/ -v
```

Expected: FAIL to compile — `AppSpec` has no field `Hosts`.

- [ ] **Step 3: Write minimal implementation**

In `internal/orchestrator/orchestrator.go`, add to the `AppSpec` struct, after `WritableRootFilesystem`:

```go
	// Hosts are the hostnames routed to this workload. Empty means the
	// workload is reachable only inside the cluster.
	//
	// Plain strings rather than a richer type: the orchestrator's job is to
	// route a name, and which names are legitimate — platform-issued or
	// customer-claimed — is a decision that belongs above this seam.
	Hosts []string

	// TLS requests terminated TLS for Hosts.
	//
	// It carries no certificate reference. Platform hostnames are served from
	// the ingress controller's own default certificate, so the workload's
	// routing never names a Secret — see the design note on why a per-app
	// Secret reference cannot work when every app has its own namespace.
	TLS bool
```

Add near `nameRE`-style declarations at the top of the file (or beside the other validation helpers):

```go
// A dotted DNS name. Deliberately strict: no scheme, no port, no path, no
// uppercase. Anything that arrives here in one of those shapes is a caller
// that has confused a URL with a hostname, and failing early is cheaper than
// an Ingress that silently never matches.
var hostRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)
```

Add to `AppSpec.Validate()`, before its final `return nil`:

```go
	if len(s.Hosts) > 0 && s.Port == 0 {
		return errors.New("app spec: hosts require a port to route to")
	}
	for _, h := range s.Hosts {
		if !hostRE.MatchString(h) {
			return fmt.Errorf("app spec: %q is not a valid hostname", h)
		}
	}
```

Ensure `regexp` is imported in `internal/orchestrator/orchestrator.go`.

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/orchestrator/... -v
go vet ./internal/orchestrator/...
```

Expected: PASS, and the existing `internal/orchestrator/k8s` tests still pass.

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/orchestrator.go internal/orchestrator/appspec_test.go
git commit -m "Give AppSpec hostnames and a TLS flag

TLS carries no certificate reference on purpose. An Ingress's TLS Secret
must live in the Ingress's own namespace and every app has its own, so a
single pre-provisioned wildcard cannot be named from fifty namespaces.
The certificate comes from the ingress controller's default instead.

Hosts with no port is rejected: routing to a workload that takes no
traffic is a wiring mistake, and an Ingress pointing at a Service that was
never created fails much later and much more confusingly."
```

---

### Task 5: Ingress apply, prune, and delete

**Files:**
- Create: `internal/orchestrator/k8s/ingress.go`
- Create: `internal/orchestrator/k8s/ingress_test.go`
- Modify: `internal/orchestrator/k8s/app.go` (`ApplyApp` ~line 34, `DeleteApp` ~line 187)

**Interfaces:**
- Consumes: `AppSpec.Hosts`, `AppSpec.TLS` from Task 4; `servicePort` and `applyOpts()` which already exist.
- Produces: `func (o *Orchestrator) applyIngress(ctx, spec) error`, `func (o *Orchestrator) deleteIngress(ctx, ref) error`, `func (o *Orchestrator) deleteService(ctx, ref) error` (all unexported).

- [ ] **Step 1: Write the failing test**

Create `internal/orchestrator/k8s/ingress_test.go`:

```go
package k8s

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

func TestApplyAppCreatesIngressForHosts(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}

	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "web.apps.example.com" {
		t.Fatalf("rules = %+v, want one rule for web.apps.example.com", ing.Spec.Rules)
	}

	// The backend is the Service's stable port, not the container port. That
	// indirection is why changing an app's port does not rewrite its routing.
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend.Name != spec.Name || backend.Port.Number != servicePort {
		t.Fatalf("backend = %s:%d, want %s:%d",
			backend.Name, backend.Port.Number, spec.Name, servicePort)
	}

	if ing.Spec.IngressClassName != nil {
		t.Fatalf("ingressClassName = %q, want unset so the cluster default applies",
			*ing.Spec.IngressClassName)
	}
}

func TestApplyAppCreatesNoIngressWithoutHosts(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	if err := o.ApplyApp(ctx, testSpec()); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses("yacht-demo").
		Get(ctx, "web", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// TLS carries the hosts and no secretName. The absence is the whole mechanism:
// the certificate comes from the controller's default, and an implementation
// that "helpfully" fills a secret name silently reintroduces the cross-
// namespace problem this design exists to avoid.
func TestIngressTLSCarriesNoSecretName(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	spec.TLS = true

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}

	if len(ing.Spec.TLS) != 1 {
		t.Fatalf("tls = %+v, want one entry", ing.Spec.TLS)
	}
	if ing.Spec.TLS[0].SecretName != "" {
		t.Fatalf("secretName = %q, want empty", ing.Spec.TLS[0].SecretName)
	}
	if len(ing.Spec.TLS[0].Hosts) != 1 || ing.Spec.TLS[0].Hosts[0] != "web.apps.example.com" {
		t.Fatalf("tls hosts = %v, want [web.apps.example.com]", ing.Spec.TLS[0].Hosts)
	}
}

func TestApplyAppWithoutTLSEmitsNoTLSBlock(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	spec.TLS = false

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if len(ing.Spec.TLS) != 0 {
		t.Fatalf("tls = %+v, want none", ing.Spec.TLS)
	}
}

// Removing the last hostname must remove the Ingress. Converging only forward
// leaves an app routable at a name it no longer owns.
func TestApplyAppPrunesIngressWhenHostsGoAway(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	spec.Hosts = nil
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp without hosts: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want the ingress pruned, got %v", err)
	}
}

// Clearing the port already skipped Service creation but left any existing
// Service behind. Same class of bug as the Ingress one, fixed at the same time.
func TestApplyAppPrunesServiceAndIngressWhenPortCleared(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	spec.Port = 0
	spec.Hosts = nil
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp with no port: %v", err)
	}

	if _, err := client.CoreV1().Services(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("want the service pruned, got %v", err)
	}
	if _, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("want the ingress pruned, got %v", err)
	}
}

func TestDeleteAppRemovesIngress(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	if err := o.DeleteApp(ctx, spec.Ref); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	_, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestDeleteAppIsIdempotentWithNoIngress(t *testing.T) {
	ctx := context.Background()
	o, _ := testOrchestrator(t)

	// Deleting an app that never had an Ingress must not error, or a delete
	// after a partial apply becomes unretryable.
	if err := o.DeleteApp(ctx, testSpec().Ref); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/orchestrator/k8s/ -run Ingress -v
```

Expected: FAIL — no Ingress is ever created, so the Get returns NotFound.

- [ ] **Step 3: Write the implementation**

Create `internal/orchestrator/k8s/ingress.go`:

```go
package k8s

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// applyIngress routes the spec's hostnames to its Service.
//
// ingressClassName is left unset so the cluster's default IngressClass
// applies. Naming a class here would hard-code which controller is installed,
// which is the coupling this design otherwise avoids.
//
// The TLS block, when present, lists hosts and names no Secret. An Ingress's
// TLS Secret must live in the Ingress's own namespace, and every app has its
// own namespace — so one pre-provisioned wildcard cannot be referenced from
// all of them. The certificate comes from the ingress controller's configured
// default instead, which also keeps the private key out of tenant namespaces.
func (o *Orchestrator) applyIngress(ctx context.Context, spec orchestrator.AppSpec) error {
	pathType := networkingv1.PathTypePrefix

	rules := make([]*networkingv1ac.IngressRuleApplyConfiguration, 0, len(spec.Hosts))
	for _, host := range spec.Hosts {
		rules = append(rules, networkingv1ac.IngressRule().
			WithHost(host).
			WithHTTP(networkingv1ac.HTTPIngressRuleValue().
				WithPaths(networkingv1ac.HTTPIngressPath().
					WithPath("/").
					WithPathType(pathType).
					WithBackend(networkingv1ac.IngressBackend().
						WithService(networkingv1ac.IngressServiceBackend().
							WithName(spec.Name).
							WithPort(networkingv1ac.ServiceBackendPort().
								WithNumber(servicePort)))))))
	}

	ingSpec := networkingv1ac.IngressSpec().WithRules(rules...)

	if spec.TLS {
		ingSpec = ingSpec.WithTLS(networkingv1ac.IngressTLS().
			WithHosts(spec.Hosts...))
	}

	ing := networkingv1ac.Ingress(spec.Name, spec.Namespace).
		WithLabels(orchestrator.ObjectLabels(spec.Ref)).
		WithSpec(ingSpec)

	if _, err := o.client.NetworkingV1().Ingresses(spec.Namespace).
		Apply(ctx, ing, applyOpts()); err != nil {
		return fmt.Errorf("k8s: apply ingress %s: %w", spec.Ref, err)
	}
	return nil
}

// deleteIngress removes an app's Ingress, tolerating its absence.
func (o *Orchestrator) deleteIngress(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.NetworkingV1().Ingresses(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete ingress %s: %w", ref, err)
	}
	return nil
}

// deleteService removes an app's Service, tolerating its absence.
//
// Needed because clearing an app's port stops the Service being applied but
// does not remove one already there. Converging only forward leaves the old
// object serving traffic nobody asked for.
func (o *Orchestrator) deleteService(ctx context.Context, ref orchestrator.Ref) error {
	err := o.client.CoreV1().Services(ref.Namespace).
		Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete service %s: %w", ref, err)
	}
	return nil
}
```

In `internal/orchestrator/k8s/app.go`, replace the body of `ApplyApp` between `applyDeployment` and the closing log with:

```go
	// A workload with no port takes no traffic and needs neither a Service nor
	// an Ingress. Prune rather than merely skip: an app that used to have a
	// port would otherwise keep serving through objects nothing maintains.
	if spec.Port > 0 {
		if err := o.applyService(ctx, spec); err != nil {
			return err
		}
	} else if err := o.deleteService(ctx, spec.Ref); err != nil {
		return err
	}

	if spec.Port > 0 && len(spec.Hosts) > 0 {
		if err := o.applyIngress(ctx, spec); err != nil {
			return err
		}
	} else if err := o.deleteIngress(ctx, spec.Ref); err != nil {
		return err
	}
```

In `DeleteApp`, after the Service delete and before the log line, add:

```go
	if err := o.deleteIngress(ctx, ref); err != nil {
		return err
	}
```

Update the `DeleteApp` doc comment to read "removes a workload's Deployment, Service and Ingress."

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/orchestrator/k8s/ -v
go test ./... -race -count=1
go vet ./...
```

Expected: PASS. The pre-existing `TestApplyAppCreatesServiceOnlyWhenPortSet` must still pass.

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/k8s/ingress.go \
        internal/orchestrator/k8s/ingress_test.go \
        internal/orchestrator/k8s/app.go
git commit -m "Route hostnames to apps with an Ingress, and prune it

ingressClassName is unset so the cluster default applies; naming a class
would hard-code which controller is installed. The TLS block names no
Secret, which is the mechanism rather than an omission — the certificate
comes from the controller's default because a per-app Secret reference
cannot cross namespaces.

Also prunes the Service when a port is cleared. That gap already existed:
clearing a port stopped the Service being applied but left an existing one
serving. Fixed here rather than reproduced for Ingress."
```

---

### Task 6: Configuration

**Files:**
- Modify: `internal/config/config.go`
- Modify: `.env.example`
- Test: `internal/config/config_test.go` (create — package has no test file yet)

**Interfaces:**
- Consumes: nothing.
- Produces: `Config.AppDomain string`, `Config.WildcardTLS bool`, `Config.ReservedDomains []string`.

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// A database URL is required by validate(); supply one so these tests are
	// about the new variables and nothing else.
	t.Setenv("YACHT_DATABASE_URL", "postgres://localhost/yacht?sslmode=disable")
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestAppDomainDefaultsToOff(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AppDomain != "" {
		t.Fatalf("AppDomain = %q, want empty by default", c.AppDomain)
	}
	if c.WildcardTLS {
		t.Fatal("WildcardTLS should default to false")
	}
}

func TestAppDomainAndReservedDomainsLoad(t *testing.T) {
	setEnv(t, map[string]string{
		"YACHT_APP_DOMAIN":       "apps.example.com",
		"YACHT_WILDCARD_TLS":     "true",
		"YACHT_RESERVED_DOMAINS": "internal.example.com, admin.example.com",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AppDomain != "apps.example.com" {
		t.Fatalf("AppDomain = %q", c.AppDomain)
	}
	if !c.WildcardTLS {
		t.Fatal("WildcardTLS = false, want true")
	}
	if len(c.ReservedDomains) != 2 ||
		c.ReservedDomains[0] != "internal.example.com" ||
		c.ReservedDomains[1] != "admin.example.com" {
		t.Fatalf("ReservedDomains = %v, want both entries trimmed", c.ReservedDomains)
	}
}

// Wildcard TLS with nothing to apply it to is incoherent rather than merely
// useless: the operator believes apps are served over TLS and nothing says
// otherwise. Fail at startup instead.
func TestWildcardTLSWithoutAnAppDomainFails(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_WILDCARD_TLS": "true"})
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted YACHT_WILDCARD_TLS with no YACHT_APP_DOMAIN")
	}
	if !strings.Contains(err.Error(), "YACHT_APP_DOMAIN") {
		t.Fatalf("error should name the missing variable, got: %v", err)
	}
}

func TestMalformedAppDomainFails(t *testing.T) {
	setEnv(t, map[string]string{"YACHT_APP_DOMAIN": "not a domain"})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a malformed YACHT_APP_DOMAIN")
	}
}

// Every variable config.go reads must be documented, or an operator finds out
// a setting exists by reading the source.
func TestEveryVariableIsDocumented(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	for _, name := range []string{
		"YACHT_APP_DOMAIN", "YACHT_WILDCARD_TLS", "YACHT_RESERVED_DOMAINS",
	} {
		if !strings.Contains(string(src), name) {
			t.Fatalf("%s is not read by config.go", name)
		}
		if !strings.Contains(string(example), name) {
			t.Fatalf("%s is not documented in .env.example", name)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/config/ -v
```

Expected: FAIL to compile — `Config` has no field `AppDomain`.

- [ ] **Step 3: Write minimal implementation**

Add to the `Config` struct in `internal/config/config.go`, after `OwnerName`:

```go
	// AppDomain is the platform domain every app gets a hostname under, such
	// as apps.example.com. Empty switches per-app hostnames off entirely.
	AppDomain string

	// WildcardTLS serves platform hostnames over TLS using the ingress
	// controller's configured default certificate.
	//
	// Yacht cannot verify that default exists. An install without one serves
	// the wrong certificate rather than failing, so this is logged at startup
	// and shown on the settings page — a capability the engine cannot check is
	// one it should be noisy about.
	WildcardTLS bool

	// ReservedDomains are additional suffixes no tenant may claim. AppDomain
	// is always reserved whether or not it appears here.
	ReservedDomains []string
```

Add to the `Load()` literal:

```go
		AppDomain:       env("YACHT_APP_DOMAIN", ""),
		WildcardTLS:     envBool("YACHT_WILDCARD_TLS", false),
		ReservedDomains: envList("YACHT_RESERVED_DOMAINS"),
```

Add the helper beside `envBool`:

```go
// envList reads a comma-separated variable, trimming each entry and dropping
// empties, so a trailing comma or a stray space is not a silent extra entry.
func envList(key string) []string {
	raw := strings.Split(os.Getenv(key), ",")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
```

Add to `validate()`:

```go
	if c.AppDomain != "" && !appDomainRE.MatchString(strings.ToLower(c.AppDomain)) {
		errs = append(errs, errors.New("YACHT_APP_DOMAIN must be a valid dotted domain name"))
	}
	if c.WildcardTLS && c.AppDomain == "" {
		errs = append(errs, errors.New(
			"YACHT_WILDCARD_TLS requires YACHT_APP_DOMAIN — there would be no hostnames to serve"))
	}
```

And the pattern near the top of the file:

```go
var appDomainRE = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)
```

Import `regexp`.

- [ ] **Step 4: Document the variables**

Append to `.env.example`:

```bash
# Platform domain. Every app gets <name>.<this> as a hostname the moment it is
# created. Point a wildcard DNS record (*.apps.example.com) at the cluster.
# Empty switches per-app hostnames off.
YACHT_APP_DOMAIN=

# Serve platform hostnames over TLS using the ingress controller's default
# certificate, which the operator provisions as a wildcard for YACHT_APP_DOMAIN.
#
# One wildcard rather than a certificate per app because Let's Encrypt allows
# 50 certificates per registered domain per week — a subdomain per app stops
# being issuable after app #50, and the failure surfaces as a rate limit
# against a domain nobody touched.
YACHT_WILDCARD_TLS=false

# Additional domain suffixes no tenant may claim, comma-separated.
# YACHT_APP_DOMAIN is always reserved whether or not it is listed here.
YACHT_RESERVED_DOMAINS=
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test ./internal/config/ -v
go vet ./internal/config/
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go .env.example
git commit -m "Add app domain, wildcard TLS and reserved domain settings

Wildcard TLS without an app domain fails at startup rather than loading:
the operator believes apps are served over TLS and nothing would say
otherwise.

Yacht cannot verify the controller actually has a default certificate, so
that case is logged and surfaced rather than enforced. A capability the
engine cannot check is one it should be noisy about."
```

---

### Task 7: Issue hostnames from the app service

**Files:**
- Modify: `internal/app/service.go` (`Service` ~line 100, `NewService` ~line 108, `Create` ~line 171, `apply` ~line 241)
- Modify: `internal/app/service_test.go` (update `NewService` call sites)

**Interfaces:**
- Consumes: `domain.EnsureManaged`, `domain.HostsForApp`, `domain.ErrHostTaken` (Task 3); `AppSpec.Hosts`/`TLS` (Task 4); `Config.AppDomain`/`WildcardTLS` (Task 6).
- Produces:
  - `type Options struct { AppDomain string; WildcardTLS bool }`
  - `func NewService(pool *pgxpool.Pool, orch orchestrator.Orchestrator, log *slog.Logger, opts Options) *Service`
  - `App.Host string` — the platform hostname, empty when the feature is off. Populated on read in `Get` and `List`.
  - `App.TLS bool` — whether the platform serves that hostname over TLS. Populated on read from `Options.WildcardTLS`. Task 9 uses it for the link scheme.
  - `func (a App) URLScheme() string` — `"https"` when `a.TLS`, else `"http"`.

- [ ] **Step 1: Write the failing test**

Add to `internal/app/service_test.go`:

```go
func TestCreateIssuesAManagedHostname(t *testing.T) {
	svc, _ := testService(t, Options{AppDomain: "apps.example.com", WildcardTLS: true})
	ctx := context.Background()

	created, err := svc.Create(ctx, "owner-1", CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.Host != "web.apps.example.com" {
		t.Fatalf("Host = %q, want web.apps.example.com", created.Host)
	}
}

func TestCreateWithoutAnAppDomainIssuesNoHostname(t *testing.T) {
	svc, _ := testService(t, Options{})
	ctx := context.Background()

	created, err := svc.Create(ctx, "owner-1", CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Host != "" {
		t.Fatalf("Host = %q, want empty when the feature is off", created.Host)
	}
}

// Changing the app domain moves each app's URL the next time it is applied,
// rather than all at once at startup. The managed column is what makes that
// safe: the reconcile rewrites only rows Yacht issued.
func TestApplyReissuesWhenTheAppDomainChanges(t *testing.T) {
	svc, _ := testService(t, Options{AppDomain: "apps.example.com"})
	ctx := context.Background()

	if _, err := svc.Create(ctx, "owner-1", CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc.opts.AppDomain = "apps.acme.com"

	if err := svc.Redeploy(ctx, "owner-1", "web"); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}

	got, err := svc.Get(ctx, "owner-1", "web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Host != "web.apps.acme.com" {
		t.Fatalf("Host = %q, want it reissued to web.apps.acme.com", got.Host)
	}
}

func TestApplyPassesHostsToTheOrchestrator(t *testing.T) {
	svc, orch := testService(t, Options{AppDomain: "apps.example.com", WildcardTLS: true})
	ctx := context.Background()

	if _, err := svc.Create(ctx, "owner-1", CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spec := orch.lastAppSpec()
	if len(spec.Hosts) != 1 || spec.Hosts[0] != "web.apps.example.com" {
		t.Fatalf("spec.Hosts = %v, want [web.apps.example.com]", spec.Hosts)
	}
	if !spec.TLS {
		t.Fatal("spec.TLS = false, want true when wildcard TLS is on")
	}
}
```

Adapt `testService` to the existing test harness in `service_test.go`: it currently constructs a `Service` with a fake orchestrator and a pool. Give it an `Options` parameter and have the fake orchestrator record the last `AppSpec` passed to `ApplyApp`, exposed as `lastAppSpec()`. These tests require Postgres; follow whatever skip the file already uses.

- [ ] **Step 2: Run test to verify it fails**

```bash
export YACHT_TEST_DATABASE_URL="postgres://ericmaro@localhost:5432/yacht_test?sslmode=disable"
go test ./internal/app/ -v
```

Expected: FAIL to compile — `Options` and `App.Host` are undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/app/service.go`:

Add to the `App` struct:

```go
	// Host is the platform-issued hostname, empty when no app domain is
	// configured. Read from the domains table rather than stored on the app,
	// so there is one place a hostname lives.
	Host string

	// TLS reports whether the platform serves Host over TLS. Populated on
	// read from configuration rather than stored, because it is a property of
	// the install and not of the app.
	TLS bool
```

Add beside `Ref()`:

```go
// URLScheme is https when the platform serves TLS and http otherwise, so the
// dashboard never offers a link that cannot connect.
func (a App) URLScheme() string {
	if a.TLS {
		return "https"
	}
	return "http"
}
```

Add `Options` and thread it through:

```go
// Options are the settings the service needs beyond its dependencies.
type Options struct {
	// AppDomain is the platform domain apps get hostnames under. Empty
	// switches per-app hostnames off.
	AppDomain string

	// WildcardTLS serves those hostnames over TLS from the ingress
	// controller's default certificate.
	WildcardTLS bool
}
```

Add `opts Options` to the `Service` struct and to `NewService`:

```go
func NewService(
	pool *pgxpool.Pool, orch orchestrator.Orchestrator, log *slog.Logger, opts Options,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, q: dbgen.New(pool), orch: orch, log: log, opts: opts}
}
```

In `Create`, after `created := toApp(row)` and **before** `s.apply(ctx, created)`:

```go
	// Issued inside the same transaction as the app row, so an app cannot
	// exist without its URL and no later step can forget to add one.
	host, err := domain.EnsureManaged(ctx, q, domain.ManagedInput{
		OwnerID: ownerID, AppID: created.ID, AppName: created.Name,
		AppDomain: s.opts.AppDomain, TLS: s.opts.WildcardTLS,
	})
	if err != nil {
		if errors.Is(err, domain.ErrHostTaken) {
			return App{}, err
		}
		return App{}, fmt.Errorf("app: issue hostname: %w", err)
	}
	created.Host = host
```

Replace `apply` with a version that reconciles the hostname and passes it on:

```go
// apply ensures the namespace and converges the workload.
//
// The managed hostname is reconciled here rather than only at create, so an
// app domain change moves each app's URL the next time it is applied. Rewriting
// every app at startup would put a config typo in the path of the whole
// install at once.
func (s *Service) apply(ctx context.Context, a App) error {
	if err := s.orch.EnsureNamespace(ctx, orchestrator.NamespaceSpec{
		Owner: orchestrator.OwnerID(a.OwnerID),
		Name:  a.Namespace,
	}); err != nil {
		return err
	}

	hosts, err := s.reconcileHosts(ctx, a)
	if err != nil {
		return err
	}

	return s.orch.ApplyApp(ctx, orchestrator.AppSpec{
		Ref:           a.Ref(),
		Image:         a.Image,
		Replicas:      a.Replicas,
		Port:          a.Port,
		Env:           a.Env,
		CPURequest:    a.CPURequest,
		CPULimit:      a.CPULimit,
		MemoryRequest: a.MemoryRequest,
		MemoryLimit:   a.MemoryLimit,
		Hosts:         hosts,
		TLS:           s.opts.WildcardTLS && len(hosts) > 0,
	})
}

// reconcileHosts brings the managed hostname in line with current config and
// returns every hostname routed to the app.
func (s *Service) reconcileHosts(ctx context.Context, a App) ([]string, error) {
	if _, err := domain.EnsureManaged(ctx, s.q, domain.ManagedInput{
		OwnerID: a.OwnerID, AppID: a.ID, AppName: a.Name,
		AppDomain: s.opts.AppDomain, TLS: s.opts.WildcardTLS,
	}); err != nil {
		return nil, fmt.Errorf("app: reconcile hostname: %w", err)
	}
	return domain.HostsForApp(ctx, s.q, a.ID)
}
```

In `attachStatus` (or wherever `Get`/`List` hydrate an `App`), populate `Host` by reading the managed domain. Add to `Get` after the row is converted:

```go
	if hosts, err := domain.HostsForApp(ctx, s.q, a.ID); err == nil && len(hosts) > 0 {
		a.Host = hosts[0]
	}
```

Do the same in `List` for each app. `ListDomainsByApp` orders `managed DESC`, so index 0 is the platform hostname when one exists.

Import `"github.com/codeblocktz/yacht/internal/domain"` and ensure `errors` is imported.

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/app/ -v
go build ./...
```

Expected: `internal/app` PASSes. `go build ./...` FAILS in `cmd/yacht` because `NewService` now takes four arguments — Task 8 fixes that.

- [ ] **Step 5: Commit**

```bash
git add internal/app/service.go internal/app/service_test.go
git commit -m "Issue an app's hostname at create, and reissue it on apply

The hostname is written in the same transaction as the app row, so an app
cannot exist without its URL.

Reconciling on apply rather than at startup means an app domain change
moves URLs app-by-app as each is next applied. Rewriting every app at boot
would put a config typo in the path of the whole install at once."
```

---

### Task 8: Wire configuration through main

**Files:**
- Modify: `cmd/yacht/main.go` (~line 74)

**Interfaces:**
- Consumes: `app.Options` (Task 7), `Config.AppDomain`/`WildcardTLS` (Task 6).
- Produces: nothing new.

- [ ] **Step 1: Update the wiring**

In `run()`, replace the `app.NewService` call:

```go
	apps := app.NewService(pool, orch, log, app.Options{
		AppDomain:   cfg.AppDomain,
		WildcardTLS: cfg.WildcardTLS,
	})
```

- [ ] **Step 2: Add the startup warning**

After the `apps := ...` block, add:

```go
	// Yacht cannot check that the ingress controller actually has a default
	// certificate: there is no API for "what will you serve for an unknown
	// host". Without one, apps are served the wrong certificate rather than
	// failing, so the one thing available is to say so plainly at startup.
	if cfg.WildcardTLS {
		log.Info("wildcard TLS enabled — platform hostnames are served from the "+
			"ingress controller's default certificate; Yacht cannot verify one is "+
			"configured",
			slog.String("app_domain", cfg.AppDomain))
	}
	if cfg.AppDomain != "" {
		log.Info("per-app hostnames enabled",
			slog.String("app_domain", cfg.AppDomain),
			slog.String("dns", "point *."+cfg.AppDomain+" at this cluster"))
	}
```

- [ ] **Step 3: Verify the whole tree builds and tests**

```bash
export PATH=/usr/local/go/bin:$PATH
export YACHT_TEST_DATABASE_URL="postgres://ericmaro@localhost:5432/yacht_test?sslmode=disable"
go vet ./...
CGO_ENABLED=0 go build ./...
go test ./... -race -count=1
```

Expected: all clean, all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/yacht/main.go
git commit -m "Wire the app domain through to the app service

Logs the DNS record an operator has to create, and says plainly that the
default certificate cannot be verified — the one honest response to a
dependency the engine has no way to check."
```

---

### Task 9: Show the URL in the dashboard

**Files:**
- Modify: `internal/web/pages.templ` (app list rows, app detail header, settings page)
- Modify: `internal/web/server.go` (settings handler data, if it carries a struct)
- Test: `internal/web/apps_test.go`

**Interfaces:**
- Consumes: `app.App.Host` (Task 7), `config` values surfaced through the settings handler.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the failing test**

Add to `internal/web/apps_test.go`:

```go
func TestAppDetailShowsTheHostnameAsALink(t *testing.T) {
	srv := testServer(t, withApps([]app.App{{
		Name:  "web",
		Image: "nginx:alpine",
		Host:  "web.apps.example.com",
		Port:  8080,
	}}))

	body := getBody(t, srv, "/apps/web")

	if !strings.Contains(body, "web.apps.example.com") {
		t.Fatal("app detail does not show the hostname")
	}
	// A URL a user cannot click is a URL they have to retype.
	if !strings.Contains(body, `href="https://web.apps.example.com"`) {
		t.Fatal("the hostname is not rendered as a link")
	}
}

func TestAppWithNoHostnameShowsNoLink(t *testing.T) {
	srv := testServer(t, withApps([]app.App{{
		Name: "web", Image: "nginx:alpine", Port: 8080,
	}}))

	body := getBody(t, srv, "/apps/web")

	if strings.Contains(body, `href="https://"`) {
		t.Fatal("an app with no hostname rendered an empty link")
	}
}
```

Adapt `testServer`, `withApps` and `getBody` to whatever helpers `internal/web`'s tests already use — read the top of `apps_test.go` first and match them rather than introducing new ones.

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/web/ -run Hostname -v
```

Expected: FAIL — the hostname is not rendered.

- [ ] **Step 3: Render the URL**

In `internal/web/pages.templ`, in the app detail header, after the app name:

```templ
	if d.App.Host != "" {
		<a class="app-url mono" href={ templ.SafeURL(d.App.URLScheme() + "://" + d.App.Host) } target="_blank" rel="noreferrer">
			{ d.App.Host }
		</a>
	}
```

And in the app list row, the same guarded by `if a.Host != ""`.

`App.Host`, `App.TLS` and `URLScheme()` all come from Task 7 — nothing new is added to `internal/app` here beyond populating them, which Task 7 already does.

On the settings page, add a row showing the app domain and TLS posture:

```templ
<div class="row">
	<span class="label">App domain</span>
	if d.AppDomain == "" {
		<span class="value muted">not configured — apps are reachable only inside the cluster</span>
	} else {
		<span class="value mono">*.{ d.AppDomain }</span>
	}
</div>
<div class="row">
	<span class="label">Platform TLS</span>
	if d.WildcardTLS {
		<span class="value">shared wildcard certificate, served by the ingress controller</span>
	} else {
		<span class="value muted">off</span>
	}
</div>
```

Thread `AppDomain` and `WildcardTLS` into the settings page data struct and into `web.Options` so `main.go` can supply them.

- [ ] **Step 4: Regenerate assets and run tests**

```bash
make generate
make css
go test ./... -race -count=1
go vet ./...
git diff --exit-code -- '*_templ.go' internal/web/assets/css/app.css || echo "regenerated — stage these"
```

Expected: tests PASS. Generated files change and must be committed; CI fails on drift.

- [ ] **Step 5: Add a gallery state**

In `internal/web/gallery_test.go`, add an app with a hostname to the fixture set so `make gallery` renders it. Follow the existing fixture pattern in that file.

```bash
YACHT_GALLERY_OUT=/tmp/yacht-gallery go test ./internal/web -run Gallery -count=1
```

Open one of the rendered app pages and confirm the URL appears and is clickable. This is the step that catches what tests do not — the project's own notes record a rendering bug found by looking at a screenshot rather than by a test.

- [ ] **Step 6: Commit**

```bash
git add internal/web/ internal/app/service.go
git commit -m "Show each app's URL in the dashboard

The scheme follows the platform TLS setting rather than being hardcoded to
https, so an install without wildcard TLS does not offer links that cannot
connect."
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
make generate && make css
git diff --exit-code -- '*_templ.go' internal/web/assets/css/app.css
git status --short
```

All must be clean. `git status` must show no untracked files — and check `git check-ignore` on anything new under `docs/`, since `.gitignore` has silently swallowed files in this repository before.

- [ ] **Verify against a real cluster**

Nothing above proves an Ingress actually routes. The fake clientset confirms the object is *created*, not that traffic arrives. Before calling this done:

1. Point `*.apps.<your domain>` at a K3s cluster.
2. `YACHT_APP_DOMAIN=apps.<your domain> YACHT_WILDCARD_TLS=false` and create an app.
3. `curl http://<name>.apps.<your domain>` and confirm the app answers.
4. Provision a wildcard as Traefik's default certificate, set `YACHT_WILDCARD_TLS=true`, redeploy, and confirm `https://` serves the right certificate.

Step 4 is the one with no test coverage by construction, and the one most likely to be wrong.

---

## Out of scope

Recorded so they are not quietly absorbed:

- **Customer custom domains** — claim, DNS TXT verification, publish, per-host cert-manager Certificates. `Reserved` is built ready for it; `domains.verified` already exists.
- **Volume resize** — needs volume *provisioning* first, which this repository does not have at all.
- **Accounts, teams, sign-in** — sub-projects B and C.
