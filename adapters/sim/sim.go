// Package sim is the simulator adapter: one virtual lamp on an always-
// available built-in backend. It exists to prove the C4 contract and to give
// every environment a device to demo against — the adapter every other
// adapter is tested like.
package sim

import (
	"context"
	"fmt"
	"sync"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

const DeviceID = "lamp-1"

type builtinBackend struct{}

func (builtinBackend) Name() string { return "builtin" }
func (builtinBackend) Probe(context.Context) sdk.Availability {
	return sdk.Availability{Available: true, Detail: "simulator is self-contained"}
}

// Adapter is the sim adapter.
type Adapter struct {
	mu sync.Mutex
	on bool
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "sim", Protocol: "sim", Version: "0.1.0"}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{builtinBackend{}} }

func (a *Adapter) Start(_ context.Context, _ sdk.Backend, sink sdk.Sink) error {
	sink.UpsertDevice(sdk.Device{
		ID: DeviceID,
		Profile: &profile.DeviceProfile{
			Schema: 1, Vendor: "anet", Model: "sim-lamp", Protocol: "sim",
			Caps: []profile.Capability{
				{ID: "light.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState},
			},
			MaxTrust: profile.T4, // in-process: physically closed loop by construction
		},
		Online: true,
	})
	return nil
}

func (a *Adapter) Stop(context.Context) error { return nil }

func (a *Adapter) Invoke(_ context.Context, call sdk.Call) (effect.Effect, error) {
	if call.DeviceID != DeviceID {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown device %q", call.DeviceID)).Build(), nil
	}
	if call.Capability != "light.onoff" {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if want, ok := call.Args["on"].(bool); ok {
		a.on = want
	}
	state := 0.0
	if a.on {
		state = 1.0
	}
	return sdk.NewEffect().Metric("power_state", state).Verified().Build(), nil
}
