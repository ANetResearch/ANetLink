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

// Endpoint is one addressable facet of a device (R02 §3 / K210 D50): a
// 4-gang power strip has four, a thermostat may expose climate + humidity,
// a Matter node has one endpoint per device type. Single-endpoint devices
// need not use this — Caps on the DeviceProfile remain the flat shorthand.
type Endpoint struct {
	ID   string       // endpoint id, e.g. "1" or "hvac.main"
	Name string       // optional human label
	Caps []Capability // capabilities on this endpoint (order significant)
}

// Find returns the capability entry for id on this endpoint.
func (e *Endpoint) Find(id string) (Capability, bool) {
	for _, c := range e.Caps {
		if c.ID == id {
			return c, true
		}
	}
	return Capability{}, false
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
	MaxTrust VerifyTrust // effect-verification ceiling (link-minimum rule)
	// AuthTrust is the identity-authentication level (K210 D49). Zero value
	// A0 (unknown) is the honest default for protocols without device
	// identity (Modbus, MQTT); adapters that authenticate set it explicitly.
	AuthTrust AuthTrust
	// Endpoints models multi-facet devices (D50). Empty for single-endpoint
	// devices, which use Caps directly.
	Endpoints []Endpoint
	// Firmware is the revision the device reports. It is here rather than
	// in side metadata because quirk matching needs it (D03 §7.3): the same
	// model on two firmwares genuinely behaves differently, and a
	// correction that cannot be keyed on the revision either misses the
	// devices that need it or corrupts the ones that do not.
	//
	// CID-significant, and absent ≡ "" so profiles that never set it keep
	// the content ids they already have.
	Firmware string
}

// AllCaps returns every capability the device exposes, flattening endpoints
// (device-level Caps first, then each endpoint's in order). Callers that do
// not care about endpoint structure use this.
func (p *DeviceProfile) AllCaps() []Capability {
	out := append([]Capability(nil), p.Caps...)
	for _, e := range p.Endpoints {
		out = append(out, e.Caps...)
	}
	return out
}

// Endpoint returns the endpoint with the given id.
func (p *DeviceProfile) Endpoint(id string) (*Endpoint, bool) {
	for i := range p.Endpoints {
		if p.Endpoints[i].ID == id {
			return &p.Endpoints[i], true
		}
	}
	return nil, false
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
	if p.AuthTrust != A0 {
		obj[7] = uint64(p.AuthTrust) // auth axis; absent ≡ A0 (C-D1)
	}
	if p.Firmware != "" {
		obj[9] = p.Firmware
	}
	if len(p.Endpoints) > 0 {
		eps := make([]any, 0, len(p.Endpoints))
		for _, e := range p.Endpoints {
			ecaps := make([]any, 0, len(e.Caps))
			for _, c := range e.Caps {
				m := map[uint64]any{1: c.ID, 2: uint64(c.Access)}
				if c.Unit != "" {
					m[3] = c.Unit
				}
				ecaps = append(ecaps, m)
			}
			em := map[uint64]any{1: e.ID}
			if e.Name != "" {
				em[2] = e.Name
			}
			if len(ecaps) > 0 {
				em[3] = ecaps
			}
			eps = append(eps, em)
		}
		obj[8] = eps
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

// Find returns the capability entry for id, searching device-level caps
// first and then every endpoint.
func (p *DeviceProfile) Find(id string) (Capability, bool) {
	for _, c := range p.Caps {
		if c.ID == id {
			return c, true
		}
	}
	for i := range p.Endpoints {
		if c, ok := p.Endpoints[i].Find(id); ok {
			return c, true
		}
	}
	return Capability{}, false
}
