package spake2p

import (
	"bytes"
	"math/big"
	"testing"
)

// TestMatterSPAKE2PVector pins our SPAKE2+ against the very vector
// connectedhomeip tests itself with (src/crypto/tests/
// SPAKE2P_RFC_test_vectors.h, test_vector_16). Matching this means our
// primitive agrees with the Matter reference implementation byte for byte —
// including the confirmation-MAC key pairing, which is what a real device
// checks at Pake2.
func TestMatterSPAKE2PVector(t *testing.T) {
	w0 := hx(t, "e6887cf9bdfb7579c69bf47928a84514b5e355ac034863f7ffaf4390e67d798c")
	w1 := hx(t, "24b5ae4abda868ec9336ffc3b78ee31c5755bef1759227ef5372ca139b94e512")
	L := hx(t, "0495645cfb74df6e58f9748bb83a86620bab7c82e107f57d6870da8cbcb2ff9f7063a14b6402c62f99afcb9706a4d1a143273259fe76f1c605a3639745a92154b9")
	x := hx(t, "8b0f3f383905cf3a3bb955ef8fb62e24849dd349a05ca79aafb18041d30cbdb6")
	X := hx(t, "04af09987a593d3bac8694b123839422c3cc87e37d6b41c1d630f000dd64980e537ae704bcede04ea3bec9b7475b32fa2ca3b684be14d11645e38ea6609eb39e7e")
	y := hx(t, "2e0895b0e763d6d5a9564433e64ac3cac74ff897f6c3445247ba1bab40082a91")
	Y := hx(t, "04417592620aebf9fd203616bbb9f121b730c258b286f890c5f19fea833a9c900cbe9057bc549a3e19975be9927f0e7614f08d1f0a108eede5fd7eb5624584a4f4")
	Z := hx(t, "0471a35282d2026f36bf3ceb38fcf87e3112a4452f46e9f7b47fd769cfb570145b62589c76b7aa1eb6080a832e5332c36898426912e29c40ef9e9c742eee82bf30")
	V := hx(t, "046718981bf15bc4db538fc1f1c1d058cb0eececf1dbe1b1ea08a4e25275d382e82b348c8131d8ed669d169c2e03a858db7cf6ca2853a4071251a39fbe8cfc39bc")
	Ka := hx(t, "f9cab9adcc0ed8e5a4db11a8505914b2")
	Ke := hx(t, "801db297654816eb4f02868129b9dc89")
	KcA := hx(t, "0d248d7d19234f1486b2efba5179c52d")
	KcB := hx(t, "556291df26d705a2caedd6474dd0079b")
	macKcA := hx(t, "d4376f2da9c72226dd151b77c2919071155fc22a2068d90b5faa6c78c11e77dd")
	macKcB := hx(t, "0660a680663e8c5695956fb22dff298b1d07a526cf3cc591adfecd1f6ef6e02e")

	w0i := new(big.Int).SetBytes(w0)
	w1i := new(big.Int).SetBytes(w1)

	// L = w1*G must match the reference.
	if got := ComputeL(w1i); !bytes.Equal(got, L) {
		t.Fatalf("L:\n got  %x\n want %x", got, L)
	}

	prover := NewProver(w0i, w1i, x)
	if got := prover.Share(); !bytes.Equal(got, X) {
		t.Fatalf("X (shareP):\n got  %x\n want %x", got, X)
	}
	verifier, err := NewVerifier(w0i, L, y)
	if err != nil {
		t.Fatal(err)
	}
	if got := verifier.Share(); !bytes.Equal(got, Y) {
		t.Fatalf("Y (shareV):\n got  %x\n want %x", got, Y)
	}

	// The connectedhomeip vector's own context and identities.
	ctx := []byte("SPAKE2+-P256-SHA256-HKDF draft-01")
	pc, err := prover.Finish(ctx, []byte("client"), []byte("server"), Y)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pc.Ke, Ke) {
		t.Fatalf("Ke:\n got  %x\n want %x", pc.Ke, Ke)
	}
	// MAC_KcA is what the PROVER sends (cA); MAC_KcB is the VERIFIER's (cB).
	if !bytes.Equal(pc.CAtoB, macKcA) {
		t.Fatalf("cA (MAC_KcA):\n got  %x\n want %x", pc.CAtoB, macKcA)
	}
	if !bytes.Equal(pc.CBtoA, macKcB) {
		t.Fatalf("cB (MAC_KcB):\n got  %x\n want %x", pc.CBtoA, macKcB)
	}
	_, _, _, _ = Z, V, Ka, KcA
	_ = KcB
	t.Log("SPAKE2+ matches the connectedhomeip reference vector")
}
