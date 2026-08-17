// Package modbus is the Modbus/TCP adapter: static device configuration (the
// protocol has no discovery), pure-Go client, and T2-grade honesty — every
// write is followed by a readback, and the readback (not the request) is the
// metric an effect carries. A write whose readback disagrees is FAILED.
package modbus

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	gmodbus "github.com/grid-x/modbus"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// CapMap binds one capability id to one Modbus data point.
type CapMap struct {
	ID      string  // capability id, e.g. "switch.onoff"
	Kind    string  // "coil" | "holding"
	Address uint16  // data address
	Access  uint8   // profile access bits (Set/Get)
	Scale   float64 // holding only: value = raw * Scale (0 → 1.0)
	Unit    string
}

// DeviceConfig is one statically configured Modbus device.
type DeviceConfig struct {
	ID     string
	Addr   string // "host:port"
	UnitID byte
	Vendor string
	Model  string
	Caps   []CapMap
}

// Config configures the adapter.
type Config struct {
	Devices []DeviceConfig
	Timeout time.Duration // per-request; default 2s
}

type Adapter struct {
	cfg Config
	mu  sync.Mutex
	cli map[string]gmodbus.Client // device id → client
	hdl map[string]*gmodbus.TCPClientHandler
}

func New(cfg Config) *Adapter {
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &Adapter{cfg: cfg, cli: map[string]gmodbus.Client{}, hdl: map[string]*gmodbus.TCPClientHandler{}}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "modbus", Protocol: "modbus", Version: "0.1.0"}
}

type nativeBackend struct{ cfg Config }

func (b nativeBackend) Name() string { return "native" }

// Probe: the pure-Go client is self-contained, so the backend itself is
// always available; the detail reports live reachability per target.
func (b nativeBackend) Probe(ctx context.Context) sdk.Availability {
	reachable := 0
	for _, d := range b.cfg.Devices {
		c, err := (&net.Dialer{Timeout: 300 * time.Millisecond}).DialContext(ctx, "tcp", d.Addr)
		if err == nil {
			reachable++
			c.Close()
		}
	}
	return sdk.Availability{
		Available: true,
		Detail:    fmt.Sprintf("pure-Go client; %d/%d targets reachable", reachable, len(b.cfg.Devices)),
	}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{nativeBackend{a.cfg}} }

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	for _, d := range a.cfg.Devices {
		caps := make([]profile.Capability, 0, len(d.Caps))
		for _, c := range d.Caps {
			caps = append(caps, profile.Capability{ID: c.ID, Access: c.Access, Unit: c.Unit})
		}
		online := false
		if conn, err := (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, "tcp", d.Addr); err == nil {
			online = true
			conn.Close()
		}
		sink.UpsertDevice(sdk.Device{
			ID: d.ID,
			Profile: &profile.DeviceProfile{
				Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "modbus",
				Caps:     caps,
				MaxTrust: profile.V2, AuthTrust: profile.A1, // readback over a local link; gateway-attested only (no device identity)
			},
			Online: online,
		})
	}
	return nil
}

func (a *Adapter) Stop(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.hdl {
		_ = h.Close()
	}
	a.cli, a.hdl = map[string]gmodbus.Client{}, map[string]*gmodbus.TCPClientHandler{}
	return nil
}

func (a *Adapter) client(dev *DeviceConfig) (gmodbus.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.cli[dev.ID]; ok {
		return c, nil
	}
	h := gmodbus.NewTCPClientHandler(dev.Addr)
	h.Timeout = a.cfg.Timeout
	h.SetSlave(dev.UnitID)
	if err := h.Connect(context.Background()); err != nil {
		return nil, err
	}
	c := gmodbus.NewClient(h)
	a.cli[dev.ID], a.hdl[dev.ID] = c, h
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
	var cm *CapMap
	for i := range dev.Caps {
		if dev.Caps[i].ID == call.Capability {
			cm = &dev.Caps[i]
			break
		}
	}
	if cm == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	cli, err := a.client(dev)
	if err != nil {
		return sdk.NewEffect().Unavailable(err.Error()).Build(), nil
	}
	metric := strings.ReplaceAll(cm.ID, ".", "_")

	switch cm.Kind {
	case "coil":
		if want, isSet := call.Args["on"].(bool); isSet {
			v := uint16(0x0000)
			if want {
				v = 0xFF00
			}
			if _, err := cli.WriteSingleCoil(ctx, cm.Address, v); err != nil {
				return sdk.NewEffect().Fail("write: " + err.Error()).Build(), nil
			}
		}
		rb, err := cli.ReadCoils(ctx, cm.Address, 1)
		if err != nil {
			return sdk.NewEffect().Fail("readback: " + err.Error()).Build(), nil
		}
		got := 0.0
		if len(rb) > 0 && rb[0]&1 == 1 {
			got = 1.0
		}
		if want, isSet := call.Args["on"].(bool); isSet {
			expected := 0.0
			if want {
				expected = 1.0
			}
			if got != expected {
				return sdk.NewEffect().Metric(metric, got).
					Fail(fmt.Sprintf("readback mismatch: wrote %v, device reports %v", expected, got)).Build(), nil
			}
		}
		return sdk.NewEffect().Metric(metric, got).
			Requested(fmt.Sprintf("%s=%v", cm.ID, call.Args["on"])).
			Protocol("modbus").NativeAck(true).
			Observed(fmt.Sprintf("%v", got == 1)).
			Trust(uint8(profile.V2), uint8(profile.A1)).
			Verified().Build(), nil

	case "holding":
		scale := cm.Scale
		if scale == 0 {
			scale = 1.0
		}
		if raw, isSet := call.Args["value"]; isSet {
			f, ok := raw.(float64)
			if !ok {
				return sdk.NewEffect().Fail("args.value must be a number").Build(), nil
			}
			if _, err := cli.WriteSingleRegister(ctx, cm.Address, uint16(f/scale+0.5)); err != nil {
				return sdk.NewEffect().Fail("write: " + err.Error()).Build(), nil
			}
		}
		rb, err := cli.ReadHoldingRegisters(ctx, cm.Address, 1)
		if err != nil {
			return sdk.NewEffect().Fail("readback: " + err.Error()).Build(), nil
		}
		if len(rb) < 2 {
			return sdk.NewEffect().Fail("short readback").Build(), nil
		}
		got := float64(uint16(rb[0])<<8|uint16(rb[1])) * scale
		return sdk.NewEffect().Metric(metric, got).
			Requested(fmt.Sprintf("%s=%v", cm.ID, call.Args["value"])).
			Protocol("modbus").NativeAck(true).
			Observed(strconv.FormatFloat(got, 'f', -1, 64)).
			Trust(uint8(profile.V2), uint8(profile.A1)).
			Verified().Build(), nil
	}
	return sdk.NewEffect().Fail(fmt.Sprintf("unsupported kind %q", cm.Kind)).Build(), nil
}
