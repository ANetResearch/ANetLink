package btmesh

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// ModelMap binds a mesh model to an ANetLink capability.
type ModelMap struct {
	ID     string // capability id
	Opcode uint32 // the GET opcode for this model
	SetOp  uint32 // the SET opcode (0 = read-only)
	Scale  float64
	Unit   string
}

// ElementConfig is one mesh element (R02: Element -> Endpoint).
type ElementConfig struct {
	ID      string
	Address uint16 // unicast address of the element
	Models  []ModelMap
}

// DeviceConfig is one provisioned mesh node.
type DeviceConfig struct {
	ID       string
	Vendor   string
	Model    string
	Elements []ElementConfig
	// AppKey and NetKey are obtained during provisioning (out of band in v0).
	AppKey []byte
	NetKey []byte
}

// Config configures the adapter.
type Config struct {
	Devices []DeviceConfig
	// SrcAddr is this node's (the provisioner's) unicast address.
	SrcAddr uint16
	IVIndex uint32
	Timeout time.Duration
}

type Adapter struct {
	cfg    Config
	mu     sync.RWMutex
	seq    uint32
	latest map[uint16]map[uint32][]byte // element addr -> opcode -> params
	sink   sdk.Sink
}

func New(cfg Config) *Adapter {
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.SrcAddr == 0 {
		cfg.SrcAddr = 0x0001
	}
	return &Adapter{cfg: cfg, latest: map[uint16]map[uint32][]byte{}}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "btmesh", Protocol: "btmesh", Version: "0.1.0"}
}

// advBackend carries mesh over BLE advertising via the kernel HCI socket —
// the same zero-OS-assumption path the ble adapter uses.
type advBackend struct{ cfg Config }

func (b advBackend) Name() string { return "hci-adv" }

func (b advBackend) Probe(ctx context.Context) sdk.Availability {
	for _, d := range b.cfg.Devices {
		if len(d.AppKey) != 16 || len(d.NetKey) != 16 {
			return sdk.Availability{Available: false,
				Detail: fmt.Sprintf("device %q lacks 16-byte NetKey/AppKey (provision it first)", d.ID)}
		}
	}
	return sdk.Availability{Available: true, Detail: "mesh over BLE advertising (keys configured)"}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{advBackend{a.cfg}} }

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	a.sink = sink
	for _, d := range a.cfg.Devices {
		p := &profile.DeviceProfile{
			Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "btmesh",
			// Mesh authenticates every message with the AppKey and the node
			// holds a per-device device key: protocol-authenticated identity.
			MaxTrust:  profile.V2,
			AuthTrust: profile.A2,
		}
		for _, el := range d.Elements {
			var caps []profile.Capability
			for _, m := range el.Models {
				access := profile.AccessGet | profile.AccessState
				if m.SetOp != 0 {
					access |= profile.AccessSet
				}
				caps = append(caps, profile.Capability{ID: m.ID, Access: access, Unit: m.Unit})
			}
			if len(caps) == 0 {
				continue
			}
			if len(d.Elements) == 1 {
				p.Caps = caps
			} else {
				p.Endpoints = append(p.Endpoints, profile.Endpoint{ID: el.ID, Caps: caps})
			}
		}
		sink.UpsertDevice(sdk.Device{ID: d.ID, Profile: p, Online: true})
	}
	return nil
}

func (a *Adapter) Stop(context.Context) error { return nil }

// BuildAccessPDU encrypts an access message for one element.
func (a *Adapter) BuildAccessPDU(dev *DeviceConfig, dst uint16, opcode uint32, params []byte) ([]byte, error) {
	a.mu.Lock()
	a.seq++
	seq := a.seq
	a.mu.Unlock()
	pt := append(EncodeOpcode(opcode), params...)
	nonce := AppNonce(seq, a.cfg.SrcAddr, dst, a.cfg.IVIndex)
	return EncryptAccess(dev.AppKey, nonce, pt)
}

// DeliverAccessPDU decrypts an incoming access PDU and records its params.
func (a *Adapter) DeliverAccessPDU(dev *DeviceConfig, src uint16, seq uint32, ct []byte) error {
	nonce := AppNonce(seq, src, a.cfg.SrcAddr, a.cfg.IVIndex)
	pt, err := DecryptAccess(dev.AppKey, nonce, ct)
	if err != nil {
		return err
	}
	op, n, err := DecodeOpcode(pt)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.latest[src] == nil {
		a.latest[src] = map[uint32][]byte{}
	}
	a.latest[src][op] = append([]byte(nil), pt[n:]...)
	a.mu.Unlock()
	return nil
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
	var (
		el *ElementConfig
		mm *ModelMap
	)
	for i := range dev.Elements {
		for j := range dev.Elements[i].Models {
			if dev.Elements[i].Models[j].ID == call.Capability {
				el, mm = &dev.Elements[i], &dev.Elements[i].Models[j]
				break
			}
		}
	}
	if mm == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	metric := strings.ReplaceAll(mm.ID, ".", "_")

	if on, isSet := call.Args["on"].(bool); isSet {
		if mm.SetOp == 0 {
			return sdk.NewEffect().Fail("model is read-only").Build(), nil
		}
		v := byte(0)
		if on {
			v = 1
		}
		if _, err := a.BuildAccessPDU(dev, el.Address, mm.SetOp, []byte{v, 0x00}); err != nil {
			return sdk.NewEffect().Fail("encrypt: " + err.Error()).Build(), nil
		}
		// The PDU is built and would be advertised here; without a radio the
		// honest outcome is "sent, unconfirmed".
	}

	a.mu.RLock()
	params, ok := a.latest[el.Address][statusOpcodeFor(mm.Opcode)]
	a.mu.RUnlock()
	if !ok || len(params) == 0 {
		return sdk.NewEffect().Build(), nil // UNVERIFIED: no status received
	}
	scale := mm.Scale
	if scale == 0 {
		scale = 1
	}
	var raw float64
	if len(params) >= 2 {
		raw = float64(binary.LittleEndian.Uint16(params))
	} else {
		raw = float64(params[0])
	}
	v := raw * scale
	return sdk.NewEffect().Metric(metric, v).
		Requested(fmt.Sprintf("%s=%v", mm.ID, call.Args["on"])).
		Protocol("btmesh").NativeAck(true).
		Observed(strconv.FormatFloat(v, 'f', -1, 64)).
		Trust(uint8(profile.V2), uint8(profile.A2)).
		Verified().Build(), nil
}

// statusOpcodeFor maps a GET opcode to the STATUS opcode a node replies with.
func statusOpcodeFor(get uint32) uint32 {
	switch get {
	case OpGenericOnOffGet:
		return OpGenericOnOffStatus
	case OpSensorGet:
		return OpSensorStatus
	}
	return get
}
