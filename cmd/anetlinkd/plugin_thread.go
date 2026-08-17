//go:build !no_thread

package main

import (
	"encoding/json"

	"github.com/ANetResearch/ANetLink/adapters/thread"
	"github.com/ANetResearch/ANetLink/sdk"
)

func init() {
	registerPlugin(adapterPlugin{
		name: "thread",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var c thread.Config
			if err := json.Unmarshal(b, &c); err != nil {
				return nil, err
			}
			return thread.New(c), nil
		},
	})
}
