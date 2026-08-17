//go:build !no_btmesh

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/btmesh"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "btmesh",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c btmesh.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return btmesh.New(c), nil
		},
	})
}
