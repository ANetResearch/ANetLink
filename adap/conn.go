package adap

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
)

// maxFrame bounds one message. A DeviceProfile for a PLC with a thousand
// registers is large; a frame claiming to be larger than this is a bug or
// an attack, and either way must not become an allocation.
const maxFrame = 4 << 20 // 4 MiB

// Conn is one ADAP session over a socket: length-prefixed CoreDet-CBOR
// frames, with a mutex so concurrent senders cannot interleave a frame.
//
// The framing is a 4-byte big-endian length and then the body. CBOR is
// self-delimiting, so this is redundant in principle — and worth having
// anyway: it lets a receiver reject an oversized frame before decoding it,
// and it turns a desynchronised stream into an error instead of a plausible
// misparse.
type Conn struct {
	rw   net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
	once sync.Once
}

func NewConn(c net.Conn) *Conn {
	return &Conn{rw: c, br: bufio.NewReaderSize(c, 1<<16)}
}

// Send writes one message.
func (c *Conn) Send(m *Message) error {
	if m.V == 0 {
		m.V = Version
	}
	if m.TS == 0 {
		m.TS = uint64(time.Now().UnixMilli())
	}
	b, err := coredet.Marshal(m)
	if err != nil {
		return errf(ErrMalformed, "encode %s: %v", m.Op, err)
	}
	if len(b) > maxFrame {
		return errf(ErrMalformed, "%s frame is %d bytes, limit is %d", m.Op, len(b), maxFrame)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.rw.Write(hdr[:]); err != nil {
		return err
	}
	_, err = c.rw.Write(b)
	return err
}

// Recv reads one message.
func (c *Conn) Recv() (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxFrame {
		// Do not attempt to resynchronise: a bad length means the stream is
		// no longer framed, and reading on would decode noise as messages.
		return nil, errf(ErrMalformed, "frame length %d out of range", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.br, b); err != nil {
		return nil, err
	}
	var m Message
	if err := coredet.Unmarshal(b, &m); err != nil {
		return nil, errf(ErrMalformed, "decode: %v", err)
	}
	if m.V != Version {
		// Major version is not negotiated (spec §1: same-version release).
		return nil, errf(ErrMalformed, "ADAP v%d, this runtime speaks v%d", m.V, Version)
	}
	return &m, nil
}

// SetDeadline bounds a single exchange.
func (c *Conn) SetDeadline(t time.Time) error { return c.rw.SetDeadline(t) }

// Close shuts the session down once.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() { err = c.rw.Close() })
	return err
}

// Expired reports whether a message with a ttl has outlived it.
//
// This is a safety property, not an optimisation (spec §5.1.4). A message
// that says "unlock the door" and arrives 45 seconds late because something
// upstream blocked must not be executed: the world it was issued for is
// gone. The receiver drops it and answers EXPIRED, and it MUST NOT run the
// action first and report lateness afterwards.
func Expired(m *Message, now time.Time) bool {
	if m.TTL == 0 {
		return false
	}
	return uint64(now.UnixMilli()) > m.TS+m.TTL
}

// ErrClosed is returned when a session has gone away.
var ErrClosed = errors.New("adap: session closed")

func isClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrClosed)
}

var _ = fmt.Sprintf
