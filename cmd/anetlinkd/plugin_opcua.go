//go:build !no_opcua

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/opcua"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "opcua",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c opcua.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return opcua.New(c), nil
		},
	})
}
