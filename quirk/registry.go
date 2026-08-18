package quirk

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"
	"github.com/ANetResearch/ANetLink/profile"
)

// Registry holds the loaded quirks.
//
// Declarative and hot-loadable by design (D03 §7.5): a correction for a
// newly-shipped batch of thermostats should be a file an operator drops in,
// not a release of the daemon. That is also why loading validates: a quirk
// that cannot do what it claims must fail at load, where someone is
// watching, rather than silently doing nothing for months.
type Registry struct {
	mu     sync.RWMutex
	quirks []Quirk
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{} }

type quirkFile struct {
	Quirks []Quirk `yaml:"quirks" json:"quirks"`
}

// LoadYAML adds quirks from a YAML document.
func (r *Registry) LoadYAML(data []byte) (int, error) {
	var f quirkFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return 0, fmt.Errorf("quirk: parse: %w", err)
	}
	if len(f.Quirks) == 0 {
		return 0, fmt.Errorf("quirk: document declares no quirks")
	}
	seen := map[string]bool{}
	r.mu.Lock()
	for _, q := range r.quirks {
		seen[q.ID] = true
	}
	r.mu.Unlock()

	for _, q := range f.Quirks {
		if err := q.Validate(); err != nil {
			return 0, fmt.Errorf("quirk: %w", err)
		}
		if seen[q.ID] {
			// Two quirks with one id make the outcome depend on load order,
			// which is the one thing a correction layer must never do.
			return 0, fmt.Errorf("quirk: duplicate id %q", q.ID)
		}
		seen[q.ID] = true
	}

	r.mu.Lock()
	r.quirks = append(r.quirks, f.Quirks...)
	sortBySpecificity(r.quirks)
	r.mu.Unlock()
	return len(f.Quirks), nil
}

// LoadDir loads every .yaml/.yml file in a directory, in name order.
func (r *Registry) LoadDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	total := 0
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return total, err
		}
		got, err := r.LoadYAML(data)
		if err != nil {
			return total, fmt.Errorf("%s: %w", n, err)
		}
		total += got
	}
	return total, nil
}

// Len reports how many quirks are loaded.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.quirks)
}

// All returns the loaded quirks, most specific first.
func (r *Registry) All() []Quirk {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Quirk(nil), r.quirks...)
}

// Applied is the set of quirks that matched one device, most specific
// first, together with the fixes they contribute.
type Applied struct {
	Quirks []Quirk
}

// IDs names the applied quirks, for evidence and logs.
func (a Applied) IDs() string {
	ids := make([]string, 0, len(a.Quirks))
	for _, q := range a.Quirks {
		ids = append(ids, q.ID)
	}
	return strings.Join(ids, ",")
}

// Empty reports whether nothing matched.
func (a Applied) Empty() bool { return len(a.Quirks) == 0 }

// For returns the quirks matching a device.
//
// firmware is passed separately because TSIR-D has no firmware field: it is
// device metadata, not a capability, and adapters that know it supply it.
func (r *Registry) For(p *profile.DeviceProfile, firmware string) Applied {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Quirk
	for _, q := range r.quirks {
		if q.Match.Matches(p, firmware) {
			out = append(out, q)
		}
	}
	return Applied{Quirks: out}
}

// fixes flattens the matched quirks into fixes, most specific first. Where
// two quirks fix the same capability the more specific one is applied and
// the other skipped, so corrections never compound into a doubled scale.
func (a Applied) fixes() []Fix {
	var out []Fix
	claimed := map[string]bool{} // capability+type already handled
	for _, q := range a.Quirks {
		for _, f := range q.Fixes {
			key := string(f.Type) + "|" + f.Capability + "|" + f.Metric
			if claimed[key] {
				continue
			}
			claimed[key] = true
			out = append(out, f)
		}
	}
	return out
}

// ApplyProfile returns the corrected profile.
//
// The returned profile is a copy: an adapter's own view of its device must
// not change under it, and two runtimes with different quirk sets must be
// able to hold different views of the same hardware.
func (a Applied) ApplyProfile(p *profile.DeviceProfile) *profile.DeviceProfile {
	if a.Empty() || p == nil {
		return p
	}
	out := *p
	out.Caps = append([]profile.Capability(nil), p.Caps...)
	out.Endpoints = append([]profile.Endpoint(nil), p.Endpoints...)
	for i := range out.Endpoints {
		out.Endpoints[i].Caps = append([]profile.Capability(nil), p.Endpoints[i].Caps...)
	}

	for _, f := range a.fixes() {
		switch f.Type {
		case FixDropCapability:
			out.Caps = dropCap(out.Caps, f.Capability)
			for i := range out.Endpoints {
				out.Endpoints[i].Caps = dropCap(out.Endpoints[i].Caps, f.Capability)
			}
		case FixRename:
			renameCap(out.Caps, f.Capability, f.To)
			for i := range out.Endpoints {
				renameCap(out.Endpoints[i].Caps, f.Capability, f.To)
			}
		case FixUnit:
			setUnit(out.Caps, f.Capability, f.Unit)
			for i := range out.Endpoints {
				setUnit(out.Endpoints[i].Caps, f.Capability, f.Unit)
			}
		}
	}
	// Trust is never touched. A device that lies about its units is not
	// thereby more trustworthy, and letting a declarative file promote a
	// reading would put the trust model in whoever ships the quirk.
	out.MaxTrust = p.MaxTrust
	out.AuthTrust = p.AuthTrust
	return &out
}

func dropCap(caps []profile.Capability, id string) []profile.Capability {
	out := caps[:0]
	for _, c := range caps {
		if c.ID != id {
			out = append(out, c)
		}
	}
	return out
}

func renameCap(caps []profile.Capability, from, to string) {
	for i := range caps {
		if caps[i].ID == from {
			caps[i].ID = to
		}
	}
}

func setUnit(caps []profile.Capability, id, unit string) {
	for i := range caps {
		if id == "" || caps[i].ID == id {
			caps[i].Unit = unit
		}
	}
}

// ApplyMetrics corrects a metric map in place-safe fashion, returning the
// corrected copy and whether anything changed.
func (a Applied) ApplyMetrics(metrics map[string]float64) (map[string]float64, bool) {
	if a.Empty() || len(metrics) == 0 {
		return metrics, false
	}
	out := make(map[string]float64, len(metrics))
	for k, v := range metrics {
		out[k] = v
	}
	changed := false

	for _, f := range a.fixes() {
		target := f.metricFor()
		for name := range out {
			if target != "" && name != target {
				continue
			}
			v := out[name]
			switch f.Type {
			case FixScale:
				out[name] = v * f.Factor
				changed = true
			case FixOffset:
				out[name] = v + f.Offset
				changed = true
			case FixEnumRemap:
				if to, ok := f.Map[strconv.FormatFloat(v, 'g', -1, 64)]; ok {
					out[name] = to
					changed = true
				}
			case FixClamp:
				if f.Min != nil && v < *f.Min {
					out[name] = *f.Min
					changed = true
				}
				if f.Max != nil && v > *f.Max {
					out[name] = *f.Max
					changed = true
				}
			case FixRename:
				// A renamed capability renames its metric with it.
				from := strings.ReplaceAll(f.Capability, ".", "_")
				to := strings.ReplaceAll(f.To, ".", "_")
				if name == from {
					out[to] = v
					delete(out, from)
					changed = true
				}
			}
		}
	}
	return out, changed
}

// ApplyEffect corrects an effect's metrics and records the correction in its
// evidence.
//
// The label is not decoration. A corrected reading is not the reading the
// device produced, and a consumer that cannot tell the two apart cannot
// audit the correction — which is the whole reason Evidence exists.
func (a Applied) ApplyEffect(e effect.Effect) effect.Effect {
	if a.Empty() || e.Record == nil {
		return e
	}
	corrected, changed := a.ApplyMetrics(e.Record.Metrics)
	if !changed {
		return e
	}
	rec := *e.Record
	rec.Metrics = corrected
	e.Record = &rec

	ev := effect.Evidence{}
	if e.Evidence != nil {
		ev = *e.Evidence
	}
	ev.Quirk = a.IDs()
	e.Evidence = &ev
	return e
}

// compile-time proof the record type we copy is the one ANetCore defines.
var _ = tsir.EffectRecord{}

// defaults holds the corrections shipped with the distribution: deviations
// we have seen on real hardware and can state a reason for.
//
// Embedded rather than installed so a default build corrects the devices it
// already claims to support. Operators add their own with LoadDir, and a
// site file overrides a shipped one by being more specific — never by
// silently replacing it.
//
//go:embed defaults/*.yaml
var defaultsFS embed.FS

// LoadDefaults adds the shipped corrections.
func (r *Registry) LoadDefaults() (int, error) {
	entries, err := defaultsFS.ReadDir("defaults")
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	total := 0
	for _, n := range names {
		data, err := defaultsFS.ReadFile("defaults/" + n)
		if err != nil {
			return total, err
		}
		got, err := r.LoadYAML(data)
		if err != nil {
			return total, fmt.Errorf("defaults/%s: %w", n, err)
		}
		total += got
	}
	return total, nil
}
