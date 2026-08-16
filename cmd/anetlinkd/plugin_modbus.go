//go:build !no_modbus

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/modbus"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "modbus",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c modbus.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return modbus.New(c), nil
		},
	})
}
