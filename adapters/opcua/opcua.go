// Package opcua is the OPC UA adapter — ANetLink's reach into industrial
// automation (the OPC UA branch of the physical-world narrow waist, R02 §4).
// Pure Go (gopcua), no cgo. OPC UA Variables map to properties, Methods map
// to actions; the honest model is T2 — every write is read back and the
// readback is the metric.
//
// R02 protocol mapping realized here:
//
//	OPC UA Object   -> Node (device)
//	OPC UA Variable -> Property (capability with GET, or SET+GET if writable)
//	OPC UA Method   -> Action  (invokable capability)
package opcua

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// NodeMap binds one capability id to one OPC UA node.
type NodeMap struct {
	ID     string // capability id, e.g. "sensor.temperature"
	NodeID string // OPC UA NodeID, e.g. "ns=2;s=Temperature"
	Access uint8  // profile access bits
	Unit   string
}

// DeviceConfig is one statically configured OPC UA server (an OPC UA server
// is one ANetLink Node exposing a set of variables).
type DeviceConfig struct {
	ID       string
	Endpoint string // "opc.tcp://host:port"
	Vendor   string
	Model    string
	Nodes    []NodeMap
}

// Config configures the adapter.
type Config struct {
	Devices []DeviceConfig
	Timeout time.Duration // per-op; default 4s
}

type Adapter struct {
	cfg     Config
	mu      sync.Mutex
	clients map[string]*opcua.Client
}

func New(cfg Config) *Adapter {
	if cfg.Timeout == 0 {
		cfg.Timeout = 4 * time.Second
	}
	return &Adapter{cfg: cfg, clients: map[string]*opcua.Client{}}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "opcua", Protocol: "opcua", Version: "0.1.0"}
}

type nativeBackend struct{ cfg Config }

func (b nativeBackend) Name() string { return "native" }

// Probe: the pure-Go client is self-contained; report per-endpoint reach.
func (b nativeBackend) Probe(ctx context.Context) sdk.Availability {
	reachable := 0
	for _, d := range b.cfg.Devices {
		c, err := opcua.NewClient(d.Endpoint)
		if err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		if err := c.Connect(cctx); err == nil {
			reachable++
			_ = c.Close(cctx)
		}
		cancel()
	}
	return sdk.Availability{
		Available: true,
		Detail:    fmt.Sprintf("pure-Go OPC UA client; %d/%d endpoints reachable", reachable, len(b.cfg.Devices)),
	}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{nativeBackend{a.cfg}} }

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	for _, d := range a.cfg.Devices {
		caps := make([]profile.Capability, 0, len(d.Nodes))
		for _, nm := range d.Nodes {
			caps = append(caps, profile.Capability{ID: nm.ID, Access: nm.Access, Unit: nm.Unit})
		}
		online := false
		if c, err := opcua.NewClient(d.Endpoint); err == nil {
			cctx, cancel := context.WithTimeout(ctx, time.Second)
			if err := c.Connect(cctx); err == nil {
				online = true
				_ = c.Close(cctx)
			}
			cancel()
		}
		sink.UpsertDevice(sdk.Device{
			ID: d.ID,
			Profile: &profile.DeviceProfile{
				Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "opcua",
				Caps:     caps,
				MaxTrust: profile.V2, AuthTrust: profile.A2, // value readback + protocol-authenticated session
			},
			Online: online,
		})
	}
	return nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.clients {
		_ = c.Close(ctx)
	}
	a.clients = map[string]*opcua.Client{}
	return nil
}

func (a *Adapter) client(ctx context.Context, dev *DeviceConfig) (*opcua.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[dev.ID]; ok {
		return c, nil
	}
	c, err := opcua.NewClient(dev.Endpoint)
	if err != nil {
		return nil, err
	}
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}
	a.clients[dev.ID] = c
	return c, nil
}

func (a *Adapter) Invoke(ctx context.Context, call sdk.Call) (effect.Effect, error) {
	var dev *DeviceConfig
	for i := range a.cfg.Devices {
		if a.cfg.Devices[i].ID == call.DeviceID {
			dev = &a.cfg.Devices[i]
			break
		}
	}
	if dev == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown device %q", call.DeviceID)).Build(), nil
	}
	var nm *NodeMap
	for i := range dev.Nodes {
		if dev.Nodes[i].ID == call.Capability {
			nm = &dev.Nodes[i]
			break
		}
	}
	if nm == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	c, err := a.client(cctx, dev)
	if err != nil {
		return sdk.NewEffect().Unavailable(err.Error()).Build(), nil
	}
	nid, err := ua.ParseNodeID(nm.NodeID)
	if err != nil {
		return sdk.NewEffect().Fail("bad node id: " + err.Error()).Build(), nil
	}
	metric := strings.ReplaceAll(nm.ID, ".", "_")

	// SET path: write, then read back and verify.
	if raw, isSet := call.Args["value"]; isSet {
		v, err := ua.NewVariant(coerce(raw))
		if err != nil {
			return sdk.NewEffect().Fail("bad value: " + err.Error()).Build(), nil
		}
		req := &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{
			NodeID:      nid,
			AttributeID: ua.AttributeIDValue,
			Value:       &ua.DataValue{EncodingMask: ua.DataValueValue, Value: v},
		}}}
		resp, err := c.Write(cctx, req)
		if err != nil {
			return sdk.NewEffect().Fail("write: " + err.Error()).Build(), nil
		}
		if len(resp.Results) == 0 || resp.Results[0] != ua.StatusOK {
			return sdk.NewEffect().Fail(fmt.Sprintf("write rejected: %v", resp.Results)).Build(), nil
		}
	}

	// GET / readback.
	val, err := c.Node(nid).Value(cctx)
	if err != nil {
		return sdk.NewEffect().Fail("readback: " + err.Error()).Build(), nil
	}
	if val == nil {
		return sdk.NewEffect().Unavailable("node has no value").Build(), nil
	}
	f, ok := toFloat(val.Value())
	if !ok {
		return sdk.NewEffect().Unavailable(fmt.Sprintf("non-numeric value %v", val.Value())).Build(), nil
	}
	return sdk.NewEffect().Metric(metric, f).Verified().Build(), nil
}

// coerce turns JSON-decoded args into OPC UA-friendly Go types.
func coerce(v any) any {
	switch x := v.(type) {
	case float64:
		return x
	case bool:
		return x
	case string:
		return x
	default:
		return v
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case uint16:
		return float64(x), true
	case int16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}
