package database

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func packet(seq byte, payload []byte) []byte {
	n := len(payload)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, payload...)
}

// meteredPipe is a metered connection over a pipe, already past the
// greeting, with the server's end to write replies into.
func meteredPipe(t *testing.T, l Limits) (server net.Conn, conn *meteredConn, meter *wireMeter) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	meter = newWireMeter(l.orDefault())
	return server, &meteredConn{Conn: client, meter: meter, greeted: true}, meter
}

// command has the driver send one: the meter follows the reply from it.
func command(t *testing.T, server net.Conn, conn *meteredConn, cmd byte) {
	t.Helper()
	go func() { _, _ = io.Copy(io.Discard, server) }()
	if _, err := conn.Write(packet(0, []byte{cmd, 'x'})); err != nil {
		t.Fatal(err)
	}
}

// Packets within the limit pass through untouched, however the reads split
// them — a header can arrive a byte at a time. The first packet over it ends
// the connection with none of its header handed on, so the driver never sizes
// a buffer from it, and the meter says how large it was.
func TestAMySQLPacketTooLargeIsRefusedAtItsHeader(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	meter := newWireMeter(Limits{Message: 1 << 10}.orDefault())
	conn := &meteredConn{Conn: client, meter: meter, greeted: true}

	small := append(packet(0, bytes.Repeat([]byte("a"), 300)), packet(1, []byte("ok"))...)
	huge := []byte{0xff, 0xff, 0xff, 2, 'x', 'x'}
	go func() {
		for _, b := range small {
			if _, err := server.Write([]byte{b}); err != nil {
				return
			}
		}
		_, _ = server.Write(huge)
	}()

	got := make([]byte, len(small))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("packets within the limit: %v", err)
	}
	if !bytes.Equal(got, small) {
		t.Fatal("packets within the limit were changed on the way through")
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 || !errors.Is(err, ErrRowTooLarge) {
		t.Fatalf("a 16 MB packet read %d bytes, err %v; want none and ErrRowTooLarge", n, err)
	}
	if _, err := conn.Read(buf); !errors.Is(err, ErrRowTooLarge) {
		t.Errorf("a read after the refusal gave %v", err)
	}
	if err := meter.refusal(); !errors.Is(err, ErrRowTooLarge) {
		t.Errorf("the meter says %v, want ErrRowTooLarge", err)
	}
}

// The column count a result starts with is believed by the driver, which
// makes a slice that long before reading a column: 2^64-1 is a crash. A count
// above MySQL's 4096 is refused before the packet carrying it is complete,
// after a query, an execute, or a prepare.
func TestAColumnCountNoResultCanHaveIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd   byte
		reply []byte
	}{
		"eight-byte count":   {comQuery, []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
		"three-byte count":   {comQuery, []byte{0xfd, 0x00, 0x00, 0x01}},
		"two-byte count":     {comStmtExecute, []byte{0xfc, 0x01, 0x10}}, // 4097
		"prepare OK columns": {comStmtPrepare, []byte{0x00, 1, 0, 0, 0, 0xff, 0xff, 0, 0, 0, 0, 0}},
	} {
		t.Run(name, func(t *testing.T) {
			server, conn, meter := meteredPipe(t, Limits{})
			command(t, server, conn, tc.cmd)
			go func() { _, _ = server.Write(packet(1, tc.reply)) }()
			_, err := io.ReadFull(conn, make([]byte, 4+len(tc.reply)))
			if !errors.Is(err, ErrResultTooLarge) || !errors.Is(meter.refusal(), ErrResultTooLarge) {
				t.Fatalf("read %v, meter %v; want ErrResultTooLarge", err, meter.refusal())
			}
		})
	}
}

// A whole ordinary conversation passes untouched: a result of 4096 columns,
// their definitions and its EOF, rows — one whose first value is longer than
// 4096 bytes, so it starts as a large column count would — the EOF that ends
// them, and a second result after it.
func TestAnOrdinaryResultPassesTheMeter(t *testing.T) {
	server, conn, meter := meteredPipe(t, Limits{})
	command(t, server, conn, comQuery)
	long := append([]byte{0xfc, 0x00, 0x20}, bytes.Repeat([]byte("v"), 0x2000)...)
	var reply []byte
	seq := byte(1)
	add := func(p []byte) { reply = append(reply, packet(seq, p)...); seq++ }
	add([]byte{0xfc, 0x00, 0x10}) // 4096 columns
	for range mysqlMaxColumns {
		add([]byte{3, 'd', 'e', 'f', 0, 0, 0, 1, 'c', 0, 0x0c})
	}
	add([]byte{0xfe, 0, 0, 0x0a, 0}) // EOF, more results follow
	add(long)
	add([]byte{0xfb})                // a NULL
	add([]byte{0xfe, 0, 0, 0x0a, 0}) // EOF, more results
	add([]byte{0x01})                // a second result of one column
	add([]byte{3, 'd', 'e', 'f', 0, 0, 0, 1, 'c', 0, 0x0c})
	add([]byte{0xfe, 0, 0, 0x02, 0})
	add(long)
	add([]byte{0xfe, 0, 0, 0x02, 0})
	go func() { _, _ = server.Write(reply) }()

	got := make([]byte, len(reply))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("an ordinary result: %v (meter %v)", err, meter.refusal())
	}
	if !bytes.Equal(got, reply) {
		t.Fatal("an ordinary result was changed on the way through")
	}
}

// Many small packets add up: past Limits.Wire on one connection, it ends.
func TestAConnectionEndsPastItsByteBudget(t *testing.T) {
	server, conn, meter := meteredPipe(t, Limits{Wire: 64 << 10})
	go func() {
		row := packet(1, bytes.Repeat([]byte("r"), 1000))
		for {
			if _, err := server.Write(row); err != nil {
				return
			}
		}
	}()
	_, err := io.Copy(io.Discard, conn)
	if !errors.Is(err, ErrResultTooLarge) || !errors.Is(meter.refusal(), ErrResultTooLarge) {
		t.Errorf("an endless stream gave %v, want ErrResultTooLarge", err)
	}
}

// A MariaDB server's greeting carries the capability to leave a result's
// column definitions out, which would put the meter out of step with the
// driver; it is taken out. A MySQL server's greeting is left alone.
func TestTheGreetingNeverOffersToCacheColumnDefinitions(t *testing.T) {
	maria := fakeGreeting(false)
	stripMetadataCache(maria)
	if ext := binary.LittleEndian.Uint32(maria[extCapsOffset(maria):]); ext != 0 {
		t.Errorf("MariaDB's extended capabilities read %#x, want none", ext)
	}
	mysql := fakeGreeting(true)
	before := append([]byte(nil), mysql...)
	stripMetadataCache(mysql)
	if !bytes.Equal(before, mysql) {
		t.Error("a MySQL greeting was changed")
	}
}

func extCapsOffset(g []byte) int {
	return 1 + bytes.IndexByte(g[1:], 0) + 1 + 4 + 8 + 1 + 2 + 3 + 2 + 7
}

// fakeGreeting is a protocol 10 greeting: MySQL sets the lowest capability
// bit, MariaDB clears it and puts its extended capabilities after the
// reserved bytes.
func fakeGreeting(mysql bool) []byte {
	caps := uint32(0x00080000 | 0x8000 | 0x2000 | 0x0200) // plugin auth, secure conn, transactions, protocol 41
	if mysql {
		caps |= 1
	}
	g := []byte{10}
	g = append(g, "8.0.0-fake"...)
	g = append(g, 0)
	g = append(g, 1, 0, 0, 0)    // connection id
	g = append(g, "12345678"...) // scramble, first part
	g = append(g, 0)             // filler
	g = binary.LittleEndian.AppendUint16(g, uint16(caps))
	g = append(g, 0x21, 2, 0) // character set, status
	g = binary.LittleEndian.AppendUint16(g, uint16(caps>>16))
	g = append(g, 21)               // scramble length
	g = append(g, 0, 0, 0, 0, 0, 0) // reserved
	if mysql {
		g = append(g, 0, 0, 0, 0)
	} else {
		g = append(g, 0x10, 0, 0, 0) // MariaDB: cache metadata
	}
	g = append(g, "123456789012"...) // scramble, second part
	g = append(g, 0)
	g = append(g, "mysql_native_password"...)
	return append(g, 0)
}

// fakeMySQL is just enough of a MySQL server to sign the browser in and
// answer its statements as a test says, over the real driver. hostile says
// what to answer a statement with, or nil for an OK.
func fakeMySQL(t *testing.T, hostile func(cmd byte, query string) [][]byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); serveFakeMySQL(c, hostile) }()
		}
	}()
	return ln.Addr().String()
}

func serveFakeMySQL(c net.Conn, hostile func(byte, string) [][]byte) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	readPacket := func() ([]byte, error) {
		var h [4]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return nil, err
		}
		p := make([]byte, int(h[0])|int(h[1])<<8|int(h[2])<<16)
		_, err := io.ReadFull(c, p)
		return p, err
	}
	ok := []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
	if _, err := c.Write(packet(0, fakeGreeting(true))); err != nil {
		return
	}
	if _, err := readPacket(); err != nil {
		return
	}
	if _, err := c.Write(packet(2, ok)); err != nil {
		return
	}
	for {
		p, err := readPacket()
		if err != nil || len(p) == 0 || p[0] == 0x01 {
			return
		}
		if p[0] == 0x19 { // COM_STMT_CLOSE has no reply
			continue
		}
		replies := hostile(p[0], string(p[1:]))
		if replies == nil {
			replies = [][]byte{ok}
		}
		var out []byte
		for i, r := range replies {
			out = append(out, packet(byte(i+1), r)...)
		}
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// connectionID answers SELECT CONNECTION_ID() as a server does.
func connectionID() [][]byte {
	def := []byte{3, 'd', 'e', 'f', 0, 0, 0, 15}
	def = append(def, "CONNECTION_ID()"...)
	def = append(def, 0, 0x0c, 0x3f, 0, 21, 0, 0, 0, 0x08, 0x81, 0, 0, 0, 0)
	eof := []byte{0xfe, 0, 0, 0x02, 0}
	return [][]byte{{0x01}, def, eof, {0x01, '7'}, eof}
}

// The column count, over the real driver: a server that answers the
// browser's first statement with 2^64-1 columns, or a prepared statement
// with 65535, is refused cleanly, without the driver making a slice for
// them. Before the meter followed the reply, the first of these crashed the
// engine.
func TestAServerAnnouncingImpossibleColumnsIsRefused(t *testing.T) {
	huge := []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	for name, answer := range map[string]func(byte, string) [][]byte{
		"a query's column count": func(cmd byte, q string) [][]byte {
			if strings.Contains(q, "CONNECTION_ID") {
				return [][]byte{huge}
			}
			return nil
		},
		"a prepared statement's columns": func(cmd byte, q string) [][]byte {
			switch {
			case strings.Contains(q, "CONNECTION_ID"):
				return connectionID()
			case cmd == comStmtPrepare:
				return [][]byte{{0x00, 1, 0, 0, 0, 0xff, 0xff, 2, 0, 0, 0, 0}}
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := MySQL(Target{Addr: fakeMySQL(t, answer), User: "root", Database: "d"},
				Limits{Connect: 2 * time.Second, Statement: time.Second})
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := b.Rows(context.Background(), "d", "t", 0)
			runtime.ReadMemStats(&after)
			if !errors.Is(err, ErrResultTooLarge) {
				t.Fatalf("got %v, want ErrResultTooLarge", err)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
				t.Errorf("refusing it allocated %d MB", grew>>20)
			}
		})
	}
}
