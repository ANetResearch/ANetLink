//go:build !no_zigbee

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/zigbee"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "zigbee",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c zigbee.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return zigbee.New(c), nil
		},
	})
}
