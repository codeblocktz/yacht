package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// scratch makes a database of its own on the test server, filled with what
// the browser is asked about, and drops it afterwards. A database rather than
// a schema in the shared one, because the browser is pointed at a whole
// database and lists everything in it.
func scratch(t *testing.T) Target {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run the data browser against Postgres")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "yacht_browse_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to scratch: %v", err)
	}
	defer db.Close(ctx)
	for _, stmt := range []string{
		`CREATE TABLE users (id int PRIMARY KEY, email text NOT NULL, note text, data jsonb, seen timestamptz)`,
		`INSERT INTO users SELECT g, 'user' || g || '@example.com',
		     CASE WHEN g % 3 = 0 THEN NULL ELSE repeat('é', g) END,
		     jsonb_build_object('n', g), timestamptz '2026-01-01 00:00:00+00' + g * interval '1 hour'
		 FROM generate_series(120, 1, -1) g`,
		`CREATE SCHEMA billing`,
		`CREATE TABLE billing.invoices (number text, total numeric(10,2))`,
		`INSERT INTO billing.invoices VALUES ('A-1', 12.50), ('A-2', NULL)`,
		`CREATE VIEW recent AS SELECT id, email FROM users WHERE id > 100`,
		`ANALYZE`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	pw, _ := u.User.Password()
	return Target{
		Addr: net.JoinHostPort(u.Hostname(), portOr(u.Port(), "5432")),
		User: u.User.Username(), Password: pw, Database: name,
	}
}

func portOr(p, d string) string {
	if p == "" {
		return d
	}
	return p
}

func TestTheBrowserListsTablesAndViewsWithEstimates(t *testing.T) {
	b := Postgres(scratch(t), Limits{})
	tables, err := b.Tables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Table{}
	for _, tb := range tables {
		got[tb.Schema+"."+tb.Name] = tb
	}
	if u := got["public.users"]; u.Kind != "table" || u.Rows != 120 || u.Bytes <= 0 {
		t.Errorf("users = %+v, want a table of 120 estimated rows with a size", u)
	}
	if v := got["public.recent"]; v.Kind != "view" {
		t.Errorf("recent = %+v, want a view", v)
	}
	if _, ok := got["billing.invoices"]; !ok {
		t.Errorf("tables in other schemas are listed too: %v", tables)
	}
	// public first, because it is where an app's tables almost always are.
	if tables[0].Schema != "public" {
		t.Errorf("first table is %s.%s, want public first", tables[0].Schema, tables[0].Name)
	}
}

func TestATableIsPagedInPrimaryKeyOrder(t *testing.T) {
	b := Postgres(scratch(t), Limits{})
	ctx := context.Background()

	first, err := b.Rows(ctx, "public", "users", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != PageSize || !first.More {
		t.Fatalf("first page has %d rows, more=%v; want %d and more", len(first.Rows), first.More, PageSize)
	}
	if strings.Join(first.Columns, ",") != "id,email,note,data,seen" {
		t.Errorf("columns = %v", first.Columns)
	}
	// Inserted in descending order, read in key order.
	if first.Rows[0][0].Text != "1" || first.Rows[49][0].Text != "50" {
		t.Errorf("first page runs %s..%s, want 1..50", first.Rows[0][0].Text, first.Rows[49][0].Text)
	}
	// The server's own rendering of each type, and NULL distinct from "".
	if first.Rows[2][2] != (Value{Null: true}) {
		t.Errorf("a NULL reads %+v", first.Rows[2][2])
	}
	if first.Rows[0][3].Text != `{"n": 1}` {
		t.Errorf("jsonb reads %q", first.Rows[0][3].Text)
	}
	// In the server's time zone, whatever it is, as psql would show it.
	if !strings.HasPrefix(first.Rows[0][4].Text, "2026-01-01 ") {
		t.Errorf("timestamptz reads %q", first.Rows[0][4].Text)
	}

	last, err := b.Rows(ctx, "public", "users", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Rows) != 20 || last.More || last.Rows[0][0].Text != "101" {
		t.Errorf("last page = %d rows from %q, more=%v; want 20 from 101 and no more",
			len(last.Rows), last.Rows[0][0].Text, last.More)
	}
}

func TestATableNameIsCheckedBeforeItIsQuoted(t *testing.T) {
	b := Postgres(scratch(t), Limits{})
	ctx := context.Background()
	for _, name := range []string{
		"users; DROP TABLE users",
		`users" WHERE false; --`,
		"pg_authid",
		"nope",
	} {
		if _, err := b.Rows(ctx, "public", name, 0); err != ErrNoSuchTable {
			t.Errorf("Rows(%q) = %v, want ErrNoSuchTable", name, err)
		}
	}
	if _, err := b.Rows(ctx, "pg_catalog", "pg_authid", 0); err != ErrNoSuchTable {
		t.Errorf("a catalog table is not browsable: %v", err)
	}
	// And the table is still there.
	if res, err := b.Rows(ctx, "public", "users", 0); err != nil || len(res.Rows) == 0 {
		t.Fatalf("users after the attempts: %v", err)
	}
}

func TestAQueryReadsAndNeverWrites(t *testing.T) {
	target := scratch(t)
	b := Postgres(target, Limits{})
	ctx := context.Background()

	res, err := b.Query(ctx, "SELECT number, total FROM billing.invoices ORDER BY number")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0][1].Text != "12.50" || !res.Rows[1][1].Null {
		t.Errorf("rows = %+v", res.Rows)
	}

	for _, sql := range []string{
		"INSERT INTO users (id, email) VALUES (999, 'x')",
		"UPDATE users SET email = 'x'",
		"DELETE FROM users",
		"DROP TABLE users",
		"CREATE TABLE evil (i int)",
		"WITH gone AS (DELETE FROM users RETURNING *) SELECT count(*) FROM gone",
		// Each of these tries to turn read-only off from inside: the
		// snapshot taken before the query is what makes Postgres refuse.
		"SET TRANSACTION READ WRITE",
		"SELECT set_config('transaction_read_only', 'off', true)",
		// Two statements, the second of which would be a write if the
		// first had worked, are refused before either runs.
		"SET TRANSACTION READ WRITE; DELETE FROM users",
		"COMMIT; DELETE FROM users",
	} {
		if _, err := b.Query(ctx, sql); err == nil {
			t.Errorf("%q succeeded; the browser must refuse it", sql)
		}
	}

	// Nothing above landed.
	check, err := b.Query(ctx, "SELECT count(*) FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if check.Rows[0][0].Text != "120" {
		t.Errorf("users holds %s rows after the attempts, want 120", check.Rows[0][0].Text)
	}
	if _, err := b.Query(ctx, "SELECT 1 FROM evil"); err == nil {
		t.Error("a table was created")
	}

	// A statement that is not a SELECT but only reads is answered with its
	// command, and one with columns with its rows.
	if res, err := b.Query(ctx, "SHOW server_version"); err != nil || len(res.Rows) != 1 {
		t.Errorf("SHOW = %+v, %v", res, err)
	}
	if res, err := b.Query(ctx, "EXPLAIN SELECT * FROM users"); err != nil || len(res.Rows) == 0 {
		t.Errorf("EXPLAIN = %+v, %v", res, err)
	}
}

func TestAQueryIsCancelledOnTheServerAtItsTimeout(t *testing.T) {
	target := scratch(t)
	b := Postgres(target, Limits{Statement: 300 * time.Millisecond})
	start := time.Now()
	_, err := b.Query(context.Background(), "SELECT pg_sleep(10)")
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want the timeout explained", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v for a 300ms timeout", took)
	}
	// Raising the timeout from inside the statement does not lift it from
	// the statement already running.
	_, err = b.Query(context.Background(),
		"SELECT set_config('statement_timeout', '0', true), pg_sleep(10)")
	if err == nil {
		t.Error("a statement lifted its own timeout")
	}
}

func TestAResultIsCappedInRowsAndBytes(t *testing.T) {
	target := scratch(t)
	ctx := context.Background()

	b := Postgres(target, Limits{Rows: 10})
	res, err := b.Query(ctx, "SELECT g FROM generate_series(1, 10000000) g")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 10 || !res.Truncated {
		t.Errorf("rows = %d truncated=%v, want 10 and truncated", len(res.Rows), res.Truncated)
	}

	b = Postgres(target, Limits{Cell: 8, Bytes: 40})
	res, err = b.Query(ctx, "SELECT note FROM users WHERE note IS NOT NULL ORDER BY id DESC")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Rows) > 6 {
		t.Errorf("kept %d rows, truncated=%v; the byte budget should stop it", len(res.Rows), res.Truncated)
	}
	first := res.Rows[0][0]
	// é is two bytes: eight bytes is four whole characters, never half of one.
	if !first.Cut || first.Text != "éééé" {
		t.Errorf("a long value reads %q cut=%v, want four whole characters, cut", first.Text, first.Cut)
	}
}

func TestStatsAndConfigDescribeTheDatabase(t *testing.T) {
	b := Postgres(scratch(t), Limits{})
	ctx := context.Background()
	s, err := b.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version == "" || s.SizeBytes <= 0 || s.Connections < 1 || s.MaxConns <= 0 || s.Tables < 3 {
		t.Errorf("stats = %+v", s)
	}
	if len(s.Largest) == 0 || s.Largest[0].Name != "users" {
		t.Errorf("largest = %+v, want users first", s.Largest)
	}
	c, err := b.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Settings) == 0 || c.Settings[0].Name != "max_connections" {
		t.Errorf("settings = %+v", c.Settings)
	}
	if len(c.Extensions) == 0 || c.Extensions[0].Name != "plpgsql" {
		t.Errorf("extensions = %+v", c.Extensions)
	}
}

func TestAWrongPasswordIsSaidPlainly(t *testing.T) {
	target := scratch(t)
	target.Password = "wrong"
	target.User = "yacht_nobody"
	_, err := Postgres(target, Limits{}).Tables(context.Background())
	if err == nil {
		t.Fatal("signed in with the wrong credentials")
	}
}

func TestAnUnreachableDatabaseFailsQuickly(t *testing.T) {
	// A port nothing listens on, in the documentation range.
	b := Postgres(Target{Addr: "192.0.2.1:5432", User: "x", Database: "x"},
		Limits{Connect: 300 * time.Millisecond})
	start := time.Now()
	if _, err := b.Tables(context.Background()); err == nil {
		t.Fatal("connected to nothing")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v to give up", took)
	}
}
