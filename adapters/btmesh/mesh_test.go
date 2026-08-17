package btmesh

import (
	"bytes"
	"context"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

func hx(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type memSink struct {
	mu      sync.Mutex
	devices map[string]sdk.Device
}

func (m *memSink) UpsertDevice(d sdk.Device)        { m.mu.Lock(); m.devices[d.ID] = d; m.mu.Unlock() }
func (m *memSink) RemoveDevice(id string)           { m.mu.Lock(); delete(m.devices, id); m.mu.Unlock() }
func (m *memSink) State(string, map[string]float64) {}
func (m *memSink) get(id string) (sdk.Device, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	return d, ok
}

// TestAESCMACRFC4493 pins our CMAC against the RFC 4493 test vectors — the
// MAC every mesh key derivation depends on.
func TestAESCMACRFC4493(t *testing.T) {
	key := hx(t, "2b7e151628aed2a6abf7158809cf4f3c")
	cases := []struct{ msg, want string }{
		{"", "bb1d6929e95937287fa37d129b756746"},
		{"6bc1bee22e409f96e93d7e117393172a", "070a16b46b4d4144f79bdd9dd04a287c"},
		{"6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411",
			"dfa66747de9ae63030ca32611497c827"},
	}
	for _, c := range cases {
		got, err := aesCMAC(key, hx(t, c.msg))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, hx(t, c.want)) {
			t.Fatalf("CMAC(%s):\n got  %x\n want %s", c.msg, got, c.want)
		}
	}
}

// TestCCMRoundTrip proves our pure-Go CCM encrypts and decrypts, and that a
// tampered tag or ciphertext is rejected.
func TestCCMRoundTrip(t *testing.T) {
	key := hx(t, "000102030405060708090a0b0c0d0e0f")
	nonce := hx(t, "00000003020100a0a1a2a3a4a5")
	pt := []byte("mesh access payload")

	ct, err := ccmSeal(key, nonce, pt, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(ct) != len(pt)+4 {
		t.Fatalf("ciphertext length %d, want %d", len(ct), len(pt)+4)
	}
	got, err := ccmOpen(key, nonce, ct, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip: %q", got)
	}

	// tampering must fail authentication
	bad := append([]byte(nil), ct...)
	bad[0] ^= 0xFF
	if _, err := ccmOpen(key, nonce, bad, 4); err != ErrCCMAuth {
		t.Fatal("tampered ciphertext must fail authentication")
	}
	bad = append([]byte(nil), ct...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := ccmOpen(key, nonce, bad, 4); err != ErrCCMAuth {
		t.Fatal("tampered tag must fail authentication")
	}
	// wrong key must fail too
	wrong := hx(t, "0f0e0d0c0b0a09080706050403020100")
	if _, err := ccmOpen(wrong, nonce, ct, 4); err != ErrCCMAuth {
		t.Fatal("wrong key must fail authentication")
	}
}

func TestOpcodeCodec(t *testing.T) {
	cases := []uint32{0x00, 0x52, OpGenericOnOffGet, OpGenericOnOffStatus, 0xC00102}
	for _, op := range cases {
		b := EncodeOpcode(op)
		got, n, err := DecodeOpcode(b)
		if err != nil || n != len(b) || got != op {
			t.Fatalf("opcode %06x round-trip: got %06x n=%d err=%v", op, got, n, err)
		}
	}
	if _, _, err := DecodeOpcode(nil); err == nil {
		t.Fatal("empty opcode must error")
	}
	if _, _, err := DecodeOpcode([]byte{0x82}); err == nil {
		t.Fatal("truncated 2-byte opcode must error")
	}
}

func TestKeyDerivation(t *testing.T) {
	netKey := hx(t, "7dd7364cd842ad18c17c2b820c84c3d6")
	nid, enc, priv, err := k2(netKey, []byte{0x00})
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != 16 || len(priv) != 16 {
		t.Fatalf("k2 key lengths: %d %d", len(enc), len(priv))
	}
	if nid > 0x7F {
		t.Fatalf("NID must be 7 bits: %02x", nid)
	}
	// deterministic
	nid2, enc2, _, _ := k2(netKey, []byte{0x00})
	if nid != nid2 || !bytes.Equal(enc, enc2) {
		t.Fatal("k2 must be deterministic")
	}
	appKey := hx(t, "63964771734fbd76e3b40519d1d94a48")
	aid, err := k4(appKey)
	if err != nil {
		t.Fatal(err)
	}
	if aid > 0x3F {
		t.Fatalf("AID must be 6 bits: %02x", aid)
	}
}

func testAdapter() (*Adapter, *DeviceConfig, *memSink) {
	dev := DeviceConfig{
		ID: "mesh-lamp", Vendor: "acme", Model: "meshbulb",
		AppKey: bytes.Repeat([]byte{0x11}, 16),
		NetKey: bytes.Repeat([]byte{0x22}, 16),
		Elements: []ElementConfig{{ID: "1", Address: 0x0005, Models: []ModelMap{
			{ID: "switch.onoff", Opcode: OpGenericOnOffGet, SetOp: OpGenericOnOffSet},
			{ID: "sensor.temperature", Opcode: OpSensorGet, Scale: 0.01, Unit: "°C"},
		}}},
	}
	a := New(Config{Devices: []DeviceConfig{dev}, SrcAddr: 0x0001})
	sink := &memSink{devices: map[string]sdk.Device{}}
	return a, &a.cfg.Devices[0], sink
}

func TestProfileProjection(t *testing.T) {
	a, _, sink := testAdapter()
	if err := a.Start(context.Background(), nil, sink); err != nil {
		t.Fatal(err)
	}
	d, ok := sink.get("mesh-lamp")
	if !ok {
		t.Fatal("device not exposed")
	}
	if d.Profile.MaxTrust != profile.V2 || d.Profile.AuthTrust != profile.A2 {
		t.Fatalf("mesh trust axes must be V2/A2: V%d/A%d", d.Profile.MaxTrust, d.Profile.AuthTrust)
	}
	c, ok := d.Profile.Find("switch.onoff")
	if !ok || c.Access&profile.AccessSet == 0 {
		t.Fatalf("on/off model must be settable: %+v", c)
	}
	c, _ = d.Profile.Find("sensor.temperature")
	if c.Access&profile.AccessSet != 0 {
		t.Fatal("sensor model must be read-only")
	}
}

// TestAccessPDURoundTrip encrypts an access message and decrypts it back
// through the adapter — the mesh security layer end to end.
func TestAccessPDURoundTrip(t *testing.T) {
	a, dev, sink := testAdapter()
	_ = a.Start(context.Background(), nil, sink)

	// build a Generic OnOff Status as a node would send it
	pt := append(EncodeOpcode(OpGenericOnOffStatus), 0x01)
	nonce := AppNonce(7, 0x0005, a.cfg.SrcAddr, a.cfg.IVIndex)
	ct, err := EncryptAccess(dev.AppKey, nonce, pt)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverAccessPDU(dev, 0x0005, 7, ct); err != nil {
		t.Fatalf("decrypt incoming status: %v", err)
	}

	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "mesh-lamp", Capability: "switch.onoff"})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["switch_onoff"] != 1 {
		t.Fatalf("status decode wrong: %+v", eff)
	}
	if eff.Evidence == nil || eff.Evidence.Protocol != "btmesh" || eff.Evidence.AuthTrust != 2 {
		t.Fatalf("evidence wrong: %+v", eff.Evidence)
	}
}

func TestForgedPDURejected(t *testing.T) {
	a, dev, _ := testAdapter()
	pt := append(EncodeOpcode(OpGenericOnOffStatus), 0x01)
	nonce := AppNonce(9, 0x0005, a.cfg.SrcAddr, a.cfg.IVIndex)
	ct, _ := EncryptAccess(bytes.Repeat([]byte{0x99}, 16), nonce, pt) // wrong AppKey
	if err := a.DeliverAccessPDU(dev, 0x0005, 9, ct); err != ErrAuthFail {
		t.Fatalf("PDU encrypted with the wrong AppKey must be rejected, got %v", err)
	}
}

func TestNoStatusIsUnverified(t *testing.T) {
	a, _, sink := testAdapter()
	_ = a.Start(context.Background(), nil, sink)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "mesh-lamp", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unverified {
		t.Fatalf("a set with no status back must be UNVERIFIED: %+v", eff)
	}
}

func TestProbeRequiresKeys(t *testing.T) {
	av := advBackend{Config{Devices: []DeviceConfig{{ID: "unprovisioned"}}}}.Probe(context.Background())
	if av.Available {
		t.Fatal("a node without keys must not report available")
	}
	if av.Detail == "" {
		t.Fatal("probe must explain why")
	}
}
