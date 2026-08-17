//go:build dahua

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/dahua"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "dahua",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c dahua.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return dahua.New(c), nil
		},
	})
}
