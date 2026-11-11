package database

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
)

func packet(seq byte, payload []byte) []byte {
	n := len(payload)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, payload...)
}

// Packets within the limit pass through untouched, however the reads split
// them — a header can arrive a byte at a time. The first packet over it ends
// the connection with none of its header handed on, so the driver never sizes
// a buffer from it, and the meter says how large it was. Without a server, so
// the bound is checked wherever the tests run.
func TestAMySQLPacketTooLargeIsRefusedAtItsHeader(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	meter := &wireMeter{limit: 1 << 10}
	conn := &meteredConn{Conn: client, meter: meter}

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
	if n != 0 || !errors.Is(err, errPacketTooLarge) {
		t.Fatalf("a 16 MB packet read %d bytes, err %v; want none and errPacketTooLarge", n, err)
	}
	if _, err := conn.Read(buf); !errors.Is(err, errPacketTooLarge) {
		t.Errorf("a read after the refusal gave %v", err)
	}
	if err := meter.tooLarge(); !errors.Is(err, ErrRowTooLarge) {
		t.Errorf("the meter says %v, want ErrRowTooLarge", err)
	}
}
