package ble

import (
	"context"
	"sync"
	"testing"

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

// realAdvertisingReport builds an HCI LE Advertising Report exactly as the
// kernel delivers it, so the decoder is exercised against real wire layout.
func realAdvertisingReport(mac [6]byte, name string, svcUUID uint16, svcData []byte, rssi int8) []byte {
	var ad []byte
	// complete local name
	ad = append(ad, byte(1+len(name)), 0x09)
	ad = append(ad, name...)
	// service data, 16-bit UUID
	sd := append([]byte{byte(svcUUID), byte(svcUUID >> 8)}, svcData...)
	ad = append(ad, byte(1+len(sd)), 0x16)
	ad = append(ad, sd...)

	pkt := []byte{hciEventPkt, evtLEMeta, 0, subEvtLEAdvReport, 0x01, 0x00, 0x00}
	// address is little-endian on the wire
	for i := 5; i >= 0; i-- {
		pkt = append(pkt, mac[i])
	}
	pkt = append(pkt, byte(len(ad)))
	pkt = append(pkt, ad...)
	pkt = append(pkt, byte(rssi))
	pkt[2] = byte(len(pkt) - 3)
	return pkt
}

func TestParseAdvertisingReport(t *testing.T) {
	mac := [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	// 0x181A = Environmental Sensing; payload: temp 2295 (=22.95 °C at 0.01)
	pkt := realAdvertisingReport(mac, "TempTag", 0x181A, []byte{0xF7, 0x08, 0x40}, -67)
	adv, err := ParseAdvertisingReport(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if adv.Address != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("address decode: %s", adv.Address)
	}
	if adv.LocalName != "TempTag" {
		t.Fatalf("name decode: %q", adv.LocalName)
	}
	if adv.RSSI != -67 {
		t.Fatalf("rssi decode: %d", adv.RSSI)
	}
	sd, ok := adv.ServiceData[0x181A]
	if !ok || len(sd) != 3 || sd[0] != 0xF7 {
		t.Fatalf("service data decode: %x %v", sd, ok)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, bad := range [][]byte{nil, {0x04}, {0x04, 0x3E, 0x02, 0x02}, {0x01, 0x02, 0x03, 0x04, 0x05}} {
		if _, err := ParseAdvertisingReport(bad); err == nil {
			t.Fatalf("garbage %x must not parse", bad)
		}
	}
}

func testAdapter() (*Adapter, *memSink) {
	a := New(Config{Devices: []DeviceConfig{{
		ID: "tag-1", Address: "AA:BB:CC:DD:EE:FF", Vendor: "acme", Model: "envtag",
		Sensors: []SensorMap{
			{ID: "sensor.temperature", ServiceID: 0x181A, Offset: 0, Length: 2, Signed: true, Scale: 0.01, Unit: "°C"},
			{ID: "sensor.humidity", ServiceID: 0x181A, Offset: 2, Length: 1, Scale: 1, Unit: "%"},
		},
	}}})
	sink := &memSink{devices: map[string]sdk.Device{}}
	a.sink = sink
	return a, sink
}

func TestSensorDecodeAndRead(t *testing.T) {
	a, sink := testAdapter()
	mac := [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	pkt := realAdvertisingReport(mac, "TempTag", 0x181A, []byte{0xF7, 0x08, 0x40}, -67)
	adv, err := ParseAdvertisingReport(pkt)
	if err != nil {
		t.Fatal(err)
	}
	a.Ingest(adv)

	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "tag-1", Capability: "sensor.temperature"})
	if err != nil {
		t.Fatal(err)
	}
	got := eff.Record.Metrics["sensor_temperature"]
	if !eff.Verifiable() || got < 22.94 || got > 22.96 {
		t.Fatalf("temperature decode wrong: %+v (got %v)", eff, got)
	}
	if eff.Evidence == nil || eff.Evidence.AuthTrust != 0 || eff.Evidence.NativeAck {
		t.Fatalf("BLE advertisements have no identity and no ack: %+v", eff.Evidence)
	}

	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "tag-1", Capability: "sensor.humidity"})
	if eff.Record.Metrics["sensor_humidity"] != 64 {
		t.Fatalf("humidity decode wrong: %+v", eff)
	}

	// ingesting marks the device online and publishes state
	if d, _ := sink.get("tag-1"); !d.Online {
		t.Fatal("device must come online on first advertisement")
	}
	sink.mu.Lock()
	n := len(sink.states)
	sink.mu.Unlock()
	if n == 0 {
		t.Fatal("state stream must publish decoded sensors")
	}
}

func TestUnseenDeviceAndWriteRejected(t *testing.T) {
	a, _ := testAdapter()
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "tag-1", Capability: "sensor.temperature"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("never-advertised device must be UNAVAILABLE: %+v", eff)
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{
		DeviceID: "tag-1", Capability: "sensor.temperature", Args: map[string]any{"value": 1.0},
	})
	if eff.Status != effect.Failed {
		t.Fatalf("write to an advertisement-only sensor must FAIL: %+v", eff)
	}
}

// TestProbeHonestWithoutRadio: on a host with no Bluetooth controller (CI,
// containers, this dev box) the probe must say so instead of pretending.
func TestProbeHonestWithoutRadio(t *testing.T) {
	av := hciBackend{Config{}}.Probe(context.Background())
	if av.Detail == "" {
		t.Fatal("probe must always explain itself")
	}
	if _, err := hciAvailable(); err != nil && av.Available {
		t.Fatal("probe claims available while no controller can be opened")
	}
	t.Logf("HCI probe on this host: available=%v (%s)", av.Available, av.Detail)
}

// TestSignedDecoding pins the two's-complement path used by real sensors.
func TestSignedDecoding(t *testing.T) {
	adv := &Advertisement{ServiceData: map[uint16][]byte{0x181A: {0x30, 0xF8}}} // -2000
	v, ok := decodeSensor(adv, SensorMap{ServiceID: 0x181A, Offset: 0, Length: 2, Signed: true, Scale: 0.01})
	if !ok || v > -19.99 || v < -20.01 {
		t.Fatalf("signed decode wrong: %v %v", v, ok)
	}
	profileCheck := profile.V2 // referenced so the import stays meaningful
	_ = profileCheck
}
