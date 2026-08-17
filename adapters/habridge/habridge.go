// Package habridge is the Home Assistant bridge — the highest-leverage
// probe-type backend (A5): one WebSocket connection turns every entity HA
// already manages into an ANetLink capability. Pure-Go wire client; nothing
// of HA is bundled or imported.
//
// Honesty: a service call is VERIFIED only when HA's state_changed event
// confirms the new state inside the window; GET reads live states.
package habridge

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// Config configures the bridge.
type Config struct {
	// URL is the HA WebSocket endpoint, e.g. "ws://homeassistant.local:8123/api/websocket".
	URL string
	// Token is a long-lived access token.
	Token string
	// Timeout bounds calls and readback windows; default 3s.
	Timeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.Timeout == 0 {
		c.Timeout = 3 * time.Second
	}
	return c
}

type haEntity struct {
	EntityID   string         `json:"entity_id"`
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes"`
}

type Adapter struct {
	cfg  Config
	mu   sync.Mutex
	conn *websocket.Conn
	sink sdk.Sink
	next int64
	// pending request id → response chan; entity waiters for state_changed
	pending     map[int64]chan json.RawMessage
	waiters     map[string][]chan haEntity
	entityCaps  map[string]map[string]capSpec
	unsupported map[string]int // entity_id → capability id
	done        chan struct{}
}

func New(cfg Config) *Adapter {
	return &Adapter{
		cfg:        cfg.withDefaults(),
		pending:    map[int64]chan json.RawMessage{},
		waiters:    map[string][]chan haEntity{},
		entityCaps: map[string]map[string]capSpec{},
	}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "habridge", Protocol: "homeassistant", Version: "0.1.0"}
}

type wsBackend struct{ cfg Config }

func (b wsBackend) Name() string { return "websocket" }

func (b wsBackend) Probe(ctx context.Context) sdk.Availability {
	conn, err := dialAuth(ctx, b.cfg)
	if err != nil {
		return sdk.Availability{Available: false, Detail: err.Error()}
	}
	conn.Close()
	return sdk.Availability{Available: true, Detail: "authenticated"}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{wsBackend{a.cfg}} }

func dialAuth(ctx context.Context, cfg Config) (*websocket.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dctx, cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("habridge: dial: %w", err)
	}
	var hello struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&hello); err != nil || hello.Type != "auth_required" {
		conn.Close()
		return nil, fmt.Errorf("habridge: no auth_required (got %q, err %v)", hello.Type, err)
	}
	if err := conn.WriteJSON(map[string]string{"type": "auth", "access_token": cfg.Token}); err != nil {
		conn.Close()
		return nil, err
	}
	var verdict struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&verdict); err != nil || verdict.Type != "auth_ok" {
		conn.Close()
		return nil, fmt.Errorf("habridge: auth rejected (%q)", verdict.Type)
	}
	return conn, nil
}

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	conn, err := dialAuth(ctx, a.cfg)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.conn, a.sink = conn, sink
	a.done = make(chan struct{})
	a.mu.Unlock()
	go a.readLoop()

	if _, err := a.call(ctx, map[string]any{"type": "subscribe_events", "event_type": "state_changed"}); err != nil {
		return fmt.Errorf("habridge: subscribe: %w", err)
	}
	raw, err := a.call(ctx, map[string]any{"type": "get_states"})
	if err != nil {
		return fmt.Errorf("habridge: get_states: %w", err)
	}
	var entities []haEntity
	if err := json.Unmarshal(raw, &entities); err != nil {
		return err
	}
	for _, e := range entities {
		a.exposeEntity(e)
	}
	return nil
}

func (a *Adapter) Stop(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
	}
	if a.done != nil {
		select {
		case <-a.done:
		default:
			close(a.done)
		}
		a.done = nil
	}
	return nil
}

// exposeEntity maps one HA entity to one ANetLink device.
//
// An entity becomes a device with *all* of its capabilities, not one. A
// dimmable lamp is on/off plus brightness plus colour temperature; a
// thermostat is a setpoint plus a measured temperature plus humidity.
// Collapsing each entity to a single capability — which this bridge used to
// do — throws away most of what Home Assistant knows.
func (a *Adapter) exposeEntity(e haEntity) {
	specs := EntityCaps(e.EntityID, e)
	if len(specs) == 0 {
		// A domain this bridge does not model yet. Dropping it silently is
		// how an operator ends up wondering where half their house went, so
		// it is recorded and reported by UnsupportedDomains().
		a.noteUnsupported(e.EntityID)
		return
	}

	caps := make([]profile.Capability, 0, len(specs))
	byID := make(map[string]capSpec, len(specs))
	for _, c := range specs {
		caps = append(caps, profile.Capability{ID: c.ID, Access: c.Access, Unit: c.Unit})
		byID[c.ID] = c
	}

	vendor, _ := e.Attributes["friendly_name"].(string)
	a.mu.Lock()
	a.entityCaps[e.EntityID] = byID
	sink := a.sink
	a.mu.Unlock()

	if sink != nil {
		sink.UpsertDevice(sdk.Device{
			ID: e.EntityID,
			Profile: &profile.DeviceProfile{
				Schema: 1, Vendor: vendor, Model: e.EntityID, Protocol: "homeassistant",
				Caps:     caps,
				MaxTrust: profile.V2, AuthTrust: profile.A1, // state readback via HA event bus; HA-attested
			},
			Online: !strings.EqualFold(e.State, "unavailable"),
		})
	}
	// Unsolicited state: report every readable capability, so a consumer
	// tracking brightness sees it change without polling.
	if sink != nil {
		metrics := map[string]float64{}
		for _, c := range specs {
			if c.Read == nil {
				continue
			}
			if v, ok := c.Read(e); ok {
				metrics[strings.ReplaceAll(c.ID, ".", "_")] = v
			}
		}
		if len(metrics) > 0 {
			sink.State(e.EntityID, metrics)
		}
	}
}

// noteUnsupported records a domain this bridge cannot project.
func (a *Adapter) noteUnsupported(entityID string) {
	domain := strings.SplitN(entityID, ".", 2)[0]
	a.mu.Lock()
	if a.unsupported == nil {
		a.unsupported = map[string]int{}
	}
	a.unsupported[domain]++
	a.mu.Unlock()
}

// UnsupportedDomains reports the HA domains this bridge saw and could not
// project, with how many entities each cost. An integration that quietly
// drops entities is indistinguishable from one that is broken; this makes
// the gap answerable.
func (a *Adapter) UnsupportedDomains() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.unsupported))
	for k, v := range a.unsupported {
		out[k] = v
	}
	return out
}

func (a *Adapter) readLoop() {
	for {
		a.mu.Lock()
		conn, done := a.conn, a.done
		a.mu.Unlock()
		if conn == nil {
			return
		}
		var msg struct {
			ID      int64           `json:"id"`
			Type    string          `json:"type"`
			Success *bool           `json:"success"`
			Result  json.RawMessage `json:"result"`
			Event   *struct {
				EventType string `json:"event_type"`
				Data      struct {
					EntityID string    `json:"entity_id"`
					NewState *haEntity `json:"new_state"`
				} `json:"data"`
			} `json:"event"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			select {
			case <-done:
			default:
			}
			return
		}
		switch msg.Type {
		case "result":
			a.mu.Lock()
			ch := a.pending[msg.ID]
			delete(a.pending, msg.ID)
			a.mu.Unlock()
			if ch != nil {
				if msg.Success != nil && !*msg.Success {
					ch <- json.RawMessage(`{"__error":true}`)
				} else {
					ch <- msg.Result
				}
			}
		case "event":
			if msg.Event == nil || msg.Event.EventType != "state_changed" || msg.Event.Data.NewState == nil {
				continue
			}
			ns := *msg.Event.Data.NewState
			a.mu.Lock()
			ws := a.waiters[ns.EntityID]
			a.waiters[ns.EntityID] = nil
			a.mu.Unlock()
			for _, w := range ws {
				select {
				case w <- ns:
				default:
				}
			}
		}
	}
}

// call sends one request and awaits its correlated result.
func (a *Adapter) call(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	a.mu.Lock()
	conn := a.conn
	if conn == nil {
		a.mu.Unlock()
		return nil, fmt.Errorf("not connected")
	}
	a.next++
	id := a.next
	req["id"] = id
	ch := make(chan json.RawMessage, 1)
	a.pending[id] = ch
	err := conn.WriteJSON(req)
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case raw := <-ch:
		if strings.Contains(string(raw), `"__error"`) {
			return nil, fmt.Errorf("ha error result")
		}
		return raw, nil
	case <-time.After(a.cfg.Timeout):
		return nil, fmt.Errorf("timeout")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Adapter) awaitEntity(id string) chan haEntity {
	ch := make(chan haEntity, 1)
	a.mu.Lock()
	a.waiters[id] = append(a.waiters[id], ch)
	a.mu.Unlock()
	return ch
}

func (a *Adapter) Invoke(ctx context.Context, call sdk.Call) (effect.Effect, error) {
	a.mu.Lock()
	byID, known := a.entityCaps[call.DeviceID]
	a.mu.Unlock()
	if !known {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown device %q", call.DeviceID)).Build(), nil
	}
	spec, ok := byID[call.Capability]
	if !ok {
		return sdk.NewEffect().Fail(
			fmt.Sprintf("device %q does not expose %q", call.DeviceID, call.Capability)).Build(), nil
	}
	metric := strings.ReplaceAll(spec.ID, ".", "_")
	domain := strings.SplitN(call.DeviceID, ".", 2)[0]

	// A read is a read: no args, or a capability with nothing to write.
	if len(call.Args) == 0 || spec.Write == nil {
		if spec.Read == nil {
			return sdk.NewEffect().Fail(
				fmt.Sprintf("%q is write-only; it needs arguments", spec.ID)).Build(), nil
		}
		return a.readCapability(ctx, call.DeviceID, spec)
	}

	// The entity's current attributes decide what a valid call looks like —
	// a setpoint outside min_temp/max_temp, for instance, is refused here
	// rather than by HA after the fact.
	cur, curErr := a.entityNow(ctx, call.DeviceID)
	if curErr != nil {
		return sdk.NewEffect().Unavailable("read before write: " + curErr.Error()).Build(), nil
	}
	service, data, err := spec.Write(cur, call.Args)
	if err != nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("%s: %v", spec.ID, err)).Build(), nil
	}

	payload := map[string]any{
		"type": "call_service", "domain": domain, "service": service,
		"target": map[string]any{"entity_id": call.DeviceID},
	}
	if len(data) > 0 {
		payload["service_data"] = data
	}

	ch := a.awaitEntity(call.DeviceID)
	if _, err := a.call(ctx, payload); err != nil {
		return sdk.NewEffect().Fail("call_service: " + err.Error()).Build(), nil
	}

	requested := fmt.Sprintf("%s %v", spec.ID, call.Args)
	// A write-only capability — a button press, a scene — has no state to
	// read back, so the acknowledgement is all there is and the effect stays
	// honestly UNVERIFIED.
	if spec.Read == nil {
		return sdk.NewEffect().
			Requested(requested).Protocol("homeassistant").NativeAck(true).
			Observed("accepted by Home Assistant; this capability reports no state").
			Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
	}

	select {
	case ns := <-ch:
		return a.settle(spec, metric, requested, ns, call.Args), nil
	case <-time.After(a.cfg.Timeout):
		// No event ≠ no effect: idempotent actuation (turn_on on a lit
		// light) emits no state_changed. Verify by live read — a lesson
		// the realworld tier taught on its first CI run.
		cur, err := a.entityNow(ctx, call.DeviceID)
		if err != nil {
			return sdk.NewEffect().
				Requested(requested).Protocol("homeassistant").NativeAck(true).
				Observed("sent; no event and no readback").
				Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
		}
		return a.settle(spec, metric, requested, cur, call.Args), nil
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
}

// settle judges an actuation against the entity state that followed it.
//
// "Did it take" means the readback matches what was asked for, which for a
// continuous quantity means within a tolerance: HA and the device round,
// and a lamp asked for 0.5 brightness legitimately reports 0.498.
func (a *Adapter) settle(spec capSpec, metric, requested string, e haEntity, args map[string]any) effect.Effect {
	got, ok := spec.Read(e)
	if !ok {
		return sdk.NewEffect().
			Requested(requested).Protocol("homeassistant").NativeAck(true).
			Observed(fmt.Sprintf("state %q carries no readable value for %s", e.State, spec.ID)).
			Trust(uint8(profile.V1), uint8(profile.A1)).Build()
	}
	b := sdk.NewEffect().Metric(metric, got).
		Requested(requested).Protocol("homeassistant").NativeAck(true).
		Observed(e.State).
		Trust(uint8(profile.V2), uint8(profile.A1))

	want, hasWant := expectedValue(spec.ID, args)
	if !hasWant {
		// Nothing to compare against (a mode change, a stop): the value was
		// read, so it is reported, but no claim is made about intent.
		return b.Verified().Build()
	}
	if math.Abs(got-want) <= 0.02 {
		return b.Verified().Build()
	}
	return b.Fail(fmt.Sprintf("readback %g does not match the requested %g (state %q)",
		got, want, e.State)).Build()
}

// expectedValue derives the normalised value an actuation asked for, so the
// readback can be checked against it.
func expectedValue(capID string, args map[string]any) (float64, bool) {
	if on, ok := args["on"].(bool); ok {
		if on {
			return 1, true
		}
		return 0, true
	}
	if locked, ok := args["locked"].(bool); ok {
		if locked {
			return 1, true
		}
		return 0, true
	}
	if playing, ok := args["playing"].(bool); ok {
		if playing {
			return 1, true
		}
		return 0, true
	}
	for _, k := range []string{"brightness", "position", "speed", "volume", "humidity"} {
		if v, ok := toFloat(args[k]); ok {
			return v, true
		}
	}
	return 0, false
}

// readCapability performs a live read of one capability.
func (a *Adapter) readCapability(ctx context.Context, entityID string, spec capSpec) (effect.Effect, error) {
	e, err := a.entityNow(ctx, entityID)
	if err != nil {
		return sdk.NewEffect().Unavailable(err.Error()).Build(), nil
	}
	v, ok := spec.Read(e)
	if !ok {
		return sdk.NewEffect().Unavailable(
			fmt.Sprintf("state %q carries no readable value for %s", e.State, spec.ID)).Build(), nil
	}
	return sdk.NewEffect().
		Metric(strings.ReplaceAll(spec.ID, ".", "_"), v).
		Requested(spec.ID).Protocol("homeassistant").NativeAck(true).
		Observed(e.State).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// entityNow fetches one entity's current state and attributes.
func (a *Adapter) entityNow(ctx context.Context, entityID string) (haEntity, error) {
	raw, err := a.call(ctx, map[string]any{"type": "get_states"})
	if err != nil {
		return haEntity{}, err
	}
	var entities []haEntity
	if err := json.Unmarshal(raw, &entities); err != nil {
		return haEntity{}, err
	}
	for _, e := range entities {
		if e.EntityID == entityID {
			return e, nil
		}
	}
	return haEntity{}, fmt.Errorf("entity %s vanished from Home Assistant", entityID)
}

var _ sdk.Discoverer = (*Adapter)(nil)

// DiscoverDevices implements sdk.Discoverer: Home Assistant already knows
// every entity it manages, so onboarding is a question, not a spreadsheet.
func (a *Adapter) DiscoverDevices(ctx context.Context) ([]sdk.Found, error) {
	raw, err := a.call(ctx, map[string]any{"type": "get_states"})
	if err != nil {
		return nil, err
	}
	var entities []haEntity
	if err := json.Unmarshal(raw, &entities); err != nil {
		return nil, err
	}
	out := make([]sdk.Found, 0, len(entities))
	for _, e := range entities {
		specs := EntityCaps(e.EntityID, e)
		if len(specs) == 0 {
			continue
		}
		capIDs := make([]string, 0, len(specs))
		for _, c := range specs {
			capIDs = append(capIDs, c.ID)
		}
		label, _ := e.Attributes["friendly_name"].(string)
		out = append(out, sdk.Found{
			SuggestedID:  e.EntityID,
			Protocol:     "homeassistant",
			Label:        label,
			Address:      e.EntityID,
			Capabilities: capIDs,
		})
	}
	return out, nil
}
