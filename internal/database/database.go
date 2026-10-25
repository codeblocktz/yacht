// Package database reads a customer's database for the dashboard: the tables
// it holds, a page of a table's rows, a read-only query, and how it is doing.
//
// Not Yacht's own database, which is package store. This one is somebody
// else's — a database app a team deployed — so everything here is written for
// a server the engine does not control: every connection is short-lived,
// every statement has a deadline on the server and on the client, every
// result is capped before it is held in memory, and nothing reached from here
// can write.
//
// The engine dials the database's Service from its own process with the
// app's own credentials. It is the one path into a team's namespace that
// starts outside the team, and it is admitted by the isolation policy the way
// the ingress controller and the waker are: the engine runs in a platform
// namespace, which every team's policy lets in (orchestrator/k8s/network.go).
// What keeps it to the team's own database is above this package — the app is
// looked up by the signed-in team, and its address and password are that
// app's.
package database

import (
	"context"
	"errors"
	"time"
)

// ErrNoBrowser means the engine has no data browser for this database: Redis
// and MongoDB have no tables to list or SQL to run, and are reached through
// Connect instead.
var ErrNoBrowser = errors.New("database: no data browser for this engine")

// ErrNoSuchTable is a table that is not in the database, or not one the
// browser lists. Every table name a request carries is checked against the
// catalog before it is quoted into a statement, so a name that is not there
// is refused rather than tried.
var ErrNoSuchTable = errors.New("database: no such table")

// Target is where a database is and how to sign in to it.
type Target struct {
	// Addr is host:port, dialable from the engine's process: the Service's
	// cluster address.
	Addr     string
	User     string
	Password string
	Database string
}

// Limits bound everything the browser does to somebody else's server. The
// zero value is DefaultLimits; tests shorten them.
type Limits struct {
	// Connect bounds dialling and signing in. A database that is starting, or
	// whose Service has no ready pod, should fail the page quickly rather than
	// hold it.
	Connect time.Duration

	// Statement is set on the server as statement_timeout, so a query that
	// runs too long is cancelled where it runs rather than only abandoned by
	// the engine and left to finish on its own.
	Statement time.Duration

	// Lock is lock_timeout. A read that waits on somebody's migration holding
	// a lock is a read that should say so, not one that queues behind it.
	Lock time.Duration

	// Rows is the most rows a query returns; a page of a table is PageSize.
	Rows int

	// Cell is the most bytes of one value kept; the rest is cut and marked.
	Cell int

	// Bytes is the most bytes of values a whole result keeps, whatever the
	// number of rows, so a wide table of large values cannot hold megabytes
	// of somebody else's data in the engine's memory per request.
	Bytes int
}

// DefaultLimits are the browser's bounds.
var DefaultLimits = Limits{
	Connect:   5 * time.Second,
	Statement: 5 * time.Second,
	Lock:      2 * time.Second,
	Rows:      200,
	Cell:      1000,
	Bytes:     1 << 20,
}

// PageSize is how many rows a page of a table shows.
const PageSize = 50

func (l Limits) orDefault() Limits {
	d := DefaultLimits
	if l.Connect > 0 {
		d.Connect = l.Connect
	}
	if l.Statement > 0 {
		d.Statement = l.Statement
	}
	if l.Lock > 0 {
		d.Lock = l.Lock
	}
	if l.Rows > 0 {
		d.Rows = l.Rows
	}
	if l.Cell > 0 {
		d.Cell = l.Cell
	}
	if l.Bytes > 0 {
		d.Bytes = l.Bytes
	}
	return d
}

// Browser is one engine's reading of one database.
type Browser interface {
	// Tables lists the tables and views, with the planner's estimate of how
	// many rows each holds rather than a count: counting a large table is a
	// full scan of somebody's production data to draw a sidebar.
	Tables(ctx context.Context) ([]Table, error)

	// Rows reads one page of a table, in primary-key order where it has one.
	Rows(ctx context.Context, schema, table string, page int) (Result, error)

	// Query runs one statement read-only and returns what it produced.
	Query(ctx context.Context, sql string) (Result, error)

	// Stats is how the database is doing: its size, its connections, its
	// version.
	Stats(ctx context.Context) (Stats, error)

	// Config is the server's settings that matter for sizing and tuning, and
	// what it has installed.
	Config(ctx context.Context) (Config, error)
}

// Table is one table or view.
type Table struct {
	Schema string
	Name   string
	// Kind is "table", "view", "materialized view", "partitioned table" or
	// "foreign table".
	Kind string
	// Rows is an estimate; negative when the server has none yet.
	Rows  int64
	Bytes int64
}

// Result is what a statement produced, as text — the server's own rendering
// of each value, which is what psql shows and what somebody comparing the two
// expects.
type Result struct {
	Columns []string
	Rows    [][]Value

	// Command is the server's tag for a statement that returns no rows, such
	// as "SET" or "EXPLAIN".
	Command string

	// Truncated says there was more than was kept: more rows than the limit,
	// or more bytes.
	Truncated bool

	// More says a page of a table has a next page.
	More bool

	// Elapsed is how long the statement took, as the engine saw it.
	Elapsed time.Duration
}

// Value is one cell. Null is distinct from the empty string, as in SQL.
type Value struct {
	Text string
	Null bool
	// Cut says Text is the start of a longer value.
	Cut bool
}

// Stats is a database's vital signs.
type Stats struct {
	Version     string
	SizeBytes   int64
	Connections int64
	MaxConns    int64
	Uptime      time.Duration
	Tables      int64
	// CacheHit is the share of block reads served from memory, 0–1, or
	// negative before there has been any read to measure.
	CacheHit float64
	Commits  int64
	Largest  []Table
}

// Config is the settings a database runs with.
type Config struct {
	Settings   []Setting
	Extensions []Extension
}

// Setting is one server setting.
type Setting struct {
	Name  string
	Value string
	Unit  string
	About string
}

// Extension is one installed extension.
type Extension struct {
	Name    string
	Version string
}

// keep cuts a value to the per-cell limit and counts it against the
// result's, reporting false once the result is full.
type budget struct {
	limits Limits
	used   int
}

func (b *budget) cell(raw []byte) (Value, bool) {
	if raw == nil {
		return Value{Null: true}, b.used <= b.limits.Bytes
	}
	v := Value{Text: string(raw)}
	if len(raw) > b.limits.Cell {
		v.Text, v.Cut = cutUTF8(raw, b.limits.Cell), true
	}
	b.used += len(v.Text)
	return v, b.used <= b.limits.Bytes
}

// cutUTF8 returns at most n bytes of b, ending on a whole character so a cut
// value still renders.
func cutUTF8(b []byte, n int) string {
	if n >= len(b) {
		return string(b)
	}
	for n > 0 && b[n]&0xC0 == 0x80 {
		n--
	}
	return string(b[:n])
}
