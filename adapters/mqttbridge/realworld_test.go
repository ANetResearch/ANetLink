//go:build realworld

// Real-service tier (L1): a REAL broker (mosquitto), the adapter as one real
// client, and a second real client playing the device side over the wire —
// no in-process broker. Honest limit: without a radio stick the *device* is
// synthetic; L2 replaces the responder with a live zigbee2mqtt install
// (ANETLINK_MQTT_BROKER pointed at its broker, responder disabled).
package mqttbridge

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/ANetResearch/ANetLink/sdk"
)

func realBroker(t *testing.T) string {
	t.Helper()
	b := os.Getenv("ANETLINK_MQTT_BROKER")
	if b == "" {
		t.Skip("set ANETLINK_MQTT_BROKER (L1: docker eclipse-mosquitto; L2: the broker of a live zigbee2mqtt)")
	}
	return b
}

func TestRealBrokerZ2MFlow(t *testing.T) {
	broker := realBroker(t)
	live := os.Getenv("ANETLINK_MQTT_LIVE_Z2M") == "1" // L2: real zigbee2mqtt owns the topics

	if !live {
		// device side: a REAL second MQTT client over the REAL broker
		opts := paho.NewClientOptions().AddBroker(broker).SetClientID("rw-devicesim")
		dev := paho.NewClient(opts)
		if tok := dev.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
			t.Fatalf("device client connect: %v", tok.Error())
		}
		defer dev.Disconnect(100)
		devices := `[{"friendly_name":"rw-lamp","ieee_address":"0xrw","definition":{"vendor":"real","model":"wire",
			"exposes":[{"type":"binary","name":"state","property":"state","access":7}]}}]`
		if tok := dev.Publish("zigbee2mqtt/bridge/devices", 0, true, devices); tok.Wait() && tok.Error() != nil {
			t.Fatal(tok.Error())
		}
		dev.Subscribe("zigbee2mqtt/rw-lamp/set", 0, func(_ paho.Client, m paho.Message) {
			var req map[string]string
			_ = json.Unmarshal(m.Payload(), &req)
			resp, _ := json.Marshal(map[string]string{"state": req["state"]})
			dev.Publish("zigbee2mqtt/rw-lamp", 0, false, resp)
		})
		defer dev.Publish("zigbee2mqtt/bridge/devices", 0, true, "[]") // clear retained
	}

	a := New(Config{Broker: broker, Timeout: 3 * time.Second})
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatalf("probe against real broker: %v", err)
	}
	sink := &memSink{devices: map[string]sdk.Device{}}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if len(sink.devices) == 0 {
		t.Fatal("no devices discovered over the real broker")
	}
	if live {
		t.Logf("live zigbee2mqtt inventory: %d devices — actuation left to operator-supervised runs", len(sink.devices))
		return
	}

	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "rw-lamp", Capability: "switch.onoff", Args: map[string]any{"on": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["switch_onoff"] != 1 {
		t.Fatalf("real-broker echo must verify: %+v", eff)
	}
}
