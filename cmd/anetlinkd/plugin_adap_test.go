//go:build adap

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func adapPlugin(t *testing.T) adapterPlugin {
	t.Helper()
	for _, p := range plugins {
		if p.name == "adap" {
			return p
		}
	}
	t.Fatal("adap plugin not registered")
	return adapterPlugin{}
}

// AL-10: allow_uid let another user drive an out-of-tree adapter socket.
// It is gone; a config that still has it is told so instead of being
// quietly narrowed.
func TestAdapAllowUIDIsRemoved(t *testing.T) {
	p := adapPlugin(t)
	_, err := p.fromJSON(json.RawMessage(`{"sockets": [{"name": "m", "socket": "/tmp/x/m.sock", "allow_uid": 1000}]}`))
	if err == nil || !strings.Contains(err.Error(), "allow_uid") {
		t.Fatalf("allow_uid must be refused with its name, got %v", err)
	}
	a, err := p.fromJSON(json.RawMessage(`{"sockets": [{"name": "m", "socket": "/tmp/x/m.sock"}]}`))
	if err != nil || a == nil {
		t.Fatalf("plain config: %v", err)
	}
}
