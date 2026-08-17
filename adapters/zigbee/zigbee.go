package zigbee

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// Z-Stack MT commands we use (AF subsystem).
const (
	afDataRequest byte = 0x01
	afIncomingMsg byte = 0x81
	sysPing       byte = 0x01
	sysVersion    byte = 0x02
)

// EndpointConfig is one Zigbee endpoint on a device.
type EndpointConfig struct {
	ID       string   // ANetLink endpoint id
	Endpoint byte     // Zigbee endpoint number
	Clusters []uint16 // clusters present on it
}

// DeviceConfig is one joined Zigbee device (paired out of band or by a
// previous session; commissioning/permit-join is a follow-up).
type DeviceConfig struct {
	ID        string
	ShortAddr uint16 // network address
	Vendor    string
	Model     string
	Endpoints []EndpointConfig
}

// Config configures the adapter.
type Config struct {
	// Port is the coordinator serial device, e.g. "/dev/ttyUSB0".
	Port string
	// Baud defaults to 115200 (Z-Stack default).
	Baud    int
	Devices []DeviceConfig
	Timeout time.Duration
}

type Adapter struct {
	cfg  Config
	mu   sync.Mutex
	port serial.Port
	seq  byte
	sink sdk.Sink
	stop context.CancelFunc
	// pending correlates AF data confirms/responses by (addr, cluster).
	pending map[uint32]chan []byte
}

func New(cfg Config) *Adapter {
	if cfg.Baud == 0 {
		cfg.Baud = 115200
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Adapter{cfg: cfg, pending: map[uint32]chan []byte{}}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "zigbee", Protocol: "zigbee", Version: "0.1.0"}
}

// coordinatorBackend is the self-contained path: our own Z-Stack MT driver
// over the serial port. No zigbee2mqtt, no Python.
type coordinatorBackend struct{ cfg Config }

func (b coordinatorBackend) Name() string { return "zstack" }

func (b coordinatorBackend) Probe(ctx context.Context) sdk.Availability {
	if b.cfg.Port == "" {
		return sdk.Availability{Available: false, Detail: "no coordinator port configured"}
	}
	p, err := serial.Open(b.cfg.Port, &serial.Mode{BaudRate: b.cfg.Baud})
	if err != nil {
		return sdk.Availability{Available: false,
			Detail: fmt.Sprintf("coordinator %s not openable: %v", b.cfg.Port, err)}
	}
	defer p.Close()
	// SYS_PING proves it is a Z-Stack coordinator, not just any tty.
	f := &Frame{Type: TypeSREQ, Subsys: SubsysSys, Cmd: sysPing}
	pkt, _ := f.Encode()
	_ = p.SetReadTimeout(time.Second)
	if _, err := p.Write(pkt); err != nil {
		return sdk.Availability{Available: false, Detail: "write SYS_PING: " + err.Error()}
	}
	if _, err := ReadFrame(p); err != nil {
		return sdk.Availability{Available: false,
			Detail: fmt.Sprintf("%s opened but did not answer SYS_PING (not a Z-Stack coordinator?)", b.cfg.Port)}
	}
	return sdk.Availability{Available: true, Detail: "Z-Stack coordinator on " + b.cfg.Port}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{coordinatorBackend{a.cfg}} }

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	p, err := serial.Open(a.cfg.Port, &serial.Mode{BaudRate: a.cfg.Baud})
	if err != nil {
		return fmt.Errorf("zigbee: open %s: %w", a.cfg.Port, err)
	}
	a.port = p
	a.sink = sink
	rctx, cancel := context.WithCancel(context.Background())
	a.stop = cancel
	go a.readLoop(rctx, p)

	for _, d := range a.cfg.Devices {
		sink.UpsertDevice(sdk.Device{ID: d.ID, Profile: a.profileFor(d), Online: true})
	}
	return nil
}

// profileFor projects the device's clusters into capabilities/endpoints
// (R02 §4: Endpoint -> Endpoint, Cluster -> Capability).
func (a *Adapter) profileFor(d DeviceConfig) *profile.DeviceProfile {
	p := &profile.DeviceProfile{
		Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "zigbee",
		// Zigbee reads back attributes over an encrypted network with a
		// per-device link key, so identity is protocol-authenticated.
		MaxTrust:  profile.V2,
		AuthTrust: profile.A2,
	}
	for _, ep := range d.Endpoints {
		var caps []profile.Capability
		for _, cl := range ep.Clusters {
			cc, ok := CapabilityForCluster(cl)
			if !ok {
				continue
			}
			access := profile.AccessGet | profile.AccessState
			if cc.Writable {
				access |= profile.AccessSet
			}
			caps = append(caps, profile.Capability{ID: cc.Capability, Access: access, Unit: cc.Unit})
		}
		if len(caps) == 0 {
			continue
		}
		if len(d.Endpoints) == 1 {
			p.Caps = caps // single-endpoint device: flat form
		} else {
			p.Endpoints = append(p.Endpoints, profile.Endpoint{ID: ep.ID, Caps: caps})
		}
	}
	return p
}

func (a *Adapter) readLoop(ctx context.Context, r io.Reader) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		f, err := ReadFrame(r)
		if err != nil {
			return
		}
		if f.Subsys == SubsysAF && f.Cmd == afIncomingMsg && len(f.Data) > 17 {
			// AF_INCOMING_MSG: groupID(2) clusterID(2) srcAddr(2) srcEP(1)
			// dstEP(1) wasBroadcast(1) linkQuality(1) securityUse(1)
			// timestamp(4) seq(1) len(1) data[]
			cluster := binary.LittleEndian.Uint16(f.Data[2:])
			addr := binary.LittleEndian.Uint16(f.Data[4:])
			payload := f.Data[17:]
			if n := int(f.Data[16]); n <= len(payload) {
				payload = payload[:n]
			}
			a.deliver(addr, cluster, payload)
		}
	}
}

func key(addr, cluster uint16) uint32 { return uint32(addr)<<16 | uint32(cluster) }

func (a *Adapter) deliver(addr, cluster uint16, payload []byte) {
	a.mu.Lock()
	ch := a.pending[key(addr, cluster)]
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (a *Adapter) await(addr, cluster uint16) chan []byte {
	ch := make(chan []byte, 1)
	a.mu.Lock()
	a.pending[key(addr, cluster)] = ch
	a.mu.Unlock()
	return ch
}

// sendAF wraps a ZCL payload in an AF_DATA_REQUEST and writes it.
func (a *Adapter) sendAF(addr uint16, endpoint byte, cluster uint16, zcl []byte) error {
	a.mu.Lock()
	a.seq++
	seq := a.seq
	port := a.port
	a.mu.Unlock()
	if port == nil {
		return fmt.Errorf("zigbee: coordinator not started")
	}
	data := make([]byte, 0, 10+len(zcl))
	data = binary.LittleEndian.AppendUint16(data, addr)
	data = append(data, endpoint, 1) // dstEndpoint, srcEndpoint
	data = binary.LittleEndian.AppendUint16(data, cluster)
	data = append(data, seq, 0x00, 30) // transID, options, radius
	data = append(data, byte(len(zcl)))
	data = append(data, zcl...)
	f := &Frame{Type: TypeSREQ, Subsys: SubsysAF, Cmd: afDataRequest, Data: data}
	pkt, err := f.Encode()
	if err != nil {
		return err
	}
	_, err = port.Write(pkt)
	return err
}

func (a *Adapter) Stop(context.Context) error {
	if a.stop != nil {
		a.stop()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.port != nil {
		return a.port.Close()
	}
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
	// resolve capability -> (endpoint, cluster)
	var (
		ep  byte
		cc  ClusterCapability
		got bool
	)
	for _, e := range dev.Endpoints {
		for _, cl := range e.Clusters {
			if c, ok := CapabilityForCluster(cl); ok && c.Capability == call.Capability {
				ep, cc, got = e.Endpoint, c, true
				break
			}
		}
		if got {
			break
		}
	}
	if !got {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	metric := strings.ReplaceAll(cc.Capability, ".", "_")

	a.mu.Lock()
	a.seq++
	zseq := a.seq
	a.mu.Unlock()

	if on, isSet := call.Args["on"].(bool); isSet {
		if !cc.Writable {
			return sdk.NewEffect().Fail("capability is read-only").Build(), nil
		}
		if err := a.sendAF(dev.ShortAddr, ep, cc.Cluster, BuildOnOffCommand(zseq, on)); err != nil {
			return sdk.NewEffect().Unavailable(err.Error()).Build(), nil
		}
	}

	// Read back the attribute — the readback is the metric (T2 honesty).
	ch := a.await(dev.ShortAddr, cc.Cluster)
	if err := a.sendAF(dev.ShortAddr, ep, cc.Cluster, BuildReadAttributes(zseq+1, cc.Attribute)); err != nil {
		return sdk.NewEffect().Unavailable(err.Error()).Build(), nil
	}
	select {
	case payload := <-ch:
		attrs, err := ParseReadAttributesResponse(payload)
		if err != nil {
			return sdk.NewEffect().Fail("readback: " + err.Error()).Build(), nil
		}
		for _, av := range attrs {
			if av.ID != cc.Attribute || av.Status != 0 {
				continue
			}
			scale := cc.Scale
			if scale == 0 {
				scale = 1
			}
			v := av.Value * scale
			return sdk.NewEffect().Metric(metric, v).
				Requested(fmt.Sprintf("%s=%v", cc.Capability, call.Args["on"])).
				Protocol("zigbee").NativeAck(true).
				Observed(strconv.FormatFloat(v, 'f', -1, 64)).
				Trust(uint8(profile.V2), uint8(profile.A2)).
				Verified().Build(), nil
		}
		return sdk.NewEffect().Fail("attribute not in response").Build(), nil
	case <-time.After(a.cfg.Timeout):
		return sdk.NewEffect().Build(), nil // sent, no readback: UNVERIFIED
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
}

// Deliver injects an incoming ZCL payload (used by tests and by non-serial
// transports).
func (a *Adapter) Deliver(addr, cluster uint16, payload []byte) { a.deliver(addr, cluster, payload) }
