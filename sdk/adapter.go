// Package sdk is the C4 contract: what every ANetLink protocol adapter
// implements. Adapters are compile-time Go modules (K207 A3); the runtime
// selects one backend per adapter at startup via probing (A5) and everything
// north of the runtime sees devices and capabilities, never adapters.
package sdk

import (
	"context"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
)

// Info identifies an adapter.
type Info struct {
	Name     string // "zigbee", "matter", "sim", ...
	Protocol string // ecosystem protocol family this adapter covers
	Version  string
}

// Device is one concrete device an adapter exposes.
type Device struct {
	ID      string // adapter-scoped stable id
	Profile *profile.DeviceProfile
	Online  bool
}

// Sink is where an adapter reports inventory and state. Implemented by the
// runtime; adapters must not retain it past Stop.
type Sink interface {
	UpsertDevice(d Device)
	RemoveDevice(id string)
	// State reports unsolicited metric updates for a device (STATE access).
	State(deviceID string, metrics map[string]float64)
}

// Call is one device capability invocation.
type Call struct {
	DeviceID   string
	Capability string
	Args       map[string]any
}

// Adapter is C4. One adapter covers one protocol family through one of
// possibly several backends (A5 双后端制: probe host facilities first, always
// carry a self-contained fallback).
type Adapter interface {
	Info() Info
	// Backends returns this adapter's backends in priority order. The
	// runtime probes them and starts the adapter with the first available
	// one (or the configured override).
	Backends() []Backend
	Start(ctx context.Context, backend Backend, sink Sink) error
	Stop(ctx context.Context) error
	Invoke(ctx context.Context, call Call) (effect.Effect, error)
}

// Found is one device an adapter discovered on its own — the raw material
// for zero-configuration onboarding.
type Found struct {
	// SuggestedID is a stable id derived from something the device owns
	// (IEEE address, instance number, MAC), safe to write into a config.
	SuggestedID string
	// Protocol names the transport it was found on.
	Protocol string
	// Label is a human-friendly name if the device advertised one.
	Label string
	// Address is how to reach it (adapter-specific form).
	Address string
	// Capabilities lists the capability ids it appears to offer.
	Capabilities []string
	// ConfigHint is a ready-to-paste JSON fragment for this device.
	ConfigHint string
}

// Discoverer is implemented by adapters that can enumerate their network
// without a hand-written device list. This is the difference between "you
// can integrate any protocol" and "integrating is convenient" — the point of
// ANetLink (K210): a user should be able to ask what is out there.
type Discoverer interface {
	Discover(ctx context.Context) ([]Found, error)
}

// Pairer is implemented by adapters that can admit new devices — opening a
// Zigbee network, commissioning a Matter node, provisioning a mesh node.
// Buying a device and using it should not require editing configuration.
type Pairer interface {
	// Pair admits devices for the given window and returns what joined,
	// described well enough to use immediately.
	Pair(ctx context.Context, window time.Duration) ([]Found, error)
}
