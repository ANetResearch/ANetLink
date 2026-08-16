//go:build realworld

// Real-service tier (L1): a REAL OPC UA server (Microsoft opc-plc simulator
// or any real PLC/SCADA endpoint). Cross-implementation: our gopcua client
// against a C#/.NET server. Point ANETLINK_OPCUA_ENDPOINT at it.
package opcua

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

func realEndpoint(t *testing.T) string {
	t.Helper()
	ep := os.Getenv("ANETLINK_OPCUA_ENDPOINT")
	if ep == "" {
		t.Skip("set ANETLINK_OPCUA_ENDPOINT to a real OPC UA server (L1: docker mcr.microsoft.com/iotedge/opc-plc; L2: a real PLC/SCADA)")
	}
	return ep
}

func TestRealOPCUARead(t *testing.T) {
	ep := realEndpoint(t)
	// opc-plc exposes ns=3;s=SpikeData (float) and ns=3;s=FastUInt1 (uint).
	a := New(Config{Devices: []DeviceConfig{{
		ID: "opcplc-1", Endpoint: ep, Vendor: "microsoft", Model: "opc-plc",
		Nodes: []NodeMap{
			{ID: "sensor.spike", NodeID: "ns=3;s=SpikeData", Access: profile.AccessGet},
			{ID: "sensor.fast", NodeID: "ns=3;s=FastUInt1", Access: profile.AccessGet},
		},
	}}, Timeout: 8 * time.Second})
	sink := &memSink{devices: map[string]sdk.Device{}}
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if !sink.devices["opcplc-1"].Online {
		t.Fatal("real OPC UA server must probe online")
	}

	for _, cap := range []string{"sensor.spike", "sensor.fast"} {
		eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "opcplc-1", Capability: cap})
		if err != nil {
			t.Fatal(err)
		}
		if !eff.Verifiable() {
			t.Fatalf("real read of %s must verify: %+v", cap, eff)
		}
		t.Logf("real opc-plc %s = %v", cap, eff.Record.Metrics)
	}
}
