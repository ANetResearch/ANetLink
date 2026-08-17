package can

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.einride.tech/can"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

type memSink struct {
	mu      sync.Mutex
	devices map[string]sdk.Device
	states  []map[string]float64
}

func (m *memSink) UpsertDevice(d sdk.Device) { m.mu.Lock(); m.devices[d.ID] = d; m.mu.Unlock() }
func (m *memSink) RemoveDevice(id string)    { m.mu.Lock(); delete(m.devices, id); m.mu.Unlock() }
func (m *memSink) State(_ string, s map[string]float64) {
	m.mu.Lock()
	m.states = append(m.states, s)
	m.mu.Unlock()
}
func (m *memSink) get(id string) (sdk.Device, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	return d, ok
}

func testConfig(iface string) Config {
	return Config{Devices: []DeviceConfig{{
		ID: "ecu-1", Iface: iface, Vendor: "test", Model: "ecu",
		Signals: []SignalMap{
			{ID: "sensor.rpm", FrameID: 0x100, StartBit: 0, Length: 16, Scale: 0.25, Access: profile.AccessGet | profile.AccessState, Unit: "rpm"},
			{ID: "sensor.temp", FrameID: 0x100, StartBit: 16, Length: 8, Scale: 1, Offset: -40, Access: profile.AccessGet, Unit: "°C"},
			{ID: "switch.fan", FrameID: 0x200, StartBit: 0, Length: 1, Access: profile.AccessSet, Writable: true},
		},
	}}, Timeout: time.Second}
}

// TestSignalCodec pins the bit-level signal encode/decode — the part that is
// pure logic and must be exact regardless of whether a bus is present.
func TestSignalCodec(t *testing.T) {
	var d can.Data
	encodeSignal(&d, 0, 16, 4000) // rpm raw
	encodeSignal(&d, 16, 8, 90)   // temp raw
	if got := decodeSignal(d, 0, 16); got != 4000 {
		t.Fatalf("rpm round-trip: %d", got)
	}
	if got := decodeSignal(d, 16, 8); got != 90 {
		t.Fatalf("temp round-trip: %d", got)
	}
	// a 1-bit signal
	var b can.Data
	encodeSignal(&b, 3, 1, 1)
	if decodeSignal(b, 3, 1) != 1 || decodeSignal(b, 2, 1) != 0 {
		t.Fatalf("single-bit signal wrong: %v", b)
	}
	// overwriting clears
	encodeSignal(&b, 3, 1, 0)
	if decodeSignal(b, 3, 1) != 0 {
		t.Fatal("signal not cleared")
	}
}

// TestIngestAndRead drives the adapter's decode path with frames as if they
// arrived from the bus — no kernel interface required.
func TestIngestAndRead(t *testing.T) {
	a := New(testConfig("vcan-absent"))
	sink := &memSink{devices: map[string]sdk.Device{}}
	a.sink = sink

	var d can.Data
	encodeSignal(&d, 0, 16, 4000) // 4000 * 0.25 = 1000 rpm
	encodeSignal(&d, 16, 8, 90)   // 90 - 40 = 50 °C
	a.Ingest(can.Frame{ID: 0x100, Length: 8, Data: d})

	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "ecu-1", Capability: "sensor.rpm"})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["sensor_rpm"] != 1000 {
		t.Fatalf("rpm decode wrong: %+v", eff)
	}
	if eff.Evidence == nil || eff.Evidence.Protocol != "can" || eff.Evidence.AuthTrust != 0 {
		t.Fatalf("evidence wrong (CAN has no identity → A0): %+v", eff.Evidence)
	}

	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "ecu-1", Capability: "sensor.temp"})
	if eff.Record.Metrics["sensor_temp"] != 50 {
		t.Fatalf("temp offset wrong: %+v", eff)
	}

	// state stream was published for the ingested frame
	sink.mu.Lock()
	n := len(sink.states)
	sink.mu.Unlock()
	if n == 0 {
		t.Fatal("Ingest must publish state updates")
	}
}

func TestUnseenFrameUnavailable(t *testing.T) {
	a := New(testConfig("vcan-absent"))
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "ecu-1", Capability: "switch.fan"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("never-seen frame must be UNAVAILABLE: %+v", eff)
	}
}

func TestUnknownCapability(t *testing.T) {
	a := New(testConfig("vcan-absent"))
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "ecu-1", Capability: "ghost"})
	if err != nil || eff.Status != effect.Failed {
		t.Fatalf("unknown capability must FAIL: %+v", eff)
	}
}

// TestProbeReportsMissingInterface: honesty about a bus that is not there.
func TestProbeReportsMissingInterface(t *testing.T) {
	av := socketBackend{testConfig("vcan-definitely-absent")}.Probe(context.Background())
	if av.Available {
		t.Fatal("absent SocketCAN interface must not report available")
	}
	if av.Detail == "" {
		t.Fatal("probe must explain why")
	}
}

// TestVirtualBus (L1-ish): if a SocketCAN interface exists, exercise the real
// kernel path. Skipped when the environment has none.
func TestVirtualBus(t *testing.T) {
	iface := "vcan0"
	a := New(testConfig(iface))
	av := socketBackend{a.cfg}.Probe(context.Background())
	if !av.Available {
		t.Skipf("no %s interface (set one up with: ip link add dev vcan0 type vcan && ip link set vcan0 up)", iface)
	}
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, _ := sdk.Select(context.Background(), "", a.Backends()...)
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if d, ok := sink.get("ecu-1"); !ok || !d.Online {
		t.Fatalf("device should be online on a live bus: %+v", d)
	}
}
