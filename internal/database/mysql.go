package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQL browses a MySQL database.
//
// The same guards as Postgres, in MySQL's terms. The session is read-only
// before anything the request sends, so every transaction in it is, and a
// statement inside one cannot write a table. Statements go one at a time:
// the driver's multi-statement mode is off, so "SELECT 1; DELETE ..." is a
// syntax error rather than two statements. max_execution_time bounds a
// SELECT on the server; anything else that outlives the client's deadline is
// killed from a second connection, because MySQL, unlike Postgres, has no
// timeout for every kind of statement and carries on with a query whose
// client has gone.
func MySQL(t Target, l Limits) Browser { return &mysqlBrowser{target: t, limits: l.orDefault()} }

type mysqlBrowser struct {
	target Target
	limits Limits
}

func (m *mysqlBrowser) config() *mysql.Config {
	c := mysql.NewConfig()
	c.Net, c.Addr = "tcp", m.target.Addr
	c.User, c.Passwd, c.DBName = m.target.User, m.target.Password, m.target.Database
	c.Timeout = m.limits.Connect
	c.MultiStatements = false
	c.ParseTime = false
	// MySQL 8's default authentication sends the password in the clear to a
	// server without TLS only if this is allowed; the cluster's network is
	// the path, as it is for every app on it.
	c.AllowCleartextPasswords = false
	c.AllowNativePasswords = true
	return c
}

// session opens one connection, made read-only before anything else runs.
func (m *mysqlBrowser) session(ctx context.Context) (*sql.DB, *sql.Conn, int64, error) {
	connector, err := mysql.NewConnector(m.config())
	if err != nil {
		return nil, nil, 0, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	dctx, cancel := context.WithTimeout(ctx, m.limits.Connect)
	defer cancel()
	conn, err := db.Conn(dctx)
	if err != nil {
		db.Close()
		return nil, nil, 0, fmt.Errorf("database: connect: %w", mysqlFriendly(err))
	}
	ms := strconv.FormatInt(m.limits.Statement.Milliseconds(), 10)
	lock := strconv.Itoa(int(m.limits.Lock.Seconds() + 0.999))
	var id int64
	for _, stmt := range []string{
		"SET SESSION TRANSACTION READ ONLY",
		"SET SESSION max_execution_time = " + ms,
		"SET SESSION lock_wait_timeout = " + lock,
		"SET SESSION innodb_lock_wait_timeout = " + lock,
	} {
		if _, err := conn.ExecContext(dctx, stmt); err != nil {
			conn.Close()
			db.Close()
			return nil, nil, 0, fmt.Errorf("database: prepare session: %w", mysqlFriendly(err))
		}
	}
	if err := conn.QueryRowContext(dctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		conn.Close()
		db.Close()
		return nil, nil, 0, mysqlFriendly(err)
	}
	return db, conn, id, nil
}

// readOnly runs fn in a read-only transaction that is always rolled back,
// and kills whatever is still running on the server when fn gives up.
func (m *mysqlBrowser) readOnly(ctx context.Context, fn func(context.Context, *sql.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, m.limits.Connect+m.limits.Statement*3)
	defer cancel()
	db, conn, id, err := m.session(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "START TRANSACTION READ ONLY"); err != nil {
		return mysqlFriendly(err)
	}
	err = fn(ctx, conn)
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, errStopped)) {
		m.kill(id)
	} else {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = conn.ExecContext(rctx, "ROLLBACK")
		rcancel()
	}
	if errors.Is(err, errStopped) {
		return nil
	}
	return err
}

// errStopped is collect stopping at its limit: the statement is killed, and
// what was read is the result.
var errStopped = errors.New("database: stopped at the limit")

// kill ends a statement on the server from a second connection.
func (m *mysqlBrowser) kill(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), m.limits.Connect)
	defer cancel()
	connector, err := mysql.NewConnector(m.config())
	if err != nil {
		return
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	_, _ = db.ExecContext(ctx, "KILL QUERY "+strconv.FormatInt(id, 10))
}

// systemSchemas are MySQL's own, not listed and not browsable.
const systemSchemas = "'mysql', 'information_schema', 'performance_schema', 'sys'"

func (m *mysqlBrowser) Tables(ctx context.Context) ([]Table, error) {
	var out []Table
	err := m.readOnly(ctx, func(ctx context.Context, conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `
			SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE,
			       COALESCE(TABLE_ROWS, -1), COALESCE(DATA_LENGTH + INDEX_LENGTH, 0)
			FROM information_schema.TABLES
			WHERE TABLE_SCHEMA NOT IN (`+systemSchemas+`)
			ORDER BY TABLE_SCHEMA = DATABASE() DESC, TABLE_SCHEMA, TABLE_NAME
			LIMIT 1000`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Table
			var typ string
			if err := rows.Scan(&t.Schema, &t.Name, &typ, &t.Rows, &t.Bytes); err != nil {
				return err
			}
			t.Kind = "table"
			if typ == "VIEW" {
				t.Kind, t.Rows = "view", -1
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("database: list tables: %w", mysqlFriendly(err))
	}
	return out, nil
}

// quoteMySQL quotes an identifier the catalog has already confirmed.
func quoteMySQL(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func (m *mysqlBrowser) Rows(ctx context.Context, schema, table string, page int) (Result, error) {
	if page < 0 {
		page = 0
	}
	var res Result
	err := m.readOnly(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var n int
		if err := conn.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND TABLE_SCHEMA NOT IN (`+systemSchemas+`)`,
			schema, table).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNoSuchTable
		}
		rows, err := conn.QueryContext(ctx, `
			SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY'
			ORDER BY ORDINAL_POSITION`, schema, table)
		if err != nil {
			return err
		}
		var keys []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, quoteMySQL(k))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		stmt := "SELECT * FROM " + quoteMySQL(schema) + "." + quoteMySQL(table)
		if len(keys) > 0 {
			stmt += " ORDER BY " + strings.Join(keys, ", ")
		}
		stmt += fmt.Sprintf(" LIMIT %d OFFSET %d", PageSize+1, page*PageSize)
		res, err = m.collect(ctx, conn, stmt, PageSize+1)
		if err != nil {
			return err
		}
		if len(res.Rows) > PageSize {
			res.Rows, res.More, res.Truncated = res.Rows[:PageSize], true, false
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNoSuchTable) {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("database: read %s.%s: %w", schema, table, mysqlFriendly(err))
	}
	return res, nil
}

func (m *mysqlBrowser) Query(ctx context.Context, stmt string) (Result, error) {
	stmt = strings.TrimSpace(stmt)
	if stmt == "" {
		return Result{}, errors.New("database: there is no query to run")
	}
	var res Result
	err := m.readOnly(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		res, err = m.collect(ctx, conn, stmt, m.limits.Rows)
		return err
	})
	if err != nil {
		return Result{}, mysqlFriendly(err)
	}
	return res, nil
}

// collect runs one statement through the text protocol — no arguments, so
// the driver sends it as written and every value comes back as the server
// prints it — and keeps at most max rows.
func (m *mysqlBrowser) collect(ctx context.Context, conn *sql.Conn, stmt string, max int) (Result, error) {
	qctx, cancel := context.WithTimeout(ctx, m.limits.Statement+time.Second)
	defer cancel()
	start := time.Now()
	rows, err := conn.QueryContext(qctx, stmt)
	if err != nil {
		return Result{}, err
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return Result{}, err
	}
	res := Result{Columns: cols}
	if len(cols) == 0 {
		rows.Close()
		res.Command = "OK"
		res.Elapsed = time.Since(start)
		return res, rows.Err()
	}
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	b := budget{limits: m.limits}
	for rows.Next() {
		if len(res.Rows) >= max {
			res.Truncated = true
			break
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return Result{}, err
		}
		row := make([]Value, len(cols))
		full := true
		for i, v := range raw {
			var ok bool
			if v == nil {
				row[i], ok = b.cell(nil)
			} else {
				row[i], ok = b.cell(append([]byte{}, v...))
			}
			full = full && ok
		}
		res.Rows = append(res.Rows, row)
		if !full {
			res.Truncated = true
			break
		}
	}
	res.Elapsed = time.Since(start)
	if res.Truncated {
		// Closing would read the rest of the result to throw it away; the
		// statement is killed instead, and the connection with it.
		cancel()
		rows.Close()
		return res, errStopped
	}
	rows.Close()
	return res, rows.Err()
}

// mysqlStatus reads one server status counter.
const mysqlStatus = `SELECT COALESCE((SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = ?), '0')`

func (m *mysqlBrowser) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := m.readOnly(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, `
			SELECT VERSION(), @@max_connections,
			       (SELECT COALESCE(SUM(DATA_LENGTH + INDEX_LENGTH), 0) FROM information_schema.TABLES
			        WHERE TABLE_SCHEMA NOT IN (`+systemSchemas+`)),
			       (SELECT COUNT(*) FROM information_schema.TABLES
			        WHERE TABLE_SCHEMA NOT IN (`+systemSchemas+`))`,
		).Scan(&s.Version, &s.MaxConns, &s.SizeBytes, &s.Tables); err != nil {
			return err
		}
		status := map[string]int64{}
		for _, name := range []string{"Threads_connected", "Uptime", "Com_commit",
			"Innodb_buffer_pool_read_requests", "Innodb_buffer_pool_reads"} {
			var v string
			if err := conn.QueryRowContext(ctx, mysqlStatus, name).Scan(&v); err != nil {
				return err
			}
			status[name], _ = strconv.ParseInt(v, 10, 64)
		}
		s.Connections = status["Threads_connected"]
		s.Uptime = time.Duration(status["Uptime"]) * time.Second
		s.Commits = status["Com_commit"]
		s.CacheHit = -1
		if req := status["Innodb_buffer_pool_read_requests"]; req > 0 {
			s.CacheHit = 1 - float64(status["Innodb_buffer_pool_reads"])/float64(req)
		}
		rows, err := conn.QueryContext(ctx, `
			SELECT TABLE_SCHEMA, TABLE_NAME, COALESCE(TABLE_ROWS, -1), COALESCE(DATA_LENGTH + INDEX_LENGTH, 0)
			FROM information_schema.TABLES
			WHERE TABLE_SCHEMA NOT IN (`+systemSchemas+`) AND TABLE_TYPE = 'BASE TABLE'
			ORDER BY DATA_LENGTH + INDEX_LENGTH DESC LIMIT 5`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t := Table{Kind: "table"}
			if err := rows.Scan(&t.Schema, &t.Name, &t.Rows, &t.Bytes); err != nil {
				return err
			}
			s.Largest = append(s.Largest, t)
		}
		return rows.Err()
	})
	if err != nil {
		return Stats{}, fmt.Errorf("database: read stats: %w", mysqlFriendly(err))
	}
	return s, nil
}

// mysqlSettings are the server variables worth reading when sizing a MySQL
// database or wondering why it is slow; the browser's own session variables
// are left out, for the reason they are for Postgres.
var mysqlSettings = []string{
	"max_connections", "innodb_buffer_pool_size", "innodb_redo_log_capacity",
	"innodb_flush_log_at_trx_commit", "max_allowed_packet", "long_query_time",
	"character_set_server", "collation_server", "time_zone", "sql_mode",
	"default_storage_engine",
}

func (m *mysqlBrowser) Config(ctx context.Context) (Config, error) {
	var c Config
	err := m.readOnly(ctx, func(ctx context.Context, conn *sql.Conn) error {
		for _, name := range mysqlSettings {
			var v sql.NullString
			err := conn.QueryRowContext(ctx,
				`SELECT VARIABLE_VALUE FROM performance_schema.global_variables WHERE VARIABLE_NAME = ?`,
				name).Scan(&v)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			c.Settings = append(c.Settings, Setting{Name: name, Value: v.String})
		}
		return nil
	})
	if err != nil {
		return Config{}, fmt.Errorf("database: read settings: %w", mysqlFriendly(err))
	}
	return c, nil
}

// mysqlFriendly turns MySQL's refusals into the sentence a person needs.
func mysqlFriendly(err error) error {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("the database did not answer in time")
		}
		return err
	}
	switch me.Number {
	case 1792: // ER_CANT_EXECUTE_IN_READ_ONLY_TRANSACTION
		return fmt.Errorf("the data browser only reads — %s", me.Message)
	case 3024, 1317: // max_execution_time exceeded; query interrupted
		return fmt.Errorf("the query ran for longer than the browser allows and was cancelled — %s", me.Message)
	case 1205: // lock wait timeout
		return fmt.Errorf("the table is locked by something else, so the read gave up — %s", me.Message)
	case 1045:
		return fmt.Errorf("the database refused the app's own credentials — %s", me.Message)
	case 1064:
		if strings.Contains(me.Message, ";") {
			return fmt.Errorf("one statement at a time — %s", me.Message)
		}
	}
	return errors.New(me.Message)
}
