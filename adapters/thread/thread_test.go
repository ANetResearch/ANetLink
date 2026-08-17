package thread

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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

// fakeOTBR serves the subset of the OpenThread Border Router REST API the
// adapter consumes — the real wire shape, no bundled binary.
func fakeOTBR(t *testing.T, state string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/node", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"State": state, "NetworkName": "anet-thread", "Rloc16": 0x4400,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testConfig(brURL string) Config {
	return Config{
		BorderRouterURL: brURL,
		Devices: []DeviceConfig{{
			ID: "sensor-1", Address: "fd00::1", Vendor: "acme", Model: "threadsensor",
			Resources: []ResourceMap{
				{ID: "sensor.temperature", Path: "/temp", Access: profile.AccessGet, Unit: "°C", Scale: 0.01},
				{ID: "switch.onoff", Path: "/light", Access: profile.AccessSet | profile.AccessGet},
			},
		}},
		Timeout: 2 * time.Second,
	}
}

func TestProbePrefersHostOTBR(t *testing.T) {
	srv := fakeOTBR(t, "leader")
	a := New(testConfig(srv.URL))
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "host-otbr" {
		t.Fatalf("an existing border router must win: %s", b.Name())
	}
	av := hostBackend{a.cfg}.Probe(context.Background())
	if !av.Available || av.Detail == "" {
		t.Fatalf("probe should report the Thread state: %+v", av)
	}
}

func TestProbeReportsUnreachableBorderRouter(t *testing.T) {
	a := New(testConfig("http://127.0.0.1:1"))
	av := hostBackend{a.cfg}.Probe(context.Background())
	if av.Available {
		t.Fatal("unreachable border router must not report available")
	}
	// with no managed binary either, Select must fail loudly
	if _, err := sdk.Select(context.Background(), "", a.Backends()...); err == nil {
		t.Fatal("no usable backend must be an error, not a silent no-op")
	}
}

// TestManagedBackendStaysInert: the cross-language exemption must be off
// unless explicitly configured, so a plain build is pure Go.
func TestManagedBackendStaysInert(t *testing.T) {
	av := managedBackend{Config{}}.Probe(context.Background())
	if av.Available {
		t.Fatal("managed OTBR must be unavailable when not configured")
	}
	if av.Detail == "" {
		t.Fatal("probe must explain that the managed backend is disabled")
	}
	// configured binary that does not exist is also honestly unavailable
	av = managedBackend{Config{Managed: ManagedOTBR{Binary: "/definitely/not/otbr-agent", RadioURL: "spinel+hdlc+uart:///dev/ttyACM0"}}}.Probe(context.Background())
	if av.Available {
		t.Fatal("missing otbr-agent binary must not report available")
	}
	t.Logf("managed backend without a bundle: %s", av.Detail)
}

func TestStartExposesDevicesAndState(t *testing.T) {
	srv := fakeOTBR(t, "router")
	a := New(testConfig(srv.URL))
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, _ := sdk.Select(context.Background(), "", a.Backends()...)
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())

	d, ok := sink.get("sensor-1")
	if !ok || !d.Online {
		t.Fatalf("device should be exposed and online: %+v", d)
	}
	if d.Profile.MaxTrust != profile.V2 || d.Profile.AuthTrust != profile.A2 {
		t.Fatalf("thread trust axes must be V2/A2: V%d/A%d", d.Profile.MaxTrust, d.Profile.AuthTrust)
	}
	st, err := a.State(context.Background())
	if err != nil || st != "router" {
		t.Fatalf("border router state: %q %v", st, err)
	}
}

// TestInvokeUnreachableDeviceIsHonest: the CoAP path exists now, so an
// unreachable device must time out honestly rather than invent a reading.
func TestInvokeUnreachableDeviceIsHonest(t *testing.T) {
	srv := fakeOTBR(t, "leader")
	a := New(testConfig(srv.URL))
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, _ := sdk.Select(context.Background(), "", a.Backends()...)
	_ = a.Start(context.Background(), b, sink)
	defer a.Stop(context.Background())

	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "sensor-1", Capability: "sensor.temperature"})
	if err != nil {
		t.Fatal(err)
	}
	// fd00::1 is not on this host: the request must fail, not fabricate.
	if eff.Status != effect.Unavailable {
		t.Fatalf("unreachable device must be UNAVAILABLE, never a fake value: %+v", eff)
	}
	if eff.Record != nil {
		t.Fatal("no metrics may be reported when the device never answered")
	}

	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "ghost", Capability: "x"})
	if eff.Status != effect.Failed {
		t.Fatalf("unknown device must FAIL: %+v", eff)
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "sensor-1", Capability: "ghost"})
	if eff.Status != effect.Failed {
		t.Fatalf("unknown capability must FAIL: %+v", eff)
	}
}

// --- CoAP device path ---

// fakeCoAPDevice answers CoAP GET/PUT on a real UDP socket, so both
// directions of our codec meet a real wire.
type fakeCoAPDevice struct {
	conn  *net.UDPConn
	mu    sync.Mutex
	value string
}

func newCoAPDevice(t *testing.T, initial string) *fakeCoAPDevice {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6loopback, Port: 0})
	if err != nil {
		t.Skipf("no IPv6 loopback available: %v", err)
	}
	d := &fakeCoAPDevice{conn: conn, value: initial}
	go d.serve()
	t.Cleanup(func() { conn.Close() })
	return d
}

func (d *fakeCoAPDevice) port() int { return d.conn.LocalAddr().(*net.UDPAddr).Port }

func (d *fakeCoAPDevice) serve() {
	buf := make([]byte, 1500)
	for {
		n, peer, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		m, derr := decodeCoAP(buf[:n])
		if derr != nil {
			continue
		}
		d.mu.Lock()
		if m.Code == codePUT && len(m.Payload) > 0 {
			d.value = string(m.Payload)
		}
		val := d.value
		d.mu.Unlock()

		code := codeContent
		payload := []byte(val)
		if m.Code == codePUT {
			code = codeChanged
			payload = nil
		}
		resp := encodeCoAP(typeACK, code, m.MessageID, m.Token, "", payload)
		_, _ = d.conn.WriteToUDP(resp, peer)
	}
}

func TestCoAPCodec(t *testing.T) {
	req := encodeCoAP(typeCON, codeGET, 0x1234, []byte{1, 2, 3, 4}, "/sensors/temp", nil)
	m, err := decodeCoAP(req)
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != typeCON || m.Code != codeGET || m.MessageID != 0x1234 {
		t.Fatalf("header round-trip: %+v", m)
	}
	if len(m.Token) != 4 || m.Token[0] != 1 {
		t.Fatalf("token round-trip: %x", m.Token)
	}

	withPayload := encodeCoAP(typeACK, codeContent, 0x0001, nil, "", []byte("21.5"))
	m, err = decodeCoAP(withPayload)
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Payload) != "21.5" {
		t.Fatalf("payload round-trip: %q", m.Payload)
	}
	for _, bad := range [][]byte{nil, {0x40}, {0xC0, 0x01, 0x00, 0x00}} {
		if _, err := decodeCoAP(bad); err == nil {
			t.Fatalf("malformed CoAP %x must be rejected", bad)
		}
	}
}

func TestCoAPReadAgainstRealSocket(t *testing.T) {
	dev := newCoAPDevice(t, "2150")
	srv := fakeOTBR(t, "leader")
	cfg := testConfig(srv.URL)
	cfg.Devices[0].Address = "::1"
	a := New(cfg)
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, _ := sdk.Select(context.Background(), "", a.Backends()...)
	_ = a.Start(context.Background(), b, sink)
	defer a.Stop(context.Background())

	// Reaching the device requires the well-known CoAP port; on a test host we
	// cannot bind 5683, so exercise the request path directly against the
	// device's ephemeral port instead.
	m, err := a.coapRequestAt(context.Background(), fmt.Sprintf("[::1]:%d", dev.port()), "/temp", codeGET, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !coapClassOK(m.Code) || string(m.Payload) != "2150" {
		t.Fatalf("CoAP read: code=%02x payload=%q", m.Code, m.Payload)
	}
}

func TestCoAPWriteThenReadBack(t *testing.T) {
	dev := newCoAPDevice(t, "0")
	a := New(testConfig("http://127.0.0.1:1"))
	addr := fmt.Sprintf("[::1]:%d", dev.port())

	if _, err := a.coapRequestAt(context.Background(), addr, "/light", codePUT, []byte("1")); err != nil {
		t.Fatal(err)
	}
	m, err := a.coapRequestAt(context.Background(), addr, "/light", codeGET, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Payload) != "1" {
		t.Fatalf("write did not take: %q", m.Payload)
	}
}

func TestCoAPTimeoutIsHonest(t *testing.T) {
	a := New(Config{Timeout: 300 * time.Millisecond})
	_, err := a.coapRequestAt(context.Background(), "[::1]:1", "/x", codeGET, nil)
	if err == nil {
		t.Fatal("an unreachable device must not appear to succeed")
	}
}
