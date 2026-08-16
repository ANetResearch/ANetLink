//go:build !no_mqtt

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/mqttbridge"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "mqtt",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c mqttbridge.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return mqttbridge.New(c), nil
		},
	})
}
