package thread

import (
	"context"
	"encoding/json"
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

func TestInvokeIsHonestAboutMissingCoAP(t *testing.T) {
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
	// The device path is not implemented yet — it must say so rather than
	// fabricate a reading.
	if eff.Status != effect.Unavailable {
		t.Fatalf("unimplemented device path must be UNAVAILABLE, never a fake value: %+v", eff)
	}
	if eff.Record != nil {
		t.Fatal("no metrics may be reported for an unimplemented path")
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
