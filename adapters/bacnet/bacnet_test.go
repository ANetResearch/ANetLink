package bacnet

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

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

// fakeBACnetDevice answers ReadProperty/WriteProperty on Present_Value the
// way ASHRAE 135 specifies — a real UDP responder speaking the real wire
// format, so both directions of our codec are exercised.
type fakeBACnetDevice struct {
	conn   *net.UDPConn
	mu     sync.Mutex
	values map[uint32]float64 // objectID -> present value
}

func newFakeDevice(t *testing.T) *fakeBACnetDevice {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeBACnetDevice{conn: conn, values: map[uint32]float64{
		encodeObjectID(ObjAnalogInput, 1): 21.5,
		encodeObjectID(ObjAnalogValue, 2): 18.0,
		encodeObjectID(ObjBinaryValue, 3): 0,
	}}
	go d.serve()
	t.Cleanup(func() { conn.Close() })
	return d
}

func (d *fakeBACnetDevice) addr() string { return d.conn.LocalAddr().String() }

func (d *fakeBACnetDevice) serve() {
	buf := make([]byte, 1500)
	for {
		n, peer, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := buf[:n]
		if len(pkt) < 10 || pkt[0] != 0x81 {
			continue
		}
		apdu := pkt[6:] // BVLL(4) + NPDU(2)
		if len(apdu) < 4 {
			continue
		}
		invoke, service := apdu[2], apdu[3]
		if len(apdu) < 9 {
			continue
		}
		oid := binary.BigEndian.Uint32(apdu[5:9])

		switch service {
		case 0x0C: // ReadProperty
			d.mu.Lock()
			v := d.values[oid]
			d.mu.Unlock()
			d.conn.WriteToUDP(d.complexAck(invoke, oid, v, isBinary(uint16(oid>>22))), peer)
		case 0x0F: // WriteProperty
			if v, ok := parseWrittenValue(apdu); ok {
				d.mu.Lock()
				d.values[oid] = v
				d.mu.Unlock()
			}
			d.conn.WriteToUDP(d.simpleAck(invoke, service), peer)
		}
	}
}

// complexAck builds a ComplexACK ReadProperty response carrying the value.
func (d *fakeBACnetDevice) complexAck(invoke uint8, oid uint32, v float64, binaryObj bool) []byte {
	apdu := []byte{0x30, invoke, 0x0C, 0x0C}
	var o [4]byte
	binary.BigEndian.PutUint32(o[:], oid)
	apdu = append(apdu, o[:]...)
	apdu = append(apdu, 0x19, byte(propPresentValue), 0x3E)
	if binaryObj {
		b := byte(0)
		if v != 0 {
			b = 1
		}
		apdu = append(apdu, 0x91, b)
	} else {
		var f [4]byte
		binary.BigEndian.PutUint32(f[:], math.Float32bits(float32(v)))
		apdu = append(apdu, 0x44)
		apdu = append(apdu, f[:]...)
	}
	apdu = append(apdu, 0x3F)
	return bvll(append([]byte{0x01, 0x00}, apdu...))
}

func (d *fakeBACnetDevice) simpleAck(invoke, service uint8) []byte {
	return bvll(append([]byte{0x01, 0x00}, 0x20, invoke, service))
}

// parseWrittenValue pulls the value out of a WriteProperty APDU.
func parseWrittenValue(apdu []byte) (float64, bool) {
	for i := 0; i < len(apdu)-1; i++ {
		if apdu[i] == 0x3E {
			v := apdu[i+1:]
			if len(v) >= 5 && v[0] == 0x44 {
				return float64(math.Float32frombits(binary.BigEndian.Uint32(v[1:5]))), true
			}
			if len(v) >= 2 && v[0] == 0x91 {
				return float64(v[1]), true
			}
		}
	}
	return 0, false
}

func startAdapter(t *testing.T, addr string) (*Adapter, *memSink) {
	t.Helper()
	a := New(Config{Devices: []DeviceConfig{{
		ID: "ahu-1", Addr: addr, Vendor: "test", Model: "vav",
		Objects: []ObjectMap{
			{ID: "sensor.temperature", Type: ObjAnalogInput, Instance: 1, Access: profile.AccessGet, Unit: "°C"},
			{ID: "climate.setpoint", Type: ObjAnalogValue, Instance: 2, Access: profile.AccessSet | profile.AccessGet, Unit: "°C"},
			{ID: "switch.onoff", Type: ObjBinaryValue, Instance: 3, Access: profile.AccessSet | profile.AccessGet},
		},
	}}, Timeout: 2 * time.Second})
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, sink
}

func TestDiscoveryAndTrust(t *testing.T) {
	dev := newFakeDevice(t)
	_, sink := startAdapter(t, dev.addr())
	d, ok := sink.get("ahu-1")
	if !ok {
		t.Fatal("device missing")
	}
	if d.Profile.MaxTrust != profile.V2 || d.Profile.AuthTrust != profile.A1 {
		t.Fatalf("BACnet trust axes must be V2/A1, got V%d/A%d", d.Profile.MaxTrust, d.Profile.AuthTrust)
	}
	if _, ok := d.Profile.Find("climate.setpoint"); !ok {
		t.Fatal("setpoint capability missing")
	}
}

func TestReadAnalogInput(t *testing.T) {
	dev := newFakeDevice(t)
	a, _ := startAdapter(t, dev.addr())
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "ahu-1", Capability: "sensor.temperature"})
	if err != nil {
		t.Fatal(err)
	}
	got := eff.Record.Metrics["sensor_temperature"]
	if !eff.Verifiable() || got < 21.49 || got > 21.51 {
		t.Fatalf("read wrong: %+v", eff)
	}
	if eff.Evidence == nil || eff.Evidence.Protocol != "bacnet" {
		t.Fatalf("evidence missing: %+v", eff.Evidence)
	}
}

func TestWriteAnalogValueVerified(t *testing.T) {
	dev := newFakeDevice(t)
	a, _ := startAdapter(t, dev.addr())
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "ahu-1", Capability: "climate.setpoint", Args: map[string]any{"value": 22.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := eff.Record.Metrics["climate_setpoint"]
	if !eff.Verifiable() || got < 22.49 || got > 22.51 {
		t.Fatalf("write+readback wrong: %+v (got %v)", eff, got)
	}
}

func TestWriteBinaryValue(t *testing.T) {
	dev := newFakeDevice(t)
	a, _ := startAdapter(t, dev.addr())
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "ahu-1", Capability: "switch.onoff", Args: map[string]any{"value": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["switch_onoff"] != 1 {
		t.Fatalf("binary write wrong: %+v", eff)
	}
}

func TestUnknownCapabilityAndDeadDevice(t *testing.T) {
	dev := newFakeDevice(t)
	a, _ := startAdapter(t, dev.addr())
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "ahu-1", Capability: "ghost"})
	if err != nil || eff.Status != effect.Failed {
		t.Fatalf("unknown capability must FAIL: %+v", eff)
	}

	dead := New(Config{Devices: []DeviceConfig{{
		ID: "dead", Addr: "127.0.0.1:1",
		Objects: []ObjectMap{{ID: "sensor.x", Type: ObjAnalogInput, Instance: 9, Access: profile.AccessGet}},
	}}, Timeout: 400 * time.Millisecond})
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, _ := sdk.Select(context.Background(), "", dead.Backends()...)
	_ = dead.Start(context.Background(), b, sink)
	eff, err = dead.Invoke(context.Background(), sdk.Call{DeviceID: "dead", Capability: "sensor.x"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("dead device must be UNAVAILABLE: %+v", eff)
	}
}

// TestParseIAm pins BACnet discovery decoding — how a building network
// announces itself.
func TestParseIAm(t *testing.T) {
	// BVLL + NPDU + unconfirmed I-Am with device instance 1234
	apdu := []byte{0x10, 0x00, 0xC4}
	var oid [4]byte
	binary.BigEndian.PutUint32(oid[:], (8<<22)|1234) // object type 8 = device
	apdu = append(apdu, oid[:]...)
	apdu = append(apdu, 0x22, 0x01, 0xE0) // max APDU 480
	pkt := bvll(append([]byte{0x01, 0x00}, apdu...))

	d, err := ParseIAm(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if d.Instance != 1234 {
		t.Fatalf("instance decode: %d", d.Instance)
	}
	if d.MaxAPDU != 480 {
		t.Fatalf("max APDU decode: %d", d.MaxAPDU)
	}
	if _, err := ParseIAm([]byte{0x81, 0x0b, 0x00, 0x08, 0x01, 0x00, 0x10, 0x08}); err == nil {
		t.Fatal("a Who-Is must not parse as an I-Am")
	}
}
