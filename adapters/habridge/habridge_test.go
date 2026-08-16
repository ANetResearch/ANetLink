package habridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/sdk"
)

type memSink struct{ devices map[string]sdk.Device }

func (m *memSink) UpsertDevice(d sdk.Device)        { m.devices[d.ID] = d }
func (m *memSink) RemoveDevice(id string)           { delete(m.devices, id) }
func (m *memSink) State(string, map[string]float64) {}

// fakeHA implements the subset of the HA WebSocket API the bridge uses.
type fakeHA struct {
	mu       sync.Mutex
	states   map[string]haEntity
	upgrader websocket.Upgrader
	silent   bool // do not emit state_changed after call_service
}

func newFakeHA(silent bool) *fakeHA {
	return &fakeHA{
		silent: silent,
		states: map[string]haEntity{
			"switch.kettle": {EntityID: "switch.kettle", State: "off",
				Attributes: map[string]any{"friendly_name": "Kettle"}},
			"sensor.kitchen_temp": {EntityID: "sensor.kitchen_temp", State: "21.5",
				Attributes: map[string]any{"device_class": "temperature", "unit_of_measurement": "°C"}},
		},
	}
}

func (f *fakeHA) handler(w http.ResponseWriter, r *http.Request) {
	conn, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var wmu sync.Mutex
	writeJSON := func(v any) { wmu.Lock(); defer wmu.Unlock(); _ = conn.WriteJSON(v) }
	writeJSON(map[string]string{"type": "auth_required"})
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"access_token"`
	}
	if conn.ReadJSON(&auth) != nil || auth.Token != "sesame" {
		writeJSON(map[string]string{"type": "auth_invalid"})
		return
	}
	writeJSON(map[string]string{"type": "auth_ok"})
	subscribed := false
	for {
		var req map[string]any
		if conn.ReadJSON(&req) != nil {
			return
		}
		id := req["id"]
		switch req["type"] {
		case "subscribe_events":
			subscribed = true
			writeJSON(map[string]any{"id": id, "type": "result", "success": true})
		case "get_states":
			f.mu.Lock()
			var list []haEntity
			for _, e := range f.states {
				list = append(list, e)
			}
			f.mu.Unlock()
			writeJSON(map[string]any{"id": id, "type": "result", "success": true, "result": list})
		case "call_service":
			target := req["target"].(map[string]any)["entity_id"].(string)
			service := req["service"].(string)
			f.mu.Lock()
			e := f.states[target]
			if strings.HasSuffix(service, "_on") {
				e.State = "on"
			} else {
				e.State = "off"
			}
			f.states[target] = e
			f.mu.Unlock()
			writeJSON(map[string]any{"id": id, "type": "result", "success": true})
			if subscribed && !f.silent {
				writeJSON(map[string]any{"id": 1, "type": "event", "event": map[string]any{
					"event_type": "state_changed",
					"data":       map[string]any{"entity_id": target, "new_state": e},
				}})
			}
		}
	}
}

func startRig(t *testing.T, silent bool) (*Adapter, *memSink) {
	t.Helper()
	f := newFakeHA(silent)
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/websocket"
	a := New(Config{URL: url, Token: "sesame", Timeout: 700 * time.Millisecond})
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

func TestProbeAuth(t *testing.T) {
	f := newFakeHA(false)
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/websocket"
	av := wsBackend{Config{URL: url, Token: "sesame"}.withDefaults()}.Probe(context.Background())
	if !av.Available {
		t.Fatalf("good token must authenticate: %s", av.Detail)
	}
	av = wsBackend{Config{URL: url, Token: "wrong"}.withDefaults()}.Probe(context.Background())
	if av.Available {
		t.Fatal("bad token must not be available")
	}
}

func TestEntityMapping(t *testing.T) {
	_, sink := startRig(t, false)
	sw, ok := sink.devices["switch.kettle"]
	if !ok {
		t.Fatal("switch not exposed")
	}
	if _, ok := sw.Profile.Find("switch.onoff"); !ok {
		t.Fatalf("switch capability missing: %+v", sw.Profile.Caps)
	}
	se := sink.devices["sensor.kitchen_temp"]
	if c, ok := se.Profile.Find("sensor.temperature"); !ok || c.Unit != "°C" {
		t.Fatalf("sensor mapping wrong: %+v", se.Profile.Caps)
	}
}

func TestServiceCallVerifiedByEvent(t *testing.T) {
	a, _ := startRig(t, false)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "switch.kettle", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["switch_onoff"] != 1 {
		t.Fatalf("event-confirmed set must verify: %+v", eff)
	}
}

func TestSilentHAUnverified(t *testing.T) {
	a, _ := startRig(t, true)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "switch.kettle", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unverified {
		t.Fatalf("no event = UNVERIFIED, got %+v", eff)
	}
}

func TestSensorLiveRead(t *testing.T) {
	a, _ := startRig(t, false)
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "sensor.kitchen_temp", Capability: "sensor.temperature"})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["sensor_temperature"] != 21.5 {
		t.Fatalf("live read wrong: %+v", eff)
	}
}

var _ = json.Marshal // keep import if fixture changes
