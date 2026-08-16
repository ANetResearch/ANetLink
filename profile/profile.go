// Package profile defines the ANetLink device capability model: what a device
// can do (capabilities × access mask) and how strongly its effects can be
// verified (trust ladder). Profiles are content-addressed CoreDet-CBOR
// objects, mountable on ADP cards as typed CIDs.
package profile

import (
	"fmt"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
)

// Access mask bits: what a capability exposes.
const (
	AccessState uint8 = 1 << 0 // 0b001 STATE — unsolicited state stream
	AccessSet   uint8 = 1 << 1 // 0b010 SET   — actuation
	AccessGet   uint8 = 1 << 2 // 0b100 GET   — on-demand read
)

// Capability is one device capability entry.
type Capability struct {
	ID     string // taxonomy id, e.g. "light.onoff"
	Access uint8  // AccessState|AccessSet|AccessGet
	Unit   string // optional measurement unit
}

// DeviceProfile describes one device model. Field order in the canonical
// encoding is fixed by integer keys; caps array order is author-significant
// (CoreDet C-D3).
type DeviceProfile struct {
	Schema   uint64 // profile schema MAJOR
	Vendor   string
	Model    string
	Protocol string // "zigbee" | "matter" | "modbus" | "sim" | ...
	Caps     []Capability
	MaxTrust Trust // ceiling for this device+transport (link-minimum rule)
}

// Canonical returns the CoreDet-CBOR preimage of the profile
// (_CONVENTIONS §2/§4: int keys, absent≠null, arrays keep author order).
func (p *DeviceProfile) Canonical() ([]byte, error) {
	if p.Schema == 0 {
		return nil, fmt.Errorf("profile: schema major must be ≥1")
	}
	caps := make([]any, 0, len(p.Caps))
	for _, c := range p.Caps {
		m := map[uint64]any{1: c.ID, 2: uint64(c.Access)}
		if c.Unit != "" {
			m[3] = c.Unit
		}
		caps = append(caps, m)
	}
	obj := map[uint64]any{1: p.Schema, 6: uint64(p.MaxTrust)}
	if p.Vendor != "" {
		obj[2] = p.Vendor
	}
	if p.Model != "" {
		obj[3] = p.Model
	}
	if p.Protocol != "" {
		obj[4] = p.Protocol
	}
	if len(caps) > 0 {
		obj[5] = caps
	}
	return coredet.Marshal(obj)
}

// CID returns the content id of the canonical profile.
func (p *DeviceProfile) CID() (string, error) {
	pre, err := p.Canonical()
	if err != nil {
		return "", err
	}
	return anetcid.Sum(pre)
}

// Find returns the capability entry for id, if declared.
func (p *DeviceProfile) Find(id string) (Capability, bool) {
	for _, c := range p.Caps {
		if c.ID == id {
			return c, true
		}
	}
	return Capability{}, false
}
