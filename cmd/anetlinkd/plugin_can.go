//go:build !no_can

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/can"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "can",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c can.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return can.New(c), nil
		},
	})
}
