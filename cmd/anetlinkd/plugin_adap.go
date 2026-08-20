//go:build adap

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ANetResearch/ANetLink/adap"
	"github.com/ANetResearch/ANetLink/sdk"
)

// The out-of-tree adapter escape hatch (C4 over UDS).
//
// Additive, like the vendor adapters: hosting a process we did not compile,
// possibly written in another language and reaching hardware on our behalf,
// is a decision an operator makes deliberately. It is not something a
// default build should quietly be capable of.
//
// Config:
//
//	{"adap": {"sockets": [
//	   {"name": "matter", "socket": "/run/anetlink/matter.sock", "allow_uid": 1000}
//	]}}
func init() {
	registerPlugin(adapterPlugin{
		name: "adap",
		fromJSON: func(b json.RawMessage) (sdk.Adapter, error) {
			var cfg struct {
				Sockets []struct {
					Name     string  `json:"name"`
					Socket   string  `json:"socket"`
					AllowUID *uint32 `json:"allow_uid"`
					TimeoutS int     `json:"timeout_s"`
				} `json:"sockets"`
			}
			if len(b) > 0 {
				if err := json.Unmarshal(b, &cfg); err != nil {
					return nil, err
				}
			}
			if len(cfg.Sockets) == 0 {
				return nil, nil
			}
			// One plugin entry, one hosted adapter. Several out-of-tree
			// adapters are several config entries and several sockets:
			// multiplexing them onto one socket would make it ambiguous
			// which process owns a device.
			if len(cfg.Sockets) > 1 {
				return nil, fmt.Errorf(
					"adap: one socket per adapter; got %d — give each its own config entry",
					len(cfg.Sockets))
			}
			s := cfg.Sockets[0]
			if s.Socket == "" {
				return nil, fmt.Errorf("adap: socket path is required")
			}
			hc := adap.HostConfig{Name: s.Name, Socket: s.Socket, AllowUID: s.AllowUID}
			if s.TimeoutS > 0 {
				hc.Timeout = time.Duration(s.TimeoutS) * time.Second
			}
			return adap.NewHost(hc), nil
		},
	})
}
