// Package frame implements the Matter message framing (Matter Core Spec §4.4
// Message Frame + §4.8 Protocol Header): the outer envelope that carries a
// protocol payload (e.g. a PASE Pake message) between two nodes over UDP.
// Brick five of the native stack — the last framing layer between the PASE
// logic (matter/pase) and a real device on the wire.
//
// Scope (v0): the unencrypted path used during PASE — session id 0, no
// privacy/encryption, no source/destination node ids. That is exactly what
// _matterc._udp commissioning needs; the secured (post-PASE) path with
// AES-CCM privacy is a follow-up.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Message-header flag bits (§4.4.1.1, byte 0).
const (
	flagSourcePresent = 0x04 // S flag
	flagDSIZMask      = 0x03 // destination-id size: 0 none, 1 node(8B), 2 group(2B)
	versionShift      = 4    // message format version in the high nibble
)

// Security-flags bits (§4.4.1.4).
const (
	secPrivacy    = 0x80
	secControl    = 0x40
	secExtensions = 0x20
	secTypeMask   = 0x03 // 0 unicast(session), 1 group
)

// Exchange-flags bits (§4.8.1.1).
const (
	xInitiator     = 0x01 // I: sender is the exchange initiator
	xAck           = 0x02 // A: message carries an acknowledgement
	xReliability   = 0x04 // R: sender requests reliable delivery (MRP)
	xSecuredExt    = 0x08
	xVendorPresent = 0x10 // V: protocol vendor id present
)

// Standard protocol ids (§4.10).
const (
	ProtocolSecureChannel uint16 = 0x0000
)

// Secure Channel opcodes used by PASE (§4.9.2).
const (
	OpPBKDFParamRequest  uint8 = 0x20
	OpPBKDFParamResponse uint8 = 0x21
	OpPASEPake1          uint8 = 0x22
	OpPASEPake2          uint8 = 0x23
	OpPASEPake3          uint8 = 0x24
	OpStatusReport       uint8 = 0x40
)

// Message is one decoded Matter message on the unencrypted path.
type Message struct {
	SessionID      uint16
	MessageCounter uint32
	// Protocol header:
	ExchangeID  uint16
	ProtocolID  uint16
	Opcode      uint8
	Initiator   bool
	Reliability bool
	Ack         bool
	AckCounter  uint32 // valid when Ack
	VendorID    uint16 // valid when non-zero and V flag set
	// Application payload (e.g. the PASE TLV body).
	Payload []byte
}

// Encode serializes the message (unencrypted path).
func (m *Message) Encode() ([]byte, error) {
	if m.SessionID != 0 {
		return nil, errors.New("frame: v0 encodes only the unencrypted path (session 0)")
	}
	var b []byte
	// Message header: flags(v=0, S=0, DSIZ=0), session id, security flags, counter.
	b = append(b, 0x00)                                  // message flags
	b = binary.LittleEndian.AppendUint16(b, m.SessionID) // session id
	b = append(b, 0x00)                                  // security flags: unicast, no privacy
	b = binary.LittleEndian.AppendUint32(b, m.MessageCounter)

	// Protocol header.
	var xf byte
	if m.Initiator {
		xf |= xInitiator
	}
	if m.Reliability {
		xf |= xReliability
	}
	if m.Ack {
		xf |= xAck
	}
	if m.VendorID != 0 {
		xf |= xVendorPresent
	}
	b = append(b, xf, m.Opcode)
	b = binary.LittleEndian.AppendUint16(b, m.ExchangeID)
	if m.VendorID != 0 {
		b = binary.LittleEndian.AppendUint16(b, m.VendorID)
	}
	b = binary.LittleEndian.AppendUint16(b, m.ProtocolID)
	if m.Ack {
		b = binary.LittleEndian.AppendUint32(b, m.AckCounter)
	}
	b = append(b, m.Payload...)
	return b, nil
}

// Decode parses an unencrypted Matter message.
func Decode(b []byte) (*Message, error) {
	if len(b) < 8 {
		return nil, errors.New("frame: short message header")
	}
	msgFlags := b[0]
	if msgFlags>>versionShift != 0 {
		return nil, fmt.Errorf("frame: unsupported message format version %d", msgFlags>>versionShift)
	}
	m := &Message{}
	m.SessionID = binary.LittleEndian.Uint16(b[1:])
	secFlags := b[3]
	if secFlags&secPrivacy != 0 || m.SessionID != 0 {
		return nil, errors.New("frame: encrypted/secured messages not supported in v0")
	}
	m.MessageCounter = binary.LittleEndian.Uint32(b[4:])
	i := 8
	if msgFlags&flagSourcePresent != 0 {
		i += 8 // skip source node id
	}
	switch msgFlags & flagDSIZMask {
	case 1:
		i += 8
	case 2:
		i += 2
	}
	if len(b) < i+4 {
		return nil, errors.New("frame: short protocol header")
	}
	xf := b[i]
	m.Opcode = b[i+1]
	m.Initiator = xf&xInitiator != 0
	m.Reliability = xf&xReliability != 0
	m.Ack = xf&xAck != 0
	i += 2
	m.ExchangeID = binary.LittleEndian.Uint16(b[i:])
	i += 2
	if xf&xVendorPresent != 0 {
		if len(b) < i+2 {
			return nil, errors.New("frame: truncated vendor id")
		}
		m.VendorID = binary.LittleEndian.Uint16(b[i:])
		i += 2
	}
	if len(b) < i+2 {
		return nil, errors.New("frame: truncated protocol id")
	}
	m.ProtocolID = binary.LittleEndian.Uint16(b[i:])
	i += 2
	if m.Ack {
		if len(b) < i+4 {
			return nil, errors.New("frame: truncated ack counter")
		}
		m.AckCounter = binary.LittleEndian.Uint32(b[i:])
		i += 4
	}
	m.Payload = append([]byte(nil), b[i:]...)
	return m, nil
}
