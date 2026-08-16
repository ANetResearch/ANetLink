package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ANetResearch/ANetLink/adapters/modbus"
	"github.com/ANetResearch/ANetLink/runtime"
)

// fileConfig is anetlinkd's on-disk configuration (JSON, v0).
type fileConfig struct {
	// BackendOverride forces a named backend per adapter (A5 escape hatch).
	BackendOverride map[string]string `json:"backend_override,omitempty"`
	// Modbus configures the modbus adapter; absent = adapter not registered.
	Modbus *modbus.Config `json:"modbus,omitempty"`
	// Sim keeps the simulator lamp registered (default true, so a bare
	// anetlinkd always has something to show).
	Sim *bool `json:"sim,omitempty"`
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

func (c *fileConfig) simEnabled() bool { return c.Sim == nil || *c.Sim }

func (c *fileConfig) runtimeConfig() runtime.Config {
	return runtime.Config{BackendOverride: c.BackendOverride}
}
