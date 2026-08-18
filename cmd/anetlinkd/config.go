package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ANetResearch/ANetLink/quirk"
	"github.com/ANetResearch/ANetLink/runtime"
)

// fileConfig is anetlinkd's on-disk configuration (JSON).
//
//	{
//	  "backend_override": {"mqtt": "zigbee2mqtt"},
//	  "adapters": {
//	    "sim":           {"enabled": true},
//	    "modbus":        { ...modbus.Config... },
//	    "mqtt":          { ...mqttbridge.Config... },
//	    "homeassistant": { ...habridge.Config... }
//	  }
//	}
//
// An adapter key only takes effect when its plugin is compiled in
// (`no_<name>` build tags subtract plugins — K207 presets).
type fileConfig struct {
	BackendOverride map[string]string          `json:"backend_override,omitempty"`
	Adapters        map[string]json.RawMessage `json:"adapters,omitempty"`
	// QuirkDir adds site-local vendor corrections on top of the shipped
	// ones (D03 §7). Declarative and hot-loadable by design: a correction
	// for a newly-shipped batch of thermostats should be a file an operator
	// drops in, not a release of this daemon.
	QuirkDir string `json:"quirk_dir,omitempty"`
	// NoDefaultQuirks disables the corrections shipped with the build, for
	// an operator who would rather curate the whole set themselves.
	NoDefaultQuirks bool `json:"no_default_quirks,omitempty"`
}

func loadConfig(path string) (*fileConfig, error) {
	if path == "" {
		return &fileConfig{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c fileConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

func (c *fileConfig) runtimeConfig() (runtime.Config, error) {
	reg := quirk.NewRegistry()
	if !c.NoDefaultQuirks {
		if _, err := reg.LoadDefaults(); err != nil {
			return runtime.Config{}, fmt.Errorf("shipped quirks: %w", err)
		}
	}
	if c.QuirkDir != "" {
		// A quirk directory that does not load is an operator error worth
		// stopping for: starting without the corrections they configured
		// would silently publish wrong numbers.
		if _, err := reg.LoadDir(c.QuirkDir); err != nil {
			return runtime.Config{}, fmt.Errorf("quirk_dir %s: %w", c.QuirkDir, err)
		}
	}
	return runtime.Config{BackendOverride: c.BackendOverride, Quirks: reg}, nil
}

// buildAdapters instantiates every compiled-in plugin that is configured
// (or default-on). Unknown config keys fail loudly: a configured adapter
// whose plugin was compiled out is an operator error, not a silent no-op.
func (c *fileConfig) buildAdapters() ([]namedAdapter, error) {
	known := map[string]bool{}
	var out []namedAdapter
	for _, p := range plugins {
		known[p.name] = true
		raw, configured := c.Adapters[p.name]
		if !configured && !p.defaultOn {
			continue
		}
		a, err := p.fromJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("adapter %q: %w", p.name, err)
		}
		if a != nil {
			out = append(out, namedAdapter{p.name, a})
		}
	}
	for key := range c.Adapters {
		if !known[key] {
			// Point at the right fix: the vendor adapters are opt-in and
			// need a tag added, while the protocol adapters are opt-out and
			// need one removed. Telling an operator to drop `no_onvif` when
			// the real problem is a missing `-tags onvif` wastes their time.
			if additiveTags[key] {
				return nil, fmt.Errorf(
					"adapter %q is configured but not compiled in — rebuild with `-tags %s`", key, key)
			}
			return nil, fmt.Errorf(
				"adapter %q configured but not compiled into this build (built with no_%s?)", key, key)
		}
	}
	return out, nil
}
