package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A wrapper that runs its own registry fills in Settings › Registry from its
// configuration at start: stored once, untouched when it is the same, and
// replaced when it differs.
func TestAWrapperSetsTheRegistryFromItsOwnConfiguration(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run engine composition tests")
	}
	ctx := context.Background()
	compose := func(key string) *Engine {
		t.Helper()
		e, err := New(ctx, Config{
			DatabaseURL: dsn, Addr: "127.0.0.1:0", ShutdownTimeout: time.Second,
			OwnerID: "wrapper-registry", OwnerName: "Wrapper", MaxConcurrentBuilds: 2,
			SecretKey: key,
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
	e := compose("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	clear := func() {
		if _, err := e.Pool.Exec(context.Background(), `DELETE FROM platform_registry`); err != nil {
			t.Errorf("clear registry: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	updated := func() time.Time {
		t.Helper()
		var at time.Time
		if err := e.Pool.QueryRow(ctx, `SELECT updated_at FROM platform_registry`).Scan(&at); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		return at
	}

	if s, err := e.RegistrySettings(ctx); err != nil || s.Configured() {
		t.Fatalf("a fresh install's registry is %+v, %v; want none", s, err)
	}
	r := Registry{Host: "Registry.Example.Test", Repository: "apps", Username: "builder", Password: "first"}
	if err := e.SetRegistry(ctx, r); err != nil {
		t.Fatalf("SetRegistry: %v", err)
	}
	s, err := e.RegistrySettings(ctx)
	if err != nil {
		t.Fatalf("RegistrySettings: %v", err)
	}
	if s.Host != "registry.example.test" || s.Repository != "apps" || s.Username != "builder" || s.Insecure {
		t.Errorf("stored %+v, want the host lowercased and the rest as given", s)
	}
	first := updated()

	// The same again, as at every start: nothing is written.
	if err := e.SetRegistry(ctx, r); err != nil {
		t.Fatalf("SetRegistry again: %v", err)
	}
	if again := updated(); !again.Equal(first) {
		t.Errorf("setting the same registry wrote it again (%s, then %s)", first, again)
	}

	// A new password is a change.
	r.Password = "second"
	if err := e.SetRegistry(ctx, r); err != nil {
		t.Fatalf("SetRegistry with a new password: %v", err)
	}
	if updated().Equal(first) {
		t.Error("a new password was not stored")
	}

	// The form's checks hold here too.
	if err := e.SetRegistry(ctx, Registry{Host: "https://registry.example.test", Username: "u", Password: "p"}); err == nil {
		t.Error("a host with a scheme was accepted")
	}

	// With no key to seal the password, nothing is stored.
	unsealed := compose("")
	if err := unsealed.SetRegistry(ctx, r); !errors.Is(err, ErrRegistryNoSecretKey) {
		t.Errorf("SetRegistry with no secret key = %v, want ErrRegistryNoSecretKey", err)
	}
}
