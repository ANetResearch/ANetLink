//go:build !no_sim

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/sim"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name:      "sim",
		defaultOn: true, // a bare anetlinkd always has something to show
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var cfg struct {
				Enabled *bool `json:"enabled"`
			}
			if len(b) > 0 {
				if err := json.Unmarshal(b, &cfg); err != nil {
					return nil, err
				}
			}
			if cfg.Enabled != nil && !*cfg.Enabled {
				return nil, nil // explicit off
			}
			return sim.New(), nil
		},
	})
}
