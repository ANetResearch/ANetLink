package zigbee

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ANetResearch/ANetLink/sdk"
)

// Zigbee device discovery. The coordinator already knows the network — the
// user should not have to hand-write a device list. These are the ZDO
// commands that ask it (Z-Stack MT, ZDO subsystem).
const (
	zdoActiveEPReq    byte = 0x05 // which endpoints does a node have
	zdoSimpleDescReq  byte = 0x04 // which clusters live on an endpoint
	zdoActiveEPRsp    byte = 0x85
	zdoSimpleDescRsp  byte = 0x84
	utilGetDeviceInfo byte = 0x00 // coordinator's own info
	zdoMgmtLqiReq     byte = 0x31 // neighbour table = joined devices
	zdoMgmtLqiRsp     byte = 0xB1
)

// DiscoveredDevice is one node found on the network, with its endpoints and
// clusters filled in — ready to be turned into a DeviceConfig.
type DiscoveredDevice struct {
	ShortAddr uint16
	IEEE      uint64
	Endpoints []EndpointConfig
}

// Discover walks the coordinator's neighbour table, then asks each node for
// its endpoints and their clusters. The result is what the user would
// otherwise have had to write by hand.
func (a *Adapter) Discover(ctx context.Context) ([]DiscoveredDevice, error) {
	a.mu.Lock()
	port := a.port
	a.mu.Unlock()
	if port == nil {
		return nil, fmt.Errorf("zigbee: coordinator not started")
	}

	neighbours, err := a.mgmtLQI(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveredDevice, 0, len(neighbours))
	for _, n := range neighbours {
		dev := DiscoveredDevice{ShortAddr: n.ShortAddr, IEEE: n.IEEE}
		eps, err := a.activeEndpoints(ctx, n.ShortAddr)
		if err != nil {
			continue // a sleeping end device is not a hard failure
		}
		for _, ep := range eps {
			clusters, err := a.simpleDescriptor(ctx, n.ShortAddr, ep)
			if err != nil {
				continue
			}
			dev.Endpoints = append(dev.Endpoints, EndpointConfig{
				ID: fmt.Sprintf("%d", ep), Endpoint: ep, Clusters: clusters,
			})
		}
		if len(dev.Endpoints) > 0 {
			out = append(out, dev)
		}
	}
	return out, nil
}

// neighbour is one entry of the coordinator's LQI table.
type neighbour struct {
	ShortAddr uint16
	IEEE      uint64
}

func (a *Adapter) mgmtLQI(ctx context.Context) ([]neighbour, error) {
	resp, err := a.zdoRequest(ctx, zdoMgmtLqiReq, zdoMgmtLqiRsp, []byte{0x00, 0x00, 0x00})
	if err != nil {
		return nil, err
	}
	return ParseMgmtLQIResponse(resp)
}

// ParseMgmtLQIResponse decodes a ZDO Mgmt_Lqi_rsp neighbour table.
func ParseMgmtLQIResponse(b []byte) ([]neighbour, error) {
	// srcAddr(2) status(1) neighbourTableEntries(1) startIndex(1) count(1)
	// then per entry: extPanID(8) extAddr(8) nwkAddr(2) flags(1) permit(1)
	// depth(1) lqi(1) = 22 bytes
	if len(b) < 6 {
		return nil, fmt.Errorf("zigbee: short LQI response")
	}
	count := int(b[5])
	entries := b[6:]
	const entryLen = 22
	out := make([]neighbour, 0, count)
	for i := 0; i < count && (i+1)*entryLen <= len(entries); i++ {
		e := entries[i*entryLen : (i+1)*entryLen]
		out = append(out, neighbour{
			IEEE:      binary.LittleEndian.Uint64(e[8:16]),
			ShortAddr: binary.LittleEndian.Uint16(e[16:18]),
		})
	}
	return out, nil
}

func (a *Adapter) activeEndpoints(ctx context.Context, addr uint16) ([]byte, error) {
	req := make([]byte, 4)
	binary.LittleEndian.PutUint16(req, addr)
	binary.LittleEndian.PutUint16(req[2:], addr)
	resp, err := a.zdoRequest(ctx, zdoActiveEPReq, zdoActiveEPRsp, req)
	if err != nil {
		return nil, err
	}
	return ParseActiveEPResponse(resp)
}

// ParseActiveEPResponse decodes a ZDO Active_EP_rsp.
func ParseActiveEPResponse(b []byte) ([]byte, error) {
	// srcAddr(2) status(1) nwkAddr(2) activeEPCount(1) activeEPList[]
	if len(b) < 6 {
		return nil, fmt.Errorf("zigbee: short active-EP response")
	}
	if b[2] != 0 {
		return nil, fmt.Errorf("zigbee: active-EP status 0x%02x", b[2])
	}
	n := int(b[5])
	if len(b) < 6+n {
		return nil, fmt.Errorf("zigbee: truncated endpoint list")
	}
	return append([]byte(nil), b[6:6+n]...), nil
}

func (a *Adapter) simpleDescriptor(ctx context.Context, addr uint16, ep byte) ([]uint16, error) {
	req := make([]byte, 5)
	binary.LittleEndian.PutUint16(req, addr)
	binary.LittleEndian.PutUint16(req[2:], addr)
	req[4] = ep
	resp, err := a.zdoRequest(ctx, zdoSimpleDescReq, zdoSimpleDescRsp, req)
	if err != nil {
		return nil, err
	}
	return ParseSimpleDescResponse(resp)
}

// ParseSimpleDescResponse decodes a ZDO Simple_Desc_rsp and returns the
// input clusters (the ones a controller can read/command).
func ParseSimpleDescResponse(b []byte) ([]uint16, error) {
	// srcAddr(2) status(1) nwkAddr(2) len(1) endpoint(1) profileID(2)
	// deviceID(2) version(1) numInClusters(1) inClusters[2n] ...
	if len(b) < 12 {
		return nil, fmt.Errorf("zigbee: short simple-descriptor response")
	}
	if b[2] != 0 {
		return nil, fmt.Errorf("zigbee: simple-descriptor status 0x%02x", b[2])
	}
	numIn := int(b[11])
	if len(b) < 12+numIn*2 {
		return nil, fmt.Errorf("zigbee: truncated cluster list")
	}
	out := make([]uint16, 0, numIn)
	for i := 0; i < numIn; i++ {
		out = append(out, binary.LittleEndian.Uint16(b[12+i*2:]))
	}
	return out, nil
}

// zdoRequest sends a ZDO command and waits for its matching response.
func (a *Adapter) zdoRequest(ctx context.Context, reqCmd, rspCmd byte, data []byte) ([]byte, error) {
	a.mu.Lock()
	port := a.port
	if port == nil {
		a.mu.Unlock()
		return nil, fmt.Errorf("zigbee: coordinator not started (no serial port)")
	}
	if a.zdoWaiters == nil {
		a.zdoWaiters = map[byte]chan []byte{}
	}
	ch := make(chan []byte, 1)
	a.zdoWaiters[rspCmd] = ch
	a.mu.Unlock()

	f := &Frame{Type: TypeSREQ, Subsys: SubsysZDO, Cmd: reqCmd, Data: data}
	pkt, err := f.Encode()
	if err != nil {
		return nil, err
	}
	if _, err := port.Write(pkt); err != nil {
		return nil, err
	}
	select {
	case b := <-ch:
		return b, nil
	case <-time.After(a.cfg.Timeout):
		return nil, fmt.Errorf("zigbee: ZDO 0x%02x timed out", reqCmd)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// AsDeviceConfigs converts discovery results into ready-to-use configs,
// naming devices by their IEEE address so they stay stable across restarts.
func AsDeviceConfigs(found []DiscoveredDevice) []DeviceConfig {
	out := make([]DeviceConfig, 0, len(found))
	for _, d := range found {
		// keep only endpoints that expose at least one mappable cluster
		var eps []EndpointConfig
		for _, ep := range d.Endpoints {
			var keep []uint16
			for _, c := range ep.Clusters {
				if _, ok := CapabilityForCluster(c); ok {
					keep = append(keep, c)
				}
			}
			if len(keep) > 0 {
				eps = append(eps, EndpointConfig{ID: ep.ID, Endpoint: ep.Endpoint, Clusters: keep})
			}
		}
		if len(eps) == 0 {
			continue
		}
		out = append(out, DeviceConfig{
			ID:        fmt.Sprintf("zb-%016x", d.IEEE),
			ShortAddr: d.ShortAddr,
			Endpoints: eps,
		})
	}
	return out
}

var _ sdk.Discoverer = (*Adapter)(nil)

// DiscoverDevices implements sdk.Discoverer: enumerate the Zigbee network and
// describe each node in a form the operator can paste into a config.
func (a *Adapter) DiscoverDevices(ctx context.Context) ([]sdk.Found, error) {
	raw, err := a.Discover(ctx)
	if err != nil {
		return nil, err
	}
	cfgs := AsDeviceConfigs(raw)
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
			Address:      fmt.Sprintf("0x%04x", c.ShortAddr),
			Capabilities: caps,
			ConfigHint:   string(hint),
		})
	}
	return out, nil
}
