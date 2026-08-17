package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/sdk"
)

// adapterPlugin is one compile-time adapter slot (K207 §4: build-tag
// composition — 拔插头 in the build).
//
// There are two tag conventions, and which one an adapter uses says
// something about it:
//
//   - Subtractive (`//go:build !no_<name>`) — the protocol adapters. They
//     speak open standards to hardware on the local network, depend on
//     nothing outside the distribution, and are in by default; a
//     distribution subtracts what it does not ship.
//
//   - Additive (`//go:build <name>`) — the vendor and camera adapters
//     (onvif, hikvision, dahua). They are out by default and must be asked
//     for: `go build -tags onvif,hikvision`. These reach devices through
//     vendor HTTP APIs and carry credentials, so a default build stays
//     free of them rather than shipping an attack surface nobody enabled.
//
// A distribution that wants everything: `-tags onvif,hikvision,dahua`.
type adapterPlugin struct {
	name      string // config key under "adapters"
	defaultOn bool   // register even without a config block (sim)
	fromJSON  func(json.RawMessage) (sdk.Adapter, error)
}

// additiveTags names the adapters that must be asked for at build time, so
// a misconfiguration can point at the right fix instead of the wrong one.
var additiveTags = map[string]bool{"onvif": true, "hikvision": true, "dahua": true}

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
