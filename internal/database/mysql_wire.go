package database

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
)

// The MySQL driver reads a packet whole, into a buffer the size its header
// announces, and joins a row that spans several packets into one: a row is
// as large as the server's max_allowed_packet lets it be, up to a gigabyte,
// and the server is the team's, configured however they like. The driver's
// own MaxAllowedPacket bounds only what it sends.
//
// So the bound is on the wire, under the driver: the connection is wrapped,
// each packet's header is read as it passes, and a packet larger than
// Limits.Message ends the connection before the driver has seen the header —
// so before it has made a buffer for it. A row bigger than one packet starts
// with a packet of the largest size, so it is refused at its first.
//
// Plain connections only: the browser does not ask for TLS (see config), and
// a header inside TLS could not be read here.

// errPacketTooLarge ends a connection at a packet too large. The driver
// reports a read error as an invalid connection rather than passing it on, so
// what the meter saw is read back from the meter, not from the error.
var errPacketTooLarge = errors.New("database: a packet is larger than the browser will read")

// wireMeter is one browser request's view of the packets it was sent.
type wireMeter struct {
	limit int
	over  atomic.Int64 // the size of the packet refused, once one is
}

// tooLarge is the row-too-large error for a packet the meter refused, or nil.
func (w *wireMeter) tooLarge() error {
	if size := w.over.Load(); size > 0 {
		return rowTooLarge(int(size), w.limit)
	}
	return nil
}

// dial opens a connection whose packets are metered.
func (w *wireMeter) dial(d *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &meteredConn{Conn: conn, meter: w}, nil
	}
}

// meteredConn follows the packet framing in what it reads: a four-byte header
// — a 24-bit little-endian length and a sequence number — and that many
// bytes of payload, over and over.
type meteredConn struct {
	net.Conn
	meter     *wireMeter
	header    [4]byte
	got       int // bytes of the current header read so far
	remaining int // bytes of the current payload still to come
}

func (c *meteredConn) Read(p []byte) (int, error) {
	if c.meter.over.Load() > 0 {
		return 0, errPacketTooLarge
	}
	n, err := c.Conn.Read(p)
	if serr := c.scan(p[:n]); serr != nil {
		// None of it is handed over: the header announcing the packet is in
		// it, and the driver sizes its buffer from that.
		return 0, serr
	}
	return n, err
}

func (c *meteredConn) scan(b []byte) error {
	for len(b) > 0 {
		if c.remaining > 0 {
			k := min(c.remaining, len(b))
			c.remaining -= k
			b = b[k:]
			continue
		}
		k := copy(c.header[c.got:], b)
		c.got += k
		b = b[k:]
		if c.got < len(c.header) {
			return nil
		}
		c.got = 0
		size := int(c.header[0]) | int(c.header[1])<<8 | int(c.header[2])<<16
		if size > c.meter.limit {
			c.meter.over.Store(int64(size))
			return errPacketTooLarge
		}
		c.remaining = size
	}
	return nil
}
