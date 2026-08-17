// Package spake2p implements SPAKE2+ over P-256 as Matter uses it for PASE
// (Matter Core Spec §3.10; RFC 9383). This is the augmented PAKE that lets a
// commissioner prove knowledge of the setup passcode to a device and derive
// a shared secret, without either side learning a reusable credential — the
// third brick of the native Matter stack, and the one with no pure-Go
// off-the-shelf part (K207 A5: 自研兜底).
//
// Roles: prover = commissioner (client), verifier = device (server). Matter
// fixes the context string and computes w0,w1 from the passcode via a
// PBKDF2 whose parameters the device advertises in PBKDFParamResponse.
package spake2p

import (
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

// Curve is P-256 (Matter's SPAKE2+ ciphersuite: P256-SHA256-HKDF-HMAC).
var curve = elliptic.P256()

// M and N are the fixed points from RFC 9383 §4 for P-256 (compressed form).
var (
	mBytes = mustHex("02886e2f97ace46e55ba9dd7242579f2993b64e16ef3dcab95afd497333d8fa12f")
	nBytes = mustHex("03d8bbd6c639c62937b04d997f38c3770719c629d7014d49a24b4f98baa1292b49")
)

func mustHex(s string) []byte {
	b := make([]byte, len(s)/2)
	if _, err := fmt.Sscanf(s, "%x", &b); err != nil {
		panic(err)
	}
	return b
}

// n is the order of the P-256 group.
var n = curve.Params().N

// ComputeW0W1 derives (w0, w1) from a passcode and PBKDF2 salt/iterations
// (Matter §3.10: Crypto_PBKDF over a 40-byte output split into two
// 40-byte... actually two scalars). Matter feeds the little-endian 32-bit
// passcode into PBKDF2-SHA256, producing 80 bytes = w0s||w1s (40 each),
// each reduced mod n.
func ComputeW0W1(passcode uint32, salt []byte, iterations int) (w0, w1 *big.Int) {
	pin := []byte{byte(passcode), byte(passcode >> 8), byte(passcode >> 16), byte(passcode >> 24)}
	out := pbkdf2.Key(pin, salt, iterations, 80, sha256.New)
	w0 = new(big.Int).Mod(new(big.Int).SetBytes(out[:40]), n)
	w1 = new(big.Int).Mod(new(big.Int).SetBytes(out[40:]), n)
	return w0, w1
}

// L = w1 * G is the verifier's registration record (the device stores L, not
// w1 — that's the "+" in SPAKE2+).
func ComputeL(w1 *big.Int) []byte {
	x, y := curve.ScalarBaseMult(w1.Bytes())
	return elliptic.Marshal(curve, x, y)
}

func decodePoint(b []byte) (x, y *big.Int, err error) {
	x, y = elliptic.UnmarshalCompressed(curve, b)
	if x == nil {
		x, y = elliptic.Unmarshal(curve, b)
	}
	if x == nil {
		return nil, nil, errors.New("spake2p: invalid point")
	}
	return x, y, nil
}

var (
	mx, my *big.Int
	nx, ny *big.Int
)

func init() {
	var err error
	if mx, my, err = decodePoint(mBytes); err != nil {
		panic(err)
	}
	if nx, ny, err = decodePoint(nBytes); err != nil {
		panic(err)
	}
}

// Prover is the commissioner side.
type Prover struct {
	w0, w1 *big.Int
	x      []byte // random scalar
}

// Verifier is the device side.
type Verifier struct {
	w0 *big.Int
	L  []byte // w1*G, registered
	y  []byte // random scalar
	Lx *big.Int
	Ly *big.Int
}

// NewProver seeds a prover with (w0,w1) and a caller-supplied random scalar x
// (injectable so tests can pin RFC vectors; production passes crypto/rand).
func NewProver(w0, w1 *big.Int, x []byte) *Prover { return &Prover{w0: w0, w1: w1, x: x} }

// NewVerifier seeds a verifier with w0, the registration record L, and a
// random scalar y.
func NewVerifier(w0 *big.Int, L, y []byte) (*Verifier, error) {
	lx, ly, err := decodePoint(L)
	if err != nil {
		return nil, err
	}
	return &Verifier{w0: w0, L: L, y: y, Lx: lx, Ly: ly}, nil
}

// Share (prover) computes pA = X + w0*M, X = x*G.
func (p *Prover) Share() []byte {
	xX, xY := curve.ScalarBaseMult(p.x)
	wmx, wmy := curve.ScalarMult(mx, my, mod(p.w0).Bytes())
	ax, ay := curve.Add(xX, xY, wmx, wmy)
	return elliptic.Marshal(curve, ax, ay)
}

// Share (verifier) computes pB = Y + w0*N, Y = y*G.
func (v *Verifier) Share() []byte {
	yX, yY := curve.ScalarBaseMult(v.y)
	wnx, wny := curve.ScalarMult(nx, ny, mod(v.w0).Bytes())
	bx, by := curve.Add(yX, yY, wnx, wny)
	return elliptic.Marshal(curve, bx, by)
}

func mod(v *big.Int) *big.Int { return new(big.Int).Mod(v, n) }

// transcript builds the SPAKE2+ TT hash input (RFC 9383 §3.3): length-
// prefixed context, idProver, idVerifier, M, N, pA, pB, Z, V, w0.
func transcript(context, idA, idB, pA, pB, Z, V []byte, w0 *big.Int) []byte {
	var tt []byte
	add := func(b []byte) {
		var l [8]byte
		putU64LE(l[:], uint64(len(b)))
		tt = append(tt, l[:]...)
		tt = append(tt, b...)
	}
	add(context)
	add(idA)
	add(idB)
	add(elliptic.Marshal(curve, mx, my)) // uncompressed, consistent with pA/pB/Z/V
	add(elliptic.Marshal(curve, nx, ny))
	add(pA)
	add(pB)
	add(Z)
	add(V)
	add(bigTo32(w0))
	return tt
}

func putU64LE(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}

func bigTo32(v *big.Int) []byte {
	b := mod(v).Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// Confirmation carries the derived key material.
type Confirmation struct {
	Ke    []byte // shared secret (Matter's SessionKey seed)
	CAtoB []byte // prover→verifier confirmation MAC
	CBtoA []byte // verifier→prover confirmation MAC
}

// proverKeys/verifierKeys complete the exchange from each side's private
// scalar and the peer share; they must agree iff the passcode matched.
func (p *Prover) Finish(context, idA, idB, pB []byte) (*Confirmation, error) {
	bx, by, err := decodePoint(pB)
	if err != nil {
		return nil, err
	}
	// Z = h*x*(pB - w0*N); V = h*w1*(pB - w0*N). Cofactor h=1 on P-256.
	wnx, wny := curve.ScalarMult(nx, ny, mod(p.w0).Bytes())
	tx, ty := curve.Add(bx, by, wnx, new(big.Int).Sub(curve.Params().P, wny))
	zx, zy := curve.ScalarMult(tx, ty, p.x)
	vx, vy := curve.ScalarMult(tx, ty, mod(p.w1).Bytes())
	Z := elliptic.Marshal(curve, zx, zy)
	V := elliptic.Marshal(curve, vx, vy)
	pA := p.Share()
	return derive(context, idA, idB, pA, pB, Z, V, p.w0)
}

func (v *Verifier) Finish(context, idA, idB, pA []byte) (*Confirmation, error) {
	ax, ay, err := decodePoint(pA)
	if err != nil {
		return nil, err
	}
	// Z = h*y*(pA - w0*M); V = h*y*L.
	wmx, wmy := curve.ScalarMult(mx, my, mod(v.w0).Bytes())
	tx, ty := curve.Add(ax, ay, wmx, new(big.Int).Sub(curve.Params().P, wmy))
	zx, zy := curve.ScalarMult(tx, ty, v.y)
	vx, vy := curve.ScalarMult(v.Lx, v.Ly, v.y)
	Z := elliptic.Marshal(curve, zx, zy)
	V := elliptic.Marshal(curve, vx, vy)
	pB := v.Share()
	return derive(context, idA, idB, pA, pB, Z, V, v.w0)
}

// derive runs the RFC 9383 KDF: hash the transcript, split into confirmation
// key + shared key, HKDF-expand the confirmation key, HMAC the peer shares.
func derive(context, idA, idB, pA, pB, Z, V []byte, w0 *big.Int) (*Confirmation, error) {
	tt := transcript(context, idA, idB, pA, pB, Z, V, w0)
	kmain := sha256.Sum256(tt)
	ke := kmain[:16] // shared secret = first half of K_main
	// KcA || KcB = HKDF(nil, K_main, "ConfirmationKeys", 64) — the FULL K_main
	// is the KDF input (RFC 9383 §3.3), not just its second half. 32 bytes each.
	kc := make([]byte, 64)
	r := hkdf.New(sha256.New, kmain[:], nil, []byte("ConfirmationKeys"))
	if _, err := r.Read(kc); err != nil {
		return nil, err
	}
	kcA, kcB := kc[:32], kc[32:]
	// RFC 9383 §3.3 (pinned by the appendix vector):
	//   confirmP (prover→verifier) = MAC(KcB, shareP=pA)
	//   confirmV (verifier→prover) = MAC(KcA, shareV=pB)
	macAtoB := hmac.New(sha256.New, kcB)
	macAtoB.Write(pA)
	macBtoA := hmac.New(sha256.New, kcA)
	macBtoA.Write(pB)
	return &Confirmation{Ke: ke, CAtoB: macAtoB.Sum(nil), CBtoA: macBtoA.Sum(nil)}, nil
}
