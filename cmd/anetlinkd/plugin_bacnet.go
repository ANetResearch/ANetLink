//go:build !no_bacnet

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/bacnet"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "bacnet",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c bacnet.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return bacnet.New(c), nil
		},
	})
}
