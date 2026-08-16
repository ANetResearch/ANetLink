//go:build realworld

// Real-service tier (L1): runs against a REAL third-party Modbus/TCP server
// (oitc/modbus-server or any real device), never against the in-process Go
// test server. Point ANETLINK_MODBUS_ADDR at the target; L2 = a physical
// PLC/meter at the same variable.
package modbus

import (
	"context"
	"os"
	"testing"

	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

func realAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("ANETLINK_MODBUS_ADDR")
	if addr == "" {
		t.Skip("set ANETLINK_MODBUS_ADDR to a real modbus server (L1: docker oitc/modbus-server; L2: real PLC)")
	}
	return addr
}

func TestRealServerCoilRoundTrip(t *testing.T) {
	addr := realAddr(t)
	a := New(Config{Devices: []DeviceConfig{{
		ID: "real-1", Addr: addr, UnitID: 1,
		Caps: []CapMap{
			{ID: "switch.onoff", Kind: "coil", Address: 10, Access: profile.AccessSet | profile.AccessGet},
			{ID: "sensor.reg", Kind: "holding", Address: 10, Access: profile.AccessGet},
		},
	}}})
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if d, _ := sink.get("real-1"); !d.Online {
		t.Fatal("real server must probe online")
	}

	for _, want := range []bool{true, false, true} {
		eff, err := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "real-1", Capability: "switch.onoff", Args: map[string]any{"on": want},
		})
		if err != nil {
			t.Fatal(err)
		}
		wantV := 0.0
		if want {
			wantV = 1.0
		}
		if !eff.Verifiable() || eff.Record.Metrics["switch_onoff"] != wantV {
			t.Fatalf("real coil readback on=%v: %+v", want, eff)
		}
	}

	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "real-1", Capability: "sensor.reg"})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("real holding read must verify: %+v", eff)
	}
}
