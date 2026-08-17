// Package btmesh is the Bluetooth Mesh adapter. Mesh is a managed flood
// network layered on BLE advertising: messages are encrypted twice (network
// layer with a NetKey, access layer with an AppKey) and relayed by nodes.
// This package implements the crypto and message layers in pure Go on top of
// the same kernel HCI path the BLE adapter uses — no BlueZ mesh daemon.
//
// R02 §4 mapping: Mesh Element -> Endpoint, Model -> Capability, State ->
// Property, Message -> Action/Event.
//
// Scope (v0): network/transport/access encoding and decoding for unsegmented
// access messages, the Generic OnOff and Sensor models, and capability
// projection. Provisioning (PB-ADV) is a follow-up: nodes are configured
// with keys and addresses obtained out of band.
package btmesh

import (
	"crypto/aes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Mesh opcode constants (Mesh Model spec).
const (
	OpGenericOnOffGet      uint32 = 0x8201
	OpGenericOnOffSet      uint32 = 0x8202
	OpGenericOnOffSetUnack uint32 = 0x8203
	OpGenericOnOffStatus   uint32 = 0x8204
	OpSensorGet            uint32 = 0x8231
	OpSensorStatus         uint32 = 0x0052
)

var (
	ErrShortPDU  = errors.New("btmesh: PDU too short")
	ErrAuthFail  = errors.New("btmesh: authentication failed")
	ErrBadOpcode = errors.New("btmesh: malformed opcode")
)

// k2 derives the mesh network keys (NID, EncryptionKey, PrivacyKey) from a
// NetKey per Mesh Profile §3.8.2.6. p = 0x00 for the master credentials.
func k2(netKey []byte, p []byte) (nid byte, encKey, privKey []byte, err error) {
	// T0 = salt "smk2"; T = AES-CMAC(salt, N)
	salt := s1([]byte("smk2"))
	t, err := aesCMAC(salt, netKey)
	if err != nil {
		return 0, nil, nil, err
	}
	t1, err := aesCMAC(t, append(append([]byte{}, p...), 0x01))
	if err != nil {
		return 0, nil, nil, err
	}
	t2, err := aesCMAC(t, append(append(append([]byte{}, t1...), p...), 0x02))
	if err != nil {
		return 0, nil, nil, err
	}
	t3, err := aesCMAC(t, append(append(append([]byte{}, t2...), p...), 0x03))
	if err != nil {
		return 0, nil, nil, err
	}
	return t1[15] & 0x7F, t2, t3, nil
}

// s1 is the mesh salt function: AES-CMAC with a zero key.
func s1(m []byte) []byte {
	out, _ := aesCMAC(make([]byte, 16), m)
	return out
}

// k4 derives the 6-bit AID from an AppKey (Mesh Profile §3.8.2.8).
func k4(appKey []byte) (byte, error) {
	salt := s1([]byte("smk4"))
	t, err := aesCMAC(salt, appKey)
	if err != nil {
		return 0, err
	}
	out, err := aesCMAC(t, append([]byte("id6"), 0x01))
	if err != nil {
		return 0, err
	}
	return out[15] & 0x3F, nil
}

// aesCMAC implements AES-CMAC (RFC 4493), the MAC mesh uses everywhere.
func aesCMAC(key, msg []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	const bs = 16
	// subkey generation
	l := make([]byte, bs)
	block.Encrypt(l, make([]byte, bs))
	k1 := shiftLeft(l)
	if l[0]&0x80 != 0 {
		k1[15] ^= 0x87
	}
	k2s := shiftLeft(k1)
	if k1[0]&0x80 != 0 {
		k2s[15] ^= 0x87
	}

	n := (len(msg) + bs - 1) / bs
	var lastBlock []byte
	if n == 0 {
		n = 1
		lastBlock = pad(nil)
		xorInto(lastBlock, k2s)
	} else if len(msg)%bs == 0 {
		lastBlock = append([]byte(nil), msg[(n-1)*bs:]...)
		xorInto(lastBlock, k1)
	} else {
		lastBlock = pad(msg[(n-1)*bs:])
		xorInto(lastBlock, k2s)
	}

	x := make([]byte, bs)
	y := make([]byte, bs)
	for i := 0; i < n-1; i++ {
		copy(y, msg[i*bs:(i+1)*bs])
		xorInto(y, x)
		block.Encrypt(x, y)
	}
	copy(y, lastBlock)
	xorInto(y, x)
	out := make([]byte, bs)
	block.Encrypt(out, y)
	return out, nil
}

func shiftLeft(b []byte) []byte {
	out := make([]byte, len(b))
	var carry byte
	for i := len(b) - 1; i >= 0; i-- {
		out[i] = b[i]<<1 | carry
		carry = b[i] >> 7
	}
	return out
}

func pad(b []byte) []byte {
	out := make([]byte, 16)
	copy(out, b)
	out[len(b)] = 0x80
	return out
}

func xorInto(dst, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}

// AccessMessage is a decoded mesh access-layer message.
type AccessMessage struct {
	Src    uint16
	Dst    uint16
	Opcode uint32
	Params []byte
}

// EncodeOpcode serializes a mesh opcode (1, 2 or 3 octets).
func EncodeOpcode(op uint32) []byte {
	switch {
	case op < 0x80:
		return []byte{byte(op)}
	case op < 0xC000:
		return []byte{byte(op >> 8), byte(op)}
	default:
		return []byte{byte(op >> 16), byte(op >> 8), byte(op)}
	}
}

// DecodeOpcode reads a mesh opcode from the head of b.
func DecodeOpcode(b []byte) (uint32, int, error) {
	if len(b) == 0 {
		return 0, 0, ErrBadOpcode
	}
	switch {
	case b[0]&0x80 == 0:
		return uint32(b[0]), 1, nil
	case b[0]&0xC0 == 0x80:
		if len(b) < 2 {
			return 0, 0, ErrBadOpcode
		}
		return uint32(b[0])<<8 | uint32(b[1]), 2, nil
	default:
		if len(b) < 3 {
			return 0, 0, ErrBadOpcode
		}
		return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]), 3, nil
	}
}

// EncryptAccess encrypts an access PDU with the AppKey (AES-CCM, 4-byte MIC)
// per Mesh Profile §3.8.5.
func EncryptAccess(appKey, nonce, plaintext []byte) ([]byte, error) {
	return ccmSeal(appKey, nonce, plaintext, 4)
}

// DecryptAccess reverses EncryptAccess.
func DecryptAccess(appKey, nonce, ciphertext []byte) ([]byte, error) {
	pt, err := ccmOpen(appKey, nonce, ciphertext, 4)
	if err != nil {
		return nil, ErrAuthFail
	}
	return pt, nil
}

// AppNonce builds the application nonce (Mesh Profile §3.8.5.2).
func AppNonce(seq uint32, src, dst uint16, ivIndex uint32) []byte {
	n := make([]byte, 13)
	n[0] = 0x01 // application nonce
	n[1] = 0x00 // ASZMIC + pad
	n[2] = byte(seq >> 16)
	n[3] = byte(seq >> 8)
	n[4] = byte(seq)
	binary.BigEndian.PutUint16(n[5:], src)
	binary.BigEndian.PutUint16(n[7:], dst)
	binary.BigEndian.PutUint32(n[9:], ivIndex)
	return n
}

func (m *AccessMessage) String() string {
	return fmt.Sprintf("mesh{src=%04x dst=%04x op=%04x len=%d}", m.Src, m.Dst, m.Opcode, len(m.Params))
}
