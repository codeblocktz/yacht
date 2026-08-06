package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestReleaseBackfillCohortIsClassifiedWithoutARegistry(t *testing.T) {
	ctx := context.Background()
	dsn := migrationSandbox(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, db, migrationsDir, 22); err != nil {
		t.Fatalf("migrate last supported schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO teams (id, display_name) VALUES ('backfill-team', 'Backfill')`); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	seed := func(name, image, status string) {
		t.Helper()
		var id string
		if err := db.QueryRowContext(ctx, `
			INSERT INTO apps (owner_id, name, namespace, image, replicas, port)
			VALUES ('backfill-team', $1, 'ns-' || $1, $2, 1, 8080)
			RETURNING id`, name, image).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if status != "" {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO deployments (owner_id, app_id, image, revision, status)
				VALUES ('backfill-team', $1, $2, 'legacy', $3)`, id, image, status); err != nil {
				t.Fatalf("seed deployment %s: %v", name, err)
			}
		}
	}
	seed("normal", "nginx:1.27", "active")
	seed("pending", "yacht.invalid/not-built-yet:pending", "running")
	seed("none", "nginx:1.27", "")
	seed("failed", "nginx:1.27", "failed")
	seed("older-success", "nginx:1.27", "superseded")

	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("migrate to head without registry access: %v", err)
	}
	wants := map[string]string{
		"normal": "pending", "pending": "pending_image",
		"none": "never_deployed", "failed": "never_deployed",
		"older-success": "pending",
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.name, b.state FROM app_release_backfills b
		JOIN apps a ON a.id = b.app_id`)
	if err != nil {
		t.Fatalf("read cohort: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name, state string
		if err := rows.Scan(&name, &state); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if state != wants[name] {
			t.Errorf("%s state = %q, want %q", name, state, wants[name])
		}
	}
	if seen != len(wants) {
		t.Fatalf("cohort rows = %d, want %d", seen, len(wants))
	}
}
