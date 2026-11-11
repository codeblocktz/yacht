package database

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// hugeRowServer is just enough of a Postgres server to sign the browser in,
// run its transaction, and answer the query with a row whose header claims a
// gigabyte — the header alone, which is all a client needs to decide whether
// it will read the rest. Without a database, so the bound is checked
// wherever the tests run.
func hugeRowServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveHugeRow(conn)
		}
	}()
	return ln.Addr().String()
}

func serveHugeRow(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	be := pgproto3.NewBackend(conn, conn)
	if _, err := be.ReceiveStartupMessage(); err != nil {
		return
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if be.Flush() != nil {
		return
	}
	huge := false
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			// BEGIN, SELECT 1, ROLLBACK: the browser's own statements.
			tag, _, _ := strings.Cut(strings.TrimSpace(m.String), " ")
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte(strings.ToUpper(tag))})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		case *pgproto3.Parse:
			huge = strings.Contains(m.Query, "repeat")
			be.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Bind:
			be.Send(&pgproto3.BindComplete{})
		case *pgproto3.Describe:
			be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
				Name: []byte("repeat"), DataTypeOID: 25, TypeModifier: -1, DataTypeSize: -1,
			}}})
		case *pgproto3.Execute:
			if !huge {
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")})
				continue
			}
			if be.Flush() != nil {
				return
			}
			// A DataRow announcing a gigabyte, and the first few bytes of it.
			header := []byte{'D', 0, 0, 0, 0}
			binary.BigEndian.PutUint32(header[1:], 1<<30)
			_, _ = conn.Write(append(header, 0, 1, 0x3f, 0xff, 0xff, 0xfa, 'x', 'x', 'x'))
		case *pgproto3.Sync:
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		case *pgproto3.Terminate:
			return
		}
		if be.Flush() != nil {
			return
		}
	}
}

// A row arrives as one message, read whole before anything can cut it, so
// the bound has to be on the message: one SELECT repeat('x', 1000000000)
// from any member would otherwise be a gigabyte of the engine's memory, and a
// few at once would take the dashboard down for every team. The row is
// refused at its header, said plainly, and nothing near its size is
// allocated.
func TestARowLargerThanTheBrowserReadsIsRefusedUnread(t *testing.T) {
	b := Postgres(Target{Addr: hugeRowServer(t), User: "u", Database: "d"},
		Limits{Connect: 2 * time.Second, Statement: time.Second})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := b.Query(context.Background(), "SELECT repeat('x', 1073741818)")
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrRowTooLarge) {
		t.Fatalf("a gigabyte row gave %v, want ErrRowTooLarge", err)
	}
	if !strings.Contains(err.Error(), "left(") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("refusing the row allocated %d MB", grew>>20)
	}
}

// A server's message can quote a statement of any length; what a page shows
// of it is cut, on a whole character.
func TestAServersMessageIsCutToWhatAPageShows(t *testing.T) {
	long := strings.Repeat("é", 2000)
	got := friendly(&pgconn.PgError{Code: "XX000", Message: long}).Error()
	if len(got) > 510 || !strings.HasSuffix(got, "é…") {
		t.Errorf("a long message reads %d bytes, ending %q", len(got), got[len(got)-8:])
	}
}

// What to do about a row too large depends on where it was met: a query can
// ask for less, a page of a table cannot.
func TestARowTooLargeSaysWhatToDoWhereItWasMet(t *testing.T) {
	err := rowTooLarge(8<<20, 4<<20)
	if q := withAdvice(err, queryTooLargeAdvice); !errors.Is(q, ErrRowTooLarge) || !strings.Contains(q.Error(), "left(") {
		t.Errorf("a query's refusal reads %q", q)
	}
	if p := withAdvice(err, pageTooLargeAdvice); strings.Contains(p.Error(), "left(") || !strings.Contains(p.Error(), "query") {
		t.Errorf("a page's refusal reads %q", p)
	}
	if other := errors.New("x"); withAdvice(other, pageTooLargeAdvice) != other {
		t.Error("advice was added to another error")
	}
}

// No one message is large, but there is no end to them: a server that
// answers sign-in with session parameters forever is cut off past
// Limits.Wire on the connection, and said so.
func TestAPostgresConnectionEndsPastItsByteBudget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				be := pgproto3.NewBackend(conn, conn)
				if _, err := be.ReceiveStartupMessage(); err != nil {
					return
				}
				be.Send(&pgproto3.AuthenticationOk{})
				for i := 0; ; i++ {
					be.Send(&pgproto3.ParameterStatus{Name: "p" + strconv.Itoa(i), Value: strings.Repeat("v", 500)})
					if be.Flush() != nil {
						return
					}
				}
			}()
		}
	}()

	b := Postgres(Target{Addr: ln.Addr().String(), User: "u", Database: "d"},
		Limits{Connect: 5 * time.Second, Statement: time.Second, Wire: 256 << 10})
	_, err = b.Query(context.Background(), "SELECT 1")
	if !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("an endless sign-in gave %v, want ErrResultTooLarge", err)
	}
}

// A message limit at or past MySQL's largest packet would let a row that
// spans packets through; it is kept below.
func TestTheMessageLimitStaysBelowTheLargestPacket(t *testing.T) {
	if got := (Limits{Message: 1 << 30}).orDefault().Message; got >= 0xffffff {
		t.Errorf("Message = %d, want below 0xffffff", got)
	}
}
