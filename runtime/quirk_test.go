package runtime

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/quirk"
	"github.com/ANetResearch/ANetLink/sdk"
)

// deviatingAdapter stands in for the hardware this layer exists for: it
// reports centi-degrees on the wire, exactly as an Aqara WSDCGQ11LM does.
type deviatingAdapter struct {
	sink sdk.Sink
	raw  float64
}

func (a *deviatingAdapter) Info() sdk.Info {
	return sdk.Info{Name: "fake", Protocol: "zigbee", Version: "0"}
}
func (a *deviatingAdapter) Backends() []sdk.Backend    { return []sdk.Backend{okBackend{}} }
func (a *deviatingAdapter) Stop(context.Context) error { return nil }

func (a *deviatingAdapter) Start(_ context.Context, _ sdk.Backend, sink sdk.Sink) error {
	a.sink = sink
	sink.UpsertDevice(sdk.Device{
		ID: "sensor-1",
		Profile: &profile.DeviceProfile{
			Schema: 1, Vendor: "Aqara", Model: "WSDCGQ11LM", Firmware: "0.0.0_0038",
			Protocol: "zigbee",
			Caps: []profile.Capability{
				{ID: "sensor.temperature", Access: profile.AccessGet | profile.AccessState, Unit: "raw"},
				{ID: "light.color_temp", Access: profile.AccessSet}, // advertised, not implemented
			},
			MaxTrust: profile.V2, AuthTrust: profile.A2,
		},
		Online: true,
	})
	return nil
}

func (a *deviatingAdapter) Invoke(context.Context, sdk.Call) (effect.Effect, error) {
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"sensor_temperature": a.raw}},
	}, nil
}

type okBackend struct{}

func (okBackend) Name() string { return "native" }
func (okBackend) Probe(context.Context) sdk.Availability {
	return sdk.Availability{Available: true, Detail: "fake"}
}

const testQuirks = `
quirks:
  - id: q.centi
    note: reports hundredths
    match: {vendor: Aqara, model: WSDCGQ11LM}
    fixes:
      - {type: scale, capability: sensor.temperature, factor: 0.01}
      - {type: unit, capability: sensor.temperature, unit: "°C"}
      - {type: drop_capability, capability: light.color_temp}
`

func quirkedRuntime(t *testing.T, raw float64) (*Runtime, *deviatingAdapter) {
	t.Helper()
	reg := quirk.NewRegistry()
	if _, err := reg.LoadYAML([]byte(testQuirks)); err != nil {
		t.Fatal(err)
	}
	rt := New(Config{Quirks: reg})
	a := &deviatingAdapter{raw: raw}
	if err := rt.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Stop(context.Background()) })
	return rt, a
}

// TestCorrectionReachesEverySurface is the whole point: a daemon north of
// the runtime must never see the raw vendor units, on any path. Correcting
// invoke but not the state stream would be worse than not correcting at
// all, because the two would disagree about the same capability.
func TestCorrectionReachesEverySurface(t *testing.T) {
	rt, a := quirkedRuntime(t, 2234)
	key := DeviceKey{"fake", "sensor-1"}

	// 1. the published profile
	devs := rt.Devices()
	if len(devs) != 1 {
		t.Fatalf("devices: %d", len(devs))
	}
	c, ok := devs[0].Profile.Find("sensor.temperature")
	if !ok {
		t.Fatal("capability missing")
	}
	if c.Unit != "°C" {
		t.Fatalf("the unit fix must reach the published profile, got %q", c.Unit)
	}
	if _, still := devs[0].Profile.Find("light.color_temp"); still {
		t.Fatal("the phantom capability must not be published")
	}

	// 2. invoke
	eff, err := rt.Invoke(context.Background(), local, key, "sensor.temperature", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := eff.Record.Metrics["sensor_temperature"]; v < 22.33 || v > 22.35 {
		t.Fatalf("invoke must report 22.34 °C, got %g", v)
	}
	if eff.Evidence == nil || eff.Evidence.Quirk != "q.centi" {
		t.Fatalf("the correction must be labelled in evidence: %+v", eff.Evidence)
	}

	// 3. the unsolicited state stream
	a.sink.State("sensor-1", map[string]float64{"sensor_temperature": 2234})
	st, ok := rt.LastState(key)
	if !ok {
		t.Fatal("no state recorded")
	}
	if v := st["sensor_temperature"]; v < 22.33 || v > 22.35 {
		t.Fatalf("the state stream must be corrected too, got %g", v)
	}
}

// TestRawProfileStaysInspectable: a correction you cannot compare against
// the original is a correction you cannot review.
func TestRawProfileStaysInspectable(t *testing.T) {
	rt, _ := quirkedRuntime(t, 2234)
	applied, rawCaps, ok := rt.QuirksFor(DeviceKey{"fake", "sensor-1"})
	if !ok {
		t.Fatal("device not found")
	}
	if len(applied) != 1 || applied[0] != "q.centi" {
		t.Fatalf("applied quirks: %v", applied)
	}
	var sawPhantom bool
	for _, c := range rawCaps {
		if c == "light.color_temp" {
			sawPhantom = true
		}
	}
	if !sawPhantom {
		t.Fatal("the raw profile must still show what the device claimed")
	}
}

// TestWithoutQuirksNothingChanges keeps the layer honest about being
// opt-in: a runtime with no registry must publish exactly what the adapter
// reported, wrong units and phantom capabilities included.
func TestWithoutQuirksNothingChanges(t *testing.T) {
	rt := New(Config{})
	a := &deviatingAdapter{raw: 2234}
	if err := rt.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer rt.Stop(context.Background())

	eff, err := rt.Invoke(context.Background(), local, DeviceKey{"fake", "sensor-1"}, "sensor.temperature", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Record.Metrics["sensor_temperature"] != 2234 {
		t.Fatal("with no quirks the raw reading must pass through untouched")
	}
	if eff.Evidence != nil && eff.Evidence.Quirk != "" {
		t.Fatal("no correction happened, so nothing may be labelled")
	}
}

// TestQuirksNeverRaiseTrustThroughTheRuntime: the trust downgrade rule must
// still be the last word, whatever a quirk file says.
func TestQuirksNeverRaiseTrustThroughTheRuntime(t *testing.T) {
	reg := quirk.NewRegistry()
	if _, err := reg.LoadYAML([]byte(testQuirks)); err != nil {
		t.Fatal(err)
	}
	rt := New(Config{Quirks: reg})
	a := &deviatingLowTrust{}
	if err := rt.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer rt.Stop(context.Background())

	eff, err := rt.Invoke(context.Background(), local, DeviceKey{"fake", "sensor-1"}, "sensor.temperature", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status == effect.OK {
		t.Fatalf("a V1 device cannot report a verified effect, corrected or not: %+v", eff)
	}
}

// deviatingLowTrust reports the same deviation on a device whose trust
// ceiling does not admit verification.
type deviatingLowTrust struct{ deviatingAdapter }

func (a *deviatingLowTrust) Start(_ context.Context, _ sdk.Backend, sink sdk.Sink) error {
	a.sink = sink
	sink.UpsertDevice(sdk.Device{
		ID: "sensor-1",
		Profile: &profile.DeviceProfile{
			Schema: 1, Vendor: "Aqara", Model: "WSDCGQ11LM", Protocol: "zigbee",
			Caps:     []profile.Capability{{ID: "sensor.temperature", Access: profile.AccessGet}},
			MaxTrust: profile.V1, AuthTrust: profile.A0,
		},
		Online: true,
	})
	return nil
}
