package session

import (
	"bytes"
	"encoding/hex"
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

// TestCCMNISTVector pins our AES-CCM (with associated data) against the
// worked example from NIST SP 800-38C Appendix C.1 — the same construction
// every Matter message rides on.
func TestCCMNISTVector(t *testing.T) {
	key := hx(t, "404142434445464748494a4b4c4d4e4f")
	nonce := hx(t, "10111213141516")
	aad := hx(t, "0001020304050607")
	pt := hx(t, "20212223")
	want := hx(t, "7162015b4dac255d")

	got, err := ccmSeal(key, nonce, pt, aad, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("CCM example 1:\n got  %x\n want %x", got, want)
	}
	back, err := ccmOpen(key, nonce, got, aad, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, pt) {
		t.Fatalf("round-trip: %x", back)
	}
}

// TestCCMSecondNISTVector uses example 2 (longer payload, 6-byte tag).
func TestCCMSecondNISTVector(t *testing.T) {
	key := hx(t, "404142434445464748494a4b4c4d4e4f")
	nonce := hx(t, "1011121314151617")
	aad := hx(t, "000102030405060708090a0b0c0d0e0f")
	pt := hx(t, "202122232425262728292a2b2c2d2e2f")
	want := hx(t, "d2a1f0e051ea5f62081a7792073d593d1fc64fbfaccd")

	got, err := ccmSeal(key, nonce, pt, aad, 6)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("CCM example 2:\n got  %x\n want %x", got, want)
	}
}

// TestSessionKeyDerivation: one shared secret yields three distinct keys,
// deterministically, and both peers derive the same set.
func TestSessionKeyDerivation(t *testing.T) {
	secret := hx(t, "ce198514440ba18b7ca0ab2f9e777ab4") // a real PASE Ke
	k1, err := Derive(secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := Derive(secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1.I2R, k2.I2R) || !bytes.Equal(k1.R2I, k2.R2I) {
		t.Fatal("derivation must be deterministic")
	}
	if bytes.Equal(k1.I2R, k1.R2I) {
		t.Fatal("the two directions must use different keys")
	}
	if bytes.Equal(k1.Challenge, k1.I2R) || len(k1.Challenge) != KeyLen {
		t.Fatal("attestation challenge must be a distinct 16-byte key")
	}
	// a different secret must give different keys
	other, _ := Derive(hx(t, "00112233445566778899aabbccddeeff"), nil)
	if bytes.Equal(k1.I2R, other.I2R) {
		t.Fatal("different secrets must derive different keys")
	}
	if _, err := Derive(nil, nil); err == nil {
		t.Fatal("empty secret must be rejected")
	}
}

// TestMessageRoundTrip: a message encrypted for one direction decrypts with
// that direction's key, and the header is authenticated.
func TestMessageRoundTrip(t *testing.T) {
	keys, err := Derive(hx(t, "ce198514440ba18b7ca0ab2f9e777ab4"), nil)
	if err != nil {
		t.Fatal(err)
	}
	header := hx(t, "00"+"0100"+"05"+"2a000000") // flags|session|sec|counter
	payload := []byte("read attribute: on/off")
	nonce := Nonce(0x05, 42, 0x0011223344556677)

	ct, err := Encrypt(keys.I2R, nonce, payload, header)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, payload) {
		t.Fatal("payload must not appear in plaintext on the wire")
	}
	got, err := Decrypt(keys.I2R, nonce, ct, header)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip: %q", got)
	}
}

// TestTamperingRejected: the properties that make the session trustworthy.
func TestTamperingRejected(t *testing.T) {
	keys, _ := Derive(hx(t, "ce198514440ba18b7ca0ab2f9e777ab4"), nil)
	header := hx(t, "000100052a000000")
	nonce := Nonce(0x05, 42, 0x0011223344556677)
	ct, err := Encrypt(keys.I2R, nonce, []byte("turn the lock off"), header)
	if err != nil {
		t.Fatal(err)
	}

	// flipped ciphertext byte
	bad := append([]byte(nil), ct...)
	bad[0] ^= 0xFF
	if _, err := Decrypt(keys.I2R, nonce, bad, header); err != ErrDecrypt {
		t.Fatal("tampered ciphertext must be rejected")
	}
	// flipped tag
	bad = append([]byte(nil), ct...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := Decrypt(keys.I2R, nonce, bad, header); err != ErrDecrypt {
		t.Fatal("tampered tag must be rejected")
	}
	// tampered header (authenticated as associated data)
	badHeader := append([]byte(nil), header...)
	badHeader[len(badHeader)-1] ^= 0x01 // pretend a different counter
	if _, err := Decrypt(keys.I2R, nonce, ct, badHeader); err != ErrDecrypt {
		t.Fatal("a tampered header must be rejected — it is authenticated")
	}
	// wrong direction key
	if _, err := Decrypt(keys.R2I, nonce, ct, header); err != ErrDecrypt {
		t.Fatal("the opposite direction key must not open the message")
	}
	// replayed under a different counter (different nonce)
	if _, err := Decrypt(keys.I2R, Nonce(0x05, 43, 0x0011223344556677), ct, header); err != ErrDecrypt {
		t.Fatal("a different message counter must not decrypt")
	}
}

func TestNonceLayout(t *testing.T) {
	n := Nonce(0x05, 0x01020304, 0x1122334455667788)
	if len(n) != nonceLen {
		t.Fatalf("nonce length %d, want %d", len(n), nonceLen)
	}
	if n[0] != 0x05 {
		t.Fatal("security flags must lead the nonce")
	}
	// little-endian counter then little-endian node id
	if n[1] != 0x04 || n[4] != 0x01 {
		t.Fatalf("counter encoding: %x", n[1:5])
	}
	if n[5] != 0x88 || n[12] != 0x11 {
		t.Fatalf("node id encoding: %x", n[5:])
	}
}
