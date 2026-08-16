// Package sdk is the C4 contract: what every ANetLink protocol adapter
// implements. Adapters are compile-time Go modules (K207 A3); the runtime
// selects one backend per adapter at startup via probing (A5) and everything
// north of the runtime sees devices and capabilities, never adapters.
package sdk

import (
	"context"

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
