package quirk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"
	"github.com/ANetResearch/ANetLink/profile"
)

func load(t *testing.T, yaml string) *Registry {
	t.Helper()
	r := NewRegistry()
	if _, err := r.LoadYAML([]byte(yaml)); err != nil {
		t.Fatalf("load: %v", err)
	}
	return r
}

func dev(vendor, model, fw string, caps ...profile.Capability) *profile.DeviceProfile {
	return &profile.DeviceProfile{
		Schema: 1, Vendor: vendor, Model: model, Firmware: fw, Protocol: "zigbee",
		Caps: caps, MaxTrust: profile.V2, AuthTrust: profile.A2,
	}
}

// TestTheReasonThisPackageExists: an Aqara temperature sensor reports
// centi-degrees, so 22.34 °C arrives as 2234. Without this layer the
// integration faithfully tells its user the room is 2234 degrees.
func TestTheReasonThisPackageExists(t *testing.T) {
	r := load(t, `
quirks:
  - id: quirk.aqara.wsdcgq11lm.centidegrees
    note: this family reports centi-degrees on the temperature attribute
    match: {vendor: Aqara, model: WSDCGQ11LM}
    fixes:
      - {type: scale, capability: sensor.temperature, factor: 0.01}
`)
	p := dev("Aqara", "WSDCGQ11LM", "", profile.Capability{ID: "sensor.temperature", Unit: "°C"})
	a := r.For(p, "")
	if a.Empty() {
		t.Fatal("the quirk must match its own device")
	}

	raw := effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"sensor_temperature": 2234}},
	}
	got := a.ApplyEffect(raw)
	if v := got.Record.Metrics["sensor_temperature"]; v < 22.33 || v > 22.35 {
		t.Fatalf("2234 centi-degrees must read as 22.34 °C, got %g", v)
	}
	// And the correction must be visible, or nobody can audit it.
	if got.Evidence == nil || got.Evidence.Quirk != "quirk.aqara.wsdcgq11lm.centidegrees" {
		t.Fatalf("the applied quirk must be recorded in evidence: %+v", got.Evidence)
	}
	// The original effect must not have been mutated underneath the adapter.
	if raw.Record.Metrics["sensor_temperature"] != 2234 {
		t.Fatal("the adapter's own effect was mutated")
	}
}

// TestQuirksCannotRaiseTrust is the rule that keeps the trust model out of
// the hands of whoever ships a YAML file.
func TestQuirksCannotRaiseTrust(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.trust
    match: {vendor: Acme, model: X}
    fixes:
      - {type: scale, capability: sensor.x, factor: 2}
`)
	p := dev("Acme", "X", "", profile.Capability{ID: "sensor.x"})
	p.MaxTrust = profile.V1
	p.AuthTrust = profile.A0
	out := r.For(p, "").ApplyProfile(p)
	if out.MaxTrust != profile.V1 || out.AuthTrust != profile.A0 {
		t.Fatalf("a quirk must not move the trust axes: V%d/A%d", out.MaxTrust, out.AuthTrust)
	}
}

// TestFirmwareKeyedMatching: the same model on two firmwares behaves
// differently, which is why firmware is a match key at all.
func TestFirmwareKeyedMatching(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.old
    match: {vendor: Tuya, model: TS0601, firmware: "<1.0.5"}
    fixes: [{type: scale, capability: climate.temperature, factor: 0.1}]
  - id: q.new
    match: {vendor: Tuya, model: TS0601, firmware: ">=1.0.5"}
    fixes: [{type: scale, capability: climate.temperature, factor: 0.01}]
`)
	p := dev("Tuya", "TS0601", "", profile.Capability{ID: "climate.temperature"})

	if got := r.For(p, "1.0.2").IDs(); got != "q.old" {
		t.Fatalf("firmware 1.0.2 → %q, want q.old", got)
	}
	if got := r.For(p, "1.0.9").IDs(); got != "q.new" {
		t.Fatalf("firmware 1.0.9 → %q, want q.new", got)
	}
	// An unknown firmware satisfies neither: "we do not know" must not be
	// read as "it matches", or a quirk for one revision corrupts another.
	if got := r.For(p, ""); !got.Empty() {
		t.Fatalf("unknown firmware must match no firmware-keyed quirk, got %q", got.IDs())
	}
}

// TestVendorWildcard: Tuya-family devices identify as _TZE200_xxxxxxxx with
// a per-batch suffix, so the vendor key has to be a prefix.
func TestVendorWildcard(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.tze200
    match: {vendor: "_TZE200_*", model: TS0601}
    fixes: [{type: scale, capability: climate.temperature, factor: 0.1}]
`)
	for _, v := range []string{"_TZE200_a1b2c3d4", "_TZE200_zzzz"} {
		if r.For(dev(v, "TS0601", ""), "").Empty() {
			t.Fatalf("%s should match the _TZE200_ prefix", v)
		}
	}
	if !r.For(dev("_TZE204_other", "TS0601", ""), "").Empty() {
		t.Fatal("a different prefix must not match")
	}
}

// TestMostSpecificWins: two quirks matching one device must resolve the
// same way every run, and must not compound into a doubled scale.
func TestMostSpecificWins(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.broad
    match: {vendor: Acme}
    fixes: [{type: scale, capability: sensor.t, factor: 0.1}]
  - id: q.exact
    match: {vendor: Acme, model: M1, firmware: "1.0"}
    fixes: [{type: scale, capability: sensor.t, factor: 0.01}]
`)
	a := r.For(dev("Acme", "M1", "1.0"), "1.0")
	if len(a.Quirks) != 2 {
		t.Fatalf("both match; got %d", len(a.Quirks))
	}
	if a.Quirks[0].ID != "q.exact" {
		t.Fatalf("the most specific must come first, got %s", a.Quirks[0].ID)
	}
	// 1000 scaled once by 0.01 is 10; scaled by both would be 1.
	got, _ := a.ApplyMetrics(map[string]float64{"sensor_t": 1000})
	if got["sensor_t"] != 10 {
		t.Fatalf("corrections must not compound: got %g, want 10", got["sensor_t"])
	}
}

func TestEnumRemap(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.mode
    note: this vendor orders hvac modes differently from the standard
    match: {vendor: Acme, model: T1}
    fixes:
      - type: enum_remap
        capability: climate.mode
        map: {"0": 2, "1": 0, "2": 1}
`)
	a := r.For(dev("Acme", "T1", ""), "")
	for in, want := range map[float64]float64{0: 2, 1: 0, 2: 1} {
		got, _ := a.ApplyMetrics(map[string]float64{"climate_mode": in})
		if got["climate_mode"] != want {
			t.Fatalf("mode %g → %g, want %g", in, got["climate_mode"], want)
		}
	}
	// A value outside the map is left alone rather than zeroed.
	got, _ := a.ApplyMetrics(map[string]float64{"climate_mode": 7})
	if got["climate_mode"] != 7 {
		t.Fatalf("an unmapped enum value must pass through, got %g", got["climate_mode"])
	}
}

// TestDropCapability covers the commonest lie in the field: a device that
// advertises a capability it does not implement.
func TestDropCapability(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.nocolor
    note: this batch advertises colour temperature and ignores every write
    match: {vendor: Acme, model: L1}
    fixes: [{type: drop_capability, capability: light.color_temp}]
`)
	p := dev("Acme", "L1", "",
		profile.Capability{ID: "light.onoff"},
		profile.Capability{ID: "light.color_temp"})
	out := r.For(p, "").ApplyProfile(p)
	if _, ok := out.Find("light.color_temp"); ok {
		t.Fatal("the phantom capability must be gone")
	}
	if _, ok := out.Find("light.onoff"); !ok {
		t.Fatal("but the real one must remain")
	}
	// The adapter's own profile must be untouched: two runtimes with
	// different quirk sets hold different views of the same hardware.
	if _, ok := p.Find("light.color_temp"); !ok {
		t.Fatal("the source profile was mutated")
	}
}

func TestRenameAndUnit(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.rename
    match: {vendor: Acme, model: S1}
    fixes:
      - {type: rename, capability: sensor.value, to: sensor.temperature}
      - {type: unit, capability: sensor.temperature, unit: "°C"}
`)
	p := dev("Acme", "S1", "", profile.Capability{ID: "sensor.value", Unit: "raw"})
	a := r.For(p, "")
	out := a.ApplyProfile(p)
	c, ok := out.Find("sensor.temperature")
	if !ok {
		t.Fatalf("rename did not take: %+v", out.Caps)
	}
	if c.Unit != "°C" {
		t.Fatalf("unit fix did not take: %q", c.Unit)
	}
	m, _ := a.ApplyMetrics(map[string]float64{"sensor_value": 21})
	if m["sensor_temperature"] != 21 {
		t.Fatalf("the metric must be renamed with the capability: %+v", m)
	}
	if _, still := m["sensor_value"]; still {
		t.Fatal("the old metric name must not survive the rename")
	}
}

func TestClampReportsPhysicallyPossibleValues(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.clamp
    note: this sensor emits 0x8000 as a sentinel when its probe is open
    match: {vendor: Acme, model: P1}
    fixes: [{type: clamp, capability: sensor.temperature, min: -40, max: 85}]
`)
	a := r.For(dev("Acme", "P1", ""), "")
	got, _ := a.ApplyMetrics(map[string]float64{"sensor_temperature": 32768})
	if got["sensor_temperature"] != 85 {
		t.Fatalf("a sentinel must be clamped to the possible range, got %g", got["sensor_temperature"])
	}
	got, _ = a.ApplyMetrics(map[string]float64{"sensor_temperature": 21.5})
	if got["sensor_temperature"] != 21.5 {
		t.Fatalf("an in-range value must pass through untouched, got %g", got["sensor_temperature"])
	}
}

// TestNoMatchIsFree: a device nothing matches must come through byte for
// byte, with no evidence label suggesting a correction happened.
func TestNoMatchIsFree(t *testing.T) {
	r := load(t, `
quirks:
  - id: q.other
    match: {vendor: Other, model: X}
    fixes: [{type: scale, capability: sensor.t, factor: 0.01}]
`)
	a := r.For(dev("Acme", "M", ""), "")
	if !a.Empty() {
		t.Fatal("nothing should match")
	}
	in := effect.Effect{Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"sensor_t": 100}}}
	out := a.ApplyEffect(in)
	if out.Record.Metrics["sensor_t"] != 100 {
		t.Fatal("an unmatched device must not be corrected")
	}
	if out.Evidence != nil && out.Evidence.Quirk != "" {
		t.Fatal("no correction happened, so nothing may be labelled")
	}
}

// TestValidationRejectsQuirksThatCannotWork, at load time where somebody is
// watching, rather than silently doing nothing for months.
func TestValidationRejectsQuirksThatCannotWork(t *testing.T) {
	bad := map[string]string{
		"no id":           `quirks: [{match: {vendor: A}, fixes: [{type: scale, factor: 2}]}]`,
		"matches all":     `quirks: [{id: q, match: {}, fixes: [{type: scale, factor: 2}]}]`,
		"no fixes":        `quirks: [{id: q, match: {vendor: A}, fixes: []}]`,
		"zero factor":     `quirks: [{id: q, match: {vendor: A}, fixes: [{type: scale, factor: 0}]}]`,
		"unknown type":    `quirks: [{id: q, match: {vendor: A}, fixes: [{type: transmute}]}]`,
		"empty enum map":  `quirks: [{id: q, match: {vendor: A}, fixes: [{type: enum_remap}]}]`,
		"rename no to":    `quirks: [{id: q, match: {vendor: A}, fixes: [{type: rename, capability: a}]}]`,
		"clamp no bounds": `quirks: [{id: q, match: {vendor: A}, fixes: [{type: clamp}]}]`,
		"clamp inverted":  `quirks: [{id: q, match: {vendor: A}, fixes: [{type: clamp, min: 10, max: 1}]}]`,
	}
	for name, y := range bad {
		r := NewRegistry()
		if _, err := r.LoadYAML([]byte(y)); err == nil {
			t.Errorf("%s: must be rejected at load", name)
		}
	}
}

// TestDuplicateIDsRejected: two quirks with one id make the outcome depend
// on load order, which a correction layer must never do.
func TestDuplicateIDsRejected(t *testing.T) {
	r := NewRegistry()
	y := `quirks: [{id: q.same, match: {vendor: A}, fixes: [{type: scale, factor: 2}]}]`
	if _, err := r.LoadYAML([]byte(y)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadYAML([]byte(y)); err == nil {
		t.Fatal("a duplicate id must be refused")
	}
}

func TestFirmwareComparison(t *testing.T) {
	// Field firmware is not semver and never will be.
	cases := []struct {
		version, constraint string
		want                bool
	}{
		{"V5.6.5 build 200316", ">=5.6.0", true},
		{"V5.6.5 build 200316", ">=5.7.0", false},
		{"0.0.0_0038", ">=0.0.0", true},
		{"1.0.9", "<1.0.5", false},
		{"1.0.2", "<1.0.5", true},
		{"2.800.0000009.9.R", ">=2.800", true},
		{"1.4.7", "=1.4.7", true},
		{"1.4.8", "1.4.7", false},
	}
	for _, c := range cases {
		got, err := firmwareSatisfies(c.version, c.constraint)
		if err != nil {
			t.Fatalf("%s %s: %v", c.version, c.constraint, err)
		}
		if got != c.want {
			t.Errorf("%q satisfies %q → %v, want %v", c.version, c.constraint, got, c.want)
		}
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(
		`quirks: [{id: q.a, match: {vendor: A}, fixes: [{type: scale, factor: 2}]}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "b.yml"), []byte(
		`quirks: [{id: q.b, match: {vendor: B}, fixes: [{type: scale, factor: 3}]}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)

	r := NewRegistry()
	n, err := r.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || r.Len() != 2 {
		t.Fatalf("loaded %d (registry %d), want 2", n, r.Len())
	}
	// A bad file must fail the load rather than be skipped quietly.
	os.WriteFile(filepath.Join(dir, "c.yaml"), []byte(`quirks: [{id: q.c, match: {}}]`), 0o644)
	if _, err := NewRegistry().LoadDir(dir); err == nil || !strings.Contains(err.Error(), "c.yaml") {
		t.Fatalf("a bad file must name itself in the error, got %v", err)
	}
}
