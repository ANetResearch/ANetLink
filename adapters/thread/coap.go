package thread

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// CoAP (RFC 7252) over the Thread mesh — the device path. Thread devices are
// IPv6 endpoints speaking CoAP, so once the border router gives us the
// network, reaching a device is a UDP request. Pure Go, no library.

const coapPort = 5683

// CoAP message types and codes.
const (
	typeCON byte = 0
	typeACK byte = 2

	codeGET  byte = 0x01
	codePUT  byte = 0x03
	codePOST byte = 0x02

	codeContent byte = 0x45 // 2.05
	codeChanged byte = 0x44 // 2.04
)

var (
	ErrCoAPTimeout = errors.New("thread: CoAP request timed out")
	ErrCoAPStatus  = errors.New("thread: CoAP error response")
)

// coapMessage is a decoded CoAP message.
type coapMessage struct {
	Type      byte
	Code      byte
	MessageID uint16
	Token     []byte
	Payload   []byte
}

// encodeCoAP builds a CoAP request for a URI path.
func encodeCoAP(msgType, code byte, msgID uint16, token []byte, path string, payload []byte) []byte {
	b := make([]byte, 0, 32+len(payload))
	// version 1, type, token length
	b = append(b, 0x40|msgType<<4|byte(len(token)), code)
	b = binary.BigEndian.AppendUint16(b, msgID)
	b = append(b, token...)

	// Uri-Path options (number 11), one per segment, delta-encoded.
	prev := 0
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		delta := 11 - prev
		prev = 11
		l := len(seg)
		switch {
		case delta < 13 && l < 13:
			b = append(b, byte(delta<<4|l))
		case delta < 13:
			b = append(b, byte(delta<<4|13), byte(l-13))
		default:
			b = append(b, byte(13<<4|l), byte(delta-13))
		}
		b = append(b, seg...)
	}
	if len(payload) > 0 {
		b = append(b, 0xFF) // payload marker
		b = append(b, payload...)
	}
	return b
}

// decodeCoAP parses a CoAP response.
func decodeCoAP(b []byte) (*coapMessage, error) {
	if len(b) < 4 {
		return nil, errors.New("thread: short CoAP message")
	}
	if b[0]>>6 != 1 {
		return nil, errors.New("thread: unsupported CoAP version")
	}
	tkl := int(b[0] & 0x0F)
	if len(b) < 4+tkl {
		return nil, errors.New("thread: truncated CoAP token")
	}
	m := &coapMessage{
		Type:      (b[0] >> 4) & 0x03,
		Code:      b[1],
		MessageID: binary.BigEndian.Uint16(b[2:4]),
		Token:     append([]byte(nil), b[4:4+tkl]...),
	}
	// skip options until the payload marker
	i := 4 + tkl
	for i < len(b) {
		if b[i] == 0xFF {
			m.Payload = append([]byte(nil), b[i+1:]...)
			return m, nil
		}
		delta, length := int(b[i]>>4), int(b[i]&0x0F)
		i++
		for _, ext := range []*int{&delta, &length} {
			switch *ext {
			case 13:
				if i >= len(b) {
					return nil, errors.New("thread: truncated CoAP option")
				}
				*ext = int(b[i]) + 13
				i++
			case 14:
				if i+1 >= len(b) {
					return nil, errors.New("thread: truncated CoAP option")
				}
				*ext = int(binary.BigEndian.Uint16(b[i:])) + 269
				i += 2
			}
		}
		i += length
	}
	return m, nil
}

// coapRequest performs one CON request to a device on the well-known CoAP
// port and waits for its ACK.
func (a *Adapter) coapRequest(ctx context.Context, addr, path string, code byte, payload []byte) (*coapMessage, error) {
	host := addr
	if !strings.Contains(addr, "]") && strings.Contains(addr, ":") {
		host = "[" + addr + "]" // bare IPv6
	}
	return a.coapRequestAt(ctx, fmt.Sprintf("%s:%d", host, coapPort), path, code, payload)
}

// coapRequestAt is coapRequest against an explicit host:port.
func (a *Adapter) coapRequestAt(ctx context.Context, hostPort, path string, code byte, payload []byte) (*coapMessage, error) {
	conn, err := net.Dial("udp", hostPort)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var idBuf [2]byte
	var token [4]byte
	_, _ = rand.Read(idBuf[:])
	_, _ = rand.Read(token[:])
	msgID := binary.BigEndian.Uint16(idBuf[:])

	req := encodeCoAP(typeCON, code, msgID, token[:], path, payload)
	deadline := time.Now().Add(a.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	// CoAP CON retransmission: 3 attempts with doubling backoff.
	wait := 500 * time.Millisecond
	buf := make([]byte, 1500)
	for attempt := 0; attempt < 3 && time.Now().Before(deadline); attempt++ {
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		until := time.Now().Add(wait)
		if until.After(deadline) {
			until = deadline
		}
		_ = conn.SetReadDeadline(until)
		n, err := conn.Read(buf)
		if err != nil {
			wait *= 2
			continue
		}
		m, derr := decodeCoAP(buf[:n])
		if derr != nil {
			continue
		}
		if m.MessageID != msgID {
			continue // not our exchange
		}
		return m, nil
	}
	return nil, ErrCoAPTimeout
}

// coapClassOK reports whether a response code is a 2.xx success.
func coapClassOK(code byte) bool { return code>>5 == 2 }
