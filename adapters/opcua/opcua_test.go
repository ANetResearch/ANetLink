package opcua

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/ua"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

type memSink struct{ devices map[string]sdk.Device }

func (m *memSink) UpsertDevice(d sdk.Device)        { m.devices[d.ID] = d }
func (m *memSink) RemoveDevice(id string)           { delete(m.devices, id) }
func (m *memSink) State(string, map[string]float64) {}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

// inProcServer runs a real gopcua OPC UA server with a writable map
// namespace — real protocol, no external dependency (the OPC UA analogue of
// mbserver/mochi). Returns the endpoint and the namespace index.
func inProcServer(t *testing.T) (endpoint string, nsIdx uint16) {
	t.Helper()
	port := freePort(t)
	endpoint = fmt.Sprintf("opc.tcp://127.0.0.1:%d", port)
	opts := []server.Option{
		server.EnableSecurity("None", ua.MessageSecurityModeNone),
		server.EnableAuthMode(ua.UserTokenTypeAnonymous),
		server.EndPoint("127.0.0.1", port),
	}
	s := server.New(opts...)
	ns := server.NewMapNamespace(s, "AnetTest")
	ns.Data["Temperature"] = 22.5
	ns.Data["Setpoint"] = 20.0
	ns.Data["Switch"] = false
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	time.Sleep(150 * time.Millisecond)
	return endpoint, ns.ID()
}

func startAdapter(t *testing.T, endpoint string, nsIdx uint16) (*Adapter, *memSink) {
	t.Helper()
	a := New(Config{Devices: []DeviceConfig{{
		ID: "plc-1", Endpoint: endpoint, Vendor: "test", Model: "sim",
		Nodes: []NodeMap{
			{ID: "sensor.temperature", NodeID: fmt.Sprintf("ns=%d;s=Temperature", nsIdx), Access: profile.AccessGet, Unit: "°C"},
			{ID: "climate.setpoint", NodeID: fmt.Sprintf("ns=%d;s=Setpoint", nsIdx), Access: profile.AccessSet | profile.AccessGet, Unit: "°C"},
			{ID: "switch.onoff", NodeID: fmt.Sprintf("ns=%d;s=Switch", nsIdx), Access: profile.AccessSet | profile.AccessGet},
		},
	}}, Timeout: 3 * time.Second})
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
	ep, ns := inProcServer(t)
	_, sink := startAdapter(t, ep, ns)
	d, ok := sink.devices["plc-1"]
	if !ok || !d.Online {
		t.Fatalf("device missing/offline: %+v", d)
	}
	if d.Profile.MaxTrust != profile.T2 {
		t.Fatalf("opcua readback grade must be T2, got %d", d.Profile.MaxTrust)
	}
	if _, ok := d.Profile.Find("sensor.temperature"); !ok {
		t.Fatal("temperature capability not exposed")
	}
}

func TestReadVariable(t *testing.T) {
	ep, ns := inProcServer(t)
	a, _ := startAdapter(t, ep, ns)
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "plc-1", Capability: "sensor.temperature"})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["sensor_temperature"] != 22.5 {
		t.Fatalf("read wrong: %+v", eff)
	}
}

func TestWriteVerifiedByReadback(t *testing.T) {
	ep, ns := inProcServer(t)
	a, _ := startAdapter(t, ep, ns)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "plc-1", Capability: "climate.setpoint", Args: map[string]any{"value": 23.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["climate_setpoint"] != 23.5 {
		t.Fatalf("write+readback wrong: %+v", eff)
	}
}

func TestUnknownCapabilityFails(t *testing.T) {
	ep, ns := inProcServer(t)
	a, _ := startAdapter(t, ep, ns)
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "plc-1", Capability: "ghost"})
	if err != nil || eff.Status != effect.Failed {
		t.Fatalf("unknown capability must FAIL: %+v %v", eff, err)
	}
}

func TestUnreachableUnavailable(t *testing.T) {
	a := New(Config{Devices: []DeviceConfig{{
		ID: "dead", Endpoint: "opc.tcp://127.0.0.1:1", Nodes: []NodeMap{
			{ID: "sensor.x", NodeID: "ns=1;s=X", Access: profile.AccessGet},
		},
	}}, Timeout: time.Second})
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, _ := sdk.Select(context.Background(), "", a.Backends()...)
	_ = a.Start(context.Background(), b, sink)
	if sink.devices["dead"].Online {
		t.Fatal("unreachable endpoint must report offline")
	}
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "dead", Capability: "sensor.x"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("dead endpoint must be UNAVAILABLE: %+v", eff)
	}
}
