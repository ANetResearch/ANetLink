package mqttbridge

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/sdk"
)

type memSink struct{ devices map[string]sdk.Device }

func (m *memSink) UpsertDevice(d sdk.Device)        { m.devices[d.ID] = d }
func (m *memSink) RemoveDevice(id string)           { delete(m.devices, id) }
func (m *memSink) State(string, map[string]float64) {}

const devicesJSON = `[
  {"friendly_name":"lamp","ieee_address":"0x01","definition":{"vendor":"acme","model":"bulb",
    "exposes":[{"type":"light","features":[
      {"type":"binary","name":"state","property":"state","access":7}]}]}},
  {"friendly_name":"climate","ieee_address":"0x02","definition":{"vendor":"acme","model":"th",
    "exposes":[{"type":"numeric","name":"temperature","property":"temperature","unit":"°C","access":5}]}}
]`

// fakeZ2M: in-process broker + a responder that acts like zigbee2mqtt.
func fakeZ2M(t *testing.T, echo bool) (broker string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	s := mochi.New(&mochi.Options{InlineClient: true})
	_ = s.AddHook(new(auth.AllowHook), nil)
	if err := s.AddListener(listeners.NewTCP(listeners.Config{ID: "t", Address: addr})); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Close() })
	time.Sleep(30 * time.Millisecond)

	if err := s.Publish("zigbee2mqtt/bridge/devices", []byte(devicesJSON), true, 0); err != nil {
		t.Fatal(err)
	}
	if echo {
		// echo /set back as retained-less state, like a live device
		_ = s.Subscribe("zigbee2mqtt/lamp/set", 1, func(_ *mochi.Client, _ packets.Subscription, pk packets.Packet) {
			var req map[string]string
			_ = json.Unmarshal(pk.Payload, &req)
			resp, _ := json.Marshal(map[string]string{"state": req["state"]})
			_ = s.Publish("zigbee2mqtt/lamp", resp, false, 0)
		})
		_ = s.Subscribe("zigbee2mqtt/climate/get", 1, func(_ *mochi.Client, _ packets.Subscription, pk packets.Packet) {
			_ = s.Publish("zigbee2mqtt/climate", []byte(`{"temperature":23.5}`), false, 0)
		})
	}
	return "tcp://" + addr
}

func startAdapter(t *testing.T, broker string) (*Adapter, *memSink) {
	t.Helper()
	a := New(Config{Broker: broker, Timeout: 700 * time.Millisecond})
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{devices: map[string]sdk.Device{}}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, sink
}

func TestProbeFindsZ2M(t *testing.T) {
	broker := fakeZ2M(t, false)
	av := z2mBackend{Config{Broker: broker}.withDefaults()}.Probe(context.Background())
	if !av.Available {
		t.Fatalf("probe should find retained bridge/devices: %s", av.Detail)
	}
	av = z2mBackend{Config{Broker: "tcp://127.0.0.1:1", Timeout: 300 * time.Millisecond}.withDefaults()}.Probe(context.Background())
	if av.Available {
		t.Fatal("dead broker must not be available")
	}
}

func TestDiscoveryMapsExposes(t *testing.T) {
	_, sink := startAdapter(t, fakeZ2M(t, false))
	lamp, ok := sink.devices["lamp"]
	if !ok {
		t.Fatal("lamp not discovered")
	}
	if _, ok := lamp.Profile.Find("switch.onoff"); !ok {
		t.Fatalf("nested binary state not mapped: %+v", lamp.Profile.Caps)
	}
	cl := sink.devices["climate"]
	if c, ok := cl.Profile.Find("sensor.temperature"); !ok || c.Unit != "°C" {
		t.Fatalf("numeric expose not mapped: %+v", cl.Profile.Caps)
	}
}

func TestSetVerifiedByEcho(t *testing.T) {
	a, _ := startAdapter(t, fakeZ2M(t, true))
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "lamp", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["switch_onoff"] != 1 {
		t.Fatalf("echoed set must be verified: %+v", eff)
	}
}

func TestSilentDeviceUnverified(t *testing.T) {
	a, _ := startAdapter(t, fakeZ2M(t, false)) // nobody echoes
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "lamp", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unverified {
		t.Fatalf("silent device must yield UNVERIFIED, got %+v", eff)
	}
}

func TestSensorGet(t *testing.T) {
	a, _ := startAdapter(t, fakeZ2M(t, true))
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "climate", Capability: "sensor.temperature"})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["sensor_temperature"] != 23.5 {
		t.Fatalf("sensor get wrong: %+v", eff)
	}
}
