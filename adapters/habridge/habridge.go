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
	"strconv"
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
	pending map[int64]chan json.RawMessage
	waiters map[string][]chan haEntity
	caps    map[string]string // entity_id → capability id
	done    chan struct{}
}

func New(cfg Config) *Adapter {
	return &Adapter{
		cfg:     cfg.withDefaults(),
		pending: map[int64]chan json.RawMessage{},
		waiters: map[string][]chan haEntity{},
		caps:    map[string]string{},
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
func (a *Adapter) exposeEntity(e haEntity) {
	domain := strings.SplitN(e.EntityID, ".", 2)[0]
	var cap_ profile.Capability
	switch domain {
	case "switch", "light":
		cap_ = profile.Capability{ID: domain + ".onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState}
	case "sensor":
		class, _ := e.Attributes["device_class"].(string)
		if class == "" {
			class = "value"
		}
		unit, _ := e.Attributes["unit_of_measurement"].(string)
		cap_ = profile.Capability{ID: "sensor." + class, Access: profile.AccessGet | profile.AccessState, Unit: unit}
	default:
		return
	}
	vendor, _ := e.Attributes["friendly_name"].(string)
	a.mu.Lock()
	a.caps[e.EntityID] = cap_.ID
	sink := a.sink
	a.mu.Unlock()
	if sink != nil {
		sink.UpsertDevice(sdk.Device{
			ID: e.EntityID,
			Profile: &profile.DeviceProfile{
				Schema: 1, Vendor: vendor, Model: e.EntityID, Protocol: "homeassistant",
				Caps:     []profile.Capability{cap_},
				MaxTrust: profile.V2, AuthTrust: profile.A1, // state readback via HA event bus; HA-attested
			},
			Online: !strings.EqualFold(e.State, "unavailable"),
		})
	}
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
	capID := a.caps[call.DeviceID]
	a.mu.Unlock()
	if capID == "" {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown device %q", call.DeviceID)).Build(), nil
	}
	if capID != call.Capability {
		return sdk.NewEffect().Fail(fmt.Sprintf("device %q does not expose %q", call.DeviceID, call.Capability)).Build(), nil
	}
	metric := strings.ReplaceAll(capID, ".", "_")
	domain := strings.SplitN(call.DeviceID, ".", 2)[0]

	if want, isSet := call.Args["on"].(bool); isSet {
		service := "turn_off"
		if want {
			service = "turn_on"
		}
		ch := a.awaitEntity(call.DeviceID)
		if _, err := a.call(ctx, map[string]any{
			"type": "call_service", "domain": domain, "service": service,
			"target": map[string]any{"entity_id": call.DeviceID},
		}); err != nil {
			return sdk.NewEffect().Fail("call_service: " + err.Error()).Build(), nil
		}
		select {
		case ns := <-ch:
			v := 0.0
			if strings.EqualFold(ns.State, "on") {
				v = 1.0
			}
			b := sdk.NewEffect().Metric(metric, v).
				Requested(fmt.Sprintf("%s=%v", capID, want)).
				Protocol("homeassistant").NativeAck(true).
				Observed(ns.State).
				Trust(uint8(profile.V2), uint8(profile.A1))
			if (v == 1.0) == want {
				return b.Verified().Build(), nil
			}
			return b.Fail(fmt.Sprintf("readback mismatch: state %q", ns.State)).Build(), nil
		case <-time.After(a.cfg.Timeout):
			// No event ≠ no effect: idempotent actuation (turn_on on a lit
			// light) emits no state_changed. Verify by live read — a lesson
			// the realworld tier taught on its first CI run.
			return a.liveVerifySet(ctx, call.DeviceID, metric, want)
		case <-ctx.Done():
			return effect.Effect{}, ctx.Err()
		}
	}

	return a.liveRead(ctx, call.DeviceID, metric)
}

// liveVerifySet settles a set whose event window elapsed: a live state read
// equal to the target verifies it; a contradicting read means the actuation
// did not take (FAILED), an unreadable state stays UNVERIFIED.
func (a *Adapter) liveVerifySet(ctx context.Context, entityID, metric string, want bool) (effect.Effect, error) {
	eff, err := a.liveRead(ctx, entityID, metric)
	if err != nil || eff.Record == nil {
		return sdk.NewEffect().Build(), nil // sent, unconfirmed
	}
	got := eff.Record.Metrics[metric] == 1.0
	if got == want {
		return eff, nil // live read confirms the target state
	}
	return sdk.NewEffect().Metric(metric, eff.Record.Metrics[metric]).
		Fail("state readback contradicts requested state").Build(), nil
}

// liveRead fetches the entity's current state via get_states.
func (a *Adapter) liveRead(ctx context.Context, entityID, metric string) (effect.Effect, error) {
	// GET: live read via get_states.
	raw, err := a.call(ctx, map[string]any{"type": "get_states"})
	if err != nil {
		return sdk.NewEffect().Unavailable(err.Error()).Build(), nil
	}
	var entities []haEntity
	if err := json.Unmarshal(raw, &entities); err != nil {
		return effect.Effect{}, err
	}
	for _, e := range entities {
		if e.EntityID != entityID {
			continue
		}
		if strings.EqualFold(e.State, "on") || strings.EqualFold(e.State, "off") {
			v := 0.0
			if strings.EqualFold(e.State, "on") {
				v = 1.0
			}
			return sdk.NewEffect().Metric(metric, v).Verified().Build(), nil
		}
		if f, err := strconv.ParseFloat(e.State, 64); err == nil {
			return sdk.NewEffect().Metric(metric, f).Verified().Build(), nil
		}
		return sdk.NewEffect().Unavailable(fmt.Sprintf("non-numeric state %q", e.State)).Build(), nil
	}
	return sdk.NewEffect().Unavailable("entity vanished from HA").Build(), nil
}

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
		domain := strings.SplitN(e.EntityID, ".", 2)[0]
		var capID string
		switch domain {
		case "switch", "light":
			capID = domain + ".onoff"
		case "sensor":
			class, _ := e.Attributes["device_class"].(string)
			if class == "" {
				class = "value"
			}
			capID = "sensor." + class
		default:
			continue
		}
		label, _ := e.Attributes["friendly_name"].(string)
		out = append(out, sdk.Found{
			SuggestedID:  e.EntityID,
			Protocol:     "homeassistant",
			Label:        label,
			Address:      e.EntityID,
			Capabilities: []string{capID},
		})
	}
	return out, nil
}
