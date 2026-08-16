package spake2p

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// randScalar returns 32 random bytes for a private scalar.
func randScalar(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestExchangeAgrees is the load-bearing property: with the same passcode,
// prover and verifier independently derive identical Ke and matching
// confirmation MACs. This is what makes PASE a proof-of-passcode.
func TestExchangeAgrees(t *testing.T) {
	const passcode = 20202021 // the testbed matter.js light
	salt := []byte("anet-spake2p-test-salt-0001")
	iters := 1000

	w0, w1 := ComputeW0W1(passcode, salt, iters)
	L := ComputeL(w1)

	context := []byte("Matter PAKE V1 Commissioning")
	idA, idB := []byte{}, []byte{}

	prover := NewProver(w0, w1, randScalar(t))
	verifier, err := NewVerifier(w0, L, randScalar(t))
	if err != nil {
		t.Fatal(err)
	}

	pA := prover.Share()
	pB := verifier.Share()

	pc, err := prover.Finish(context, idA, idB, pB)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := verifier.Finish(context, idA, idB, pA)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(pc.Ke, vc.Ke) {
		t.Fatalf("shared secret disagreement:\n prover   %x\n verifier %x", pc.Ke, vc.Ke)
	}
	// prover's CAtoB (MAC over pB) must equal what verifier computes as CAtoB
	if !bytes.Equal(pc.CAtoB, vc.CAtoB) || !bytes.Equal(pc.CBtoA, vc.CBtoA) {
		t.Fatal("confirmation MAC mismatch on a correct passcode")
	}
	if len(pc.Ke) != 16 {
		t.Fatalf("Ke length %d, want 16", len(pc.Ke))
	}
}

// TestWrongPasscodeFails: a prover with the wrong passcode derives a
// different secret — the exchange must NOT confirm.
func TestWrongPasscodeFails(t *testing.T) {
	salt := []byte("anet-spake2p-test-salt-0001")
	iters := 1000
	w0, w1 := ComputeW0W1(20202021, salt, iters)
	L := ComputeL(w1)

	badW0, badW1 := ComputeW0W1(99999999, salt, iters)

	context := []byte("Matter PAKE V1 Commissioning")
	prover := NewProver(badW0, badW1, randScalar(t))
	verifier, _ := NewVerifier(w0, L, randScalar(t))

	pA := prover.Share()
	pB := verifier.Share()
	pc, _ := prover.Finish(context, nil, nil, pB)
	vc, _ := verifier.Finish(context, nil, nil, pA)

	if bytes.Equal(pc.Ke, vc.Ke) {
		t.Fatal("wrong passcode must NOT derive the same secret")
	}
	if bytes.Equal(pc.CAtoB, vc.CAtoB) {
		t.Fatal("wrong passcode must NOT produce a matching confirmation")
	}
}

// TestW0W1Deterministic pins that PBKDF derivation is stable and reduced.
func TestW0W1Deterministic(t *testing.T) {
	w0a, w1a := ComputeW0W1(20202021, []byte("salt-salt-salt-16"), 1000)
	w0b, w1b := ComputeW0W1(20202021, []byte("salt-salt-salt-16"), 1000)
	if w0a.Cmp(w0b) != 0 || w1a.Cmp(w1b) != 0 {
		t.Fatal("derivation not deterministic")
	}
	if w0a.Cmp(n) >= 0 || w1a.Cmp(n) >= 0 {
		t.Fatal("scalars must be reduced mod n")
	}
	// different salt → different scalars
	w0c, _ := ComputeW0W1(20202021, []byte("different-salt-16"), 1000)
	if w0a.Cmp(w0c) == 0 {
		t.Fatal("salt must affect derivation")
	}
}

// TestMNPointsValid pins that the fixed M/N points decode onto the curve.
func TestMNPointsValid(t *testing.T) {
	if mx == nil || nx == nil {
		t.Fatal("M/N did not decode")
	}
	if !curve.IsOnCurve(mx, my) || !curve.IsOnCurve(nx, ny) {
		t.Fatal("M/N not on P-256")
	}
	// sanity: the compressed encodings we hard-code round-trip
	if hex.EncodeToString(mBytes)[:2] != "02" || hex.EncodeToString(nBytes)[:2] != "03" {
		t.Fatal("M/N compressed prefixes wrong")
	}
}
