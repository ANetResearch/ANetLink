package main

import (
	"encoding/json"
	"fmt"
	"os"

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

func (c *fileConfig) runtimeConfig() runtime.Config {
	return runtime.Config{BackendOverride: c.BackendOverride}
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
			return nil, fmt.Errorf("adapter %q configured but not compiled into this build (no_%s?)", key, key)
		}
	}
	return out, nil
}
