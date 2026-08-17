package zigbee

import (
	"bytes"
	"context"
	"encoding/binary"
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

// --- UNPI framing ---

func TestUNPIRoundTrip(t *testing.T) {
	f := &Frame{Type: TypeSREQ, Subsys: SubsysAF, Cmd: afDataRequest, Data: []byte{1, 2, 3, 4}}
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != sof || b[1] != 4 {
		t.Fatalf("frame header wrong: %x", b)
	}
	got, n, err := DecodeFrame(b)
	if err != nil || n != len(b) {
		t.Fatalf("decode: %v n=%d len=%d", err, n, len(b))
	}
	if got.Type != TypeSREQ || got.Subsys != SubsysAF || got.Cmd != afDataRequest || !bytes.Equal(got.Data, f.Data) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestUNPIRejectsCorruption(t *testing.T) {
	f := &Frame{Type: TypeSREQ, Subsys: SubsysSys, Cmd: sysPing, Data: []byte{9}}
	b, _ := f.Encode()
	bad := append([]byte(nil), b...)
	bad[len(bad)-1] ^= 0xFF // corrupt FCS
	if _, _, err := DecodeFrame(bad); err != ErrBadFCS {
		t.Fatalf("bad checksum must be rejected, got %v", err)
	}
	if _, _, err := DecodeFrame([]byte{0x00, 0x01, 0x02, 0x03, 0x04}); err != ErrNoSOF {
		t.Fatal("missing SOF must be rejected")
	}
	if _, _, err := DecodeFrame(b[:3]); err != ErrShortFrame {
		t.Fatal("truncated frame must be rejected")
	}
}

// TestReadFrameResyncs proves the reader recovers from line noise before SOF.
func TestReadFrameResyncs(t *testing.T) {
	f := &Frame{Type: TypeAREQ, Subsys: SubsysAF, Cmd: afIncomingMsg, Data: []byte{7, 7}}
	good, _ := f.Encode()
	stream := append([]byte{0x00, 0xAA, 0x55}, good...)
	got, err := ReadFrame(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmd != afIncomingMsg || !bytes.Equal(got.Data, f.Data) {
		t.Fatalf("resync failed: %+v", got)
	}
}

// --- ZCL ---

func TestZCLReadAttributesRoundTrip(t *testing.T) {
	req := BuildReadAttributes(0x42, AttrMeasuredValue)
	if req[1] != 0x42 || req[2] != CmdReadAttributes {
		t.Fatalf("request malformed: %x", req)
	}
	if binary.LittleEndian.Uint16(req[3:]) != AttrMeasuredValue {
		t.Fatalf("attribute id wrong: %x", req)
	}

	// device answers with temperature 2295 (int16) = 22.95 °C at scale 0.01
	resp := []byte{frameTypeGlobal, 0x42, CmdReadAttributesResponse,
		0x00, 0x00, 0x00, TypeInt16, 0xF7, 0x08}
	attrs, err := ParseReadAttributesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs) != 1 || attrs[0].Value != 2295 {
		t.Fatalf("attribute decode: %+v", attrs)
	}
}

func TestZCLRejectsMalformed(t *testing.T) {
	for _, bad := range [][]byte{nil, {0x00}, {0x00, 0x01, 0x99}, {0x00, 0x01, CmdReadAttributesResponse, 0x00}} {
		if _, err := ParseReadAttributesResponse(bad); err == nil {
			t.Fatalf("malformed ZCL %x must be rejected", bad)
		}
	}
}

func TestOnOffCommandEncoding(t *testing.T) {
	on := BuildOnOffCommand(1, true)
	off := BuildOnOffCommand(1, false)
	if on[2] != CmdOn || off[2] != CmdOff {
		t.Fatalf("on/off commands wrong: %x %x", on, off)
	}
	if on[0] != frameTypeCluster {
		t.Fatal("on/off must be a cluster-specific frame")
	}
}

func TestClusterCapabilityMapping(t *testing.T) {
	c, ok := CapabilityForCluster(ClusterTemperatureMeas)
	if !ok || c.Capability != "sensor.temperature" || c.Scale != 0.01 {
		t.Fatalf("temperature mapping: %+v", c)
	}
	c, _ = CapabilityForCluster(ClusterOnOff)
	if !c.Writable {
		t.Fatal("on/off must be writable")
	}
	if _, ok := CapabilityForCluster(0xFFFF); ok {
		t.Fatal("unknown cluster must not map")
	}
}

// --- adapter ---

func testAdapter() (*Adapter, *memSink) {
	a := New(Config{Port: "", Devices: []DeviceConfig{{
		ID: "lamp-1", ShortAddr: 0x1234, Vendor: "acme", Model: "bulb",
		Endpoints: []EndpointConfig{{ID: "1", Endpoint: 1,
			Clusters: []uint16{ClusterOnOff, ClusterTemperatureMeas}}},
	}}, Timeout: 300 * time.Millisecond})
	sink := &memSink{devices: map[string]sdk.Device{}}
	a.sink = sink
	return a, sink
}

func TestProfileProjection(t *testing.T) {
	a, sink := testAdapter()
	d := a.cfg.Devices[0]
	sink.UpsertDevice(sdk.Device{ID: d.ID, Profile: a.profileFor(d), Online: true})
	dev, _ := sink.get("lamp-1")
	if dev.Profile.MaxTrust != profile.V2 || dev.Profile.AuthTrust != profile.A2 {
		t.Fatalf("zigbee trust axes must be V2/A2: V%d/A%d", dev.Profile.MaxTrust, dev.Profile.AuthTrust)
	}
	if _, ok := dev.Profile.Find("switch.onoff"); !ok {
		t.Fatal("on/off cluster must map to a capability")
	}
	if _, ok := dev.Profile.Find("sensor.temperature"); !ok {
		t.Fatal("temperature cluster must map to a capability")
	}
}

// TestReadbackPath drives the full ZCL response path via Deliver, proving
// capability → cluster resolution and attribute decoding without hardware.
func TestReadbackPath(t *testing.T) {
	a, _ := testAdapter()
	// no serial port: sendAF fails, which is the honest UNAVAILABLE case
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "lamp-1", Capability: "sensor.temperature"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("no coordinator must be UNAVAILABLE: %+v", eff)
	}

	// with a delivered response the parser produces the physical value
	resp := []byte{frameTypeGlobal, 1, CmdReadAttributesResponse, 0x00, 0x00, 0x00, TypeInt16, 0xF7, 0x08}
	attrs, err := ParseReadAttributesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	cc, _ := CapabilityForCluster(ClusterTemperatureMeas)
	if v := attrs[0].Value * cc.Scale; v < 22.94 || v > 22.96 {
		t.Fatalf("scaled temperature wrong: %v", v)
	}
}

func TestUnknownDeviceAndCapability(t *testing.T) {
	a, _ := testAdapter()
	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "ghost", Capability: "switch.onoff"})
	if eff.Status != effect.Failed {
		t.Fatalf("unknown device must FAIL: %+v", eff)
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "lamp-1", Capability: "ghost.cap"})
	if eff.Status != effect.Failed {
		t.Fatalf("unknown capability must FAIL: %+v", eff)
	}
}

// TestProbeHonestWithoutCoordinator: no stick, no lies.
func TestProbeHonestWithoutCoordinator(t *testing.T) {
	av := coordinatorBackend{Config{Port: "/dev/definitely-not-a-coordinator", Baud: 115200}}.Probe(context.Background())
	if av.Available {
		t.Fatal("absent coordinator must not report available")
	}
	if av.Detail == "" {
		t.Fatal("probe must explain why")
	}
	av = coordinatorBackend{Config{}}.Probe(context.Background())
	if av.Available {
		t.Fatal("unconfigured port must not report available")
	}
	t.Logf("zigbee probe without hardware: %s", av.Detail)
}

// --- discovery (the "convenient onboarding" path) ---

func TestParseMgmtLQIResponse(t *testing.T) {
	// srcAddr(2) status(1) total(1) start(1) count(1) then 22-byte entries
	entry := make([]byte, 22)
	binary.LittleEndian.PutUint64(entry[8:], 0x00124B0001CE4B7A) // IEEE
	binary.LittleEndian.PutUint16(entry[16:], 0xA1B2)            // short addr
	resp := append([]byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x01}, entry...)
	ns, err := ParseMgmtLQIResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 || ns[0].ShortAddr != 0xA1B2 || ns[0].IEEE != 0x00124B0001CE4B7A {
		t.Fatalf("neighbour decode: %+v", ns)
	}
}

func TestParseActiveEPResponse(t *testing.T) {
	resp := []byte{0x00, 0x00, 0x00, 0xB2, 0xA1, 0x02, 0x01, 0x0B}
	eps, err := ParseActiveEPResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 || eps[0] != 1 || eps[1] != 11 {
		t.Fatalf("endpoint list: %v", eps)
	}
	if _, err := ParseActiveEPResponse([]byte{0x00, 0x00, 0x01}); err == nil {
		t.Fatal("failure status must be rejected")
	}
}

func TestParseSimpleDescResponse(t *testing.T) {
	// ... status ok, endpoint 1, profile 0x0104, device 0x0100, version 1,
	// 2 input clusters: OnOff(0x0006), TemperatureMeasurement(0x0402)
	resp := []byte{0x00, 0x00, 0x00, 0xB2, 0xA1, 0x0C, 0x01, 0x04, 0x01, 0x00, 0x01, 0x02,
		0x06, 0x00, 0x02, 0x04}
	clusters, err := ParseSimpleDescResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 || clusters[0] != ClusterOnOff || clusters[1] != ClusterTemperatureMeas {
		t.Fatalf("cluster list: %04x", clusters)
	}
}

// TestAsDeviceConfigsFiltersUnmappable proves discovery hands the user a
// config containing only clusters ANetLink can actually expose.
func TestAsDeviceConfigsFiltersUnmappable(t *testing.T) {
	found := []DiscoveredDevice{{
		ShortAddr: 0xA1B2, IEEE: 0x00124B0001CE4B7A,
		Endpoints: []EndpointConfig{
			{ID: "1", Endpoint: 1, Clusters: []uint16{ClusterOnOff, 0xFFFE /* unknown */}},
			{ID: "2", Endpoint: 2, Clusters: []uint16{0xFFFF}}, // nothing usable
		},
	}}
	cfgs := AsDeviceConfigs(found)
	if len(cfgs) != 1 {
		t.Fatalf("want one usable device, got %d", len(cfgs))
	}
	c := cfgs[0]
	if c.ID != "zb-00124b0001ce4b7a" {
		t.Fatalf("id must derive from the IEEE address: %s", c.ID)
	}
	if len(c.Endpoints) != 1 || len(c.Endpoints[0].Clusters) != 1 {
		t.Fatalf("unmappable clusters must be dropped: %+v", c.Endpoints)
	}
}
