package habridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

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

// fakeHA implements the subset of the HA WebSocket API the bridge uses.
type fakeHA struct {
	mu       sync.Mutex
	states   map[string]haEntity
	upgrader websocket.Upgrader
	silent   bool // do not emit state_changed after call_service
	broken   bool // accept call_service but do NOT apply the state
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
			if !f.broken {
				if strings.HasSuffix(service, "_on") {
					e.State = "on"
				} else {
					e.State = "off"
				}
				f.states[target] = e
			}
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
	return startRigMode(t, silent, false)
}

func startRigMode(t *testing.T, silent, broken bool) (*Adapter, *memSink) {
	t.Helper()
	f := newFakeHA(silent)
	f.broken = broken
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
	sw, ok := sink.get("switch.kettle")
	if !ok {
		t.Fatal("switch not exposed")
	}
	if _, ok := sw.Profile.Find("switch.onoff"); !ok {
		t.Fatalf("switch capability missing: %+v", sw.Profile.Caps)
	}
	se, _ := sink.get("sensor.kitchen_temp")
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

// TestSilentButAppliedVerifiedByLiveRead: no state_changed event (idempotent
// actuation / quiet HA) but the state IS the target → the live-read fallback
// verifies it. The realworld tier caught this on its first CI run
// (light.ceiling_lights starts ON in the HA demo).
func TestSilentButAppliedVerifiedByLiveRead(t *testing.T) {
	a, _ := startRig(t, true)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "switch.kettle", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["switch_onoff"] != 1 {
		t.Fatalf("applied state must verify via live read: %+v", eff)
	}
}

// TestBrokenActuationFails: call_service succeeds but the state never
// changes → live read contradicts the request → FAILED, never a fake OK.
func TestBrokenActuationFails(t *testing.T) {
	a, _ := startRigMode(t, true, true)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "switch.kettle", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed {
		t.Fatalf("contradicting live read must FAIL, got %+v", eff)
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

// --- domain coverage ---

func ent(id, state string, attrs map[string]any) haEntity {
	if attrs == nil {
		attrs = map[string]any{}
	}
	return haEntity{EntityID: id, State: state, Attributes: attrs}
}

func capIDs(specs []capSpec) []string {
	out := make([]string, 0, len(specs))
	for _, c := range specs {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out
}

func hasCap(specs []capSpec, id string) (capSpec, bool) {
	for _, c := range specs {
		if c.ID == id {
			return c, true
		}
	}
	return capSpec{}, false
}

// TestDomainsBeyondTheOriginalThree is the whole point of the table: an
// installation is mostly climate, covers, locks, fans and media players,
// and every one of them used to be dropped on the floor.
func TestDomainsBeyondTheOriginalThree(t *testing.T) {
	cases := []struct {
		entity haEntity
		want   string
	}{
		{ent("climate.office", "heat", map[string]any{
			"supported_features": 1.0, "temperature": 21.0, "current_temperature": 19.4}), "climate.setpoint"},
		{ent("cover.blind", "open", map[string]any{
			"supported_features": 15.0, "current_position": 70.0}), "cover.position"},
		{ent("lock.front", "locked", nil), "lock.locked"},
		{ent("fan.desk", "on", map[string]any{"supported_features": 1.0, "percentage": 40.0}), "fan.speed"},
		{ent("media_player.tv", "playing", map[string]any{
			"supported_features": 16516.0, "volume_level": 0.3}), "media.volume"},
		{ent("vacuum.robot", "docked", map[string]any{"supported_features": 8200.0}), "vacuum.cleaning"},
		{ent("humidifier.bedroom", "on", map[string]any{"humidity": 55.0}), "humidifier.setpoint"},
		{ent("number.limit", "12", nil), "number.value"},
		{ent("button.doorbell", "unknown", nil), "button.press"},
		{ent("scene.movie", "unknown", nil), "scene.activate"},
		{ent("binary_sensor.door", "on", map[string]any{"device_class": "door"}), "binary_sensor.door"},
		{ent("alarm_control_panel.house", "armed_away", map[string]any{"supported_features": 3.0}), "alarm.armed"},
		{ent("valve.gas", "open", map[string]any{"supported_features": 4.0, "current_valve_position": 100.0}), "valve.position"},
		{ent("water_heater.tank", "eco", map[string]any{"supported_features": 1.0, "temperature": 55.0}), "water_heater.setpoint"},
	}
	for _, tc := range cases {
		specs := EntityCaps(tc.entity.EntityID, tc.entity)
		if len(specs) == 0 {
			t.Errorf("%s produced no capabilities at all", tc.entity.EntityID)
			continue
		}
		if _, ok := hasCap(specs, tc.want); !ok {
			t.Errorf("%s: want %s, got %v", tc.entity.EntityID, tc.want, capIDs(specs))
		}
	}
}

// TestNormalisationIsUniform: HA carries brightness 0-255, position 0-100
// and volume 0-1. The contract is [0,1] everywhere, and a caller must never
// have to know which native scale is underneath.
func TestNormalisationIsUniform(t *testing.T) {
	light := ent("light.desk", "on", map[string]any{
		"brightness": 128.0, "supported_color_modes": []any{"brightness"}})
	spec, ok := hasCap(EntityCaps(light.EntityID, light), "light.brightness")
	if !ok {
		t.Fatal("a dimmable light must expose brightness")
	}
	if v, _ := spec.Read(light); v < 0.49 || v > 0.51 {
		t.Fatalf("brightness 128/255 should read ~0.5, got %g", v)
	}
	_, data, err := spec.Write(light, map[string]any{"brightness": 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := data["brightness"].(int); b < 126 || b > 129 {
		t.Fatalf("0.5 should reach HA as ~128, got %v", data["brightness"])
	}

	cover := ent("cover.blind", "open", map[string]any{
		"supported_features": 15.0, "current_position": 70.0})
	cspec, _ := hasCap(EntityCaps(cover.EntityID, cover), "cover.position")
	if v, _ := cspec.Read(cover); v < 0.69 || v > 0.71 {
		t.Fatalf("position 70/100 should read ~0.7, got %g", v)
	}
	_, cdata, _ := cspec.Write(cover, map[string]any{"position": 0.25})
	if p, _ := cdata["position"].(int); p != 25 {
		t.Fatalf("0.25 should reach HA as 25, got %v", cdata["position"])
	}
}

// TestZeroBrightnessTurnsOff: HA rejects brightness 0 on turn_on, so asking
// for zero has to become turn_off or the call fails outright.
func TestZeroBrightnessTurnsOff(t *testing.T) {
	light := ent("light.desk", "on", map[string]any{
		"brightness": 200.0, "supported_color_modes": []any{"brightness"}})
	spec, _ := hasCap(EntityCaps(light.EntityID, light), "light.brightness")
	svc, _, err := spec.Write(light, map[string]any{"brightness": 0})
	if err != nil {
		t.Fatal(err)
	}
	if svc != "turn_off" {
		t.Fatalf("brightness 0 must become turn_off, got %q", svc)
	}
}

// TestCapabilitiesFollowSupportedFeatures: HA publishes what an entity can
// do precisely so a client need not guess, and claiming a control the
// device lacks turns a clean refusal into a runtime failure.
func TestCapabilitiesFollowSupportedFeatures(t *testing.T) {
	plain := ent("light.hall", "on", map[string]any{"supported_color_modes": []any{"onoff"}})
	if _, ok := hasCap(EntityCaps(plain.EntityID, plain), "light.brightness"); ok {
		t.Fatal("a non-dimmable light must not advertise brightness")
	}
	if _, ok := hasCap(EntityCaps(plain.EntityID, plain), "light.onoff"); !ok {
		t.Fatal("but it is still a light")
	}

	tunable := ent("light.lamp", "on", map[string]any{
		"supported_color_modes": []any{"color_temp"}, "color_temp_kelvin": 3000.0})
	if _, ok := hasCap(EntityCaps(tunable.EntityID, tunable), "light.color_temp"); !ok {
		t.Fatal("a tunable light must advertise colour temperature")
	}

	// A cover with no set-position bit gets open/close but not a position.
	basic := ent("cover.gate", "closed", map[string]any{"supported_features": 3.0})
	specs := EntityCaps(basic.EntityID, basic)
	if _, ok := hasCap(specs, "cover.position"); ok {
		t.Fatal("a cover without SET_POSITION must not advertise a position")
	}
	if _, ok := hasCap(specs, "cover.onoff"); !ok {
		t.Fatalf("but open/close must be there: %v", capIDs(specs))
	}
}

// TestSetpointIsNotTheMeasurement: conflating the target with the measured
// value is the most common climate integration bug there is.
func TestSetpointIsNotTheMeasurement(t *testing.T) {
	c := ent("climate.office", "heat", map[string]any{
		"supported_features": 1.0, "temperature": 21.0, "current_temperature": 19.4})
	specs := EntityCaps(c.EntityID, c)
	set, ok1 := hasCap(specs, "climate.setpoint")
	cur, ok2 := hasCap(specs, "climate.temperature")
	if !ok1 || !ok2 {
		t.Fatalf("both must exist: %v", capIDs(specs))
	}
	if v, _ := set.Read(c); v != 21 {
		t.Fatalf("setpoint: %g", v)
	}
	if v, _ := cur.Read(c); v != 19.4 {
		t.Fatalf("measured: %g", v)
	}
	if cur.Write != nil {
		t.Fatal("the measured temperature must be read-only")
	}
}

// TestSetpointOutsideTheEntityRangeIsRefused: HA rejects the whole call, so
// catching it here gives the caller a reason instead of a transport error.
func TestSetpointOutsideTheEntityRangeIsRefused(t *testing.T) {
	c := ent("climate.office", "heat", map[string]any{
		"supported_features": 1.0, "temperature": 21.0, "min_temp": 7.0, "max_temp": 35.0})
	spec, _ := hasCap(EntityCaps(c.EntityID, c), "climate.setpoint")
	if _, _, err := spec.Write(c, map[string]any{"temperature": 60.0}); err == nil {
		t.Fatal("60 °C is above the entity maximum and must be refused")
	}
	if _, _, err := spec.Write(c, map[string]any{"temperature": 22.0}); err != nil {
		t.Fatalf("22 °C is in range: %v", err)
	}
}

// TestTransitionalStatesAreNotReportedAsFact: a jammed lock is not
// unlocked, and an arming panel is not armed.
func TestTransitionalStatesAreNotReportedAsFact(t *testing.T) {
	spec, _ := hasCap(EntityCaps("lock.front", ent("lock.front", "locked", nil)), "lock.locked")
	for _, state := range []string{"jammed", "locking", "unlocking"} {
		if _, ok := spec.Read(ent("lock.front", state, nil)); ok {
			t.Fatalf("state %q must not resolve to a boolean", state)
		}
	}
	if v, ok := spec.Read(ent("lock.front", "locked", nil)); !ok || v != 1 {
		t.Fatal("but a locked lock reads locked")
	}

	panel := ent("alarm_control_panel.house", "armed_away", map[string]any{"supported_features": 3.0})
	aspec, _ := hasCap(EntityCaps(panel.EntityID, panel), "alarm.armed")
	if _, ok := aspec.Read(ent("alarm_control_panel.house", "arming", nil)); ok {
		t.Fatal("an arming panel is not yet armed")
	}
}

// TestOutOfRangeNormalisedArgsRefused keeps the [0,1] contract enforced
// rather than advisory.
func TestOutOfRangeNormalisedArgsRefused(t *testing.T) {
	light := ent("light.desk", "on", map[string]any{"supported_color_modes": []any{"brightness"}})
	spec, _ := hasCap(EntityCaps(light.EntityID, light), "light.brightness")
	for _, v := range []float64{1.5, -0.2} {
		if _, _, err := spec.Write(light, map[string]any{"brightness": v}); err == nil {
			t.Fatalf("brightness %g is outside [0,1] and must be refused", v)
		}
	}
}

// TestUnsupportedDomainsAreCountedNotSilentlyDropped: an integration that
// quietly discards entities is indistinguishable from a broken one.
func TestUnsupportedDomainsAreCountedNotSilentlyDropped(t *testing.T) {
	a := New(Config{URL: "ws://127.0.0.1:1", Token: "x"})
	a.entityCaps = map[string]map[string]capSpec{}
	a.exposeEntity(ent("weather.home", "sunny", nil))
	a.exposeEntity(ent("weather.away", "rain", nil))
	a.exposeEntity(ent("device_tracker.phone", "home", nil))

	got := a.UnsupportedDomains()
	if got["weather"] != 2 || got["device_tracker"] != 1 {
		t.Fatalf("unsupported domains must be counted: %v", got)
	}
}

func TestEveryTableEntryIsCoherent(t *testing.T) {
	for domain, specs := range domainTable {
		if len(specs) == 0 {
			t.Errorf("domain %q has no capabilities", domain)
		}
		for _, c := range specs {
			if c.ID == "" {
				t.Errorf("domain %q has a capability with no id", domain)
			}
			if c.Read == nil && c.Write == nil {
				t.Errorf("%s/%s can neither be read nor written", domain, c.ID)
			}
			if c.Write == nil && c.Access&profile.AccessSet != 0 {
				t.Errorf("%s/%s claims SET but has no writer", domain, c.ID)
			}
			if c.Read == nil && c.Access&profile.AccessGet != 0 {
				t.Errorf("%s/%s claims GET but has no reader", domain, c.ID)
			}
		}
	}
}
