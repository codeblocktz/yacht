package web

import (
	"time"

	"github.com/a-h/templ"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/database"
	"github.com/codeblocktz/yacht/internal/dbrelease"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// The states a database's tabs add. Reaching most of them by clicking needs a
// running database with tables in it, which is why they are drawn here.
func databaseGalleryPages() []galleryPage {
	now := time.Now()
	db := app.App{
		ID: uuid.New(), OwnerID: "owner-local", Name: "postgres", Namespace: app.Namespace("owner-local", "postgres"),
		Image: "postgres:16-alpine", Replicas: 1, Port: 5432, Source: app.SourcePostgres, Internal: true,
		StatusKnown: true, CreatedAt: now.Add(-72 * time.Hour),
		Status: orchestrator.AppStatus{Phase: orchestrator.PhaseRunning, Desired: 1, Ready: 1, Available: 1},
	}
	redis := db
	redis.Name, redis.Source, redis.Image = "redis", app.SourceRedis, "redis:7-alpine"
	pg, _ := app.BlueprintFor(app.SourcePostgres)
	creds := app.DatabaseCredentials{
		Source: app.SourcePostgres, Host: db.PrivateHost(), Port: 5432, User: "yacht", Database: "yacht",
		URLKey: "DATABASE_URL", URL: maskPassword("postgres://yacht:x@" + db.PrivateHost() + ":5432/yacht?sslmode=disable"),
	}
	tables := []database.Table{
		{Schema: "public", Name: "orders", Kind: "table", Rows: 18234, Bytes: 4 << 20},
		{Schema: "public", Name: "users", Kind: "table", Rows: 1204, Bytes: 512 << 10},
		{Schema: "public", Name: "recent_orders", Kind: "view", Rows: -1},
		{Schema: "audit", Name: "events", Kind: "partitioned table", Rows: 0},
	}
	rows := database.Result{
		Columns: []string{"id", "email", "name", "created_at", "settings"},
		Rows: [][]database.Value{
			{{Text: "1"}, {Text: "ana@example.com"}, {Text: "Ana"}, {Text: "2026-10-01 09:12:44+03"}, {Text: `{"theme": "dark"}`}},
			{{Text: "2"}, {Text: "bo@example.com"}, {Null: true}, {Text: "2026-10-02 11:03:10+03"}, {Text: `{"theme": "light", "notifications": {"email": true`, Cut: true}},
			{{Text: "3"}, {Text: "chen@example.com"}, {Text: ""}, {Text: "2026-10-03 16:40:02+03"}, {Null: true}},
		},
		More: true,
	}
	view := func(a app.App, tab string, d *DatabaseData, b *BackupsData) AppDetailData {
		return AppDetailData{App: a, Tab: tab, Database: d, Backups: b}
	}
	data := &DatabaseData{View: dbViewData, Browsable: true, Tables: tables, Schema: "public", Table: "users",
		Rows: &rows, Query: "SELECT status, count(*) FROM orders GROUP BY 1",
		Result: &database.Result{Columns: []string{"status", "count"}, Rows: [][]database.Value{
			{{Text: "paid"}, {Text: "17120"}}, {{Text: "refunded"}, {Text: "1114"}},
		}, Elapsed: 38 * time.Millisecond}, Creds: creds}
	failed := &DatabaseData{View: dbViewData, Browsable: true, Tables: tables, Schema: "public", Table: "users",
		Rows: &rows, Query: "DELETE FROM users", QueryError: "The data browser only reads — cannot execute DELETE in a read-only transaction", Creds: creds}
	done := now.Add(-23 * time.Hour)
	backups := &BackupsData{CanAdmin: true, BackupOverview: app.BackupOverview{
		Supported: true, Location: app.BackupInS3, Where: "acct.r2.cloudflarestorage.com/datahuru-backups/databases",
		Daily: true, Keep: 7, Running: true,
		Items: []app.Backup{
			{ID: uuid.New(), Kind: app.BackupKindBackup, Trigger: app.BackupManual, Status: app.BackupRunning,
				CreatedBy: "ana@example.com", StartedAt: now.Add(-20 * time.Second)},
			{ID: uuid.New(), Kind: app.BackupKindRestore, Trigger: app.BackupManual, Status: app.BackupFailed,
				CreatedBy: "ana@example.com", StartedAt: now.Add(-2 * time.Hour), FinishedAt: &done,
				Error: "the restore step exited 1: pg_restore: error: could not execute query: ERROR:  relation \"orders\" is locked"},
			{ID: uuid.New(), Kind: app.BackupKindBackup, Trigger: app.BackupScheduled, Status: app.BackupSucceeded,
				SizeBytes: 23 << 20, StartedAt: done, FinishedAt: &done},
		},
	}}
	return []galleryPage{
		{
			file: "states-database-add.html", path: "/apps/new/database",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "Add a database"}},
			page: stack(
				section("Add › Database", "each engine deployed by one click",
					NewDatabase(NewDatabaseData{ProjectName: "Shop", Project: "shop",
						Engines: []app.Blueprint{pg, mustBlueprint(app.SourceMySQL), mustBlueprint(app.SourceRedis), mustBlueprint(app.SourceMongo)}})),
				section("In the canvas's Add", "the disclosure, open", panelWrap(canvasAddDatabase("shop"))),
			),
		},
		{
			file: "states-database-tab.html", path: "/apps/postgres/database",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "postgres"}, {Label: "Database"}},
			page: stack(
				section("Data", "tables, a page of one, and a query's result",
					appDatabase(view(db, "database", data, nil))),
				section("A refused query", "a write, said plainly",
					appDatabase(view(db, "database", failed, nil))),
				section("Unreachable", "a database with nothing answering",
					appDatabase(view(db, "database", &DatabaseData{View: dbViewData, Browsable: true,
						Unreachable: "Connect: dial tcp 10.43.12.7:5432: connect: connection refused"}, nil))),
				section("Stats", "",
					appDatabase(view(db, "database", &DatabaseData{View: dbViewStats, Browsable: true,
						Stats: &database.Stats{Version: "16.4", SizeBytes: 31 << 20, Connections: 4, MaxConns: 100,
							Uptime: 52 * time.Hour, Tables: 4, CacheHit: 0.993, Commits: 81234,
							Largest: tables[:2]}}, nil))),
				section("Config", "",
					appDatabase(view(db, "database", &DatabaseData{View: dbViewConfig, Browsable: true, Blueprint: pg,
						Config: &database.Config{
							Settings: []database.Setting{
								{Name: "max_connections", Value: "100", About: "Sets the maximum number of concurrent connections."},
								{Name: "shared_buffers", Value: "16384", Unit: "8kB", About: "Sets the number of shared memory buffers used by the server."},
							},
							Extensions: []database.Extension{{Name: "plpgsql", Version: "1.0"}, {Name: "pg_trgm", Version: "1.6"}},
						}}, nil))),
				section("Connect", "the password masked; the reference to copy instead",
					appDatabase(view(db, "database", &DatabaseData{View: dbViewConnect, Creds: creds, CanAdmin: true}, nil))),
				section("Redis", "no tables to browse",
					appDatabase(view(redis, "database", &DatabaseData{View: dbViewData}, nil))),
			),
		},
		{
			file: "states-database-version.html", path: "/apps/postgres",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "postgres"}, {Label: "Deployments"}},
			page:   databaseVersionStates(db),
		},
		{
			file: "states-database-backups.html", path: "/apps/postgres/backups",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "postgres"}, {Label: "Backups"}},
			page: stack(
				section("To a bucket", "one running, a failed restore, a daily backup",
					appBackups(view(db, "backups", nil, backups))),
				section("On a volume", "no bucket configured, nothing yet",
					appBackups(view(db, "backups", nil, &BackupsData{BackupOverview: app.BackupOverview{
						Supported: true, Location: app.BackupOnVolume, Keep: 7}}))),
				section("Not built", "an engine with no recipe",
					appBackups(view(redis, "backups", nil, &BackupsData{BackupOverview: app.BackupOverview{
						Because: "Backups of Redis are not built yet"}}))),
			),
		},
	}
}

// databaseVersionStates is the line on a database's Deployments tab that
// says what it runs, in each of the states it can be in.
func databaseVersionStates(db app.App) templ.Component {
	v := func(minor int) dbrelease.Version { return dbrelease.Version{Major: "16", Minor: minor} }
	fixes := []dbrelease.Advisory{
		{ID: "CVE-2026-19385", URL: "https://www.postgresql.org/support/security/CVE-2026-19385/", Component: "client", Score: 8.8,
			Summary: "PostgreSQL pg_dump heap buffer overflow executes arbitrary code", Fixed: v(15)},
		{ID: "CVE-2026-16241", URL: "https://www.postgresql.org/support/security/CVE-2026-16241/", Component: "core server", Score: 7.1,
			Summary: "PostgreSQL CREATE STATISTICS does not check the schema privilege", Fixed: v(14)},
		{ID: "CVE-2026-18024", URL: "https://www.postgresql.org/support/security/CVE-2026-18024/", Component: "core server", Score: 4.3,
			Summary: "PostgreSQL ascii() function reads past end of buffer", Fixed: v(15)},
	}
	rel := func(fixes []dbrelease.Advisory) app.DatabaseRelease {
		return app.DatabaseRelease{Engine: "PostgreSQL", Version: "16.4", Known: true,
			Report: dbrelease.Report{Running: v(4), Latest: v(15), Supported: true, Fixes: fixes}}
	}
	data := func(image string, r app.DatabaseRelease) DatabaseReleaseData {
		return DatabaseReleaseData{Name: db.Name, Image: image, R: r}
	}
	update := func(image string, backup bool) *app.DatabaseUpdate {
		return &app.DatabaseUpdate{Version: "16.15", Label: "PostgreSQL 16.15", Image: image, Backup: backup}
	}
	newer, pinned, waiting, medium := rel(fixes), rel(fixes[2:]), rel(fixes), rel(fixes[2:])
	newer.Update = update(db.Image, true)
	pinned.Update = update("postgres:16.15-alpine", true)
	current := app.DatabaseRelease{Engine: "PostgreSQL", Version: "16.15", Known: true,
		Report: dbrelease.Report{Running: v(15), Latest: v(15), Supported: true}}
	ended := app.DatabaseRelease{Engine: "PostgreSQL", Version: "12.22", Known: true, Report: dbrelease.Report{
		Running: dbrelease.Version{Major: "12", Minor: 22}, Latest: dbrelease.Version{Major: "12", Minor: 22},
		EOL: time.Date(2024, 11, 21, 0, 0, 0, 0, time.UTC)}}
	medium.Update = update(db.Image, true)
	nextMajor := current
	nextMajor.NextMajor = "17"
	quietUpdate := app.DatabaseRelease{Engine: "PostgreSQL", Version: "16.15", Known: true, NextMajor: "17",
		Report: dbrelease.Report{Running: v(15), Latest: dbrelease.Version{Major: "16", Minor: 16}, Supported: true},
		Update: &app.DatabaseUpdate{Version: "16.16", Label: "PostgreSQL 16.16", Image: db.Image, Backup: true}}
	redis := app.DatabaseRelease{Engine: "Redis", Version: "7.4.1", Series: "7", Newest: "7.4.2", NextMajor: "8",
		Update: &app.DatabaseUpdate{Version: "7.4.2", Label: "Redis 7.4.2", Image: "redis:7.4.2-alpine"}}
	return stack(
		section("Up to date", "the newest release of its major version",
			panelWrap(databaseReleaseBlock(data(db.Image, current)))),
		section("A new major version", "said, never offered in place",
			panelWrap(databaseReleaseBlock(data(db.Image, nextMajor)))),
		section("An update with nothing to fix", "the Update button, a backup first",
			panelWrap(databaseReleaseBlock(data(db.Image, quietUpdate)))),
		section("Another engine", "Redis, pinned to its newer tag, without backups",
			panelWrap(databaseReleaseBlock(data("redis:7.4.1-alpine", redis)))),
		section("Missing fixes, image updated", "Update takes the newer image",
			panelWrap(databaseReleaseBlock(data(db.Image, newer)))),
		section("Missing a medium fix", "amber rather than red",
			panelWrap(databaseReleaseBlock(data(db.Image, medium)))),
		section("Pinned to a release", "Update moves the tag",
			panelWrap(databaseReleaseBlock(data("postgres:16.4-alpine", pinned)))),
		section("Image not yet updated", "nothing to press yet",
			panelWrap(databaseReleaseBlock(data(db.Image, waiting)))),
		section("Out of support", "a major version past its end",
			panelWrap(databaseReleaseBlock(data("postgres:12-alpine", ended)))),
	)
}
