//go:build onvif

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/onvif"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "onvif",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c onvif.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return onvif.New(c), nil
		},
	})
}
