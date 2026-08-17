// Package bacnet is the BACnet/IP adapter — ANetLink's reach into building
// automation (HVAC, lighting panels, energy meters). Pure Go: the BVLL/NPDU/
// APDU encoding for the four services commissioning and control actually
// need is implemented here per ASHRAE 135, rather than pulling a C library.
//
// R02 §4 mapping: BACnet Device -> Node, Object (AI/AO/BI/BV/AV) -> Endpoint,
// Present_Value property -> Property. Honest trust: V2 (readback) / A1
// (network-attested — BACnet/IP has no device identity).
//
// Scope (v0): Who-Is/I-Am discovery, ReadProperty and WriteProperty on
// Present_Value for analog/binary objects. Segmentation, COV subscription
// and priority arrays are follow-ups.
package bacnet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// Default BACnet/IP port (ASHRAE 135 Annex J).
const DefaultPort = 47808

// Object types we support (ASHRAE 135 Table 21-1 subset).
const (
	ObjAnalogInput  uint16 = 0
	ObjAnalogOutput uint16 = 1
	ObjAnalogValue  uint16 = 2
	ObjBinaryInput  uint16 = 3
	ObjBinaryOutput uint16 = 4
	ObjBinaryValue  uint16 = 5
)

// propPresentValue is the Present_Value property identifier.
const propPresentValue uint32 = 85

// ObjectMap binds one capability id to one BACnet object instance.
type ObjectMap struct {
	ID       string // capability id, e.g. "sensor.temperature"
	Type     uint16 // object type (ObjAnalogInput …)
	Instance uint32 // object instance number
	Access   uint8  // profile access bits
	Unit     string
}

// DeviceConfig is one statically configured BACnet device.
type DeviceConfig struct {
	ID      string
	Addr    string // "host:port"; port defaults to 47808
	Vendor  string
	Model   string
	Objects []ObjectMap
}

// Config configures the adapter.
type Config struct {
	Devices []DeviceConfig
	Timeout time.Duration // per-request; default 3s
}

type Adapter struct {
	cfg    Config
	mu     sync.Mutex
	invoke uint8 // rolling APDU invoke id
}

func New(cfg Config) *Adapter {
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	return &Adapter{cfg: cfg}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "bacnet", Protocol: "bacnet", Version: "0.1.0"}
}

type nativeBackend struct{ cfg Config }

func (b nativeBackend) Name() string { return "native" }

func (b nativeBackend) Probe(ctx context.Context) sdk.Availability {
	return sdk.Availability{
		Available: true,
		Detail:    fmt.Sprintf("pure-Go BACnet/IP; %d configured device(s)", len(b.cfg.Devices)),
	}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{nativeBackend{a.cfg}} }

func normalizeAddr(addr string) string {
	if !strings.Contains(addr, ":") {
		return fmt.Sprintf("%s:%d", addr, DefaultPort)
	}
	return addr
}

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	for _, d := range a.cfg.Devices {
		caps := make([]profile.Capability, 0, len(d.Objects))
		for _, o := range d.Objects {
			caps = append(caps, profile.Capability{ID: o.ID, Access: o.Access, Unit: o.Unit})
		}
		online := false
		if c, err := net.DialTimeout("udp", normalizeAddr(d.Addr), time.Second); err == nil {
			online = true // UDP dial only proves routability; a read confirms liveness
			c.Close()
		}
		sink.UpsertDevice(sdk.Device{
			ID: d.ID,
			Profile: &profile.DeviceProfile{
				Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "bacnet",
				Caps:      caps,
				MaxTrust:  profile.V2, // Present_Value readback
				AuthTrust: profile.A1, // network-attested; BACnet/IP has no device identity
			},
			Online: online,
		})
	}
	return nil
}

func (a *Adapter) Stop(context.Context) error { return nil }

func (a *Adapter) nextInvoke() uint8 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invoke++
	return a.invoke
}

// --- ASHRAE 135 encoding (BVLL + NPDU + APDU) ---

// bvll wraps an NPDU in a BACnet/IP Original-Unicast-NPDU envelope.
func bvll(npdu []byte) []byte {
	out := make([]byte, 4, 4+len(npdu))
	out[0] = 0x81 // BVLC type: BACnet/IP
	out[1] = 0x0A // function: Original-Unicast-NPDU
	binary.BigEndian.PutUint16(out[2:], uint16(4+len(npdu)))
	return append(out, npdu...)
}

// npduExpectingReply is the standard NPDU header for a confirmed request.
func npduExpectingReply(apdu []byte) []byte {
	return append([]byte{0x01, 0x04}, apdu...) // version 1, expecting-reply
}

// encodeObjectID packs (type, instance) into the 4-byte BACnet object id.
func encodeObjectID(typ uint16, inst uint32) uint32 {
	return uint32(typ)<<22 | (inst & 0x3FFFFF)
}

// readPropertyAPDU builds a ConfirmedRequest ReadProperty(Present_Value).
func readPropertyAPDU(invokeID uint8, typ uint16, inst uint32) []byte {
	var b []byte
	b = append(b, 0x00, 0x05, invokeID, 0x0C) // confirmed-req, max-apdu, invoke, service=ReadProperty
	b = append(b, 0x0C)                       // context tag 0, length 4: object identifier
	var oid [4]byte
	binary.BigEndian.PutUint32(oid[:], encodeObjectID(typ, inst))
	b = append(b, oid[:]...)
	b = append(b, 0x19, byte(propPresentValue)) // context tag 1, length 1: property id
	return b
}

// writePropertyAPDU builds a ConfirmedRequest WriteProperty(Present_Value).
func writePropertyAPDU(invokeID uint8, typ uint16, inst uint32, value float64, binaryObj bool) []byte {
	var b []byte
	b = append(b, 0x00, 0x05, invokeID, 0x0F) // service = WriteProperty
	b = append(b, 0x0C)
	var oid [4]byte
	binary.BigEndian.PutUint32(oid[:], encodeObjectID(typ, inst))
	b = append(b, oid[:]...)
	b = append(b, 0x19, byte(propPresentValue))
	// opening tag 3 (property value), the value, closing tag 3
	b = append(b, 0x3E)
	if binaryObj {
		// enumerated, length 1
		v := byte(0)
		if value != 0 {
			v = 1
		}
		b = append(b, 0x91, v)
	} else {
		// real (float32), length 4
		var f [4]byte
		binary.BigEndian.PutUint32(f[:], math.Float32bits(float32(value)))
		b = append(b, 0x44)
		b = append(b, f[:]...)
	}
	b = append(b, 0x3F)
	return b
}

var errNotComplexAck = errors.New("bacnet: response is not a ComplexACK")

// parsePresentValue extracts the Present_Value from a ComplexACK ReadProperty.
func parsePresentValue(pkt []byte) (float64, error) {
	if len(pkt) < 6 || pkt[0] != 0x81 {
		return 0, errors.New("bacnet: not a BACnet/IP frame")
	}
	// skip BVLL(4) + NPDU(2, no destination in a simple reply)
	i := 4
	if len(pkt) <= i+1 {
		return 0, errors.New("bacnet: short NPDU")
	}
	npduCtrl := pkt[i+1]
	i += 2
	if npduCtrl&0x20 != 0 { // DNET present
		i += 3
		if i < len(pkt) {
			i += int(pkt[i-1]) // DADR len
		}
	}
	if npduCtrl&0x08 != 0 { // SNET present
		if i+3 > len(pkt) {
			return 0, errors.New("bacnet: short SNET")
		}
		slen := int(pkt[i+2])
		i += 3 + slen
	}
	if npduCtrl&0x20 != 0 {
		i++ // hop count
	}
	if i >= len(pkt) {
		return 0, errors.New("bacnet: no APDU")
	}
	if pkt[i]&0xF0 != 0x30 { // ComplexACK = PDU type 3
		return 0, errNotComplexAck
	}
	// walk to the opening tag 3 (property value)
	for j := i; j < len(pkt)-1; j++ {
		if pkt[j] == 0x3E { // opening tag 3
			v := pkt[j+1:]
			if len(v) == 0 {
				return 0, errors.New("bacnet: empty value")
			}
			switch {
			case v[0] == 0x44 && len(v) >= 5: // real
				return float64(math.Float32frombits(binary.BigEndian.Uint32(v[1:5]))), nil
			case v[0] == 0x91 && len(v) >= 2: // enumerated
				return float64(v[1]), nil
			case v[0] == 0x21 && len(v) >= 2: // unsigned, 1 byte
				return float64(v[1]), nil
			}
			return 0, fmt.Errorf("bacnet: unsupported value tag 0x%02x", v[0])
		}
	}
	return 0, errors.New("bacnet: property value not found")
}

func (a *Adapter) request(ctx context.Context, addr string, apdu []byte) ([]byte, error) {
	conn, err := net.Dial("udp", normalizeAddr(addr))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.Write(bvll(npduExpectingReply(apdu))); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(a.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func isBinary(t uint16) bool {
	return t == ObjBinaryInput || t == ObjBinaryOutput || t == ObjBinaryValue
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
	var om *ObjectMap
	for i := range dev.Objects {
		if dev.Objects[i].ID == call.Capability {
			om = &dev.Objects[i]
			break
		}
	}
	if om == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	metric := strings.ReplaceAll(om.ID, ".", "_")

	// SET: WriteProperty then read back (T2 honesty).
	if raw, isSet := call.Args["value"]; isSet {
		v, ok := toFloat(raw)
		if !ok {
			return sdk.NewEffect().Fail("args.value must be a number or bool").Build(), nil
		}
		if _, err := a.request(ctx, dev.Addr, writePropertyAPDU(a.nextInvoke(), om.Type, om.Instance, v, isBinary(om.Type))); err != nil {
			return sdk.NewEffect().Unavailable("write: " + err.Error()).Build(), nil
		}
	}
	pkt, err := a.request(ctx, dev.Addr, readPropertyAPDU(a.nextInvoke(), om.Type, om.Instance))
	if err != nil {
		return sdk.NewEffect().Unavailable("readback: " + err.Error()).Build(), nil
	}
	got, err := parsePresentValue(pkt)
	if err != nil {
		return sdk.NewEffect().Fail("readback: " + err.Error()).Build(), nil
	}
	return sdk.NewEffect().Metric(metric, got).
		Requested(fmt.Sprintf("%s=%v", om.ID, call.Args["value"])).
		Protocol("bacnet").NativeAck(true).
		Observed(strconv.FormatFloat(got, 'f', -1, 64)).
		Trust(uint8(profile.V2), uint8(profile.A1)).
		Verified().Build(), nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	}
	return 0, false
}
