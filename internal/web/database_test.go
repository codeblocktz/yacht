package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/database"
)

// databaseApps is fakeApps with the database surfaces: a browser that answers
// from memory, credentials with a password the page must not show, and
// backups kept in a slice.
type databaseApps struct {
	*fakeApps
	deployed []app.Source
	queries  []string
	backups  app.BackupOverview
	started  int
	restored []uuid.UUID
}

const testPassword = "s3cret-password-value"

func (d *databaseApps) DeployDatabase(_ context.Context, ownerID string, src app.Source, _ uuid.UUID) (app.App, error) {
	d.deployed = append(d.deployed, src)
	a := sampleApp(ownerID, string(src))
	a.Source = src
	d.byOwner[ownerID] = append(d.byOwner[ownerID], a)
	return a, nil
}

func (d *databaseApps) DatabaseCredentials(_ context.Context, _, name string) (app.DatabaseCredentials, error) {
	host := name + ".yacht-abc.svc.cluster.local"
	return app.DatabaseCredentials{
		Source: app.SourcePostgres, Host: host, Port: 5432, User: "yacht", Database: "yacht",
		Password: testPassword, URLKey: "DATABASE_URL",
		URL: "postgres://yacht:" + testPassword + "@" + host + ":5432/yacht?sslmode=disable",
	}, nil
}

func (d *databaseApps) DatabaseBrowser(_ context.Context, _, _ string) (database.Browser, error) {
	return fakeBrowser{d}, nil
}

type fakeBrowser struct{ d *databaseApps }

func (fakeBrowser) Tables(context.Context) ([]database.Table, error) {
	return []database.Table{
		{Schema: "public", Name: "users", Kind: "table", Rows: 120, Bytes: 8192},
		{Schema: "billing", Name: "invoices", Kind: "table", Rows: -1},
	}, nil
}

func (fakeBrowser) Rows(_ context.Context, schema, table string, page int) (database.Result, error) {
	if table != "users" && table != "invoices" {
		return database.Result{}, database.ErrNoSuchTable
	}
	return database.Result{
		Columns: []string{"id", "email", "note"},
		Rows: [][]database.Value{
			{{Text: "1"}, {Text: "ana@example.com"}, {Null: true}},
			{{Text: "2"}, {Text: "bo@example.com"}, {Text: "long", Cut: true}},
		},
		More: page == 0,
	}, nil
}

func (b fakeBrowser) Query(_ context.Context, sql string) (database.Result, error) {
	b.d.queries = append(b.d.queries, sql)
	return database.Result{Columns: []string{"count"}, Rows: [][]database.Value{{{Text: "120"}}}}, nil
}

func (fakeBrowser) Stats(context.Context) (database.Stats, error) {
	return database.Stats{Version: "16.4", SizeBytes: 9 << 20, Connections: 3, MaxConns: 100, Tables: 2, CacheHit: 0.99}, nil
}

func (fakeBrowser) Config(context.Context) (database.Config, error) {
	return database.Config{
		Settings:   []database.Setting{{Name: "max_connections", Value: "100", About: "Sets the maximum number of concurrent connections."}},
		Extensions: []database.Extension{{Name: "plpgsql", Version: "1.0"}},
	}, nil
}

func (d *databaseApps) Backups(context.Context, string, string) (app.BackupOverview, error) {
	return d.backups, nil
}

func (d *databaseApps) StartBackup(context.Context, string, string, string) (app.Backup, error) {
	d.started++
	return app.Backup{}, nil
}

func (d *databaseApps) RestoreBackup(_ context.Context, _, _ string, id uuid.UUID, _ string) error {
	d.restored = append(d.restored, id)
	return nil
}

func (d *databaseApps) DeleteBackup(context.Context, string, string, uuid.UUID) error { return nil }

func (d *databaseApps) SetBackupSchedule(_ context.Context, _, _ string, daily bool) error {
	d.backups.Daily = daily
	return nil
}

func newDatabaseApps() *databaseApps {
	db := sampleApp("owner-1", "db")
	db.Source, db.Internal, db.Host = app.SourcePostgres, true, ""
	web := sampleApp("owner-1", "web")
	return &databaseApps{fakeApps: newFakeApps(db, web)}
}

func TestAddDatabaseOffersEveryEngineAndDeploysOneInAClick(t *testing.T) {
	apps := newDatabaseApps()
	h := testServer(t, Options{Apps: apps})

	canvas := get(t, h, "/projects/default").Body.String()
	for _, want := range []string{"Add PostgreSQL", "Add MySQL", "Add Redis", "Add MongoDB", `action="/apps/new/database"`} {
		if !strings.Contains(canvas, want) {
			t.Errorf("the canvas's Add is missing %q", want)
		}
	}

	picker := get(t, h, "/apps/new").Body.String()
	if !strings.Contains(picker, `href="/apps/new/database"`) {
		t.Error("the deploy picker does not offer Database")
	}
	if strings.Contains(picker, `href="/apps/new?source=postgres"`) {
		t.Error("the picker lists an engine on its own as well as under Database")
	}

	page := get(t, h, "/apps/new/database").Body.String()
	for _, want := range []string{"PostgreSQL", "MySQL", "Redis", "MongoDB", "engine-logo"} {
		if !strings.Contains(page, want) {
			t.Errorf("Add › Database page is missing %q", want)
		}
	}

	rec := post(t, h, "/apps/new/database", url.Values{"engine": {"mysql"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/mysql" {
		t.Fatalf("deploy = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(apps.deployed) != 1 || apps.deployed[0] != app.SourceMySQL {
		t.Errorf("deployed = %v", apps.deployed)
	}
	if rec := post(t, h, "/apps/new/database", url.Values{"engine": {"image"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("an image through Add › Database = %d", rec.Code)
	}
}

func TestADatabaseCardShowsItsEnginesLogo(t *testing.T) {
	h := testServer(t, Options{Apps: newDatabaseApps()})
	body := get(t, h, "/projects/default").Body.String()
	if !strings.Contains(body, `class="engine-logo"`) || !strings.Contains(body, engineLogos[app.SourcePostgres].Color) {
		t.Error("the Postgres card has no Postgres logo")
	}
}

func TestTheDatabaseTabBrowsesTablesAndNeverShowsThePassword(t *testing.T) {
	h := testServer(t, Options{Apps: newDatabaseApps()})

	body := get(t, h, "/apps/db/database").Body.String()
	for _, want := range []string{
		">Database<", ">Backups<", ">Data<", ">Stats<", ">Config<", ">Connect<",
		"users", "billing.", "≈ 120 rows", "ana@example.com", `class="db-null">NULL`, "Next",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Data view is missing %q", want)
		}
	}
	if web := get(t, h, "/apps/web").Body.String(); strings.Contains(web, ">Backups<") {
		t.Error("an image app has the database tabs")
	}

	rows := get(t, h, "/apps/db/database?schema=billing&table=invoices&page=1").Body.String()
	if !strings.Contains(rows, "Previous") || !strings.Contains(rows, "Page 2") {
		t.Error("paging a table does not offer the way back")
	}

	rec := post(t, h, "/apps/db/database/query", url.Values{"sql": {"SELECT count(*) FROM users"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<td>120") {
		t.Errorf("query = %d", rec.Code)
	}

	stats := get(t, h, "/apps/db/database?view=stats").Body.String()
	if !strings.Contains(stats, "16.4") || !strings.Contains(stats, "of 100 allowed") {
		t.Error("stats are missing the version or connections")
	}
	config := get(t, h, "/apps/db/database?view=config").Body.String()
	if !strings.Contains(config, "max_connections") || !strings.Contains(config, "plpgsql") {
		t.Error("config is missing the settings or extensions")
	}

	connect := get(t, h, "/apps/db/database?view=connect").Body.String()
	for _, want := range []string{"db.yacht-abc.svc.cluster.local", "${{db.DATABASE_URL}}", "yacht:••••••••@", "TCP proxy"} {
		if !strings.Contains(connect, want) {
			t.Errorf("Connect is missing %q", want)
		}
	}
	for path, body := range map[string]string{"data": body, "connect": connect, "stats": stats} {
		if strings.Contains(body, testPassword) {
			t.Errorf("the %s view shows the password", path)
		}
	}

	revealed := post(t, h, "/apps/db/database/reveal", nil).Body.String()
	if !strings.Contains(revealed, testPassword) {
		t.Error("an owner who asked to see the connection string was not shown it")
	}
}

func TestTheBackupsTabListsAndRestores(t *testing.T) {
	apps := newDatabaseApps()
	id := uuid.New()
	done := time.Now()
	apps.backups = app.BackupOverview{
		Supported: true, Location: app.BackupInS3, Where: "r2.example/backups/databases", Keep: 7,
		Items: []app.Backup{{
			ID: id, Kind: app.BackupKindBackup, Trigger: app.BackupScheduled, Status: app.BackupSucceeded,
			SizeBytes: 3 << 20, StartedAt: done.Add(-time.Minute), FinishedAt: &done,
		}},
	}
	h := testServer(t, Options{Apps: apps})

	body := get(t, h, "/apps/db/backups").Body.String()
	for _, want := range []string{"Back up now", "r2.example/backups/databases", "Backup, daily", "3.0 MiB", "Restore", "Back up every day"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Backups tab is missing %q", want)
		}
	}
	if rec := post(t, h, "/apps/db/backups", nil); rec.Code != http.StatusSeeOther || apps.started != 1 {
		t.Errorf("back up now = %d, started %d", rec.Code, apps.started)
	}
	if rec := post(t, h, "/apps/db/backups/"+id.String()+"/restore", nil); rec.Code != http.StatusSeeOther ||
		len(apps.restored) != 1 || apps.restored[0] != id {
		t.Errorf("restore = %d, %v", rec.Code, apps.restored)
	}
	post(t, h, "/apps/db/backups/schedule", url.Values{"daily": {"on"}})
	if !apps.backups.Daily {
		t.Error("the daily backup was not turned on")
	}

	apps.backups.Running = true
	if frag := get(t, h, "/apps/db/backups/fragment").Body.String(); !strings.Contains(frag, `hx-trigger="every 3s"`) {
		t.Error("a running backup's list does not poll")
	}
}
