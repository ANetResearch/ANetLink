package ble

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// SensorMap projects one advertised value into a capability. BLE sensor
// beacons (temperature/humidity tags, iBeacon-style devices) publish their
// state in the advertisement itself, so a capability is a decode rule.
type SensorMap struct {
	ID        string // capability id, e.g. "sensor.temperature"
	ServiceID uint16 // 16-bit service UUID carrying the data (0 = manufacturer data)
	Offset    int    // byte offset within the payload
	Length    int    // 1 or 2 bytes
	Signed    bool
	Scale     float64
	Unit      string
}

// DeviceConfig is one known BLE peripheral.
type DeviceConfig struct {
	ID      string
	Address string // "AA:BB:CC:DD:EE:FF"
	Vendor  string
	Model   string
	Sensors []SensorMap
}

// Config configures the adapter.
type Config struct {
	Devices []DeviceConfig
	// Adapter is the HCI controller index (hci0 = 0).
	Adapter int
	// ScanWindow bounds how long a fresh read waits for an advertisement.
	ScanWindow time.Duration
}

type Adapter struct {
	cfg    Config
	mu     sync.RWMutex
	latest map[string]*Advertisement // address -> last advertisement
	stop   context.CancelFunc
	sink   sdk.Sink
	hci    *HCIDevice
}

func New(cfg Config) *Adapter {
	if cfg.ScanWindow == 0 {
		cfg.ScanWindow = 5 * time.Second
	}
	return &Adapter{cfg: cfg, latest: map[string]*Advertisement{}}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "ble", Protocol: "ble", Version: "0.1.0"}
}

// hciBackend is the self-contained path: the kernel HCI socket, no BlueZ.
type hciBackend struct{ cfg Config }

func (b hciBackend) Name() string { return "hci" }

func (b hciBackend) Probe(ctx context.Context) sdk.Availability {
	dev, err := hciAvailable()
	if err != nil {
		return sdk.Availability{Available: false, Detail: err.Error()}
	}
	return sdk.Availability{Available: true, Detail: fmt.Sprintf("kernel HCI socket on hci%d (no BlueZ needed)", dev)}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{hciBackend{a.cfg}} }

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	h, err := OpenHCI(a.cfg.Adapter)
	if err != nil {
		return err
	}
	a.hci = h
	a.sink = sink
	sctx, cancel := context.WithCancel(context.Background())
	a.stop = cancel
	go func() {
		_ = h.Scan(sctx, a.ingest)
	}()

	for _, d := range a.cfg.Devices {
		caps := make([]profile.Capability, 0, len(d.Sensors))
		for _, s := range d.Sensors {
			caps = append(caps, profile.Capability{
				ID: s.ID, Access: profile.AccessGet | profile.AccessState, Unit: s.Unit,
			})
		}
		sink.UpsertDevice(sdk.Device{
			ID: d.ID,
			Profile: &profile.DeviceProfile{
				Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "ble",
				Caps: caps,
				// Advertisement-only sensors are broadcast: we observe a value
				// but nothing acknowledges anything, and BLE advertisements
				// carry no device identity.
				MaxTrust:  profile.V2, // the value IS the observed state
				AuthTrust: profile.A0,
			},
			Online: false, // becomes true once an advertisement arrives
		})
	}
	return nil
}

// ingest records an advertisement and publishes any decoded sensor values.
func (a *Adapter) ingest(adv *Advertisement) {
	a.mu.Lock()
	a.latest[adv.Address] = adv
	a.mu.Unlock()
	for _, d := range a.cfg.Devices {
		if !strings.EqualFold(d.Address, adv.Address) {
			continue
		}
		metrics := map[string]float64{}
		for _, s := range d.Sensors {
			if v, ok := decodeSensor(adv, s); ok {
				metrics[strings.ReplaceAll(s.ID, ".", "_")] = v
			}
		}
		if a.sink != nil {
			if len(metrics) > 0 {
				a.sink.State(d.ID, metrics)
			}
			caps := make([]profile.Capability, 0, len(d.Sensors))
			for _, s := range d.Sensors {
				caps = append(caps, profile.Capability{ID: s.ID, Access: profile.AccessGet | profile.AccessState, Unit: s.Unit})
			}
			a.sink.UpsertDevice(sdk.Device{
				ID: d.ID,
				Profile: &profile.DeviceProfile{
					Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "ble",
					Caps: caps, MaxTrust: profile.V2, AuthTrust: profile.A0,
				},
				Online: true,
			})
		}
	}
}

// decodeSensor extracts one value from an advertisement per the map.
func decodeSensor(adv *Advertisement, s SensorMap) (float64, bool) {
	var payload []byte
	if s.ServiceID == 0 {
		payload = adv.ManufData
	} else {
		payload = adv.ServiceData[s.ServiceID]
	}
	if len(payload) < s.Offset+s.Length {
		return 0, false
	}
	var raw int64
	switch s.Length {
	case 1:
		raw = int64(payload[s.Offset])
		if s.Signed {
			raw = int64(int8(payload[s.Offset]))
		}
	case 2:
		u := uint16(payload[s.Offset]) | uint16(payload[s.Offset+1])<<8
		raw = int64(u)
		if s.Signed {
			raw = int64(int16(u))
		}
	default:
		return 0, false
	}
	scale := s.Scale
	if scale == 0 {
		scale = 1
	}
	return float64(raw) * scale, true
}

func (a *Adapter) Stop(context.Context) error {
	if a.stop != nil {
		a.stop()
	}
	if a.hci != nil {
		return a.hci.Close()
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
	var sm *SensorMap
	for i := range dev.Sensors {
		if dev.Sensors[i].ID == call.Capability {
			sm = &dev.Sensors[i]
			break
		}
	}
	if sm == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	if _, isSet := call.Args["value"]; isSet {
		return sdk.NewEffect().Fail("advertisement-only sensors are read-only (GATT write is a follow-up)").Build(), nil
	}
	a.mu.RLock()
	adv := a.latest[strings.ToUpper(dev.Address)]
	a.mu.RUnlock()
	if adv == nil {
		return sdk.NewEffect().Unavailable("no advertisement seen from " + dev.Address).Build(), nil
	}
	v, ok := decodeSensor(adv, *sm)
	if !ok {
		return sdk.NewEffect().Unavailable("advertisement carries no such field").Build(), nil
	}
	metric := strings.ReplaceAll(sm.ID, ".", "_")
	return sdk.NewEffect().Metric(metric, v).
		Protocol("ble").NativeAck(false).
		Observed(fmt.Sprintf("%v", v)).
		Trust(uint8(profile.V2), uint8(profile.A0)).
		Verified().Build(), nil
}

// Ingest feeds an advertisement directly (tests and non-HCI sources).
func (a *Adapter) Ingest(adv *Advertisement) { a.ingest(adv) }
