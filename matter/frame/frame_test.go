package frame

import (
	"bytes"
	"testing"
)

func TestPake1FrameLayout(t *testing.T) {
	// A PASE Pake1 from the commissioner (initiator, reliable), counter 1,
	// exchange 0x1234, secure-channel protocol, opcode 0x22, with a tiny
	// TLV-ish payload. Pin the exact bytes.
	m := &Message{
		SessionID: 0, MessageCounter: 1,
		ExchangeID: 0x1234, ProtocolID: ProtocolSecureChannel, Opcode: OpPASEPake1,
		Initiator: true, Reliability: true,
		Payload: []byte{0x15, 0x18},
	}
	got, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x00,             // message flags: v0, no source, no dest
		0x00, 0x00,       // session id 0
		0x00,             // security flags
		0x01, 0x00, 0x00, 0x00, // message counter 1
		0x05,             // exchange flags: I|R (0x01|0x04)
		0x22,             // opcode PASE Pake1
		0x34, 0x12,       // exchange id 0x1234
		0x00, 0x00,       // protocol id 0x0000 secure channel
		0x15, 0x18,       // payload
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame bytes:\n got  %x\n want %x", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []*Message{
		{MessageCounter: 42, ExchangeID: 7, ProtocolID: ProtocolSecureChannel, Opcode: OpPBKDFParamRequest, Initiator: true, Payload: []byte("hello")},
		{MessageCounter: 99, ExchangeID: 7, ProtocolID: ProtocolSecureChannel, Opcode: OpPASEPake2, Payload: []byte{0x15, 0x24, 0x01, 0x2A, 0x18}},
		{MessageCounter: 100, ExchangeID: 7, ProtocolID: ProtocolSecureChannel, Opcode: OpPASEPake3, Ack: true, AckCounter: 99, Initiator: true},
		{MessageCounter: 5, ExchangeID: 1, ProtocolID: 0xFC01, Opcode: 0x01, VendorID: 0x1349, Payload: []byte{1, 2, 3}},
	}
	for i, m := range cases {
		b, err := m.Encode()
		if err != nil {
			t.Fatalf("case %d encode: %v", i, err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatalf("case %d decode: %v", i, err)
		}
		if got.MessageCounter != m.MessageCounter || got.ExchangeID != m.ExchangeID ||
			got.ProtocolID != m.ProtocolID || got.Opcode != m.Opcode ||
			got.Initiator != m.Initiator || got.Ack != m.Ack || got.AckCounter != m.AckCounter ||
			got.VendorID != m.VendorID || !bytes.Equal(got.Payload, m.Payload) {
			t.Fatalf("case %d round-trip mismatch:\n in  %+v\n out %+v", i, m, got)
		}
	}
}

func TestRejectsEncryptedAndTruncated(t *testing.T) {
	// non-zero session id → secured, unsupported in v0
	if _, err := Decode([]byte{0x00, 0x01, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("secured session must be rejected in v0")
	}
	// privacy bit set
	if _, err := Decode([]byte{0x00, 0x00, 0x00, 0x80, 0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("privacy flag must be rejected in v0")
	}
	for _, short := range [][]byte{{}, {0x00}, {0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00}} {
		if _, err := Decode(short); err == nil {
			t.Fatalf("short input %x must error", short)
		}
	}
	// header ok but protocol header truncated
	if _, err := Decode([]byte{0x00, 0x00, 0x00, 0x00, 1, 0, 0, 0, 0x05}); err == nil {
		t.Fatal("truncated protocol header must error")
	}
}

func TestSourceAndDestSkipped(t *testing.T) {
	// A message that (illegally for v0 encode, but legal to decode) carries a
	// source node id: DSIZ=0, S=1. We must skip the 8 source bytes.
	b := []byte{
		flagSourcePresent,      // S=1
		0x00, 0x00,             // session 0
		0x00,                   // security
		0x07, 0x00, 0x00, 0x00, // counter 7
		1, 2, 3, 4, 5, 6, 7, 8, // source node id (skipped)
		xInitiator, OpPBKDFParamRequest,
		0x09, 0x00, // exchange 9
		0x00, 0x00, // protocol id
		0xAA,       // payload
	}
	m, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.ExchangeID != 9 || m.Opcode != OpPBKDFParamRequest || !bytes.Equal(m.Payload, []byte{0xAA}) {
		t.Fatalf("source-id skip failed: %+v", m)
	}
}

func FuzzDecode(f *testing.F) {
	m := &Message{MessageCounter: 1, ExchangeID: 1, Opcode: OpPASEPake1, Payload: []byte{0x15, 0x18}}
	b, _ := m.Encode()
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		if msg, err := Decode(b); err == nil {
			_, _ = msg.Encode() // must not panic
		}
	})
}
