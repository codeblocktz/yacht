package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// freePort is a local port nothing listens on.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// withHost is dsn pointed at addr instead.
func withHost(t *testing.T, dsn, addr string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = addr
	return u.String()
}

// A database that starts taking connections while Yacht waits is waited for.
func TestWaitForDatabaseWaitsOutOneStartingUp(t *testing.T) {
	dsn := testDSN(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	real := u.Host
	if u.Port() == "" {
		real = net.JoinHostPort(u.Hostname(), "5432")
	}
	addr := freePort(t)

	// Nothing on addr for the first seconds, then a proxy to the real one.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		go func() { <-ctx.Done(); _ = l.Close() }()
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer in.Close()
				out, err := net.Dial("tcp", real)
				if err != nil {
					return
				}
				defer out.Close()
				go func() { _, _ = io.Copy(out, in) }()
				_, _ = io.Copy(in, out)
			}()
		}
	}()

	start := time.Now()
	if err := WaitForDatabase(t.Context(), withHost(t, dsn, addr), 30*time.Second, quietLogger()); err != nil {
		t.Fatalf("WaitForDatabase: %v", err)
	}
	if took := time.Since(start); took < 3*time.Second {
		t.Errorf("returned after %s, before the database was there", took)
	}
}

// Nothing ever listening is given up on when the wait is over.
func TestWaitForDatabaseGivesUp(t *testing.T) {
	dsn := "postgresql://yacht:secret@" + freePort(t) + "/yacht?sslmode=disable"
	start := time.Now()
	err := WaitForDatabase(t.Context(), dsn, 3*time.Second, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "store: connect") {
		t.Fatalf("err = %v, want a connect error", err)
	}
	if took := time.Since(start); took < 3*time.Second || took > 15*time.Second {
		t.Errorf("gave up after %s, want just over the 3s wait", took)
	}
}

// A database that answers and refuses is not waited on: waiting would not
// change a wrong password.
func TestWaitForDatabaseFailsAtOnceOnARefusal(t *testing.T) {
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("no_such_role_here", "wrong")
	start := time.Now()
	if err := WaitForDatabase(t.Context(), u.String(), 30*time.Second, quietLogger()); err == nil {
		t.Fatal("a refused login was taken")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %s to give up on a refusal, want at once", took)
	}
}

// What a server answers decides whether to wait: a Postgres that is starting
// up, recovering or full says so, and is waited for; one that refused the
// login or the database has answered, and is not.
func TestWorthWaitingFor(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nothing listening", refused, true},
		{"starting up", &pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"a wrong password", &pgconn.PgError{Code: "28P01"}, false},
		{"no such database", &pgconn.PgError{Code: "3D000"}, false},
		{"a wrong password beside a refused address",
			fmt.Errorf("%w; %w", refused, &pgconn.PgError{Code: "28P01"}), false},
		{"something else", errors.New("tls: bad certificate"), false},
	} {
		if got := worthWaitingFor(c.err); got != c.want {
			t.Errorf("%s: worthWaitingFor = %v, want %v", c.name, got, c.want)
		}
	}
}
