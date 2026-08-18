// Package runtime is the ANetLink core: it owns the device table, selects
// adapter backends (A5), routes invocations, and enforces the trust rules.
// North of here there is exactly one capability surface; adapters and
// backends are invisible (K207 A3′ ③).
package runtime

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/quirk"
	"github.com/ANetResearch/ANetLink/sdk"
)

// Config configures a Runtime.
type Config struct {
	// BackendOverride forces a named backend per adapter name (operator
	// escape hatch over A5 probing).
	BackendOverride map[string]string
	// Quirks corrects vendor deviations between an adapter's raw reading
	// and the model published north of here (D03 §7). Nil means no
	// corrections, which is the right default: a quirk that nobody loaded
	// must never be assumed.
	Quirks *quirk.Registry
}

// DeviceKey is the runtime-global device address: adapter name + device id.
type DeviceKey struct{ Adapter, Device string }

func (k DeviceKey) String() string { return k.Adapter + "/" + k.Device }

type deviceEntry struct {
	dev     sdk.Device
	adapter sdk.Adapter
	// raw is the profile the adapter reported, before correction. Kept so
	// an operator can see what the device actually claimed as against what
	// the runtime publishes — a correction you cannot compare against the
	// original is a correction you cannot review.
	raw *profile.DeviceProfile
	// quirks are the corrections matching this device, resolved once at
	// registration rather than per invocation.
	quirks quirk.Applied
}

// Runtime hosts adapters and exposes their devices as capabilities.
type Runtime struct {
	cfg       Config
	mu        sync.RWMutex
	adapters  map[string]sdk.Adapter
	backends  map[string]string // adapter name → selected backend name
	devices   map[DeviceKey]*deviceEntry
	lastState map[DeviceKey]map[string]float64
	started   bool
}

func New(cfg Config) *Runtime {
	return &Runtime{
		cfg:      cfg,
		adapters: map[string]sdk.Adapter{},
		backends: map[string]string{},
		devices:  map[DeviceKey]*deviceEntry{},
	}
}

// Register adds an adapter before Start.
func (r *Runtime) Register(a sdk.Adapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("runtime: register after start")
	}
	name := a.Info().Name
	if _, dup := r.adapters[name]; dup {
		return fmt.Errorf("runtime: duplicate adapter %q", name)
	}
	r.adapters[name] = a
	return nil
}

// Start probes backends and starts every adapter. An adapter with no usable
// backend fails startup loudly — silent capability loss is worse than a
// crash at boot.
func (r *Runtime) Start(ctx context.Context) error {
	r.mu.Lock()
	r.started = true
	adapters := make([]sdk.Adapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		adapters = append(adapters, a)
	}
	r.mu.Unlock()
	for _, a := range adapters {
		name := a.Info().Name
		b, err := sdk.Select(ctx, r.cfg.BackendOverride[name], a.Backends()...)
		if err != nil {
			return fmt.Errorf("runtime: adapter %q: %w", name, err)
		}
		if err := a.Start(ctx, b, &sink{rt: r, adapter: a}); err != nil {
			return fmt.Errorf("runtime: adapter %q start (backend %q): %w", name, b.Name(), err)
		}
		r.mu.Lock()
		r.backends[name] = b.Name()
		r.mu.Unlock()
	}
	return nil
}

// Stop stops all adapters.
func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var first error
	for _, a := range r.adapters {
		if err := a.Stop(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Backend reports which backend an adapter selected ("" = not started).
func (r *Runtime) Backend(adapter string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.backends[adapter]
}

// Devices lists the current inventory, sorted by key.
func (r *Runtime) Devices() []sdk.Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]DeviceKey, 0, len(r.devices))
	for k := range r.devices {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make([]sdk.Device, 0, len(keys))
	for _, k := range keys {
		out = append(out, r.devices[k].dev)
	}
	return out
}

// Invoke routes one capability call to the owning adapter and enforces the
// trust rules on the way back: an effect may only claim OK (verifiable) if
// the device's trust level admits verification (profile.VerifiableFrom) —
// adapters and quirks cannot raise trust, only the profile ceiling counts.
func (r *Runtime) Invoke(ctx context.Context, key DeviceKey, capability string, args map[string]any) (effect.Effect, error) {
	r.mu.RLock()
	e, ok := r.devices[key]
	r.mu.RUnlock()
	if !ok {
		return effect.Effect{}, fmt.Errorf("runtime: unknown device %s", key)
	}
	if !e.dev.Online {
		return effect.Effect{Status: effect.Unavailable, Message: "device offline"}, nil
	}
	cap_, declared := e.dev.Profile.Find(capability)
	if !declared {
		return effect.Effect{}, fmt.Errorf("runtime: device %s does not declare %q", key, capability)
	}
	if cap_.Access&(profile.AccessSet|profile.AccessGet) == 0 {
		return effect.Effect{}, fmt.Errorf("runtime: capability %q is not invokable (STATE-only)", capability)
	}
	eff, err := e.adapter.Invoke(ctx, sdk.Call{DeviceID: key.Device, Capability: capability, Args: args})
	if err != nil {
		return effect.Effect{}, err
	}
	// Corrections apply to the reading, never to the trust: a device that
	// lies about its units is not thereby more trustworthy.
	eff = e.quirks.ApplyEffect(eff)
	if eff.Status == effect.OK && e.dev.Profile.MaxTrust < profile.VerifiableFrom {
		eff.Status = effect.Unverified
		if eff.Message == "" {
			eff.Message = "downgraded: device trust level does not admit verification"
		}
	}
	return eff, nil
}

// sink receives adapter reports.
type sink struct {
	rt      *Runtime
	adapter sdk.Adapter
}

func (s *sink) UpsertDevice(d sdk.Device) {
	raw := d.Profile
	applied := quirk.Applied{}
	if s.rt.cfg.Quirks != nil {
		// Firmware is device metadata, not a capability, so TSIR-D has no
		// field for it; adapters that know it put it in the model string.
		applied = s.rt.cfg.Quirks.For(d.Profile, firmwareOf(d.Profile))
		d.Profile = applied.ApplyProfile(d.Profile)
	}
	s.rt.mu.Lock()
	defer s.rt.mu.Unlock()
	s.rt.devices[DeviceKey{s.adapter.Info().Name, d.ID}] = &deviceEntry{
		dev: d, adapter: s.adapter, raw: raw, quirks: applied,
	}
}

// firmwareOf extracts a firmware string when the adapter supplied one.
// Cameras report "V5.6.5 build 200316" in their model line; Zigbee devices
// carry it separately. Absent is absent — a quirk keyed on firmware must
// not match a device whose firmware nobody knows.
func firmwareOf(p *profile.DeviceProfile) string {
	if p == nil {
		return ""
	}
	return p.Firmware
}

func (s *sink) RemoveDevice(id string) {
	s.rt.mu.Lock()
	defer s.rt.mu.Unlock()
	delete(s.rt.devices, DeviceKey{s.adapter.Info().Name, id})
}

// State corrects an unsolicited report before it reaches anything north of
// the runtime. A state stream that carries raw vendor units while an invoke
// on the same capability carries corrected ones would be worse than no
// correction at all.
func (s *sink) State(deviceID string, metrics map[string]float64) {
	key := DeviceKey{s.adapter.Info().Name, deviceID}
	s.rt.mu.RLock()
	e, ok := s.rt.devices[key]
	s.rt.mu.RUnlock()
	if ok && !e.quirks.Empty() {
		metrics, _ = e.quirks.ApplyMetrics(metrics)
	}
	s.rt.observe(key, metrics)
}

// observe is where the STATE surface and the AEL will attach; it exists now
// so the corrected value is what they will see when they do.
func (r *Runtime) observe(key DeviceKey, metrics map[string]float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastState == nil {
		r.lastState = map[DeviceKey]map[string]float64{}
	}
	r.lastState[key] = metrics
}

// LastState returns the most recent corrected state report for a device.
func (r *Runtime) LastState(key DeviceKey) (map[string]float64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.lastState[key]
	if !ok {
		return nil, false
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, true
}

// QuirksFor reports which corrections a device is subject to, and what it
// claimed before them — so an operator can review a correction rather than
// take it on faith.
func (r *Runtime) QuirksFor(key DeviceKey) (applied []string, rawCaps []string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, found := r.devices[key]
	if !found {
		return nil, nil, false
	}
	for _, q := range e.quirks.Quirks {
		applied = append(applied, q.ID)
	}
	if e.raw != nil {
		for _, c := range e.raw.AllCaps() {
			rawCaps = append(rawCaps, c.ID)
		}
	}
	return applied, rawCaps, true
}

// DiscoverAll asks every registered adapter that supports discovery to
// enumerate its network, and returns everything found. Adapters that cannot
// discover are skipped silently; an adapter that errors contributes its
// error without failing the sweep — a Zigbee stick being unplugged should
// not hide the BACnet devices.
func (r *Runtime) DiscoverAll(ctx context.Context) ([]sdk.Found, map[string]error) {
	r.mu.RLock()
	adapters := make(map[string]sdk.Adapter, len(r.adapters))
	for name, a := range r.adapters {
		adapters[name] = a
	}
	r.mu.RUnlock()

	var (
		mu    sync.Mutex
		found []sdk.Found
		errs  = map[string]error{}
		wg    sync.WaitGroup
	)
	for name, a := range adapters {
		// Named interface, not a structural literal: an adapter whose
		// method drifts from the C4 contract must fail to compile, not be
		// silently skipped here and appear as an empty network.
		d, ok := a.(sdk.Discoverer)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(name string, d sdk.Discoverer) {
			defer wg.Done()
			got, err := d.DiscoverDevices(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[name] = err
				return
			}
			found = append(found, got...)
		}(name, d)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool {
		if found[i].Protocol != found[j].Protocol {
			return found[i].Protocol < found[j].Protocol
		}
		return found[i].SuggestedID < found[j].SuggestedID
	})
	return found, errs
}

// PairAll opens every pairing-capable adapter for the given window and
// returns everything that joined. Adapters that cannot pair are skipped.
func (r *Runtime) PairAll(ctx context.Context, window time.Duration) ([]sdk.Found, map[string]error) {
	r.mu.RLock()
	adapters := make(map[string]sdk.Adapter, len(r.adapters))
	for name, a := range r.adapters {
		adapters[name] = a
	}
	r.mu.RUnlock()

	var (
		mu     sync.Mutex
		joined []sdk.Found
		errs   = map[string]error{}
		wg     sync.WaitGroup
	)
	for name, a := range adapters {
		p, ok := a.(sdk.Pairer)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(name string, p sdk.Pairer) {
			defer wg.Done()
			got, err := p.PairDevices(ctx, window)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && len(got) == 0 {
				errs[name] = err
				return
			}
			joined = append(joined, got...)
		}(name, p)
	}
	wg.Wait()
	return joined, errs
}
