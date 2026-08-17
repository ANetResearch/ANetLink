package zigbee

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ANetResearch/ANetLink/sdk"
)

// Pairing: opening the network so a new device can join. Buying a lamp and
// getting it into ANetLink should be one sentence, not a config edit — this
// is the last mile of the convenience mission.

const (
	zdoMgmtPermitJoinReq byte = 0x36
	zdoMgmtPermitJoinRsp byte = 0xB6
	zdoTCDeviceInd       byte = 0xCA // trust-centre device indication (a join)
	zdoEndDeviceAnnceInd byte = 0xC1 // device announced itself
	// broadcastAllRouters is the ZDO broadcast address that reaches the
	// coordinator and every router, so the whole network opens at once.
	broadcastAllRouters uint16 = 0xFFFC
)

// JoinEvent reports a device that joined while the network was open.
type JoinEvent struct {
	ShortAddr uint16
	IEEE      uint64
	At        time.Time
}

// PermitJoin opens the network for the given duration (0 closes it
// immediately). Zigbee caps the window at 254 seconds.
func (a *Adapter) PermitJoin(ctx context.Context, d time.Duration) error {
	secs := int(d.Seconds())
	if secs < 0 {
		secs = 0
	}
	if secs > 254 {
		secs = 254
	}
	req := make([]byte, 5)
	req[0] = 0x0F // address mode: broadcast
	binary.LittleEndian.PutUint16(req[1:], broadcastAllRouters)
	req[3] = byte(secs)
	req[4] = 0x00 // TC significance
	_, err := a.zdoRequest(ctx, zdoMgmtPermitJoinReq, zdoMgmtPermitJoinRsp, req)
	if err != nil {
		return fmt.Errorf("zigbee: permit-join: %w", err)
	}
	return nil
}

// ParseDeviceAnnounce decodes a ZDO End_Device_annce_ind, which a device
// broadcasts the moment it joins.
func ParseDeviceAnnounce(b []byte) (*JoinEvent, error) {
	// srcAddr(2) nwkAddr(2) ieeeAddr(8) capabilities(1)
	if len(b) < 12 {
		return nil, fmt.Errorf("zigbee: short device announce")
	}
	return &JoinEvent{
		ShortAddr: binary.LittleEndian.Uint16(b[2:]),
		IEEE:      binary.LittleEndian.Uint64(b[4:12]),
		At:        time.Now(),
	}, nil
}

// joinWatchers receive join events while a Pair call is in flight.
type joinWatchers struct {
	mu sync.Mutex
	ch []chan JoinEvent
}

func (w *joinWatchers) add() chan JoinEvent {
	c := make(chan JoinEvent, 4)
	w.mu.Lock()
	w.ch = append(w.ch, c)
	w.mu.Unlock()
	return c
}

func (w *joinWatchers) remove(c chan JoinEvent) {
	w.mu.Lock()
	for i, x := range w.ch {
		if x == c {
			w.ch = append(w.ch[:i], w.ch[i+1:]...)
			break
		}
	}
	w.mu.Unlock()
}

func (w *joinWatchers) publish(e JoinEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.ch {
		select {
		case c <- e:
		default:
		}
	}
}

// Pair opens the network, waits for devices to join, interrogates each one
// for its endpoints and clusters, and returns ready-to-use configs. This is
// the "I bought a lamp" path: one call, no manual data entry.
func (a *Adapter) Pair(ctx context.Context, window time.Duration) ([]DeviceConfig, error) {
	watch := a.joins.add()
	defer a.joins.remove(watch)

	if err := a.PermitJoin(ctx, window); err != nil {
		return nil, err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = a.PermitJoin(cctx, 0) // always close the window again
		cancel()
	}()

	deadline := time.After(window)
	var joined []JoinEvent
	for {
		select {
		case e := <-watch:
			joined = append(joined, e)
		case <-deadline:
			return a.describeJoined(ctx, joined), nil
		case <-ctx.Done():
			return a.describeJoined(ctx, joined), ctx.Err()
		}
	}
}

// describeJoined interrogates each freshly joined device.
func (a *Adapter) describeJoined(ctx context.Context, joined []JoinEvent) []DeviceConfig {
	found := make([]DiscoveredDevice, 0, len(joined))
	for _, j := range joined {
		dev := DiscoveredDevice{ShortAddr: j.ShortAddr, IEEE: j.IEEE}
		eps, err := a.activeEndpoints(ctx, j.ShortAddr)
		if err != nil {
			continue
		}
		for _, ep := range eps {
			clusters, err := a.simpleDescriptor(ctx, j.ShortAddr, ep)
			if err != nil {
				continue
			}
			dev.Endpoints = append(dev.Endpoints, EndpointConfig{
				ID: fmt.Sprintf("%d", ep), Endpoint: ep, Clusters: clusters,
			})
		}
		if len(dev.Endpoints) > 0 {
			found = append(found, dev)
		}
	}
	return AsDeviceConfigs(found)
}

// PairDevices implements sdk.Pairer: open the network, describe whatever
// joins, and hand back both the capabilities and a paste-ready config.
func (a *Adapter) PairDevices(ctx context.Context, window time.Duration) ([]sdk.Found, error) {
	cfgs, err := a.Pair(ctx, window)
	if err != nil && len(cfgs) == 0 {
		return nil, err
	}
	out := make([]sdk.Found, 0, len(cfgs))
	for _, c := range cfgs {
		var caps []string
		for _, ep := range c.Endpoints {
			for _, cl := range ep.Clusters {
				if cc, ok := CapabilityForCluster(cl); ok {
					caps = append(caps, cc.Capability)
				}
			}
		}
		hint, _ := json.Marshal(c)
		out = append(out, sdk.Found{
			SuggestedID:  c.ID,
			Protocol:     "zigbee",
			Label:        "newly joined",
			Address:      fmt.Sprintf("0x%04x", c.ShortAddr),
			Capabilities: caps,
			ConfigHint:   string(hint),
		})
	}
	return out, nil
}
