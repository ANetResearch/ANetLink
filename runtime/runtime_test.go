package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/adapters/sim"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

func startedRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt := New(Config{})
	if err := rt.Register(sim.New()); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestStartSelectsBackendAndDiscovers(t *testing.T) {
	rt := startedRuntime(t)
	if rt.Backend("sim") != "builtin" {
		t.Fatalf("backend = %q", rt.Backend("sim"))
	}
	devs := rt.Devices()
	if len(devs) != 1 || devs[0].ID != sim.DeviceID {
		t.Fatalf("inventory wrong: %+v", devs)
	}
}

func TestInvokeRoundTrip(t *testing.T) {
	rt := startedRuntime(t)
	key := DeviceKey{"sim", sim.DeviceID}
	eff, err := rt.Invoke(context.Background(), key, "light.onoff", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["power_state"] != 1 {
		t.Fatalf("expected verifiable on-state, got %+v", eff)
	}
	eff, _ = rt.Invoke(context.Background(), key, "light.onoff", map[string]any{"on": false})
	if eff.Record.Metrics["power_state"] != 0 {
		t.Fatalf("lamp did not turn off: %+v", eff)
	}
}

func TestInvokeRejectsUndeclaredCapability(t *testing.T) {
	rt := startedRuntime(t)
	_, err := rt.Invoke(context.Background(), DeviceKey{"sim", sim.DeviceID}, "thermostat.set", nil)
	if err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("undeclared capability must be rejected pre-adapter, got %v", err)
	}
}

// lowTrustAdapter wraps sim but declares T1 (cloud-grade) trust.
type lowTrustAdapter struct{ *sim.Adapter }

func (l lowTrustAdapter) Info() sdk.Info { return sdk.Info{Name: "cloudsim", Protocol: "sim"} }
func (l lowTrustAdapter) Start(ctx context.Context, b sdk.Backend, s sdk.Sink) error {
	s.UpsertDevice(sdk.Device{
		ID: sim.DeviceID,
		Profile: &profile.DeviceProfile{
			Schema: 1, Protocol: "sim",
			Caps:     []profile.Capability{{ID: "light.onoff", Access: profile.AccessSet}},
			MaxTrust: profile.T1,
		},
		Online: true,
	})
	return nil
}

func TestTrustClampDowngradesOK(t *testing.T) {
	rt := New(Config{})
	if err := rt.Register(lowTrustAdapter{sim.New()}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	eff, err := rt.Invoke(context.Background(), DeviceKey{"cloudsim", sim.DeviceID}, "light.onoff", map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unverified {
		t.Fatalf("T1 device claiming OK must be downgraded to UNVERIFIED, got %s", eff.Status)
	}
}
