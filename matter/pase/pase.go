// Package pase drives the Matter PASE (Passcode-Authenticated Session
// Establishment) handshake over the spake2p primitive: the message layer
// that turns "we can do SPAKE2+" into "we established a session with the
// device that knows the passcode" (Matter Core Spec §4.13). Brick four of
// the native stack.
//
// Message set (§4.13.1): PBKDFParamRequest → PBKDFParamResponse (the device
// hands back its SPAKE2+ salt + iteration count), then Pake1 (commissioner
// pA) → Pake2 (device pB + cB) → Pake3 (commissioner cA). The bodies are
// TLV structures (our matter/tlv). This package owns the codec + the
// commissioner-side state machine; the UDP/Matter-frame transport that
// carries these bodies is wired at the exchange layer (next brick).
package pase

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetLink/matter/spake2p"
	"github.com/ANetResearch/ANetLink/matter/tlv"
)

// Matter's PAKE context prefix (Core Spec §3.10). The actual SPAKE2+ context
// is Crypto_Hash(prefix ‖ PBKDFParamRequest ‖ PBKDFParamResponse) — binding
// the handshake to the exact messages exchanged, not just a fixed string.
var pakeContextPrefix = []byte("CHIP PAKE V1 Commissioning")

func matterContext(reqBytes, respBytes []byte) []byte {
	// Matter PASE context = SHA-256(prefix ‖ PBKDFParamRequest ‖
	// PBKDFParamResponse) — confirmed against connectedhomeip PASESession.cpp
	// (mCommissioningHash: AddData(prefix), AddData(req), AddData(resp),
	// Finish → 32-byte digest passed to Spake2p.Init). RFC 9383 then
	// length-prefixes this 32-byte context inside the transcript.
	h := sha256.New()
	h.Write(pakeContextPrefix)
	h.Write(reqBytes)
	h.Write(respBytes)
	return h.Sum(nil)
}

// PBKDFParamRequest is the commissioner's opening message.
type PBKDFParamRequest struct {
	InitiatorRandom  []byte // 32 bytes
	InitiatorSession uint16
	PasscodeID       uint16 // 0 for the primary setup passcode
	HasPBKDFParams   bool
}

// Encode serializes the request as the §4.13.1.2 TLV struct.
func (r *PBKDFParamRequest) Encode() ([]byte, error) {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(1), r.InitiatorRandom)
	w.PutUint(tlv.ContextTag(2), uint64(r.InitiatorSession))
	w.PutUint(tlv.ContextTag(3), uint64(r.PasscodeID))
	w.PutBool(tlv.ContextTag(4), r.HasPBKDFParams)
	if err := w.EndContainer(); err != nil {
		return nil, err
	}
	return w.Bytes()
}

// PBKDFParamResponse is the device's reply carrying the SPAKE2+ parameters.
type PBKDFParamResponse struct {
	InitiatorRandom  []byte
	ResponderRandom  []byte
	ResponderSession uint16
	Iterations       uint32
	Salt             []byte
}

// DecodePBKDFParamResponse parses the §4.13.1.3 TLV struct.
func DecodePBKDFParamResponse(b []byte) (*PBKDFParamResponse, error) {
	r := tlv.NewReader(b)
	if err := expect(r, tlv.TypeStruct); err != nil {
		return nil, err
	}
	out := &PBKDFParamResponse{}
	for {
		e, err := r.Next()
		if err != nil {
			return nil, err
		}
		if e == nil || e.Type == tlv.TypeEnd {
			break
		}
		switch e.Tag.Context {
		case 1:
			out.InitiatorRandom = e.P
		case 2:
			out.ResponderRandom = e.P
		case 3:
			out.ResponderSession = uint16(e.U)
		case 4:
			// pbkdf_parameters struct {1: iterations, 2: salt}
			if e.Type != tlv.TypeStruct {
				return nil, errors.New("pase: pbkdf_parameters must be a struct")
			}
			for {
				pe, perr := r.Next()
				if perr != nil {
					return nil, perr
				}
				if pe == nil || pe.Type == tlv.TypeEnd {
					break
				}
				switch pe.Tag.Context {
				case 1:
					out.Iterations = uint32(pe.U)
				case 2:
					out.Salt = pe.P
				default:
					if isContainer(pe) {
						if err := r.SkipContainer(); err != nil {
							return nil, err
						}
					}
				}
			}
		default:
			// Unknown top-level field (e.g. ctx=5 SessionParameters that a
			// real device sends): skip its whole subtree if it is a container,
			// so its children do not leak into this loop.
			if isContainer(e) {
				if err := r.SkipContainer(); err != nil {
					return nil, err
				}
			}
		}
	}
	if out.Iterations == 0 || len(out.Salt) == 0 {
		return nil, errors.New("pase: response missing salt/iterations")
	}
	return out, nil
}

// Pake1 carries the commissioner's SPAKE2+ share pA (§4.13.1.4).
type Pake1 struct{ PA []byte }

func (p *Pake1) Encode() ([]byte, error) {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(1), p.PA)
	if err := w.EndContainer(); err != nil {
		return nil, err
	}
	return w.Bytes()
}

// Pake2 carries the device's share pB and confirmation cB (§4.13.1.5).
type Pake2 struct {
	PB []byte
	CB []byte
}

func DecodePake2(b []byte) (*Pake2, error) {
	r := tlv.NewReader(b)
	if err := expect(r, tlv.TypeStruct); err != nil {
		return nil, err
	}
	out := &Pake2{}
	for {
		e, err := r.Next()
		if err != nil {
			return nil, err
		}
		if e == nil || e.Type == tlv.TypeEnd {
			break
		}
		switch e.Tag.Context {
		case 1:
			out.PB = e.P
		case 2:
			out.CB = e.P
		default:
			if isContainer(e) {
				if err := r.SkipContainer(); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(out.PB) == 0 || len(out.CB) == 0 {
		return nil, errors.New("pase: Pake2 missing pB/cB")
	}
	return out, nil
}

// Pake3 carries the commissioner's confirmation cA (§4.13.1.6).
type Pake3 struct{ CA []byte }

func (p *Pake3) Encode() ([]byte, error) {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(1), p.CA)
	if err := w.EndContainer(); err != nil {
		return nil, err
	}
	return w.Bytes()
}

func isContainer(e *tlv.Element) bool {
	return e.Type == tlv.TypeStruct || e.Type == tlv.TypeArray || e.Type == tlv.TypeList
}

func expect(r *tlv.Reader, want tlv.Type) error {
	e, err := r.Next()
	if err != nil {
		return err
	}
	if e == nil || e.Type != want {
		return fmt.Errorf("pase: expected %v, got %v", want, e)
	}
	return nil
}

// Commissioner is the client-side PASE state machine. It holds the passcode
// and the SPAKE2+ prover across the message round-trips.
type Commissioner struct {
	pass uint32
	// fixedX, when set, replaces the random scalar (test seam only — lets a
	// diagnostic replay one handshake against several context candidates).
	fixedX      []byte
	overrideCtx []byte
	prover      *spake2p.Prover
	pa          []byte
	confirm     *spake2p.Confirmation
	req         *PBKDFParamRequest
	reqBytes    []byte // exact PBKDFParamRequest payload (for the context hash)
	pakeCtx     []byte // Crypto_Hash(prefix ‖ request ‖ response)
}

// NewCommissioner starts a PASE run for a setup passcode.
func NewCommissioner(passcode uint32) *Commissioner {
	return &Commissioner{pass: passcode}
}

// StartRequest produces the PBKDFParamRequest (step 1).
func (c *Commissioner) StartRequest(sessionID uint16) (*PBKDFParamRequest, []byte, error) {
	ir := make([]byte, 32)
	if _, err := rand.Read(ir); err != nil {
		return nil, nil, err
	}
	c.req = &PBKDFParamRequest{InitiatorRandom: ir, InitiatorSession: sessionID, PasscodeID: 0}
	b, err := c.req.Encode()
	c.reqBytes = b
	return c.req, b, err
}

// MakePake1 consumes the device's PBKDFParamResponse and produces Pake1
// (step 2): compute the SPAKE2+ context from the exact request+response
// bytes, derive w0/w1 from the passcode and the device's salt/iterations,
// then compute the SPAKE2+ share pA. respBytes is the raw received payload.
func (c *Commissioner) MakePake1(resp *PBKDFParamResponse, respBytes []byte) (*Pake1, []byte, error) {
	if len(c.overrideCtx) > 0 {
		c.pakeCtx = c.overrideCtx
	} else {
		c.pakeCtx = matterContext(c.reqBytes, respBytes)
	}
	w0, w1 := spake2p.ComputeW0W1(c.pass, resp.Salt, int(resp.Iterations))
	x := c.fixedX
	if len(x) == 0 {
		x = make([]byte, 32)
		if _, err := rand.Read(x); err != nil {
			return nil, nil, err
		}
	}
	c.prover = spake2p.NewProver(w0, w1, x)
	c.pa = c.prover.Share()
	p1 := &Pake1{PA: c.pa}
	b, err := p1.Encode()
	return p1, b, err
}

// MakePake3 consumes the device's Pake2, verifies the device confirmation cB
// against our own derivation, and produces Pake3 (step 3). A cB mismatch
// means the device does NOT know the passcode → the run aborts here (this is
// the mutual-auth gate).
func (c *Commissioner) MakePake3(p2 *Pake2) (*Pake3, []byte, []byte, error) {
	if c.prover == nil {
		return nil, nil, nil, errors.New("pase: Pake1 not sent")
	}
	conf, err := c.prover.Finish(c.pakeCtx, nil, nil, p2.PB)
	if err != nil {
		return nil, nil, nil, err
	}
	// The device's cB (verifier→prover, over pA) must match our CBtoA.
	if !bytes.Equal(conf.CBtoA, p2.CB) {
		return nil, nil, nil, errors.New("pase: device confirmation failed — wrong passcode or MITM")
	}
	c.confirm = conf
	p3 := &Pake3{CA: conf.CAtoB}
	b, err := p3.Encode()
	return p3, b, conf.Ke, err
}

// SharedSecret returns the established Ke after a successful MakePake3.
func (c *Commissioner) SharedSecret() ([]byte, error) {
	if c.confirm == nil {
		return nil, errors.New("pase: handshake not complete")
	}
	return c.confirm.Ke, nil
}

// SetFixedScalar pins the prover's random scalar (diagnostics/tests only).
func (c *Commissioner) SetFixedScalar(x []byte) { c.fixedX = x }

// SetContextOverride forces the SPAKE2+ context (diagnostics only).
func (c *Commissioner) SetContextOverride(ctx []byte) { c.overrideCtx = ctx }

// RequestBytes returns the encoded PBKDFParamRequest (diagnostics).
func (c *Commissioner) RequestBytes() []byte { return c.reqBytes }
