package bacnet

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/ANetResearch/ANetLink/sdk"
)

// BACnet discovery: devices answer Who-Is with I-Am, so a building network
// enumerates itself. Users should not have to type instance numbers.

// bvllBroadcast wraps an NPDU in an Original-Broadcast-NPDU envelope.
func bvllBroadcast(npdu []byte) []byte {
	out := make([]byte, 4, 4+len(npdu))
	out[0] = 0x81
	out[1] = 0x0B // Original-Broadcast-NPDU
	binary.BigEndian.PutUint16(out[2:], uint16(4+len(npdu)))
	return append(out, npdu...)
}

// whoIsAPDU builds an unconfirmed Who-Is request (no instance range = all).
func whoIsAPDU() []byte {
	return []byte{0x10, 0x08} // unconfirmed-req, service 8 = Who-Is
}

// DiscoveredDevice is one device that answered Who-Is.
type DiscoveredDevice struct {
	Instance uint32
	Addr     string
	MaxAPDU  uint16
	VendorID uint16
}

// ParseIAm decodes an I-Am response, returning the device instance.
func ParseIAm(pkt []byte) (*DiscoveredDevice, error) {
	if len(pkt) < 10 || pkt[0] != 0x81 {
		return nil, fmt.Errorf("bacnet: not a BACnet/IP frame")
	}
	// BVLL(4) + NPDU(2) then APDU
	apdu := pkt[6:]
	if len(apdu) < 2 || apdu[0] != 0x10 || apdu[1] != 0x00 { // unconfirmed I-Am
		return nil, fmt.Errorf("bacnet: not an I-Am")
	}
	i := 2
	// object identifier: tag 0xC4 + 4 bytes
	if len(apdu) < i+5 || apdu[i] != 0xC4 {
		return nil, fmt.Errorf("bacnet: malformed I-Am object id")
	}
	oid := binary.BigEndian.Uint32(apdu[i+1 : i+5])
	i += 5
	d := &DiscoveredDevice{Instance: oid & 0x3FFFFF}
	// max APDU (unsigned, tag 0x21/0x22)
	if len(apdu) > i {
		switch apdu[i] {
		case 0x21:
			if len(apdu) > i+1 {
				d.MaxAPDU = uint16(apdu[i+1])
				i += 2
			}
		case 0x22:
			if len(apdu) > i+2 {
				d.MaxAPDU = binary.BigEndian.Uint16(apdu[i+1 : i+3])
				i += 3
			}
		}
	}
	return d, nil
}

// Discover broadcasts Who-Is and collects I-Am answers until the deadline.
func Discover(ctx context.Context, broadcastAddr string, window time.Duration) ([]DiscoveredDevice, error) {
	if broadcastAddr == "" {
		broadcastAddr = fmt.Sprintf("255.255.255.255:%d", DefaultPort)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	dst, err := net.ResolveUDPAddr("udp4", broadcastAddr)
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(bvllBroadcast(append([]byte{0x01, 0x20, 0xFF, 0xFF, 0x00, 0xFF}, whoIsAPDU()...)), dst); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(window)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	seen := map[uint32]DiscoveredDevice{}
	buf := make([]byte, 1500)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			break
		}
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if d, perr := ParseIAm(buf[:n]); perr == nil {
			d.Addr = peer.String()
			seen[d.Instance] = *d
		}
	}
	out := make([]DiscoveredDevice, 0, len(seen))
	for _, d := range seen {
		out = append(out, d)
	}
	return out, nil
}

// AsDeviceConfigs turns discovered devices into configs with the common
// object set probed lazily (analog input 1..N is the usual convention; the
// operator refines from there).
func AsDeviceConfigs(found []DiscoveredDevice) []DeviceConfig {
	out := make([]DeviceConfig, 0, len(found))
	for _, d := range found {
		out = append(out, DeviceConfig{
			ID:   fmt.Sprintf("bacnet-%d", d.Instance),
			Addr: d.Addr,
			// A discovered device exposes its Present_Value objects once the
			// operator names them; discovery gives identity and reachability.
		})
	}
	return out
}

// DiscoverDevices implements sdk.Discoverer for BACnet/IP.
func (a *Adapter) DiscoverDevices(ctx context.Context) ([]sdk.Found, error) {
	found, err := Discover(ctx, "", 2*time.Second)
	if err != nil {
		return nil, err
	}
	out := make([]sdk.Found, 0, len(found))
	for _, d := range found {
		cfg := DeviceConfig{ID: fmt.Sprintf("bacnet-%d", d.Instance), Addr: d.Addr}
		hint, _ := json.Marshal(cfg)
		out = append(out, sdk.Found{
			SuggestedID: cfg.ID,
			Protocol:    "bacnet",
			Label:       fmt.Sprintf("device instance %d", d.Instance),
			Address:     d.Addr,
			ConfigHint:  string(hint),
		})
	}
	return out, nil
}
