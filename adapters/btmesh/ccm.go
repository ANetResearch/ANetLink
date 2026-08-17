package btmesh

import (
	"crypto/aes"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

// AES-CCM (NIST SP 800-38C) — Bluetooth Mesh encrypts every layer with it,
// and the Go standard library does not ship it, so it lives here in pure Go
// (K207 A3: write what the protocol needs rather than bind a C library).

var ErrCCMAuth = errors.New("btmesh: CCM authentication failed")

// ccmSeal encrypts plaintext with a tag of tagSize bytes appended.
func ccmSeal(key, nonce, plaintext []byte, tagSize int) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) < 7 || len(nonce) > 13 {
		return nil, errors.New("btmesh: CCM nonce must be 7..13 bytes")
	}
	q := 15 - len(nonce) // length field size
	if q < 2 {
		return nil, errors.New("btmesh: CCM nonce too long")
	}

	// B0: flags | nonce | length
	b0 := make([]byte, 16)
	b0[0] = byte((tagSize-2)/2)<<3 | byte(q-1)
	copy(b0[1:], nonce)
	putLen(b0[16-q:], len(plaintext))

	mac := make([]byte, 16)
	block.Encrypt(mac, b0)

	// CBC-MAC over the padded plaintext (no associated data in mesh usage).
	for i := 0; i < len(plaintext); i += 16 {
		var blk [16]byte
		copy(blk[:], plaintext[i:min(i+16, len(plaintext))])
		for j := range blk {
			mac[j] ^= blk[j]
		}
		block.Encrypt(mac, mac)
	}

	// CTR mode: A0 encrypts the tag, A1.. encrypt the payload.
	a := make([]byte, 16)
	a[0] = byte(q - 1)
	copy(a[1:], nonce)
	s0 := make([]byte, 16)
	block.Encrypt(s0, a)

	out := make([]byte, len(plaintext)+tagSize)
	ks := make([]byte, 16)
	for i := 0; i < len(plaintext); i += 16 {
		putLen(a[16-q:], i/16+1)
		block.Encrypt(ks, a)
		n := min(16, len(plaintext)-i)
		for j := 0; j < n; j++ {
			out[i+j] = plaintext[i+j] ^ ks[j]
		}
	}
	for i := 0; i < tagSize; i++ {
		out[len(plaintext)+i] = mac[i] ^ s0[i]
	}
	return out, nil
}

// ccmOpen decrypts and verifies a CCM ciphertext.
func ccmOpen(key, nonce, ciphertext []byte, tagSize int) ([]byte, error) {
	if len(ciphertext) < tagSize {
		return nil, ErrCCMAuth
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	q := 15 - len(nonce)
	ctLen := len(ciphertext) - tagSize

	a := make([]byte, 16)
	a[0] = byte(q - 1)
	copy(a[1:], nonce)
	s0 := make([]byte, 16)
	block.Encrypt(s0, a)

	plaintext := make([]byte, ctLen)
	ks := make([]byte, 16)
	for i := 0; i < ctLen; i += 16 {
		putLen(a[16-q:], i/16+1)
		block.Encrypt(ks, a)
		n := min(16, ctLen-i)
		for j := 0; j < n; j++ {
			plaintext[i+j] = ciphertext[i+j] ^ ks[j]
		}
	}

	want, err := ccmSeal(key, nonce, plaintext, tagSize)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(want[ctLen:], ciphertext[ctLen:]) != 1 {
		return nil, ErrCCMAuth
	}
	return plaintext, nil
}

func putLen(b []byte, n int) {
	for i := range b {
		b[i] = 0
	}
	binary.BigEndian.PutUint32(make([]byte, 4), uint32(n)) // keep vet quiet about intent
	for i := len(b) - 1; i >= 0 && n > 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
