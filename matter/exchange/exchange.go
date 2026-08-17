// Package exchange runs a Matter message exchange over UDP: it ties
// matter/frame (framing) to matter/pase (handshake logic) and drives the
// five PASE messages against a real device, with the Message Reliability
// Protocol (MRP, Core Spec §4.12) reduced to what commissioning needs —
// acknowledge received reliable messages, retransmit our own until acked.
// Brick six: the layer that makes the pure-Go Matter stack talk to a real
// light.
//
// Scope (v0): the unencrypted PASE session (session 0). Post-PASE the
// session becomes AES-CCM encrypted and the flow continues into
// commissioning (arm-failsafe, CSR, add-NOC…) — those are the next bricks.
package exchange

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ANetResearch/ANetLink/matter/frame"
	"github.com/ANetResearch/ANetLink/matter/pase"
)

// Conn is one UDP exchange with a device. It owns the socket, the outbound
// message counter, and the exchange id.
type Conn struct {
	udp        *net.UDPConn
	exchangeID uint16
	counter    uint32
	mu         sync.Mutex
	timeout    time.Duration
	retries    int
}

// Dial opens a UDP exchange to addr (e.g. "192.0.2.93:5540").
func Dial(addr string, exchangeID uint16) (*Conn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		return nil, err
	}
	return &Conn{udp: c, exchangeID: exchangeID, counter: 1, timeout: 800 * time.Millisecond, retries: 4}, nil
}

func (c *Conn) Close() error { return c.udp.Close() }

// send transmits a message (initiator, reliable) and returns its counter so a
// later peer ack can be matched.
func (c *Conn) send(opcode uint8, payload []byte, ackFor uint32) (uint32, error) {
	c.mu.Lock()
	ctr := c.counter
	c.counter++
	c.mu.Unlock()
	m := &frame.Message{
		MessageCounter: ctr, ExchangeID: c.exchangeID,
		ProtocolID: frame.ProtocolSecureChannel, Opcode: opcode,
		Initiator: true, Reliability: true,
	}
	if ackFor != 0 {
		m.Ack = true
		m.AckCounter = ackFor
	}
	m.Payload = payload
	b, err := m.Encode()
	if err != nil {
		return 0, err
	}
	_, err = c.udp.Write(b)
	return ctr, err
}

// roundtrip sends one reliable message and waits for a response of any of the
// wantOpcodes, retransmitting on timeout (MRP). It returns the decoded peer
// message. A StandaloneAck (empty secure-channel ack) is consumed silently.
func (c *Conn) roundtrip(ctx context.Context, opcode uint8, payload []byte, ackFor uint32, wantOpcodes ...uint8) (*frame.Message, error) {
	want := map[uint8]bool{}
	for _, o := range wantOpcodes {
		want[o] = true
	}
	buf := make([]byte, 2048)
	for attempt := 0; attempt <= c.retries; attempt++ {
		sent, err := c.send(opcode, payload, ackFor)
		if err != nil {
			return nil, err
		}
		_ = sent
		deadline := time.Now().Add(c.timeout)
		_ = c.udp.SetReadDeadline(deadline)
		for {
			n, err := c.udp.Read(buf)
			if err != nil {
				break // timeout → retransmit
			}
			msg, derr := frame.Decode(buf[:n])
			if derr != nil {
				continue
			}
			if want[msg.Opcode] {
				return msg, nil
			}
			// StatusReport / StandaloneAck / anything else: ignore, keep waiting
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if time.Now().After(deadline) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
	return nil, fmt.Errorf("exchange: no response to opcode 0x%02x after %d attempts", opcode, c.retries+1)
}

// Commission runs the full PASE handshake against the device at addr using
// the setup passcode, returning the established session secret Ke on success.
// This proves cross-implementation PASE: our pure-Go commissioner against a
// real Matter device.
func Commission(ctx context.Context, addr string, passcode uint32) (ke []byte, err error) {
	c, err := Dial(addr, 0x0001)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	com := pase.NewCommissioner(passcode)

	// 1. PBKDFParamRequest → PBKDFParamResponse
	_, reqBody, err := com.StartRequest(1)
	if err != nil {
		return nil, err
	}
	respMsg, err := c.roundtrip(ctx, frame.OpPBKDFParamRequest, reqBody, 0, frame.OpPBKDFParamResponse)
	if err != nil {
		return nil, fmt.Errorf("PBKDFParamRequest: %w", err)
	}
	resp, err := pase.DecodePBKDFParamResponse(respMsg.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode PBKDFParamResponse: %w", err)
	}

	// 2. Pake1 → Pake2  (ack the response we just got)
	_, p1Body, err := com.MakePake1(resp, respMsg.Payload)
	if err != nil {
		return nil, err
	}
	p2Msg, err := c.roundtrip(ctx, frame.OpPASEPake1, p1Body, respMsg.MessageCounter, frame.OpPASEPake2)
	if err != nil {
		return nil, fmt.Errorf("Pake1: %w", err)
	}
	p2, err := pase.DecodePake2(p2Msg.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode Pake2: %w", err)
	}

	// 3. Pake3 (+verify device cB) → StatusReport(success)
	_, p3Body, keOut, err := com.MakePake3(p2)
	if err != nil {
		return nil, fmt.Errorf("Pake2 verify (wrong passcode or MITM?): %w", err)
	}
	if _, err := c.send(frame.OpPASEPake3, p3Body, p2Msg.MessageCounter); err != nil {
		return nil, fmt.Errorf("Pake3 send: %w", err)
	}
	// A well-behaved device replies with a Secure Channel StatusReport(success).
	// We treat a clean send + our own derived Ke as success at this layer;
	// confirming the StatusReport is a refinement handled with the secured
	// session bring-up (next brick).
	if len(keOut) == 0 {
		return nil, errors.New("exchange: empty session key after Pake3")
	}
	return keOut, nil
}
