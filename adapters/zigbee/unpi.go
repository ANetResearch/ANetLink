// Package zigbee is the native Zigbee adapter: it drives a coordinator stick
// (TI CC2652/CC2531 running Z-Stack) over its serial Monitor-Test interface
// in pure Go — no zigbee2mqtt, no Python, no cgo. This is the self-contained
// fallback of K207 A5; the MQTT bridge remains the probe-type backend for
// hosts that already run zigbee2mqtt.
//
// unpi.go implements UNPI framing (TI's Unified Network Processor Interface),
// the transport every Z-Stack MT command rides on.
package zigbee

import (
	"errors"
	"fmt"
	"io"
)

// UNPI frame layout: SOF | LEN | CMD0 | CMD1 | DATA[LEN] | FCS
const (
	sof     = 0xFE
	maxData = 250
)

// Command subsystems (CMD0 low nibble) and types (CMD0 high bits).
const (
	TypePoll = 0x00
	TypeSREQ = 0x20
	TypeAREQ = 0x40
	TypeSRSP = 0x60

	SubsysSys  = 0x01
	SubsysAF   = 0x04
	SubsysZDO  = 0x05
	SubsysUtil = 0x07
)

// Frame is one UNPI frame.
type Frame struct {
	Type   byte // TypeSREQ / TypeAREQ / TypeSRSP
	Subsys byte // SubsysSys / SubsysZDO / ...
	Cmd    byte // command id within the subsystem
	Data   []byte
}

var (
	ErrShortFrame = errors.New("zigbee: short UNPI frame")
	ErrBadFCS     = errors.New("zigbee: UNPI checksum mismatch")
	ErrNoSOF      = errors.New("zigbee: missing start-of-frame")
	ErrTooLong    = errors.New("zigbee: payload exceeds UNPI limit")
)

// fcs computes the UNPI XOR checksum over LEN..DATA.
func fcs(b []byte) byte {
	var x byte
	for _, c := range b {
		x ^= c
	}
	return x
}

// Encode serializes the frame including SOF and FCS.
func (f *Frame) Encode() ([]byte, error) {
	if len(f.Data) > maxData {
		return nil, ErrTooLong
	}
	body := make([]byte, 0, 3+len(f.Data))
	body = append(body, byte(len(f.Data)), f.Type|f.Subsys, f.Cmd)
	body = append(body, f.Data...)
	out := make([]byte, 0, 2+len(body))
	out = append(out, sof)
	out = append(out, body...)
	out = append(out, fcs(body))
	return out, nil
}

// DecodeFrame parses one complete UNPI frame from b (which must start at SOF)
// and returns it plus the number of bytes consumed.
func DecodeFrame(b []byte) (*Frame, int, error) {
	if len(b) < 5 {
		return nil, 0, ErrShortFrame
	}
	if b[0] != sof {
		return nil, 0, ErrNoSOF
	}
	dataLen := int(b[1])
	total := 1 + 3 + dataLen + 1 // SOF + (LEN CMD0 CMD1) + DATA + FCS
	if len(b) < total {
		return nil, 0, ErrShortFrame
	}
	body := b[1 : total-1]
	if fcs(body) != b[total-1] {
		return nil, 0, ErrBadFCS
	}
	return &Frame{
		Type:   b[2] & 0xE0,
		Subsys: b[2] & 0x1F,
		Cmd:    b[3],
		Data:   append([]byte(nil), b[4:4+dataLen]...),
	}, total, nil
}

// ReadFrame reads exactly one frame from r, resynchronizing on SOF.
func ReadFrame(r io.Reader) (*Frame, error) {
	var one [1]byte
	// resync: read until SOF
	for {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return nil, err
		}
		if one[0] == sof {
			break
		}
	}
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	dataLen := int(hdr[0])
	if dataLen > maxData {
		return nil, ErrTooLong
	}
	buf := make([]byte, dataLen+1) // data + FCS
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	body := append(hdr[:], buf[:dataLen]...)
	if fcs(body) != buf[dataLen] {
		return nil, ErrBadFCS
	}
	return &Frame{
		Type:   hdr[1] & 0xE0,
		Subsys: hdr[1] & 0x1F,
		Cmd:    hdr[2],
		Data:   append([]byte(nil), buf[:dataLen]...),
	}, nil
}

func (f *Frame) String() string {
	return fmt.Sprintf("UNPI{type=0x%02x subsys=0x%02x cmd=0x%02x len=%d}", f.Type, f.Subsys, f.Cmd, len(f.Data))
}
