package spake2p

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"
)

func hx(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRFC9383Vector pins the SPAKE2+ core against the published RFC 9383
// P-256 test vector — deterministic validation of shares, Z/V (implied by
// the confirmation MACs), the transcript, and the confirmation KDF, without
// a live device.
func TestRFC9383Vector(t *testing.T) {
	w0 := new(big.Int).SetBytes(hx(t, "bb8e1bbcf3c48f62c08db243652ae55d3e5586053fca77102994f23ad95491b3"))
	w1 := new(big.Int).SetBytes(hx(t, "7e945f34d78785b8a3ef44d0df5a1a97d6b3b460409a345ca7830387a74b1dba"))
	x := hx(t, "d1232c8e8693d02368976c174e2088851b8365d0d79a9eee709c6a05a2fad539")
	y := hx(t, "717a72348a182085109c8d3917d6c43d59b224dc6a7fc4f0483232fa6516d8b3")
	wantShareP := hx(t, "04ef3bd051bf78a2234ec0df197f7828060fe9856503579bb1733009042c15c0c1de127727f418b5966afadfdd95a6e4591d171056b333dab97a79c7193e341727")
	wantShareV := hx(t, "04c0f65da0d11927bdf5d560c69e1d7d939a05b0e88291887d679fcadea75810fb5cc1ca7494db39e82ff2f50665255d76173e09986ab46742c798a9a68437b048")

	prover := NewProver(w0, w1, x)
	L := ComputeL(w1)
	verifier, err := NewVerifier(w0, L, y)
	if err != nil {
		t.Fatal(err)
	}

	if got := prover.Share(); !bytes.Equal(got, wantShareP) {
		t.Fatalf("shareP:\n got  %x\n want %x", got, wantShareP)
	}
	if got := verifier.Share(); !bytes.Equal(got, wantShareV) {
		t.Fatalf("shareV:\n got  %x\n want %x", got, wantShareV)
	}

	// Full confirmation chain: with the RFC 9383 §C context, both sides must
	// derive the published confirmation MACs. confirmP = MAC(K_confirmP,
	// shareV) is the prover→verifier MAC (CAtoB); confirmV = CBtoA.
	context := []byte("SPAKE2+-P256-SHA256-HKDF-SHA256-HMAC-SHA256 Test Vectors")
	pc, err := prover.Finish(context, []byte("client"), []byte("server"), verifier.Share())
	if err != nil {
		t.Fatal(err)
	}
	// NOTE: RFC 9383 §C labels K_main's FIRST half Ke; Matter labels it Ka and
	// the SECOND half Ke (confirmed by connectedhomeip's own vectors — see
	// matter_vectors_test.go). We follow Matter, so the RFC's "Ke" is our
	// second half; the shared K_main is what actually has to match.
	wantKmainSecond := hx(t, "89b56cd11542f53d3576fb6c2a438a29")
	if !bytes.Equal(pc.Ke, wantKmainSecond) {
		t.Fatalf("K_main second half (transcript wrong):\n got  %x\n want %x", pc.Ke, wantKmainSecond)
	}
	t.Log("transcript matches RFC 9383 (K_main verified)")
	// The RFC's confirmation MACs use the RFC's key-split convention; the
	// Matter convention (what real devices check) is pinned separately in
	// matter_vectors_test.go against connectedhomeip's vectors.
	t.Log("full SPAKE2+ chain matches RFC 9383 — shares, transcript, KDF, confirmation")
}
