// Package mqttbridge is the zigbee2mqtt/Tasmota-convention MQTT adapter — a
// probe-type backend (A5): it rides an ecosystem the user already runs. Our
// side is pure Go wire protocol; nothing is bundled or imported.
//
// Honesty model: a /set publish is only VERIFIED when the device's state
// topic echoes the change within the window; a silent device yields
// UNVERIFIED ("command sent, no confirmation"), never a fake OK.
package mqttbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// Config configures the bridge.
type Config struct {
	Broker    string        // e.g. "tcp://127.0.0.1:1883"
	BaseTopic string        // default "zigbee2mqtt"
	Timeout   time.Duration // readback window; default 2s
}

func (c Config) withDefaults() Config {
	out := c
	if out.BaseTopic == "" {
		out.BaseTopic = "zigbee2mqtt"
	}
	if out.Timeout == 0 {
		out.Timeout = 2 * time.Second
	}
	return out
}

// z2m wire shapes (subset we consume).
type z2mDevice struct {
	FriendlyName string `json:"friendly_name"`
	IEEEAddress  string `json:"ieee_address"`
	Definition   *struct {
		Vendor  string      `json:"vendor"`
		Model   string      `json:"model"`
		Exposes []z2mExpose `json:"exposes"`
	} `json:"definition"`
}

type z2mExpose struct {
	Type     string      `json:"type"`
	Name     string      `json:"name"`
	Property string      `json:"property"`
	Unit     string      `json:"unit"`
	Access   int         `json:"access"` // bit0 published, bit1 settable, bit2 gettable
	Features []z2mExpose `json:"features"`
}

type capBinding struct {
	property string
	binary   bool
}

type Adapter struct {
	cfg  Config
	mu   sync.Mutex
	cli  paho.Client
	sink sdk.Sink
	// per-device capability bindings and last known state
	binds map[string]map[string]capBinding // device → capID → binding
	state map[string]map[string]any        // device → last state payload
	wait  map[string][]chan map[string]any // device → readback waiters
}

func New(cfg Config) *Adapter {
	return &Adapter{
		cfg:   cfg.withDefaults(),
		binds: map[string]map[string]capBinding{},
		state: map[string]map[string]any{},
		wait:  map[string][]chan map[string]any{},
	}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "mqttbridge", Protocol: "mqtt", Version: "0.1.0"}
}

type z2mBackend struct{ cfg Config }

func (b z2mBackend) Name() string { return "zigbee2mqtt" }

// Probe connects and waits for the retained bridge/devices announcement —
// reachable broker without a zigbee2mqtt is NOT available.
func (b z2mBackend) Probe(ctx context.Context) sdk.Availability {
	cli, err := connect(b.cfg.Broker, "anetlink-probe")
	if err != nil {
		return sdk.Availability{Available: false, Detail: "broker: " + err.Error()}
	}
	defer cli.Disconnect(50)
	got := make(chan int, 1)
	tok := cli.Subscribe(b.cfg.BaseTopic+"/bridge/devices", 0, func(_ paho.Client, m paho.Message) {
		var devs []z2mDevice
		_ = json.Unmarshal(m.Payload(), &devs)
		select {
		case got <- len(devs):
		default:
		}
	})
	tok.Wait()
	select {
	case n := <-got:
		return sdk.Availability{Available: true, Detail: fmt.Sprintf("zigbee2mqtt: %d devices", n)}
	case <-time.After(b.cfg.Timeout):
		return sdk.Availability{Available: false, Detail: "broker reachable but no zigbee2mqtt bridge/devices (retained)"}
	case <-ctx.Done():
		return sdk.Availability{Available: false, Detail: ctx.Err().Error()}
	}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{z2mBackend{a.cfg}} }

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	cli, err := connect(a.cfg.Broker, "anetlink-mqttbridge")
	if err != nil {
		return fmt.Errorf("mqttbridge: %w", err)
	}
	a.mu.Lock()
	a.cli, a.sink = cli, sink
	a.mu.Unlock()

	if tok := cli.Subscribe(a.cfg.BaseTopic+"/bridge/devices", 0, a.onDevices); tok.Wait() && tok.Error() != nil {
		return tok.Error()
	}
	if tok := cli.Subscribe(a.cfg.BaseTopic+"/+", 0, a.onState); tok.Wait() && tok.Error() != nil {
		return tok.Error()
	}
	// wait for the retained device list so Start returns with inventory
	deadline := time.Now().Add(a.cfg.Timeout)
	for {
		a.mu.Lock()
		n := len(a.binds)
		a.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (a *Adapter) Stop(context.Context) error {
	a.mu.Lock()
	cli := a.cli
	a.cli = nil
	a.mu.Unlock()
	if cli != nil {
		cli.Disconnect(100)
	}
	return nil
}

func (a *Adapter) onDevices(_ paho.Client, m paho.Message) {
	var devs []z2mDevice
	if err := json.Unmarshal(m.Payload(), &devs); err != nil {
		return
	}
	for _, d := range devs {
		if d.Definition == nil || d.FriendlyName == "" {
			continue
		}
		caps, binds := mapExposes(d.Definition.Exposes)
		if len(caps) == 0 {
			continue
		}
		a.mu.Lock()
		a.binds[d.FriendlyName] = binds
		sink := a.sink
		a.mu.Unlock()
		if sink != nil {
			sink.UpsertDevice(sdk.Device{
				ID: d.FriendlyName,
				Profile: &profile.DeviceProfile{
					Schema: 1, Vendor: d.Definition.Vendor, Model: d.Definition.Model,
					Protocol: "mqtt", Caps: caps,
					MaxTrust: profile.V2, AuthTrust: profile.A1, // state readback; broker-attested (no per-device key)
				},
				Online: true,
			})
		}
	}
}

func (a *Adapter) onState(_ paho.Client, m paho.Message) {
	parts := strings.Split(m.Topic(), "/")
	name := parts[len(parts)-1]
	if name == "devices" || strings.Contains(m.Topic(), "/bridge/") {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(m.Payload(), &payload); err != nil {
		return
	}
	a.mu.Lock()
	a.state[name] = payload
	waiters := a.wait[name]
	a.wait[name] = nil
	sink := a.sink
	a.mu.Unlock()
	for _, w := range waiters {
		select {
		case w <- payload:
		default:
		}
	}
	if sink != nil {
		metrics := map[string]float64{}
		for k, v := range payload {
			if f, ok := v.(float64); ok {
				metrics[k] = f
			}
		}
		if len(metrics) > 0 {
			sink.State(name, metrics)
		}
	}
}

func (a *Adapter) awaitState(device string) chan map[string]any {
	ch := make(chan map[string]any, 1)
	a.mu.Lock()
	a.wait[device] = append(a.wait[device], ch)
	a.mu.Unlock()
	return ch
}

func (a *Adapter) Invoke(ctx context.Context, call sdk.Call) (effect.Effect, error) {
	a.mu.Lock()
	cli := a.cli
	binds := a.binds[call.DeviceID]
	a.mu.Unlock()
	if cli == nil {
		return sdk.NewEffect().Unavailable("adapter not started").Build(), nil
	}
	bind, ok := binds[call.Capability]
	if !ok {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q on %q", call.Capability, call.DeviceID)).Build(), nil
	}
	metric := strings.ReplaceAll(call.Capability, ".", "_")

	if want, isSet := call.Args["on"].(bool); isSet && bind.binary {
		state := "OFF"
		if want {
			state = "ON"
		}
		ch := a.awaitState(call.DeviceID)
		body, _ := json.Marshal(map[string]string{bind.property: state})
		if tok := cli.Publish(a.cfg.BaseTopic+"/"+call.DeviceID+"/set", 0, false, body); tok.Wait() && tok.Error() != nil {
			return sdk.NewEffect().Fail("publish: " + tok.Error().Error()).Build(), nil
		}
		select {
		case payload := <-ch:
			got, _ := payload[bind.property].(string)
			v := 0.0
			if strings.EqualFold(got, "ON") {
				v = 1.0
			}
			b := sdk.NewEffect().Metric(metric, v).
				Requested(fmt.Sprintf("%s=%v", call.Capability, want)).
				Protocol("mqtt").NativeAck(true).
				Observed(got).
				Trust(uint8(profile.V2), uint8(profile.A1))
			if (v == 1.0) == want {
				return b.Verified().Build(), nil
			}
			return b.Fail(fmt.Sprintf("readback mismatch: wanted %s, device reports %s", state, got)).Build(), nil
		case <-time.After(a.cfg.Timeout):
			return sdk.NewEffect().Build(), nil // UNVERIFIED: sent, no confirmation
		case <-ctx.Done():
			return effect.Effect{}, ctx.Err()
		}
	}

	// GET path: ask, await fresh state, fall back to cache (UNVERIFIED).
	ch := a.awaitState(call.DeviceID)
	body, _ := json.Marshal(map[string]string{bind.property: ""})
	_ = cli.Publish(a.cfg.BaseTopic+"/"+call.DeviceID+"/get", 0, false, body)
	select {
	case payload := <-ch:
		return effectFrom(payload, bind, metric, true), nil
	case <-time.After(a.cfg.Timeout):
		a.mu.Lock()
		cached := a.state[call.DeviceID]
		a.mu.Unlock()
		if cached == nil {
			return sdk.NewEffect().Unavailable("no state from device and no cache").Build(), nil
		}
		return effectFrom(cached, bind, metric, false), nil
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
}

func effectFrom(payload map[string]any, bind capBinding, metric string, fresh bool) effect.Effect {
	b := sdk.NewEffect()
	switch v := payload[bind.property].(type) {
	case string:
		val := 0.0
		if strings.EqualFold(v, "ON") {
			val = 1.0
		}
		b.Metric(metric, val)
	case float64:
		b.Metric(metric, v)
	default:
		return sdk.NewEffect().Unavailable("property absent from device state").Build()
	}
	if fresh {
		b.Verified()
	}
	return b.Build()
}

// mapExposes flattens z2m exposes (one nesting level) into capabilities.
func mapExposes(exposes []z2mExpose) ([]profile.Capability, map[string]capBinding) {
	var caps []profile.Capability
	binds := map[string]capBinding{}
	var walk func(e z2mExpose)
	walk = func(e z2mExpose) {
		for _, f := range e.Features {
			walk(f)
		}
		if e.Property == "" {
			return
		}
		var access uint8
		if e.Access&1 != 0 {
			access |= profile.AccessState
		}
		if e.Access&2 != 0 {
			access |= profile.AccessSet
		}
		if e.Access&4 != 0 {
			access |= profile.AccessGet
		}
		if access == 0 {
			return
		}
		switch e.Type {
		case "binary":
			if e.Property == "state" {
				capID := "switch.onoff"
				caps = append(caps, profile.Capability{ID: capID, Access: access})
				binds[capID] = capBinding{property: e.Property, binary: true}
			}
		case "numeric":
			capID := "sensor." + e.Property
			caps = append(caps, profile.Capability{ID: capID, Access: access, Unit: e.Unit})
			binds[capID] = capBinding{property: e.Property}
		}
	}
	for _, e := range exposes {
		walk(e)
	}
	return caps, binds
}

func connect(broker, clientID string) (paho.Client, error) {
	opts := paho.NewClientOptions().AddBroker(broker).SetClientID(clientID).
		SetConnectTimeout(2 * time.Second).SetAutoReconnect(true)
	cli := paho.NewClient(opts)
	tok := cli.Connect()
	if !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		err := tok.Error()
		if err == nil {
			err = fmt.Errorf("connect timeout")
		}
		return nil, err
	}
	return cli, nil
}
