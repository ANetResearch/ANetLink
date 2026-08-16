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

func TestClamp(t *testing.T) {
	if Clamp(T4, T1) != T1 {
		t.Fatal("ceiling must win")
	}
	if Clamp(T1, T4) != T1 {
		t.Fatal("declared below ceiling must survive")
	}
}
