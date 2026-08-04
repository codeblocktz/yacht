package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// Migration 00022 adds authenticated_at to sessions, and the value it gives to
// rows that already exist is the whole risk in it.
//
// A plain DEFAULT now() would declare every session open at the moment the
// migration runs to be freshly authenticated, and each of them would carry a
// free step-up window — long enough to add a password to somebody else's account
// from a cookie that had been sitting idle for a month. Nothing about that
// failure is visible: the sessions keep working, the column is populated, and
// the only symptom is an authorisation check answering yes when it should have
// said no.
//
// This migrates a database of its own to 00021, fills it with sessions of
// different ages, and then runs the migration under test.
func TestMigration22DatesEverySessionFromWhenItWasCreated(t *testing.T) {
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

	if err := goose.UpToContext(ctx, db, migrationsDir, 21); err != nil {
		t.Fatalf("migrate to 21: %v", err)
	}

	seedPreCredentialSessions(t, ctx, db)

	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	// Every session must be dated from when it was created, whatever its age.
	// The month-old one is the one that matters: if it comes back as recently
	// authenticated, the backfill used now().
	rows, err := db.QueryContext(ctx,
		`SELECT user_agent, created_at, authenticated_at FROM sessions ORDER BY user_agent`)
	if err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var agent string
		var created, authenticated time.Time
		if err := rows.Scan(&agent, &created, &authenticated); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if !authenticated.Equal(created) {
			t.Errorf("%s: authenticated_at = %v, want created_at %v — an existing "+
				"session was handed a step-up window it never earned",
				agent, authenticated, created)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 3 {
		t.Fatalf("read %d sessions, want 3", seen)
	}

	// The column has to be usable as well as correct: NOT NULL with a default,
	// so that CreateSession keeps working without naming it.
	var notNull bool
	var def sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT attnotnull, pg_get_expr(d.adbin, d.adrelid)
		 FROM pg_attribute a
		 LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE a.attrelid = 'sessions'::regclass AND a.attname = 'authenticated_at'`,
	).Scan(&notNull, &def); err != nil {
		t.Fatalf("read column definition: %v", err)
	}
	if !notNull {
		t.Error("authenticated_at is nullable, so a session can exist having proved nothing")
	}
	if !def.Valid {
		t.Error("authenticated_at has no default, so inserting a session must name it")
	}

	// A session inserted after the migration, the way CreateSession does it,
	// must come out authenticated now rather than null.
	var fresh time.Time
	if err := db.QueryRowContext(ctx,
		`INSERT INTO sessions (user_id, token_hash, active_team_id, expires_at)
		 SELECT id, '\x99'::bytea, 'team-mig', now() + interval '1 hour' FROM users LIMIT 1
		 RETURNING authenticated_at`).Scan(&fresh); err != nil {
		t.Fatalf("insert a session the way CreateSession does: %v", err)
	}
	if time.Since(fresh) > time.Minute {
		t.Errorf("a session created after the migration is dated %v, not now", fresh)
	}

	// Rolling back has to leave the sessions resolvable, because a rollback
	// happens on a running install and the people holding those cookies did
	// nothing wrong.
	if err := goose.DownToContext(ctx, db, migrationsDir, 21); err != nil {
		t.Fatalf("migrate back to 21: %v", err)
	}
	var after int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&after); err != nil {
		t.Fatalf("count sessions after rollback: %v", err)
	}
	if after != 4 {
		t.Errorf("counted %d sessions after rolling back, want 4", after)
	}
	var credentials int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name = 'user_credentials'`).Scan(&credentials); err != nil {
		t.Fatalf("look for user_credentials: %v", err)
	}
	if credentials != 0 {
		t.Error("user_credentials survived the rollback")
	}
}

// The state a live install is actually in when this migration runs: people with
// teams, and sessions of assorted ages, one of them long past the step-up window
// and one of them expired.
func seedPreCredentialSessions(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO teams (id, display_name) VALUES ('team-mig', 'Migration')`); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	var userID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO users (email, display_name) VALUES ('mig@example.test', 'Mig')
		 RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO memberships (user_id, owner_id, role) VALUES ($1, 'team-mig', 'owner')`,
		userID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	for _, s := range []struct {
		agent   string
		created string
		expires string
	}{
		{"a-just-now", "now()", "now() + interval '30 days'"},
		{"b-an-hour-old", "now() - interval '1 hour'", "now() + interval '29 days'"},
		// The one that would be handed a free step-up window by a now() default.
		{"c-a-month-old", "now() - interval '30 days'", "now() + interval '1 hour'"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO sessions (user_id, token_hash, active_team_id, user_agent,
				created_at, expires_at)
			 VALUES ($1, decode(md5($2), 'hex'), 'team-mig', $2, `+s.created+`, `+s.expires+`)`,
			userID, s.agent); err != nil {
			t.Fatalf("seed session %s: %v", s.agent, err)
		}
	}
}
