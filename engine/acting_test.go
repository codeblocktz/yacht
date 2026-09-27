package engine

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The engine wires its session provider with the operator check, from
// YACHT_OPERATORS: a named operator acting as a team resolves to it, and the
// same session under an engine whose list no longer names them resolves to
// their own team. The web tests prove the dashboard's side with the same
// check; this proves the engine hands it to the provider at all.
func TestTheEngineLetsOnlyNamedOperatorsAct(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run engine composition tests")
	}
	ctx := context.Background()

	build := func(operators []string) *Engine {
		e, err := New(ctx, Config{
			DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
			OwnerID: "engine-act-home", OwnerName: "Home", MaxConcurrentBuilds: 2,
			BaseURL: "https://yacht.test", SessionTTL: time.Hour, MagicLinkTTL: time.Minute,
			Operators: operators,
		}, Overrides{
			Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			Orchestrator: orchestrator.NewNoop(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(e.Close)
		return e
	}

	e := build([]string{"op@engine.test"})
	purge := func() {
		_, _ = e.Pool.Exec(ctx, `DELETE FROM teams WHERE id LIKE 'engine-act-%'`)
		_, _ = e.Pool.Exec(ctx, `DELETE FROM users WHERE lower(email) LIKE '%@engine.test'`)
	}
	purge()
	t.Cleanup(purge)

	op, err := e.Accounts.EnsureUser(ctx, "op@engine.test", "")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if _, err := e.Accounts.CreateTeam(ctx, "engine-act-op", "Operators", op.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	cust, _ := e.Accounts.EnsureUser(ctx, "cust@engine.test", "")
	if _, err := e.Accounts.CreateTeam(ctx, "engine-act-cust", "Customer", cust.ID); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	raw, err := e.Accounts.CreateSession(ctx, op.ID, "engine-act-op", "", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := e.Accounts.ResolveSession(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if _, err := e.Accounts.StartActing(ctx, sess.ID, "engine-act-cust"); err != nil {
		t.Fatalf("StartActing: %v", err)
	}

	resolve := func(e *Engine) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
		owner, err := e.Identity.Resolve(ctx, req)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return owner.ID
	}

	if got := resolve(e); got != "engine-act-cust" {
		t.Errorf("a named operator acting resolves to %q, want the customer's team", got)
	}
	if got := resolve(build(nil)); got != "engine-act-op" {
		t.Errorf("with nobody named, the same session resolves to %q, want the operator's own team", got)
	}
}
