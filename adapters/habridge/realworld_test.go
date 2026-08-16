//go:build realworld

// Real-service tier (L1): a REAL Home Assistant core (docker, demo
// integration provides real light entities) — real auth, real WebSocket API,
// real state machine. L2 = the operator's production HA at home.
package habridge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetLink/sdk"
)

func realHA(t *testing.T) (url, token string) {
	t.Helper()
	url, token = os.Getenv("ANETLINK_HA_URL"), os.Getenv("ANETLINK_HA_TOKEN")
	if url == "" || token == "" {
		t.Skip("set ANETLINK_HA_URL + ANETLINK_HA_TOKEN (L1: docker HA with demo:; L2: a real home instance)")
	}
	return url, token
}

func TestRealHomeAssistant(t *testing.T) {
	url, token := realHA(t)
	a := New(Config{URL: url, Token: token, Timeout: 8 * time.Second})
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatalf("probe/auth against real HA: %v", err)
	}
	sink := &memSink{devices: map[string]sdk.Device{}}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())

	var lightID string
	sink.mu.Lock()
	for id := range sink.devices {
		if strings.HasPrefix(id, "light.") {
			lightID = id
			break
		}
	}
	sink.mu.Unlock()
	if lightID == "" {
		t.Fatalf("real HA exposed no light entities (demo: integration missing?); got %d devices", sink.count())
	}
	t.Logf("real HA inventory: %d entities; actuating %s", sink.count(), lightID)

	for _, want := range []bool{true, false} {
		eff, err := a.Invoke(context.Background(), sdk.Call{
			DeviceID: lightID, Capability: "light.onoff", Args: map[string]any{"on": want},
		})
		if err != nil {
			t.Fatal(err)
		}
		wantV := 0.0
		if want {
			wantV = 1.0
		}
		if !eff.Verifiable() || eff.Record.Metrics["light_onoff"] != wantV {
			t.Fatalf("real HA actuation on=%v not verified: %+v", want, eff)
		}
	}
}
