package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/sdk"
)

// adapterPlugin is one compile-time adapter slot (K207 §4: build-tag
// composition; each adapter lives in a `//go:build !no_<name>` file, so a
// distribution subtracts what it does not ship — 拔插头 in the build).
type adapterPlugin struct {
	name      string // config key under "adapters"
	defaultOn bool   // register even without a config block (sim)
	fromJSON  func(json.RawMessage) (sdk.Adapter, error)
}

var plugins []adapterPlugin

func registerPlugin(p adapterPlugin) { plugins = append(plugins, p) }

type namedAdapter struct {
	Name    string
	Adapter sdk.Adapter
}

func adapterNames(as []namedAdapter) string {
	if len(as) == 0 {
		return "(none)"
	}
	s := as[0].Name
	for _, a := range as[1:] {
		s += "," + a.Name
	}
	return s
}
