// Package can is the CAN bus adapter — ANetLink's reach into vehicles and
// industrial machinery (R02 §2 bearer list). Pure Go over SocketCAN
// (go.einride.tech/can), no cgo.
//
// R02 §4 mapping: a CAN node -> Node, a signal within a frame -> Property.
// Frames are periodic broadcasts, so reads serve the last decoded value and
// writes transmit a frame. Honest trust: V1/A0 by default — classic CAN has
// neither device identity nor an application-level ack, so a bare write is
// UNVERIFIED unless the signal is also readable (then V2 via readback).
package can

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.einride.tech/can"
	"go.einride.tech/can/pkg/socketcan"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// SignalMap binds one capability to one signal inside a CAN frame.
type SignalMap struct {
	ID       string  // capability id, e.g. "sensor.rpm"
	FrameID  uint32  // CAN identifier
	StartBit uint8   // little-endian start bit within the 8-byte payload
	Length   uint8   // bit length (1..32)
	Scale    float64 // physical = raw*Scale + Offset (Scale 0 → 1.0)
	Offset   float64
	Access   uint8
	Unit     string
	Writable bool // if true, SET transmits a frame with this signal set
}

// DeviceConfig is one CAN node (a logical grouping of signals).
type DeviceConfig struct {
	ID      string
	Iface   string // SocketCAN interface, e.g. "can0" or "vcan0"
	Vendor  string
	Model   string
	Signals []SignalMap
}

// Config configures the adapter.
type Config struct {
	Devices []DeviceConfig
	Timeout time.Duration // read wait for a fresh frame; default 2s
}

type Adapter struct {
	cfg    Config
	mu     sync.RWMutex
	latest map[uint32]can.Frame // last frame seen per id
	stop   context.CancelFunc
	sink   sdk.Sink
}

func New(cfg Config) *Adapter {
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &Adapter{cfg: cfg, latest: map[uint32]can.Frame{}}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "can", Protocol: "can", Version: "0.1.0"}
}

type socketBackend struct{ cfg Config }

func (b socketBackend) Name() string { return "socketcan" }

// Probe: SocketCAN needs the kernel interface to exist. A missing interface
// is reported honestly rather than pretending the adapter is usable.
func (b socketBackend) Probe(ctx context.Context) sdk.Availability {
	up := 0
	for _, d := range b.cfg.Devices {
		conn, err := socketcan.DialContext(ctx, "can", d.Iface)
		if err == nil {
			up++
			_ = conn.Close()
		}
	}
	if up == 0 && len(b.cfg.Devices) > 0 {
		return sdk.Availability{Available: false,
			Detail: "no SocketCAN interface reachable (is can0/vcan0 up? `ip link set vcan0 up`)"}
	}
	return sdk.Availability{Available: true, Detail: fmt.Sprintf("socketcan; %d/%d interfaces up", up, len(b.cfg.Devices))}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{socketBackend{a.cfg}} }

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	rctx, cancel := context.WithCancel(context.Background())
	a.stop = cancel
	a.sink = sink
	for _, d := range a.cfg.Devices {
		caps := make([]profile.Capability, 0, len(d.Signals))
		writable := false
		for _, s := range d.Signals {
			caps = append(caps, profile.Capability{ID: s.ID, Access: s.Access, Unit: s.Unit})
			if s.Writable {
				writable = true
			}
		}
		conn, err := socketcan.DialContext(ctx, "can", d.Iface)
		online := err == nil
		if online {
			go a.receive(rctx, conn)
		}
		// A signal that can be read back after a write earns V2; a
		// write-only bus stays V1 (transmitted, unverifiable).
		verify := profile.V2
		if writable && len(caps) == 0 {
			verify = profile.V1
		}
		sink.UpsertDevice(sdk.Device{
			ID: d.ID,
			Profile: &profile.DeviceProfile{
				Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "can",
				Caps:      caps,
				MaxTrust:  verify,
				AuthTrust: profile.A0, // classic CAN has no device identity
			},
			Online: online,
		})
	}
	return nil
}

// receive pumps frames from the bus into the latest-value cache until the
// adapter stops.
func (a *Adapter) receive(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	recv := socketcan.NewReceiver(conn)
	for recv.Receive() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		a.Ingest(recv.Frame())
	}
}

func (a *Adapter) Stop(context.Context) error {
	if a.stop != nil {
		a.stop()
	}
	return nil
}

// decodeSignal extracts a little-endian signal from a frame payload.
func decodeSignal(data can.Data, start, length uint8) uint64 {
	var v uint64
	for i := uint8(0); i < length; i++ {
		bit := start + i
		byteIdx, bitIdx := bit/8, bit%8
		if int(byteIdx) >= len(data) {
			break
		}
		if data[byteIdx]&(1<<bitIdx) != 0 {
			v |= 1 << i
		}
	}
	return v
}

// encodeSignal writes a signal into a frame payload.
func encodeSignal(data *can.Data, start, length uint8, raw uint64) {
	for i := uint8(0); i < length; i++ {
		bit := start + i
		byteIdx, bitIdx := bit/8, bit%8
		if int(byteIdx) >= len(data) {
			break
		}
		if raw&(1<<i) != 0 {
			data[byteIdx] |= 1 << bitIdx
		} else {
			data[byteIdx] &^= 1 << bitIdx
		}
	}
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
	var sm *SignalMap
	for i := range dev.Signals {
		if dev.Signals[i].ID == call.Capability {
			sm = &dev.Signals[i]
			break
		}
	}
	if sm == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	metric := strings.ReplaceAll(sm.ID, ".", "_")
	scale := sm.Scale
	if scale == 0 {
		scale = 1
	}

	if raw, isSet := call.Args["value"]; isSet {
		if !sm.Writable {
			return sdk.NewEffect().Fail("signal is not writable").Build(), nil
		}
		f, ok := raw.(float64)
		if !ok {
			return sdk.NewEffect().Fail("args.value must be a number").Build(), nil
		}
		tx, err := socketcan.DialContext(ctx, "can", dev.Iface)
		if err != nil {
			return sdk.NewEffect().Unavailable("dial: " + err.Error()).Build(), nil
		}
		defer tx.Close()
		var frame can.Frame
		frame.ID = sm.FrameID
		frame.Length = 8
		encodeSignal(&frame.Data, sm.StartBit, sm.Length, uint64((f-sm.Offset)/scale))
		if err := socketcan.NewTransmitter(tx).TransmitFrame(ctx, frame); err != nil {
			return sdk.NewEffect().Unavailable("transmit: " + err.Error()).Build(), nil
		}
		// Classic CAN gives no application ack: transmitted ≠ verified.
		return sdk.NewEffect().Metric(metric, f).
			Requested(fmt.Sprintf("%s=%v", sm.ID, f)).
			Protocol("can").NativeAck(false).
			Trust(uint8(profile.V1), uint8(profile.A0)).
			Build(), nil // UNVERIFIED by construction
	}

	// GET: serve the latest decoded frame.
	a.mu.RLock()
	frame, ok := a.latest[sm.FrameID]
	a.mu.RUnlock()
	if !ok {
		return sdk.NewEffect().Unavailable(fmt.Sprintf("no frame 0x%X seen yet", sm.FrameID)).Build(), nil
	}
	rawVal := decodeSignal(frame.Data, sm.StartBit, sm.Length)
	phys := float64(rawVal)*scale + sm.Offset
	return sdk.NewEffect().Metric(metric, phys).
		Protocol("can").NativeAck(true).
		Observed(strconv.FormatFloat(phys, 'f', -1, 64)).
		Trust(uint8(profile.V2), uint8(profile.A0)).
		Verified().Build(), nil
}

// Ingest feeds a frame into the adapter's latest-value cache (used by the
// receive loop and by tests driving a virtual bus).
func (a *Adapter) Ingest(f can.Frame) {
	a.mu.Lock()
	a.latest[f.ID] = f
	a.mu.Unlock()
	if a.sink != nil {
		for _, d := range a.cfg.Devices {
			for _, s := range d.Signals {
				if s.FrameID != f.ID {
					continue
				}
				scale := s.Scale
				if scale == 0 {
					scale = 1
				}
				v := float64(decodeSignal(f.Data, s.StartBit, s.Length))*scale + s.Offset
				a.sink.State(d.ID, map[string]float64{strings.ReplaceAll(s.ID, ".", "_"): v})
			}
		}
	}
}
