package database

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
)

// The MySQL driver believes what the server sends it, and the server is the
// team's: configured however they like, or not MySQL at all. Three things it
// sizes from the server's word are bounded here, on the wire, under the
// driver:
//
//   - A packet is read whole, into a buffer its header sizes, and a row that
//     spans packets is joined into one; the driver's MaxAllowedPacket bounds
//     only what it sends. A packet larger than Limits.Message ends the
//     connection before the driver has seen its header.
//   - A result's column count is a length-encoded integer up to 2^64-1, and
//     the driver makes a slice that long before reading a column. A count
//     above MySQL's own maximum, 4096, ends the connection before the driver
//     has the packet that says it; so does a prepared statement announcing
//     more columns than that.
//   - Everything else — a flood of column definitions, of small rows, of
//     anything — is bounded by Limits.Wire, the most read on one connection.
//
// Seeing which packet is a column count takes following the conversation:
// the command the driver sends, then the result header, the column
// definitions, the rows and their end. That is exact only if the server
// cannot leave the definitions out, which a MariaDB server can when the
// client asks it to cache them; the capability is taken out of the server's
// greeting, so the driver never asks.
//
// Plain connections only: the browser does not ask for TLS (see config), and
// nothing inside TLS could be read here.

const (
	comQuery       = 0x03
	comStmtPrepare = 0x16
	comStmtExecute = 0x17

	// mysqlMaxColumns is the most columns a MySQL table or result can have.
	mysqlMaxColumns = 4096
)

// errMalformedReply is a reply that is not one MySQL sends.
var errMalformedReply = errors.New("database: the server's reply is not one MySQL sends")

// wireMeter is one connection's account of what the server sent, and why it
// was cut off if it was. The driver reports any read error as an invalid
// connection rather than passing it on, so the reason is read back from here.
type wireMeter struct {
	limit int   // the most of one packet, or one Postgres message
	total int64 // the most over the connection

	mu  sync.Mutex
	err error
}

func newWireMeter(l Limits) *wireMeter {
	return &wireMeter{limit: l.Message, total: int64(l.Wire)}
}

// fail records why the connection was cut off, the first reason only.
func (w *wireMeter) fail(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
	return w.err
}

// refusal is why the connection was cut off, or nil.
func (w *wireMeter) refusal() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// count adds n bytes read and fails past the total.
func (w *wireMeter) count(read *int64, n int) error {
	*read += int64(n)
	if *read > w.total {
		return w.fail(fmt.Errorf("%w — the server sent more than %s on one connection",
			ErrResultTooLarge, megabytes(int(w.total))))
	}
	return nil
}

// dialMySQL opens a connection whose packets are metered.
func (w *wireMeter) dialMySQL(d *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &meteredConn{Conn: conn, meter: w}, nil
	}
}

// replyState is where in a reply the next packet falls.
type replyState int

const (
	replyIdle    replyState = iota // nothing to examine until a command
	replyHeader                    // a result's first packet: OK, ERR, or a column count
	replyPrepare                   // a prepared statement's first packet
	replyColumns                   // column definitions, then an EOF in the older protocol
	replyRows                      // rows, until an EOF or OK, or an ERR
)

// meteredConn follows the packet framing in what it reads: a four-byte header
// — a 24-bit little-endian length and a sequence number — and that many
// bytes of payload, over and over.
type meteredConn struct {
	net.Conn
	meter *wireMeter

	greeted bool
	pending []byte // the greeting, rewritten, still to hand over

	header    [4]byte
	got       int // bytes of the current header read so far
	remaining int // bytes of the current payload still to come
	size      int // the current packet's payload length
	head      []byte
	read      int64

	state   replyState
	columns int // column definitions still to come
}

// Write notes the command the driver sends: a packet with sequence number
// zero, whose first byte names it. The driver writes each packet whole.
func (c *meteredConn) Write(p []byte) (int, error) {
	if len(p) >= 5 && p[3] == 0 {
		switch p[4] {
		case comQuery, comStmtExecute:
			c.state = replyHeader
		case comStmtPrepare:
			c.state = replyPrepare
		default:
			c.state = replyIdle
		}
	}
	return c.Conn.Write(p)
}

func (c *meteredConn) Read(p []byte) (int, error) {
	if err := c.meter.refusal(); err != nil {
		return 0, err
	}
	if !c.greeted {
		if err := c.readGreeting(); err != nil {
			return 0, err
		}
	}
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	n, err := c.Conn.Read(p)
	if serr := c.scan(p[:n]); serr != nil {
		// None of it is handed over: what it announces is in it, and the
		// driver sizes what it makes from that.
		return 0, c.meter.fail(serr)
	}
	return n, err
}

// readGreeting reads the server's first packet whole and takes the MariaDB
// metadata-cache capability out of it, so the driver never asks for result
// sets without their column definitions.
func (c *meteredConn) readGreeting() error {
	c.greeted = true
	var h [4]byte
	if _, err := io.ReadFull(c.Conn, h[:]); err != nil {
		return err
	}
	size := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	if size > c.meter.limit {
		return c.meter.fail(rowTooLarge(size, c.meter.limit))
	}
	if err := c.meter.count(&c.read, 4+size); err != nil {
		return err
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.Conn, payload); err != nil {
		return err
	}
	stripMetadataCache(payload)
	c.pending = append(h[:], payload...)
	return nil
}

// stripMetadataCache clears a MariaDB server's extended capabilities in its
// greeting, where the driver reads them: after the version, connection id,
// first scramble, filler, capabilities, character set, status, upper
// capabilities, scramble length and six reserved bytes. A MySQL server — one
// that sets the lowest capability bit — has none there.
func stripMetadataCache(g []byte) {
	if len(g) < 1 || g[0] != 10 {
		return
	}
	end := bytes.IndexByte(g[1:], 0)
	if end < 0 {
		return
	}
	pos := 1 + end + 1 + 4 + 8 + 1
	if len(g) < pos+2 {
		return
	}
	caps := binary.LittleEndian.Uint16(g[pos:])
	pos += 2 + 3 + 2 + 7
	if caps&1 == 0 && len(g) >= pos+4 {
		clear(g[pos : pos+4])
	}
}

func (c *meteredConn) scan(b []byte) error {
	if err := c.meter.count(&c.read, len(b)); err != nil {
		return err
	}
	for len(b) > 0 {
		if c.remaining > 0 {
			k := min(c.remaining, len(b))
			need := min(c.size, 9)
			if len(c.head) < need {
				take := min(k, need-len(c.head))
				c.head = append(c.head, b[:take]...)
				if len(c.head) == need {
					if err := c.examine(); err != nil {
						return err
					}
				}
			}
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
		c.size = int(c.header[0]) | int(c.header[1])<<8 | int(c.header[2])<<16
		if c.size > c.meter.limit {
			return rowTooLarge(c.size, c.meter.limit)
		}
		c.remaining = c.size
		c.head = c.head[:0]
	}
	return nil
}

// examine reads a packet's first bytes — up to nine, all of a short one —
// for where it falls in the reply, and refuses a column count no result can
// have.
func (c *meteredConn) examine() error {
	h := c.head
	switch c.state {
	case replyHeader:
		switch b := h[0]; {
		case b == 0x00, b == 0xff, b == 0xfb:
			// OK, ERR, or a LOCAL INFILE request: what follows, if
			// anything, is another result's first packet.
			return nil
		case b < 0xfb:
			c.columns = int(b)
		case b == 0xfc && len(h) >= 3:
			c.columns = int(binary.LittleEndian.Uint16(h[1:3]))
		case b == 0xfc:
			return errMalformedReply
		default:
			// 0xfd and 0xfe: three or eight bytes of count, 65536 or more.
			return tooManyColumns("more than 65535")
		}
		if c.columns > mysqlMaxColumns {
			return tooManyColumns(strconv.Itoa(c.columns))
		}
		c.state = replyColumns
	case replyPrepare:
		// The definitions that follow are read without a result header
		// among them, until the next command.
		c.state = replyIdle
		if h[0] == 0x00 && len(h) >= 9 {
			if n := int(binary.LittleEndian.Uint16(h[5:7])); n > mysqlMaxColumns {
				return tooManyColumns(strconv.Itoa(n))
			}
		}
	case replyColumns:
		if c.columns > 0 {
			if h[0] == 0xfe {
				// No definition starts so: out of step with the driver, so
				// the next packet is examined as a result's first.
				c.state = replyHeader
				return nil
			}
			c.columns--
			return nil
		}
		c.state = replyRows
		if h[0] == 0xfe && c.size < 9 {
			return nil // the EOF after the definitions, in the older protocol
		}
		return c.examineRow(h)
	case replyRows:
		return c.examineRow(h)
	}
	return nil
}

// examineRow ends the rows at what the driver ends them at: a packet
// starting 0xfe shorter than the largest packet — an EOF, or an OK in the
// newer protocol, which may say another result follows — or an ERR. Either
// way the next packet, if any before the next command, is a result's first.
func (c *meteredConn) examineRow(h []byte) error {
	if (h[0] == 0xfe && c.size < 0xffffff) || h[0] == 0xff {
		c.state = replyHeader
	}
	return nil
}

func tooManyColumns(n string) error {
	return fmt.Errorf("%w — the server announced %s columns, and a result has at most %d",
		ErrResultTooLarge, n, mysqlMaxColumns)
}

// countedConn is a Postgres connection that stops past Limits.Wire. A message
// is bounded on its own by pgx (see connect); this bounds them all together —
// a flood of ParameterStatus messages, of column descriptions, of small rows.
type countedConn struct {
	net.Conn
	meter *wireMeter
	read  int64
}

func (c *countedConn) Read(p []byte) (int, error) {
	if err := c.meter.refusal(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if cerr := c.meter.count(&c.read, n); cerr != nil {
		return 0, cerr
	}
	return n, err
}
