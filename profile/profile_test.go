package profile

import "testing"

func lamp() *DeviceProfile {
	return &DeviceProfile{
		Schema: 1, Vendor: "acme", Model: "lamp-1", Protocol: "sim",
		Caps:     []Capability{{ID: "light.onoff", Access: AccessSet | AccessGet | AccessState}},
		MaxTrust: T4,
	}
}

func TestCanonicalCIDStable(t *testing.T) {
	a, err := lamp().CID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := lamp().CID()
	if a != b || a == "" {
		t.Fatalf("CID not stable: %q vs %q", a, b)
	}
	if a[0] != 'b' {
		t.Fatalf("CID must be multibase 'b': %q", a)
	}
}

func TestCanonicalOmitsAbsent(t *testing.T) {
	p := &DeviceProfile{Schema: 1, MaxTrust: T0}
	pre, err := p.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	// map(2 entries): schema + maxtrust only — C-D1 absent≠null.
	if pre[0] != 0xa2 {
		t.Fatalf("expected 2-entry map (0xa2), got 0x%02x", pre[0])
	}
	full, _ := lamp().Canonical()
	if len(full) <= len(pre) {
		t.Fatal("full profile should encode longer than empty one")
	}
}

func TestSchemaRequired(t *testing.T) {
	p := &DeviceProfile{}
	if _, err := p.Canonical(); err == nil {
		t.Fatal("schema 0 must be rejected")
	}
}

func TestDualAxisTrust(t *testing.T) {
	// the two axes are independent and both survive the canonical encoding
	p := &DeviceProfile{Schema: 1, Protocol: "modbus", MaxTrust: V2, AuthTrust: A1,
		Caps: []Capability{{ID: "switch.onoff", Access: AccessSet}}}
	a, err := p.CID()
	if err != nil {
		t.Fatal(err)
	}
	// changing only the auth axis must change the CID (it is CID-significant)
	p2 := &DeviceProfile{Schema: 1, Protocol: "modbus", MaxTrust: V2, AuthTrust: A3,
		Caps: []Capability{{ID: "switch.onoff", Access: AccessSet}}}
	b, _ := p2.CID()
	if a == b {
		t.Fatal("auth_trust must be CID-significant (distinct auth → distinct CID)")
	}
	// A0 is omitted (absent ≡ A0, C-D1): a profile with A0 encodes shorter
	p0 := &DeviceProfile{Schema: 1, Protocol: "modbus", MaxTrust: V2,
		Caps: []Capability{{ID: "switch.onoff", Access: AccessSet}}}
	e0, _ := p0.Canonical()
	e1, _ := p.Canonical()
	if len(e0) >= len(e1) {
		t.Fatal("A0 auth_trust must be omitted from the encoding")
	}
}

func TestLegacyAliases(t *testing.T) {
	// T0–T4 must remain usable and identical to V0–V4
	if T0 != V0 || T2 != V2 || T4 != V4 {
		t.Fatal("legacy Trust aliases drifted from VerifyTrust")
	}
}

func TestClampAuth(t *testing.T) {
	if ClampAuth(A4, A1) != A1 {
		t.Fatal("auth ceiling must win")
	}
	if ClampAuth(A1, A4) != A1 {
		t.Fatal("declared auth below ceiling must survive")
	}
}

func TestClamp(t *testing.T) {
	if Clamp(T4, T1) != T1 {
		t.Fatal("ceiling must win")
	}
	if Clamp(T1, T4) != T1 {
		t.Fatal("declared below ceiling must survive")
	}
}
