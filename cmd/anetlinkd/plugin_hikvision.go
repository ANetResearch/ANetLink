//go:build hikvision

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/hikvision"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "hikvision",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c hikvision.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return hikvision.New(c), nil
		},
	})
}
