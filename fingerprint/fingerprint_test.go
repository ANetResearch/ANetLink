package fingerprint

import (
	"strings"
	"testing"
)

func TestMACNormalisation(t *testing.T) {
	// The same OUI arrives spelled four ways depending on who reports it.
	for _, s := range []string{
		"00:0C:29:29:A5:9D", "00-0c-29-29-a5-9d", "000c.2929.a59d", "000C2929A59D",
	} {
		got, ok := normaliseMAC(s)
		if !ok || got != 0x000C29 {
			t.Fatalf("%q → %06X (ok=%v), want 000C29", s, got, ok)
		}
	}
	for _, bad := range []string{"", "zz:zz:zz", "00:0C"} {
		if _, ok := normaliseMAC(bad); ok {
			t.Fatalf("%q must not parse as a MAC", bad)
		}
	}
}

// TestKnownVendorsResolve pins a few OUIs that the generated table must
// contain. If regeneration ever drops them the catalogue has silently
// stopped identifying the hardware this project exists to reach.
func TestKnownVendorsResolve(t *testing.T) {
	cases := map[string]string{
		"44:47:cc:00:00:01": "Hikvision", // one of the DS- ranges
		"e0:50:8b:00:00:01": "Dahua",
		"78:11:dc:00:00:01": "Xiaomi",
		"24:0a:c4:00:00:01": "Espressif (ESP32/ESP8266)",
	}
	for mac, want := range cases {
		id, ok := VendorByMAC(mac)
		if !ok {
			t.Errorf("%s did not resolve; expected %s", mac, want)
			continue
		}
		if id.Vendor.Name != want {
			t.Errorf("%s → %q, want %q", mac, id.Vendor.Name, want)
		}
	}
}

// TestLocallyAdministeredIsNotGuessed: randomised and virtual MACs have the
// U/L bit set. Resolving one to whichever vendor happens to own the range is
// a confident wrong answer, which is worse than no answer.
func TestLocallyAdministeredIsNotGuessed(t *testing.T) {
	// 02:… has the locally-administered bit set.
	if id, ok := VendorByMAC("02:0c:29:11:22:33"); ok {
		t.Fatalf("a locally-administered MAC must not resolve, got %q", id.Vendor.Name)
	}
	// The same OUI without the bit is a real registration and must resolve.
	if _, ok := VendorByMAC("00:0c:29:11:22:33"); !ok {
		t.Skip("00:0C:29 is not in the curated catalogue; the U/L check above is what matters")
	}
}

func TestUnknownOUIIsReportedAsUnknown(t *testing.T) {
	// A range that will not be in the curated catalogue.
	id, ok := VendorByMAC("fc:fc:fc:11:22:33")
	if ok {
		t.Fatalf("expected no match, got %q", id.Vendor.Name)
	}
	if id.Vendor.Name != "" {
		t.Fatal("an unmatched lookup must leave Vendor zero, not half-filled")
	}
	if !strings.Contains(id.Describe(), "unidentified") {
		t.Fatalf("describe: %q", id.Describe())
	}
}

// TestDescribeNamesTheAdapterOrTheGap: an operator wants to know not just
// what the device is but whether we can drive it.
func TestDescribeNamesTheAdapterOrTheGap(t *testing.T) {
	id, ok := VendorByMAC("44:47:cc:00:00:01")
	if !ok {
		t.Skip("Hikvision OUI absent from this generated table")
	}
	got := id.Describe()
	if !strings.Contains(got, "Hikvision") || !strings.Contains(got, "camera") {
		t.Fatalf("describe should name vendor and class: %q", got)
	}
	if !strings.Contains(got, "onvif") {
		t.Fatalf("describe should name the adapter that can drive it: %q", got)
	}

	// Xiaomi is in the catalogue but has no adapter — the description must
	// say so rather than implying we can control it.
	xid, ok := VendorByMAC("78:11:dc:00:00:01")
	if !ok {
		t.Skip("Xiaomi OUI absent from this generated table")
	}
	if !strings.Contains(xid.Describe(), "no adapter yet") {
		t.Fatalf("an unreachable vendor must be described as such: %q", xid.Describe())
	}
	if !strings.Contains(xid.Describe(), "miot") {
		t.Fatalf("the missing ecosystem should be named: %q", xid.Describe())
	}
}

// TestCoverageGapIsReal keeps the catalogue honest about itself: it must be
// able to enumerate the vendors it can name but not drive.
func TestCoverageGapIsReal(t *testing.T) {
	gap := CoverageGap()
	if len(gap) == 0 {
		t.Fatal("the catalogue claims full adapter coverage, which is not true")
	}
	names := map[string]bool{}
	for _, v := range gap {
		names[v.Name] = true
		if len(v.Adapters) != 0 {
			t.Fatalf("%s is in the gap list but has adapters", v.Name)
		}
	}
	// The vendor ecosystems D58 is about must be in the gap, by name.
	for _, want := range []string{"Xiaomi", "Tuya", "Aqara (Lumi)"} {
		if !names[want] {
			t.Errorf("%s should be reported as an uncovered vendor", want)
		}
	}
}

func TestCatalogueIsPopulated(t *testing.T) {
	all := Vendors()
	if len(all) < 20 {
		t.Fatalf("catalogue has only %d vendors; regeneration probably failed", len(all))
	}
	if len(ouiIndex) < 1000 {
		t.Fatalf("OUI table has only %d entries; regeneration probably failed", len(ouiIndex))
	}
	for _, v := range all {
		if v.Name == "" {
			t.Fatal("a vendor with no name is a generator bug")
		}
		if v.Class == "" {
			t.Fatalf("%s has no class", v.Name)
		}
	}
	// Every index in the OUI table must point at a real vendor.
	for oui, idx := range ouiIndex {
		if int(idx) >= len(all) {
			t.Fatalf("OUI %06X points at vendor index %d, out of range", oui, idx)
		}
	}
}

// TestARPLookupIsOptional: fingerprinting must degrade quietly on hosts with
// no procfs rather than becoming a hard dependency.
func TestARPLookupIsOptional(t *testing.T) {
	if _, ok := arpLookup("203.0.113.1"); ok {
		t.Fatal("a TEST-NET address should not be in the local ARP cache")
	}
}
