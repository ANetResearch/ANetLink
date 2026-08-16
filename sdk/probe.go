package sdk

import (
	"context"
	"fmt"
	"strings"
)

// Availability is a backend probe verdict.
type Availability struct {
	Available bool
	Detail    string // why (not) — surfaced to operators verbatim
}

// Backend is one way an adapter can reach its protocol: a host facility
// (BlueZ, an existing OTBR, a running zigbee2mqtt) or the self-contained
// fallback ("永远都要兜底" — every adapter's last backend must be one that
// depends on nothing outside the distribution).
type Backend interface {
	Name() string
	Probe(ctx context.Context) Availability
}

// Select picks the backend to start: the override by name when set (its
// probe must still pass — a forced-but-dead backend is a configuration
// error), otherwise the first available in priority order.
func Select(ctx context.Context, override string, backends ...Backend) (Backend, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("sdk: adapter declares no backends")
	}
	if override != "" {
		for _, b := range backends {
			if b.Name() != override {
				continue
			}
			if a := b.Probe(ctx); !a.Available {
				return nil, fmt.Errorf("sdk: forced backend %q unavailable: %s", override, a.Detail)
			}
			return b, nil
		}
		return nil, fmt.Errorf("sdk: forced backend %q not declared", override)
	}
	var report []string
	for _, b := range backends {
		a := b.Probe(ctx)
		if a.Available {
			return b, nil
		}
		report = append(report, fmt.Sprintf("%s: %s", b.Name(), a.Detail))
	}
	return nil, fmt.Errorf("sdk: no backend available — %s", strings.Join(report, "; "))
}
