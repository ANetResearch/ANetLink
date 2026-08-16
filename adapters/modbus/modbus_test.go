package modbus

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tbrandon/mbserver"

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

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func testRig(t *testing.T) (*mbserver.Server, *Adapter, *memSink) {
	t.Helper()
	srv := mbserver.NewServer()
	addr := freePort(t)
	if err := srv.ListenTCP(addr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	time.Sleep(20 * time.Millisecond)

	a := New(Config{Devices: []DeviceConfig{{
		ID: "plc-1", Addr: addr, UnitID: 1, Vendor: "test", Model: "rig",
		Caps: []CapMap{
			{ID: "switch.onoff", Kind: "coil", Address: 3, Access: profile.AccessSet | profile.AccessGet},
			{ID: "sensor.temp", Kind: "holding", Address: 7, Access: profile.AccessGet, Scale: 0.1, Unit: "°C"},
		},
	}}})
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return srv, a, sink
}

func TestDiscoveryAndTrust(t *testing.T) {
	_, _, sink := testRig(t)
	d, ok := sink.get("plc-1")
	if !ok || !d.Online {
		t.Fatalf("device missing/offline: %+v", d)
	}
	if d.Profile.MaxTrust != profile.T2 {
		t.Fatalf("modbus readback grade must be T2, got %d", d.Profile.MaxTrust)
	}
}

func TestCoilWriteReadbackVerified(t *testing.T) {
	srv, a, _ := testRig(t)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "plc-1", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["switch_onoff"] != 1 {
		t.Fatalf("expected verified on-state, got %+v", eff)
	}
	if srv.Coils[3] != 1 {
		t.Fatalf("server coil not set: %v", srv.Coils[:5])
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{
		DeviceID: "plc-1", Capability: "switch.onoff", Args: map[string]any{"on": false},
	})
	if eff.Record.Metrics["switch_onoff"] != 0 {
		t.Fatalf("off readback wrong: %+v", eff)
	}
}

func TestHoldingScaledRead(t *testing.T) {
	srv, a, _ := testRig(t)
	srv.HoldingRegisters[7] = 235 // 23.5°C at scale 0.1
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "plc-1", Capability: "sensor.temp"})
	if err != nil {
		t.Fatal(err)
	}
	got := eff.Record.Metrics["sensor_temp"]
	if !eff.Verifiable() || got < 23.49 || got > 23.51 {
		t.Fatalf("scaled read wrong: %+v", eff)
	}
}

func TestUnknownCapabilityFails(t *testing.T) {
	_, a, _ := testRig(t)
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "plc-1", Capability: "ghost"})
	if err != nil || eff.Status != effect.Failed {
		t.Fatalf("unknown capability must FAIL, got %+v %v", eff, err)
	}
}

func TestUnreachableDeviceOffline(t *testing.T) {
	a := New(Config{Devices: []DeviceConfig{{
		ID: "dead-1", Addr: "127.0.0.1:1", UnitID: 1,
		Caps: []CapMap{{ID: "switch.onoff", Kind: "coil", Address: 0, Access: profile.AccessSet}},
	}}})
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, _ := sdk.Select(context.Background(), "", a.Backends()...)
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	if d, _ := sink.get("dead-1"); d.Online {
		t.Fatal("unreachable device must report offline")
	}
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "dead-1", Capability: "switch.onoff", Args: map[string]any{"on": true}})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("dead target must be UNAVAILABLE, got %+v", eff)
	}
}

func (m *memSink) get(id string) (sdk.Device, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	return d, ok
}

func (m *memSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.devices)
}
