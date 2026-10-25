package web

import (
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/database"
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
