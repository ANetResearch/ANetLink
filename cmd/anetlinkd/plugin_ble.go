//go:build !no_ble

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/ble"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "ble",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c ble.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return ble.New(c), nil
		},
	})
}
