package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/database"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Databases is what a database app's Database tab and Add › Database need.
//
// Optional, and asserted on Apps rather than added to it: a wrapper that
// implements Apps itself keeps compiling, and an install whose Apps does not
// offer this simply has no Database tab and no Add › Database.
type Databases interface {
	DeployDatabase(ctx context.Context, ownerID string, src app.Source, projectID uuid.UUID) (app.App, error)
	DatabaseCredentials(ctx context.Context, ownerID, name string) (app.DatabaseCredentials, error)
	DatabaseBrowser(ctx context.Context, ownerID, name string) (database.Browser, error)
}

// Backups is a database's Backups tab. Optional in the same way.
type Backups interface {
	Backups(ctx context.Context, ownerID, name string) (app.BackupOverview, error)
	StartBackup(ctx context.Context, ownerID, name, actor string) (app.Backup, error)
	RestoreBackup(ctx context.Context, ownerID, name string, id uuid.UUID, actor string) error
	DeleteBackup(ctx context.Context, ownerID, name string, id uuid.UUID) error
	SetBackupSchedule(ctx context.Context, ownerID, name string, daily bool) error
}

func (s *Server) databases() Databases {
	d, _ := s.apps.(Databases)
	return d
}

func (s *Server) backupsService() Backups {
	b, _ := s.apps.(Backups)
	return b
}

// mountDatabases puts the member routes on the router: reading a database,
// running a read-only query, adding one, and taking a backup — none of which
// changes what is in it.
func (s *Server) mountDatabases(r chi.Router) {
	if s.databases() != nil {
		r.Get("/apps/new/database", s.databaseNew)
		r.Post("/apps/new/database", s.inEnvironment(s.databaseDeploy))
		r.Get("/apps/{name}/database", s.databaseTab)
		r.Post("/apps/{name}/database/query", s.databaseQuery)
	}
	if s.databaseReleases() != nil {
		r.Get("/apps/{name}/database/release", s.databaseRelease)
	}
	if s.backupsService() != nil {
		r.Get("/apps/{name}/backups", s.backupsTab)
		r.Get("/apps/{name}/backups/fragment", s.backupsFragment)
		r.Post("/apps/{name}/backups", s.backupCreate)
		r.Post("/apps/{name}/backups/schedule", s.backupSchedule)
	}
}

// mountDatabaseAdmin puts the admin routes on the router: the connection
// string with its password, and the two backup actions that cannot be
// undone — a restore replaces what is there, and a deleted backup is gone.
func (s *Server) mountDatabaseAdmin(r chi.Router) {
	if s.databases() != nil {
		r.Post("/apps/{name}/database/reveal", s.databaseReveal)
	}
	if s.backupsService() != nil {
		r.Post("/apps/{name}/backups/{id}/restore", s.backupRestore)
		r.Post("/apps/{name}/backups/{id}/delete", s.backupDelete)
	}
}

// ---------------------------------------------------------------- add

// NewDatabaseData is Add › Database as a page: each engine, one click.
type NewDatabaseData struct {
	Project, ProjectName string
	Engines              []app.Blueprint
	Error                string
}

func (s *Server) databaseEngines(ctx context.Context) []app.Blueprint {
	var out []app.Blueprint
	for _, b := range s.sources(ctx) {
		if b.Source.Database() {
			out = append(out, b)
		}
	}
	return out
}

func (s *Server) databaseNew(w http.ResponseWriter, r *http.Request) {
	d := NewDatabaseData{Engines: s.databaseEngines(r.Context())}
	d.Project, d.ProjectName = s.namedProject(r, r.URL.Query().Get("project"))
	s.render(w, r, NewDatabase(d))
}

// databaseDeploy deploys one engine, named after itself, and opens it.
func (s *Server) databaseDeploy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	project := strings.TrimSpace(r.FormValue("project"))
	fail := func(msg string) {
		d := NewDatabaseData{Engines: s.databaseEngines(ctx), Error: msg}
		d.Project, d.ProjectName = s.namedProject(r, project)
		s.renderStatus(w, r, http.StatusUnprocessableEntity, NewDatabase(d))
	}
	src := app.Source(r.FormValue("engine"))
	if !src.Database() {
		fail("Choose a database.")
		return
	}
	projectID, err := s.projectID(ctx, owner.ID, project)
	if err != nil {
		fail(err.Error())
		return
	}
	a, err := s.databases().DeployDatabase(ctx, owner.ID, src, projectID)
	if err != nil {
		s.log.Warn("add database failed", slog.String("engine", string(src)), slog.String("error", err.Error()))
		fail(strings.TrimPrefix(err.Error(), "app: "))
		return
	}
	auditTarget(ctx, a.Name)
	http.Redirect(w, r, "/apps/"+a.Name, http.StatusSeeOther)
}

// nonDatabaseSources is the picker's list without the databases, which it
// offers as one entry.
func nonDatabaseSources(all []app.Blueprint) []app.Blueprint {
	out := make([]app.Blueprint, 0, len(all))
	for _, b := range all {
		// A bucket is Add › Bucket's, one click, as a database is.
		if !b.Source.Database() && b.Source.Workload() {
			out = append(out, b)
		}
	}
	return out
}

// newDatabaseHref is Add › Database, into a project when one is named.
func newDatabaseHref(project string) string {
	return "/apps/new/database" + projectQuery("?", project)
}

// ---------------------------------------------------------------- database tab

// The Database tab's views, Railway's sub-tabs.
const (
	dbViewData    = "data"
	dbViewStats   = "stats"
	dbViewConfig  = "config"
	dbViewConnect = "connect"
)

// DatabaseData is the Database tab.
type DatabaseData struct {
	View string

	// Browsable is false for an engine with no data browser; Unreachable is
	// why the browser could not reach a database it does have one for.
	Browsable   bool
	Unreachable string

	// Data: the tables, and the open one's page of rows.
	Tables []database.Table
	Schema string
	Table  string
	Page   int
	Rows   *database.Result
	// RowsError is a page that could not be read, said beside the table
	// rather than in place of the whole tab.
	RowsError string

	// Query is what was run, and Result or QueryError what came of it.
	Query      string
	Result     *database.Result
	QueryError string

	Stats  *database.Stats
	Config *database.Config

	// Connect.
	Creds     app.DatabaseCredentials
	Blueprint app.Blueprint
	// Revealed is the one render that shows the connection string with its
	// password, for an admin who asked; every other render masks it.
	Revealed bool
	CanAdmin bool
	// Public is the address outside the cluster, where the install has a
	// TCP proxy for this database. Empty for now: see publicAddress.
	Public string
}

// BackupsData is the Backups tab.
type BackupsData struct {
	app.BackupOverview
	CanAdmin bool
	Error    string
}

func (s *Server) databaseTab(w http.ResponseWriter, r *http.Request) {
	s.renderDatabase(w, r, func(d *DatabaseData) {})
}

// databaseQuery runs one statement and shows its result under the box.
//
// A form post that renders the page, rather than a script: the result is the
// page's state, and a reload or the back button should not run it again
// silently — a browser asks before resubmitting a post.
func (s *Server) databaseQuery(w http.ResponseWriter, r *http.Request) {
	sql := strings.TrimSpace(r.FormValue("sql"))
	s.renderDatabase(w, r, func(d *DatabaseData) {
		d.View, d.Query = dbViewData, sql
		if sql == "" {
			d.QueryError = "There is no query to run."
		}
	})
}

// databaseReveal shows the connection string, password and all, once.
func (s *Server) databaseReveal(w http.ResponseWriter, r *http.Request) {
	s.log.Info("database credentials revealed",
		slog.String("app", chi.URLParam(r, "name")),
		slog.String("by", s.actorEmail(r)))
	s.renderDatabase(w, r, func(d *DatabaseData) {
		d.View, d.Revealed = dbViewConnect, true
	})
}

// renderDatabase draws the canvas with the app's Database tab open, loading
// only what the chosen view shows: each view is a connection to somebody's
// database, and the tab should not open four to draw one.
func (s *Server) renderDatabase(w http.ResponseWriter, r *http.Request, adjust func(*DatabaseData)) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	dbs := s.databases()

	d := &DatabaseData{
		View: databaseView(r.URL.Query().Get("view")),
		Page: atoiOr(r.URL.Query().Get("page"), 0),
		// Schema and table travel together, as "schema.table" would not:
		// either may contain a dot.
		Schema: r.URL.Query().Get("schema"), Table: r.URL.Query().Get("table"),
		CanAdmin: s.Allows(r, account.RoleAdmin),
	}

	s.renderCanvasWith(w, r, "", name, "database", func(panel *AppDetailData) {
		panel.Database = d
		if !panel.App.Source.Database() {
			return
		}
		creds, err := dbs.DatabaseCredentials(ctx, owner.ID, name)
		if err != nil {
			d.Unreachable = databaseSentence(err)
		}
		d.Creds = creds
		d.Blueprint, _ = app.BlueprintFor(panel.App.Source)
		d.Public = s.publicAddress(ctx, panel.App)

		adjust(d)
		if !d.Revealed {
			d.Creds.Password, d.Creds.URL = "", maskPassword(d.Creds.URL)
		}
		if d.View == dbViewConnect {
			return
		}

		b, err := dbs.DatabaseBrowser(ctx, owner.ID, name)
		switch {
		case errors.Is(err, database.ErrNoBrowser):
			return
		case err != nil:
			d.Browsable, d.Unreachable = true, databaseSentence(err)
			return
		}
		d.Browsable = true
		if d.Query != "" && d.QueryError == "" {
			if res, err := b.Query(ctx, d.Query); err != nil {
				d.QueryError = databaseSentence(err)
			} else {
				d.Result = &res
			}
		}
		s.loadDatabaseView(ctx, b, d)
	})
}

func (s *Server) loadDatabaseView(ctx context.Context, b database.Browser, d *DatabaseData) {
	switch d.View {
	case dbViewStats:
		st, err := b.Stats(ctx)
		if err != nil {
			d.Unreachable = databaseSentence(err)
			return
		}
		d.Stats = &st
	case dbViewConfig:
		c, err := b.Config(ctx)
		if err != nil {
			d.Unreachable = databaseSentence(err)
			return
		}
		d.Config = &c
	default:
		tables, err := b.Tables(ctx)
		if err != nil {
			d.Unreachable = databaseSentence(err)
			return
		}
		d.Tables = tables
		if d.Table == "" && d.Result == nil && d.QueryError == "" && len(tables) > 0 {
			d.Schema, d.Table = tables[0].Schema, tables[0].Name
		}
		if d.Table == "" {
			return
		}
		rows, err := b.Rows(ctx, d.Schema, d.Table, d.Page)
		if err != nil {
			d.RowsError = databaseSentence(err)
			return
		}
		d.Rows = &rows
	}
}

func databaseView(v string) string {
	switch v {
	case dbViewStats, dbViewConfig, dbViewConnect:
		return v
	}
	return dbViewData
}

// publicAddress is where the database is reached from outside the cluster:
// the TCP proxy's host:port, once the install has one. None yet, so Connect
// says so; this is the one place that changes when it arrives.
func (s *Server) publicAddress(context.Context, app.App) string { return "" }

// maskPassword hides the password in a connection URL and keeps the rest,
// which is what somebody checking the host or the database name needs.
func maskPassword(u string) string {
	scheme := strings.Index(u, "://")
	at := strings.LastIndex(u, "@")
	if scheme < 0 || at < scheme {
		return u
	}
	creds := u[scheme+3 : at]
	if colon := strings.Index(creds, ":"); colon >= 0 {
		return u[:scheme+3] + creds[:colon] + ":••••••••" + u[at:]
	}
	return u
}

// databaseSentence is an error as the tab shows it.
func databaseSentence(err error) string {
	switch {
	case errors.Is(err, database.ErrNoSuchTable):
		return "That table is not in the database any more."
	case errors.Is(err, app.ErrDatabaseUnreachable):
		return "The dashboard cannot reach this cluster's services from where it runs."
	}
	msg := strings.TrimPrefix(err.Error(), "app: ")
	msg = strings.TrimPrefix(msg, "database: ")
	if msg == "" {
		return "Something went wrong."
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

func atoiOr(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// actorEmail is the signed-in person's address, for a record of who did
// something. Empty where the install has no accounts.
func (s *Server) actorEmail(r *http.Request) string {
	if s.accounts == nil {
		return ""
	}
	sess, err := s.accounts.ResolveSession(r.Context(), sessionToken(r))
	if err != nil {
		return ""
	}
	u, err := s.accounts.User(r.Context(), sess.UserID)
	if err != nil {
		return ""
	}
	return u.Email
}

// ---------------------------------------------------------------- backups tab

func (s *Server) backupsTab(w http.ResponseWriter, r *http.Request) {
	s.renderBackups(w, r, "")
}

func (s *Server) renderBackups(w http.ResponseWriter, r *http.Request, errMsg string) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	s.renderCanvasWith(w, r, "", name, "backups", func(panel *AppDetailData) {
		panel.Backups = s.backupsData(r, owner.ID, name, errMsg)
	})
}

func (s *Server) backupsData(r *http.Request, ownerID, name, errMsg string) *BackupsData {
	d := &BackupsData{CanAdmin: s.Allows(r, account.RoleAdmin), Error: errMsg}
	over, err := s.backupsService().Backups(r.Context(), ownerID, name)
	if err != nil {
		if !errors.Is(err, app.ErrNotADatabase) {
			s.log.Error("read backups", slog.String("app", name), slog.String("error", err.Error()))
		}
		d.Supported, d.Because = false, databaseSentence(err)
		return d
	}
	d.BackupOverview = over
	return d
}

// backupsFragment is the list alone, polled while a backup runs.
func (s *Server) backupsFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	if err := backupList(name, *s.backupsData(r, owner.ID, name, "")).Render(ctx, w); err != nil {
		s.log.Error("render backups", slog.String("error", err.Error()))
	}
}

func (s *Server) backupCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	if _, err := s.backupsService().StartBackup(ctx, owner.ID, name, s.actorEmail(r)); err != nil {
		s.flashErr(w, r, databaseSentence(err))
	} else {
		s.flashOK(w, r, "Backup started.")
	}
	http.Redirect(w, r, "/apps/"+name+"/backups", http.StatusSeeOther)
}

func (s *Server) backupSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	daily := formChecked(r, "daily")
	if err := s.backupsService().SetBackupSchedule(ctx, owner.ID, name, daily); err != nil {
		s.flashErr(w, r, databaseSentence(err))
	} else if daily {
		s.flashOK(w, r, "Backed up every day from now on; the first starts within a minute.")
	} else {
		s.flashOK(w, r, "Daily backups are off. The ones already taken are kept.")
	}
	http.Redirect(w, r, "/apps/"+name+"/backups", http.StatusSeeOther)
}

func (s *Server) backupRestore(w http.ResponseWriter, r *http.Request) {
	s.backupAction(w, r, func(ctx context.Context, owner, name string, id uuid.UUID) (string, error) {
		return "Restore started. The database is being replaced with the backup.",
			s.backupsService().RestoreBackup(ctx, owner, name, id, s.actorEmail(r))
	})
}

func (s *Server) backupDelete(w http.ResponseWriter, r *http.Request) {
	s.backupAction(w, r, func(ctx context.Context, owner, name string, id uuid.UUID) (string, error) {
		return "Backup deleted.", s.backupsService().DeleteBackup(ctx, owner, name, id)
	})
}

func (s *Server) backupAction(
	w http.ResponseWriter, r *http.Request,
	do func(ctx context.Context, owner, name string, id uuid.UUID) (string, error),
) {
	ctx := r.Context()
	owner := identity.MustFromContext(ctx)
	name := chi.URLParam(r, "name")
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ok, err := do(ctx, owner.ID, name, id)
	switch {
	case errors.Is(err, app.ErrNotFound):
		s.flashErr(w, r, "That backup no longer exists.")
	case err != nil:
		s.log.Warn("backup action", slog.String("app", name), slog.String("error", err.Error()))
		s.flashErr(w, r, databaseSentence(err))
	default:
		s.flashOK(w, r, ok)
	}
	http.Redirect(w, r, "/apps/"+name+"/backups", http.StatusSeeOther)
}

// ---------------------------------------------------------------- formatting

// sourceLabel is a source as the picker names it.
func sourceLabel(src app.Source) string {
	if b, err := app.BlueprintFor(src); err == nil {
		return b.Label
	}
	return string(src)
}

// dbHref is a view of the Database tab, with the open table and page.
func dbHref(name, view, schema, table string, page int) string {
	q := url.Values{}
	if view != "" && view != dbViewData {
		q.Set("view", view)
	}
	if table != "" {
		q.Set("schema", schema)
		q.Set("table", table)
	}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	href := "/apps/" + name + "/database"
	if len(q) > 0 {
		href += "?" + q.Encode()
	}
	return href
}

func qualified(schema, table string) string {
	if schema == "" || schema == "public" {
		return table
	}
	return schema + "." + table
}

// estimateLabel is the planner's row estimate, said as one.
func estimateLabel(n int64) string {
	if n < 0 {
		return "not analysed"
	}
	return "≈ " + strconv.FormatInt(n, 10) + " rows"
}

func durationLabel(d time.Duration) string {
	switch {
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + " ms"
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days"
}

func cacheHitLabel(f float64) string {
	if f < 0 {
		return "—"
	}
	return strconv.FormatFloat(f*100, 'f', 1, 64) + "%"
}

// backupWhat is one row's kind and how it started, in a phrase.
func backupWhat(b app.Backup) string {
	what := "Backup"
	if b.Kind == app.BackupKindRestore {
		what = "Restore"
		if b.BackupID == nil {
			what += " from a deleted backup"
		}
	}
	switch {
	case b.Trigger == app.BackupScheduled:
		what += ", daily"
	case b.CreatedBy != "":
		what += " by " + b.CreatedBy
	}
	return what
}

func backupClass(status string) string {
	switch status {
	case app.BackupSucceeded:
		return "status-ok"
	case app.BackupFailed:
		return "status-err"
	}
	return "status-info"
}
