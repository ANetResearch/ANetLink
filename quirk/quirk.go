// Package quirk corrects vendor deviations between an adapter's raw reading
// and the TSIR-D model the runtime publishes.
//
// This layer exists because protocol standardisation is not device
// interoperability. D01's own survey found roughly 264,500 lines across the
// two large Zigbee projects that do nothing but paper over manufacturers
// departing from the specification they claim to implement — Tuya alone
// accounts for 30,087 of them. Quirks are not technical debt in this field;
// they are its fixed cost, and a design with no place for them ends up
// growing that place in the ugliest way available.
//
// Where it sits (D03 §7):
//
//	adapter raw parse
//	      ↓
//	   quirk layer   ← corrected here, never further up
//	      ↓
//	  TSIR-D model
//	      ↓
//	    runtime
//
// Three rules the whole package holds to:
//
//   - A quirk may correct a value. It may never raise trust. A device that
//     lies about its units is not thereby more trustworthy, and letting a
//     declarative file promote a reading to VERIFIED would put the trust
//     model in the hands of whoever ships the quirk.
//   - A corrected reading is labelled. The applied quirk id travels in the
//     effect's Evidence, because a corrected value is not the value the
//     device put on the wire and a consumer must be able to tell.
//   - Matching is most-specific-first and deterministic. Two quirks that
//     both match must resolve the same way on every run, or the same device
//     reports different numbers on different days.
package quirk

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANetLink/profile"
)

// Match selects which devices a quirk applies to.
//
// The key set is D03 §7.3: firmware participates because same-model
// different-firmware behaviour is a real and common situation, not a
// hypothetical one.
type Match struct {
	// Vendor and Model accept a trailing '*' wildcard, because Tuya-family
	// devices identify themselves as _TZE200_xxxxxxxx and the suffix is per
	// production batch.
	Vendor   string `yaml:"vendor,omitempty" json:"vendor,omitempty"`
	Model    string `yaml:"model,omitempty" json:"model,omitempty"`
	Protocol string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	// Firmware is a constraint: ">=1.0.5", "<2.0", "=1.4.7" or a bare
	// version meaning equality.
	Firmware string `yaml:"firmware,omitempty" json:"firmware,omitempty"`
}

// specificity ranks a match so the most precise quirk wins. The ordering is
// D03 §7.3: (vendor, model, firmware) beats (vendor, model) beats
// (vendor, model-prefix) beats anything keyed only on the protocol.
func (m Match) specificity() int {
	s := 0
	if m.Vendor != "" {
		s += 4
		if !strings.HasSuffix(m.Vendor, "*") {
			s++
		}
	}
	if m.Model != "" {
		s += 8
		if !strings.HasSuffix(m.Model, "*") {
			s += 2
		}
	}
	if m.Firmware != "" {
		s += 16
	}
	if m.Protocol != "" {
		s++
	}
	return s
}

func globMatch(pattern, value string) bool {
	if pattern == "" {
		return true // unconstrained
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(strings.TrimSuffix(pattern, "*")))
	}
	return strings.EqualFold(pattern, value)
}

// Matches reports whether this quirk applies to a device profile.
func (m Match) Matches(p *profile.DeviceProfile, firmware string) bool {
	if p == nil {
		return false
	}
	if !globMatch(m.Vendor, p.Vendor) || !globMatch(m.Model, p.Model) {
		return false
	}
	if m.Protocol != "" && !strings.EqualFold(m.Protocol, p.Protocol) {
		return false
	}
	if m.Firmware != "" {
		// An unknown firmware cannot satisfy a firmware constraint. Treating
		// "we do not know" as "it matches" is how a quirk for one revision
		// silently corrupts readings from another.
		if firmware == "" {
			return false
		}
		ok, err := firmwareSatisfies(firmware, m.Firmware)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// FixKind names a correction.
type FixKind string

const (
	// FixScale multiplies a reported value. The canonical case: a sensor
	// that reports centi-degrees, so 2234 on the wire means 22.34 °C and
	// the factor is 0.01.
	FixScale FixKind = "scale"
	// FixOffset adds a constant, for devices zeroed somewhere other than
	// where the unit says.
	FixOffset FixKind = "offset"
	// FixEnumRemap rewrites an enumeration whose ordering the vendor chose
	// differently from the standard.
	FixEnumRemap FixKind = "enum_remap"
	// FixRename moves a capability to the id it should have had.
	FixRename FixKind = "rename"
	// FixUnit corrects a mislabelled unit without touching the value.
	FixUnit FixKind = "unit"
	// FixDropCapability removes a capability the device advertises and does
	// not actually implement — the commonest lie in the field.
	FixDropCapability FixKind = "drop_capability"
	// FixClamp bounds a value to a physically possible range, so a spurious
	// reading is reported as out-of-range rather than passed on as fact.
	FixClamp FixKind = "clamp"
)

// Fix is one correction.
type Fix struct {
	Type FixKind `yaml:"type" json:"type"`
	// Capability the fix applies to. Empty means every capability, which is
	// only sensible for unit and clamp fixes.
	Capability string `yaml:"capability,omitempty" json:"capability,omitempty"`
	// Metric names the effect metric when it differs from the capability id
	// with dots replaced by underscores.
	Metric string `yaml:"metric,omitempty" json:"metric,omitempty"`

	Factor float64            `yaml:"factor,omitempty" json:"factor,omitempty"`
	Offset float64            `yaml:"offset,omitempty" json:"offset,omitempty"`
	Map    map[string]float64 `yaml:"map,omitempty" json:"map,omitempty"`
	To     string             `yaml:"to,omitempty" json:"to,omitempty"`
	Unit   string             `yaml:"unit,omitempty" json:"unit,omitempty"`
	Min    *float64           `yaml:"min,omitempty" json:"min,omitempty"`
	Max    *float64           `yaml:"max,omitempty" json:"max,omitempty"`
}

// Quirk is one declarative correction bundle.
type Quirk struct {
	ID    string `yaml:"id" json:"id"`
	Match Match  `yaml:"match" json:"match"`
	Fixes []Fix  `yaml:"fixes" json:"fixes"`
	// Note records why this quirk exists. Not decoration: a quirk with no
	// stated reason is indistinguishable from a bug someone introduced, and
	// six months later nobody can tell whether it is still needed.
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Validate rejects quirks that cannot do what they claim, at load time
// rather than silently doing nothing at runtime.
func (q Quirk) Validate() error {
	if q.ID == "" {
		return fmt.Errorf("quirk has no id")
	}
	if q.Match.specificity() == 0 {
		return fmt.Errorf("quirk %s matches every device; refuse", q.ID)
	}
	if len(q.Fixes) == 0 {
		return fmt.Errorf("quirk %s has no fixes", q.ID)
	}
	for i, f := range q.Fixes {
		switch f.Type {
		case FixScale:
			if f.Factor == 0 {
				return fmt.Errorf("quirk %s fix %d: scale needs a non-zero factor", q.ID, i)
			}
		case FixOffset:
			if f.Offset == 0 {
				return fmt.Errorf("quirk %s fix %d: offset of zero does nothing", q.ID, i)
			}
		case FixEnumRemap:
			if len(f.Map) == 0 {
				return fmt.Errorf("quirk %s fix %d: enum_remap needs a map", q.ID, i)
			}
		case FixRename:
			if f.Capability == "" || f.To == "" {
				return fmt.Errorf("quirk %s fix %d: rename needs capability and to", q.ID, i)
			}
		case FixUnit:
			if f.Unit == "" {
				return fmt.Errorf("quirk %s fix %d: unit fix needs a unit", q.ID, i)
			}
		case FixDropCapability:
			if f.Capability == "" {
				return fmt.Errorf("quirk %s fix %d: drop_capability needs a capability", q.ID, i)
			}
		case FixClamp:
			if f.Min == nil && f.Max == nil {
				return fmt.Errorf("quirk %s fix %d: clamp needs min or max", q.ID, i)
			}
			if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
				return fmt.Errorf("quirk %s fix %d: clamp min above max", q.ID, i)
			}
		default:
			return fmt.Errorf("quirk %s fix %d: unknown type %q", q.ID, i, f.Type)
		}
	}
	return nil
}

// metricFor is the effect metric name a fix targets.
func (f Fix) metricFor() string {
	if f.Metric != "" {
		return f.Metric
	}
	return strings.ReplaceAll(f.Capability, ".", "_")
}

// firmwareSatisfies evaluates a constraint like ">=1.0.5" against a version.
//
// Versions in this field are not semver and never will be: cameras report
// "V5.6.5 build 200316", Zigbee devices report "0.0.0_0038". Comparison is
// therefore on the numeric components in order, which is what those strings
// actually encode.
func firmwareSatisfies(version, constraint string) (bool, error) {
	op := "="
	rest := strings.TrimSpace(constraint)
	for _, o := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(rest, o) {
			op = o
			rest = strings.TrimSpace(strings.TrimPrefix(rest, o))
			break
		}
	}
	cmp := compareVersions(version, rest)
	switch op {
	case "=":
		return cmp == 0, nil
	case ">":
		return cmp > 0, nil
	case ">=":
		return cmp >= 0, nil
	case "<":
		return cmp < 0, nil
	case "<=":
		return cmp <= 0, nil
	}
	return false, fmt.Errorf("quirk: unknown firmware operator %q", op)
}

// numeric pulls the ordered numeric components out of a version string.
func numeric(s string) []int {
	var out []int
	cur := ""
	flush := func() {
		if cur == "" {
			return
		}
		if n, err := strconv.Atoi(cur); err == nil {
			out = append(out, n)
		}
		cur = ""
	}
	for _, r := range s {
		if r >= '0' && r <= '9' {
			cur += string(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func compareVersions(a, b string) int {
	x, y := numeric(a), numeric(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var xi, yi int
		if i < len(x) {
			xi = x[i]
		}
		if i < len(y) {
			yi = y[i]
		}
		if xi != yi {
			if xi < yi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// sortBySpecificity orders quirks most-specific-first, breaking ties by id
// so the result is stable across runs.
func sortBySpecificity(qs []Quirk) {
	sort.SliceStable(qs, func(i, j int) bool {
		si, sj := qs[i].Match.specificity(), qs[j].Match.specificity()
		if si != sj {
			return si > sj
		}
		return qs[i].ID < qs[j].ID
	})
}
