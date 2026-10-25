package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// mysqlScratch makes a database of its own on a MySQL server and drops it
// afterwards. YACHT_TEST_MYSQL is host:port of a server whose root password
// is YACHT_TEST_MYSQL_PASSWORD — `docker run -p 43306:3306 -e
// MYSQL_ROOT_PASSWORD=pw mysql:8` is one. Skipped without it: the engine's
// own gates need only Postgres.
func mysqlScratch(t *testing.T) Target {
	t.Helper()
	addr := os.Getenv("YACHT_TEST_MYSQL")
	if addr == "" {
		t.Skip("set YACHT_TEST_MYSQL to run the data browser against MySQL")
	}
	c := mysql.NewConfig()
	c.Net, c.Addr, c.User, c.Passwd = "tcp", addr, "root", os.Getenv("YACHT_TEST_MYSQL_PASSWORD")
	c.MultiStatements = false
	connector, err := mysql.NewConnector(c)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(connector)
	t.Cleanup(func() { admin.Close() })
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "yacht_browse_" + hex.EncodeToString(b)
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE DATABASE " + name,
		"CREATE TABLE " + name + ".users (id INT PRIMARY KEY, email VARCHAR(200) NOT NULL, note TEXT)",
		"CREATE TABLE " + name + ".notes (body TEXT)",
		"CREATE VIEW " + name + ".recent AS SELECT id, email FROM " + name + ".users WHERE id > 100",
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + name) })
	for i := 120; i >= 1; i-- {
		var note any
		if i%3 != 0 {
			note = strings.Repeat("é", i)
		}
		if _, err := admin.ExecContext(ctx, "INSERT INTO "+name+".users VALUES (?, ?, ?)",
			i, "user"+strconv.Itoa(i)+"@example.com", note); err != nil {
			t.Fatal(err)
		}
	}
	return Target{Addr: addr, User: "root", Password: c.Passwd, Database: name}
}

func TestMySQLBrowsesAndPagesInKeyOrder(t *testing.T) {
	target := mysqlScratch(t)
	b := MySQL(target, Limits{})
	ctx := context.Background()
	tables, err := b.Tables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, tb := range tables {
		if tb.Schema == target.Database {
			kinds[tb.Name] = tb.Kind
		}
	}
	if kinds["users"] != "table" || kinds["recent"] != "view" {
		t.Errorf("tables = %v", kinds)
	}
	if tables[0].Schema != target.Database {
		t.Errorf("the app's own database is not listed first: %+v", tables[0])
	}
	page, err := b.Rows(ctx, target.Database, "users", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != PageSize || !page.More || page.Rows[0][0].Text != "1" || !page.Rows[2][2].Null {
		t.Errorf("first page: %d rows, more=%v, first=%q, third note=%+v",
			len(page.Rows), page.More, page.Rows[0][0].Text, page.Rows[2][2])
	}
	last, err := b.Rows(ctx, target.Database, "users", 2)
	if err != nil || len(last.Rows) != 20 || last.More {
		t.Errorf("last page = %d rows, more=%v, %v", len(last.Rows), last.More, err)
	}
	for _, bad := range []string{"users`; DROP TABLE users; --", "nope"} {
		if _, err := b.Rows(ctx, target.Database, bad, 0); err != ErrNoSuchTable {
			t.Errorf("Rows(%q) = %v", bad, err)
		}
	}
	if _, err := b.Rows(ctx, "mysql", "user", 0); err != ErrNoSuchTable {
		t.Errorf("a system table is browsable: %v", err)
	}
}

func TestMySQLQueriesReadAndNeverWrite(t *testing.T) {
	target := mysqlScratch(t)
	b := MySQL(target, Limits{})
	ctx := context.Background()
	for _, stmt := range []string{
		"INSERT INTO users (id, email) VALUES (999, 'x')",
		"UPDATE users SET email = 'x'",
		"DELETE FROM users",
		"DROP TABLE notes",
		"CREATE TABLE evil (i INT)",
		"TRUNCATE TABLE users",
		"SELECT 1; DELETE FROM users",
		"SET SESSION TRANSACTION READ WRITE; DELETE FROM users",
	} {
		if _, err := b.Query(ctx, stmt); err == nil {
			t.Errorf("%q succeeded; the browser must refuse it", stmt)
		}
	}
	res, err := b.Query(ctx, "SELECT COUNT(*) FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0].Text != "120" {
		t.Errorf("users holds %s rows after the attempts", res.Rows[0][0].Text)
	}
	if _, err := b.Query(ctx, "SELECT 1 FROM notes"); err != nil {
		t.Errorf("notes is gone: %v", err)
	}
	if _, err := b.Query(ctx, "SELECT 1 FROM evil"); err == nil {
		t.Error("a table was created")
	}
	if res, err := b.Query(ctx, "SHOW TABLES"); err != nil || len(res.Rows) < 2 {
		t.Errorf("SHOW TABLES = %+v, %v", res, err)
	}
}

func TestMySQLStopsLongAndLargeResults(t *testing.T) {
	target := mysqlScratch(t)
	ctx := context.Background()

	b := MySQL(target, Limits{Statement: 500 * time.Millisecond})
	start := time.Now()
	if _, err := b.Query(ctx, "SELECT SLEEP(10)"); err == nil && time.Since(start) > 5*time.Second {
		t.Error("a sleeping SELECT ran to the end")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v for a 500ms limit", took)
	}
	start = time.Now()
	if _, err := b.Query(ctx, "DO SLEEP(10)"); err == nil {
		t.Error("a statement that outlived the deadline was reported as fine")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("DO SLEEP took %v; it should be killed at the deadline", took)
	}

	b = MySQL(target, Limits{Rows: 10})
	res, err := b.Query(ctx, "SELECT a.id FROM users a, users b, users c")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 10 || !res.Truncated {
		t.Errorf("rows = %d truncated=%v", len(res.Rows), res.Truncated)
	}

	stats, err := MySQL(target, Limits{}).Stats(ctx)
	if err != nil || stats.Version == "" || stats.MaxConns <= 0 || stats.Connections < 1 || stats.Tables < 3 {
		t.Errorf("stats = %+v, %v", stats, err)
	}
	cfg, err := MySQL(target, Limits{}).Config(ctx)
	if err != nil || len(cfg.Settings) == 0 || cfg.Settings[0].Name != "max_connections" {
		t.Errorf("config = %+v, %v", cfg, err)
	}
}
