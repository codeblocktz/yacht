package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

// Postgres browses a Postgres database.
func Postgres(t Target, l Limits) Browser { return &postgres{target: t, limits: l.orDefault()} }

type postgres struct {
	target Target
	limits Limits
}

// connect opens one connection for one request.
//
// Not pooled. A pool would hold connections to every database anybody has
// looked at, counted against each one's max_connections for as long as the
// engine runs; a connection per request is opened, used and closed in the
// time the page takes, and is gone when it is.
//
// Every guard that can live on the server does, set at startup so nothing the
// request sends can come before it: transactions are read-only by default,
// and statement_timeout and lock_timeout bound each statement where it runs.
// The client's deadline is the second line, and cancelling it sends the
// server a cancel request rather than only dropping the socket — a backend
// whose client has gone carries on with a query until it next writes to it.
func (p *postgres) connect(ctx context.Context) (*pgx.Conn, error) {
	host, port, err := net.SplitHostPort(p.target.Addr)
	if err != nil {
		return nil, fmt.Errorf("database: address %q: %w", p.target.Addr, err)
	}
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("database: port %q: %w", port, err)
	}

	cfg, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port = host, uint16(portNum)
	cfg.User, cfg.Password, cfg.Database = p.target.User, p.target.Password, p.target.Database
	cfg.ConnectTimeout = p.limits.Connect
	cfg.RuntimeParams = map[string]string{
		// So the engine's sessions are recognisable in pg_stat_activity, to
		// whoever is wondering what is connected to their database.
		"application_name":                    "yacht-dashboard",
		"default_transaction_read_only":       "on",
		"statement_timeout":                   strconv.FormatInt(p.limits.Statement.Milliseconds(), 10),
		"lock_timeout":                        strconv.FormatInt(p.limits.Lock.Milliseconds(), 10),
		"idle_in_transaction_session_timeout": strconv.FormatInt((p.limits.Statement + p.limits.Connect).Milliseconds(), 10),
	}
	// The extended protocol for everything, never the simple one: a statement
	// is parsed on its own, and the server refuses a string holding two. A
	// query box that accepted "SELECT 1; SET TRANSACTION READ WRITE; ..." would
	// be a read-only guard that lasted one statement.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{
			Conn: c, CancelRequestDelay: 0, DeadlineDelay: time.Second,
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, p.limits.Connect)
	defer cancel()
	conn, err := pgx.ConnectConfig(dialCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: connect: %w", friendly(err))
	}
	return conn, nil
}

// close ends a connection without waiting on a server that has stopped
// answering.
func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

// readOnly runs fn inside a read-only transaction that is always rolled back.
//
// Three guards, each enough on its own for a different mistake. BEGIN READ
// ONLY refuses writes. The first statement in it takes the transaction's
// snapshot, after which Postgres refuses to turn read-only off for the rest
// of it — SET TRANSACTION READ WRITE and set_config both. And it ends in
// ROLLBACK, never COMMIT, so a write that got past both would still not land.
func (p *postgres) readOnly(ctx context.Context, fn func(context.Context, *pgx.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, p.limits.Connect+p.limits.Statement*3)
	defer cancel()
	conn, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer closeConn(conn)

	if _, err := conn.Exec(ctx, "BEGIN TRANSACTION READ ONLY"); err != nil {
		return fmt.Errorf("database: begin: %w", friendly(err))
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
		defer rcancel()
		_, _ = conn.Exec(rctx, "ROLLBACK")
	}()
	if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("database: begin: %w", friendly(err))
	}
	return fn(ctx, conn)
}

// tablesSQL lists tables from information_schema — what the signed-in role
// may see, which for the database's own superuser is everything — with
// pg_class's estimate of each one's rows and size.
const tablesSQL = `
SELECT t.table_schema, t.table_name, t.table_type,
       COALESCE(c.reltuples, -1)::bigint,
       COALESCE(pg_total_relation_size(c.oid), 0)::bigint,
       COALESCE(c.relkind::text, '')
FROM information_schema.tables t
LEFT JOIN pg_namespace n ON n.nspname = t.table_schema
LEFT JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = t.table_name
WHERE t.table_schema NOT IN ('pg_catalog', 'information_schema')
  AND t.table_schema NOT LIKE 'pg\_toast%'
  AND t.table_schema NOT LIKE 'pg\_temp%'
ORDER BY t.table_schema = 'public' DESC, t.table_schema, t.table_name
LIMIT 1000`

// matviewsSQL adds materialized views, which information_schema leaves out.
const matviewsSQL = `
SELECT schemaname, matviewname, GREATEST(c.reltuples, -1)::bigint,
       pg_total_relation_size(c.oid)::bigint
FROM pg_matviews m
JOIN pg_namespace n ON n.nspname = m.schemaname
JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = m.matviewname
ORDER BY 1, 2
LIMIT 200`

func (p *postgres) Tables(ctx context.Context) ([]Table, error) {
	var out []Table
	err := p.readOnly(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, tablesSQL)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t Table
			var typ, relkind string
			if err := rows.Scan(&t.Schema, &t.Name, &typ, &t.Rows, &t.Bytes, &relkind); err != nil {
				rows.Close()
				return err
			}
			t.Kind = tableKind(typ, relkind)
			out = append(out, t)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		mv, err := conn.Query(ctx, matviewsSQL)
		if err != nil {
			return err
		}
		for mv.Next() {
			t := Table{Kind: "materialized view"}
			if err := mv.Scan(&t.Schema, &t.Name, &t.Rows, &t.Bytes); err != nil {
				mv.Close()
				return err
			}
			out = append(out, t)
		}
		return mv.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("database: list tables: %w", friendly(err))
	}
	return out, nil
}

func tableKind(infoType, relkind string) string {
	switch {
	case relkind == "p":
		return "partitioned table"
	case infoType == "VIEW":
		return "view"
	case infoType == "FOREIGN":
		return "foreign table"
	}
	return "table"
}

// Rows reads one page of a table.
//
// The name is quoted only after the catalog has confirmed it, so what reaches
// the statement is a table that exists, spelled the way Postgres spells it —
// and one Tables lists: the system catalogs are not browsable by asking for
// them by name.
// Ordered by the primary key where there is one, because without an order a
// page is whatever the server returns first, and the next page may repeat it.
func (p *postgres) Rows(ctx context.Context, schema, table string, page int) (Result, error) {
	if page < 0 {
		page = 0
	}
	var res Result
	err := p.readOnly(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		var exists bool
		if err := conn.QueryRow(ctx, `
			SELECT $1 NOT IN ('pg_catalog', 'information_schema')
			   AND $1 NOT LIKE 'pg\_toast%' AND $1 NOT LIKE 'pg\_temp%'
			   AND EXISTS (
			    SELECT 1 FROM information_schema.tables
			    WHERE table_schema = $1 AND table_name = $2
			    UNION ALL
			    SELECT 1 FROM pg_matviews WHERE schemaname = $1 AND matviewname = $2)`,
			schema, table).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNoSuchTable
		}
		ident := pgx.Identifier{schema, table}.Sanitize()

		var keys []string
		rows, err := conn.Query(ctx, `
			SELECT a.attname
			FROM pg_index i
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
			WHERE i.indrelid = $1::regclass AND i.indisprimary
			ORDER BY array_position(i.indkey::int2[], a.attnum)`, ident)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, pgx.Identifier{k}.Sanitize())
		}
		if err := rows.Err(); err != nil {
			return err
		}

		sql := "SELECT * FROM " + ident
		if len(keys) > 0 {
			sql += " ORDER BY " + strings.Join(keys, ", ")
		}
		// One more than a page, to know whether there is a next one without
		// counting the table.
		sql += fmt.Sprintf(" LIMIT %d OFFSET %d", PageSize+1, page*PageSize)

		res, err = p.collect(ctx, conn, sql, PageSize+1)
		if err != nil {
			return err
		}
		if len(res.Rows) > PageSize {
			res.Rows, res.More = res.Rows[:PageSize], true
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNoSuchTable) {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("database: read %s.%s: %w", schema, table, friendly(err))
	}
	return res, nil
}

// Query runs one statement read-only.
func (p *postgres) Query(ctx context.Context, sql string) (Result, error) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return Result{}, errors.New("database: there is no query to run")
	}
	var res Result
	err := p.readOnly(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		var err error
		res, err = p.collect(ctx, conn, sql, p.limits.Rows)
		return err
	})
	if err != nil {
		return Result{}, friendly(err)
	}
	return res, nil
}

// collect runs one statement and keeps at most max rows of it, as text.
//
// Through the wire protocol directly rather than pgx's typed rows, asking for
// every column in text: the value is then exactly what the server prints for
// it, whatever its type — including types the engine has never heard of,
// which an extension can add to any database.
//
// Reading stops at the limit rather than draining the rest: the statement is
// cancelled on the server, which is the only way to stop a large result being
// sent in full to be thrown away.
func (p *postgres) collect(ctx context.Context, conn *pgx.Conn, sql string, max int) (Result, error) {
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()

	start := time.Now()
	rr := conn.PgConn().ExecParams(qctx, sql, nil, nil, nil, nil)
	var res Result
	for _, f := range rr.FieldDescriptions() {
		res.Columns = append(res.Columns, f.Name)
	}
	b := budget{limits: p.limits}
	stopped := false
	for rr.NextRow() {
		if len(res.Rows) >= max {
			res.Truncated, stopped = true, true
			break
		}
		raw := rr.Values()
		row := make([]Value, len(raw))
		full := true
		for i, v := range raw {
			var ok bool
			row[i], ok = b.cell(v)
			full = full && ok
		}
		res.Rows = append(res.Rows, row)
		if !full {
			res.Truncated, stopped = true, true
			break
		}
	}
	if stopped {
		cancel()
		_, _ = rr.Close()
		res.Elapsed = time.Since(start)
		return res, nil
	}
	tag, err := rr.Close()
	res.Elapsed = time.Since(start)
	if err != nil {
		return Result{}, err
	}
	if len(res.Columns) == 0 {
		res.Command = tag.String()
	}
	return res, nil
}

func (p *postgres) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := p.readOnly(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		var uptimeSeconds float64
		if err := conn.QueryRow(ctx, `
			SELECT current_setting('server_version'),
			       pg_database_size(current_database()),
			       (SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()),
			       current_setting('max_connections')::bigint,
			       EXTRACT(EPOCH FROM now() - pg_postmaster_start_time())::float8,
			       (SELECT count(*) FROM information_schema.tables
			        WHERE table_schema NOT IN ('pg_catalog', 'information_schema')),
			       COALESCE((SELECT blks_hit::float8 / NULLIF(blks_hit + blks_read, 0)
			                 FROM pg_stat_database WHERE datname = current_database()), -1),
			       COALESCE((SELECT xact_commit FROM pg_stat_database
			                 WHERE datname = current_database()), 0)`,
		).Scan(&s.Version, &s.SizeBytes, &s.Connections, &s.MaxConns,
			&uptimeSeconds, &s.Tables, &s.CacheHit, &s.Commits); err != nil {
			return err
		}
		s.Uptime = time.Duration(uptimeSeconds * float64(time.Second))

		rows, err := conn.Query(ctx, `
			SELECT n.nspname, c.relname, GREATEST(c.reltuples, -1)::bigint,
			       pg_total_relation_size(c.oid)::bigint
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r', 'p', 'm')
			  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
			  AND n.nspname NOT LIKE 'pg\_toast%'
			ORDER BY pg_total_relation_size(c.oid) DESC
			LIMIT 5`)
		if err != nil {
			return err
		}
		for rows.Next() {
			t := Table{Kind: "table"}
			if err := rows.Scan(&t.Schema, &t.Name, &t.Rows, &t.Bytes); err != nil {
				rows.Close()
				return err
			}
			s.Largest = append(s.Largest, t)
		}
		return rows.Err()
	})
	if err != nil {
		return Stats{}, fmt.Errorf("database: read stats: %w", friendly(err))
	}
	return s, nil
}

// configSettings are the settings worth reading when sizing a database or
// wondering why it is slow. The browser's own session settings —
// statement_timeout, lock_timeout — are left out: what this session shows for
// them is what the engine set, not what the database's apps run with.
var configSettings = []string{
	"max_connections", "shared_buffers", "effective_cache_size", "work_mem",
	"maintenance_work_mem", "wal_level", "max_wal_size", "checkpoint_timeout",
	"random_page_cost", "autovacuum", "timezone", "server_encoding",
	"default_text_search_config", "shared_preload_libraries", "password_encryption",
}

func (p *postgres) Config(ctx context.Context) (Config, error) {
	var c Config
	err := p.readOnly(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, `
			SELECT name, setting, COALESCE(unit, ''), short_desc
			FROM pg_settings WHERE name = ANY ($1)
			ORDER BY array_position($1, name)`, configSettings)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s Setting
			if err := rows.Scan(&s.Name, &s.Value, &s.Unit, &s.About); err != nil {
				rows.Close()
				return err
			}
			c.Settings = append(c.Settings, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		ext, err := conn.Query(ctx, `SELECT extname, extversion FROM pg_extension ORDER BY extname`)
		if err != nil {
			return err
		}
		for ext.Next() {
			var e Extension
			if err := ext.Scan(&e.Name, &e.Version); err != nil {
				ext.Close()
				return err
			}
			c.Extensions = append(c.Extensions, e)
		}
		return ext.Err()
	})
	if err != nil {
		return Config{}, fmt.Errorf("database: read settings: %w", friendly(err))
	}
	return c, nil
}

// friendly turns the server's refusals into the sentence a person needs.
// Postgres's own message is kept, because it names what was refused; the
// SQLSTATE decides which explanation goes in front of it.
func friendly(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("the database did not answer in time")
		}
		return err
	}
	switch pgErr.Code {
	case "25006": // read_only_sql_transaction
		return fmt.Errorf("the data browser only reads — %s", pgErr.Message)
	case "57014": // query_canceled, which is how statement_timeout ends a query
		return fmt.Errorf("the query ran for longer than the browser allows and was cancelled — %s", pgErr.Message)
	case "55P03": // lock_not_available
		return fmt.Errorf("the table is locked by something else, so the read gave up — %s", pgErr.Message)
	case "42601": // syntax_error; "cannot insert multiple commands" lands here
		if strings.Contains(pgErr.Message, "multiple commands") {
			return errors.New("one statement at a time — the browser runs a single statement")
		}
	case "28P01", "28000":
		return fmt.Errorf("the database refused the app's own credentials — %s", pgErr.Message)
	}
	return fmt.Errorf("%s", pgErr.Message)
}
