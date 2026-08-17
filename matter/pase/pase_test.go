package pase

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/ANetResearch/ANetLink/matter/spake2p"
	"github.com/ANetResearch/ANetLink/matter/tlv"
)

// device is an in-memory PASE responder: it plays the §4.13 device side so
// the commissioner state machine can be driven through a full handshake
// without a wire. It knows a passcode via its stored (w0, L) registration.
type device struct {
	salt     []byte
	iters    uint32
	verifier *spake2p.Verifier
	pb       []byte
	conf     *spake2p.Confirmation
	ctx      []byte // shared SPAKE2+ context (matches the commissioner)
}

func newDevice(t *testing.T, passcode uint32) *device {
	t.Helper()
	salt := make([]byte, 32)
	_, _ = rand.Read(salt)
	iters := uint32(1000)
	w0, w1 := spake2p.ComputeW0W1(passcode, salt, int(iters))
	L := spake2p.ComputeL(w1)
	y := make([]byte, 32)
	_, _ = rand.Read(y)
	v, err := spake2p.NewVerifier(w0, L, y)
	if err != nil {
		t.Fatal(err)
	}
	return &device{salt: salt, iters: iters, verifier: v}
}

func (d *device) paramResponse(req *PBKDFParamRequest) *PBKDFParamResponse {
	rr := make([]byte, 32)
	_, _ = rand.Read(rr)
	return &PBKDFParamResponse{
		InitiatorRandom: req.InitiatorRandom, ResponderRandom: rr,
		ResponderSession: 1, Iterations: d.iters, Salt: d.salt,
	}
}

// pake2 consumes the commissioner's pA, computes pB + cB.
func (d *device) pake2(t *testing.T, p1 *Pake1) *Pake2 {
	t.Helper()
	conf, err := d.verifier.Finish(d.ctx, nil, nil, p1.PA)
	if err != nil {
		t.Fatal(err)
	}
	d.conf = conf
	d.pb = d.verifier.Share()
	// cB is verifier→prover, i.e. CBtoA
	return &Pake2{PB: d.pb, CB: conf.CBtoA}
}

// verifyPake3 checks the commissioner's cA (CAtoB).
func (d *device) verifyPake3(p3 *Pake3) bool {
	return bytes.Equal(d.conf.CAtoB, p3.CA)
}

func TestFullHandshakeAgrees(t *testing.T) {
	const passcode = 20202021 // testbed matter.js light
	dev := newDevice(t, passcode)
	com := NewCommissioner(passcode)

	req, reqBytes, err := com.StartRequest(1)
	if err != nil {
		t.Fatal(err)
	}
	// request must round-trip through TLV
	if len(reqBytes) == 0 {
		t.Fatal("empty request encoding")
	}

	resp := dev.paramResponse(req)
	// response must round-trip through our own codec
	respBytes := encodeResp(t, resp)
	decoded, err := DecodePBKDFParamResponse(respBytes)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Iterations != resp.Iterations || !bytes.Equal(decoded.Salt, resp.Salt) {
		t.Fatal("PBKDFParamResponse did not round-trip")
	}

	p1, p1Bytes, err := com.MakePake1(decoded, respBytes)
	if err != nil {
		t.Fatal(err)
	}
	dev.ctx = matterContext(com.reqBytes, respBytes) // device shares the context
	if _, err := decodePake1(p1Bytes); err != nil {
		t.Fatalf("Pake1 encoding invalid: %v", err)
	}

	p2 := dev.pake2(t, p1)
	p2Bytes := encodePake2(t, p2)
	p2dec, err := DecodePake2(p2Bytes)
	if err != nil {
		t.Fatal(err)
	}

	p3, _, ke, err := com.MakePake3(p2dec)
	if err != nil {
		t.Fatalf("commissioner rejected a correct device: %v", err)
	}
	if !dev.verifyPake3(p3) {
		t.Fatal("device rejected the commissioner's cA on a correct passcode")
	}

	// both sides hold the same session secret
	if !bytes.Equal(ke, dev.conf.Ke) {
		t.Fatalf("session key mismatch:\n com %x\n dev %x", ke, dev.conf.Ke)
	}
	got, _ := com.SharedSecret()
	if !bytes.Equal(got, ke) {
		t.Fatal("SharedSecret disagrees with MakePake3 return")
	}
}

func TestWrongPasscodeRejectedAtPake2(t *testing.T) {
	dev := newDevice(t, 20202021)
	com := NewCommissioner(11223344) // wrong

	req, _, _ := com.StartRequest(1)
	resp := dev.paramResponse(req)
	rb := encodeResp(t, resp)
	p1, _, err := com.MakePake1(resp, rb)
	if err != nil {
		t.Fatal(err)
	}
	dev.ctx = matterContext(com.reqBytes, rb)
	p2 := dev.pake2(t, p1)
	if _, _, _, err := com.MakePake3(p2); err == nil {
		t.Fatal("wrong passcode MUST be rejected at the cB check")
	}
}

func TestMalformedResponses(t *testing.T) {
	if _, err := DecodePBKDFParamResponse([]byte{0x15, 0x18}); err == nil {
		t.Fatal("response with no salt/iterations must error")
	}
	if _, err := DecodePake2([]byte{0x15, 0x18}); err == nil {
		t.Fatal("Pake2 with no pB/cB must error")
	}
	if _, err := DecodePBKDFParamResponse([]byte{0xff}); err == nil {
		t.Fatal("garbage must error")
	}
}

// --- test-only encoders mirroring the device side of the wire ---

func encodeResp(t *testing.T, r *PBKDFParamResponse) []byte {
	t.Helper()
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(1), r.InitiatorRandom)
	w.PutBytes(tlv.ContextTag(2), r.ResponderRandom)
	w.PutUint(tlv.ContextTag(3), uint64(r.ResponderSession))
	w.StartStruct(tlv.ContextTag(4))
	w.PutUint(tlv.ContextTag(1), uint64(r.Iterations))
	w.PutBytes(tlv.ContextTag(2), r.Salt)
	_ = w.EndContainer()
	_ = w.EndContainer()
	b, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func encodePake2(t *testing.T, p *Pake2) []byte {
	t.Helper()
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(1), p.PB)
	w.PutBytes(tlv.ContextTag(2), p.CB)
	_ = w.EndContainer()
	b, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodePake1(b []byte) (*Pake1, error) {
	r := tlv.NewReader(b)
	if _, err := r.Next(); err != nil { // struct
		return nil, err
	}
	e, err := r.Next()
	if err != nil {
		return nil, err
	}
	return &Pake1{PA: e.P}, nil
}
