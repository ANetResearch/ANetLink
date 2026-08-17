// Package session implements the Matter secured session: after PASE (or
// CASE) establishes a shared secret, every subsequent message is encrypted
// with AES-CCM and its header privacy-protected. This is what turns a
// completed handshake into a channel commissioning can run over.
//
// Matter Core Spec §4.7 (message encryption) and §4.8 (privacy).
package session

import (
	"crypto/aes"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"

	"crypto/sha256"
	"golang.org/x/crypto/hkdf"
	"io"
)

// Key lengths and the session key derivation info strings (§4.13.2.6).
const (
	KeyLen   = 16
	nonceLen = 13
	micLen   = 16
)

var (
	infoSessionKeys = []byte("SessionKeys")
	ErrDecrypt      = errors.New("session: decryption or authentication failed")
	ErrShortMessage = errors.New("session: message too short")
)

// Keys are the three keys a Matter session derives from the handshake
// secret: one per direction plus the attestation challenge.
type Keys struct {
	I2R       []byte // initiator -> responder encryption key
	R2I       []byte // responder -> initiator encryption key
	Challenge []byte // attestation challenge
}

// Derive expands the PASE/CASE shared secret into session keys
// (HKDF-SHA256, salt is empty for PASE, info "SessionKeys", 48 bytes).
func Derive(sharedSecret, salt []byte) (*Keys, error) {
	if len(sharedSecret) == 0 {
		return nil, errors.New("session: empty shared secret")
	}
	out := make([]byte, 3*KeyLen)
	r := hkdf.New(sha256.New, sharedSecret, salt, infoSessionKeys)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, err
	}
	return &Keys{
		I2R:       out[0:KeyLen],
		R2I:       out[KeyLen : 2*KeyLen],
		Challenge: out[2*KeyLen : 3*KeyLen],
	}, nil
}

// Nonce builds the message nonce: security flags, message counter, and the
// source node id (§4.7.2.1).
func Nonce(securityFlags byte, counter uint32, sourceNodeID uint64) []byte {
	n := make([]byte, nonceLen)
	n[0] = securityFlags
	binary.LittleEndian.PutUint32(n[1:], counter)
	binary.LittleEndian.PutUint64(n[5:], sourceNodeID)
	return n
}

// Encrypt seals a payload for transmission. aad is the message header, which
// is authenticated but not encrypted.
func Encrypt(key, nonce, plaintext, aad []byte) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("session: key must be %d bytes", KeyLen)
	}
	return ccmSeal(key, nonce, plaintext, aad, micLen)
}

// Decrypt opens a received payload, verifying the header as associated data.
func Decrypt(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("session: key must be %d bytes", KeyLen)
	}
	pt, err := ccmOpen(key, nonce, ciphertext, aad, micLen)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// --- AES-CCM with associated data (NIST SP 800-38C) ---

func ccmSeal(key, nonce, plaintext, aad []byte, tagSize int) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	q := 15 - len(nonce)
	if q < 2 {
		return nil, errors.New("session: nonce too long for CCM")
	}

	// B0
	b0 := make([]byte, 16)
	adaFlag := byte(0)
	if len(aad) > 0 {
		adaFlag = 0x40
	}
	b0[0] = adaFlag | byte((tagSize-2)/2)<<3 | byte(q-1)
	copy(b0[1:], nonce)
	putLen(b0[16-q:], len(plaintext))

	mac := make([]byte, 16)
	block.Encrypt(mac, b0)

	// associated data block(s)
	if len(aad) > 0 {
		var enc []byte
		if len(aad) < 0xFF00 {
			enc = make([]byte, 2)
			binary.BigEndian.PutUint16(enc, uint16(len(aad)))
		} else {
			enc = make([]byte, 6)
			enc[0], enc[1] = 0xFF, 0xFE
			binary.BigEndian.PutUint32(enc[2:], uint32(len(aad)))
		}
		buf := append(enc, aad...)
		for i := 0; i < len(buf); i += 16 {
			var blk [16]byte
			copy(blk[:], buf[i:minInt(i+16, len(buf))])
			for j := range blk {
				mac[j] ^= blk[j]
			}
			block.Encrypt(mac, mac)
		}
	}

	// payload blocks
	for i := 0; i < len(plaintext); i += 16 {
		var blk [16]byte
		copy(blk[:], plaintext[i:minInt(i+16, len(plaintext))])
		for j := range blk {
			mac[j] ^= blk[j]
		}
		block.Encrypt(mac, mac)
	}

	// CTR
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
		n := minInt(16, len(plaintext)-i)
		for j := 0; j < n; j++ {
			out[i+j] = plaintext[i+j] ^ ks[j]
		}
	}
	for i := 0; i < tagSize; i++ {
		out[len(plaintext)+i] = mac[i] ^ s0[i]
	}
	return out, nil
}

func ccmOpen(key, nonce, ciphertext, aad []byte, tagSize int) ([]byte, error) {
	if len(ciphertext) < tagSize {
		return nil, ErrShortMessage
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

	plaintext := make([]byte, ctLen)
	ks := make([]byte, 16)
	for i := 0; i < ctLen; i += 16 {
		putLen(a[16-q:], i/16+1)
		block.Encrypt(ks, a)
		n := minInt(16, ctLen-i)
		for j := 0; j < n; j++ {
			plaintext[i+j] = ciphertext[i+j] ^ ks[j]
		}
	}
	want, err := ccmSeal(key, nonce, plaintext, aad, tagSize)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(want[ctLen:], ciphertext[ctLen:]) != 1 {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

func putLen(b []byte, n int) {
	for i := range b {
		b[i] = 0
	}
	for i := len(b) - 1; i >= 0 && n > 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
