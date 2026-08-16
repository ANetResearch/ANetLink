//go:build !no_ha

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/habridge"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "homeassistant",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c habridge.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return habridge.New(c), nil
		},
	})
}
