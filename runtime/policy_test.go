package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/adapters/sim"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

var local = Caller{Via: ViaLocal}

const remoteAID = "bafyreifcvw7axfcj45itg7ggk5pf33dtq53kgpf47ltxnjl746b45n62j4"

// countingSim is the sim lamp plus a STATE-only sensor, counting every call
// that reaches the adapter: a refusal that still reached the adapter is not
// a refusal.
type countingSim struct {
	*sim.Adapter
	mu    sync.Mutex
	calls int
}

func (c *countingSim) Start(ctx context.Context, b sdk.Backend, s sdk.Sink) error {
	if err := c.Adapter.Start(ctx, b, s); err != nil {
		return err
	}
	s.UpsertDevice(sdk.Device{ID: "bare-1", Online: true}) // claimed, not yet described
	s.UpsertDevice(sdk.Device{
		ID: "meter-1",
		Profile: &profile.DeviceProfile{
			Schema: 1, Protocol: "sim",
			Caps:     []profile.Capability{{ID: "sensor.power", Access: profile.AccessState}},
			MaxTrust: profile.V4,
		},
		Online: true,
	})
	return nil
}

func (c *countingSim) Invoke(ctx context.Context, call sdk.Call) (effect.Effect, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.Adapter.Invoke(ctx, call)
}

func (c *countingSim) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func policyRuntime(t *testing.T, remote ...string) (*Runtime, *countingSim, *logSink) {
	t.Helper()
	logs := &logSink{}
	a := &countingSim{Adapter: sim.New()}
	rt := New(Config{Remote: remote, Logf: logs.logf})
	if err := rt.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return rt, a, logs
}

var lamp = DeviceKey{"sim", sim.DeviceID}

// SI-62: with nothing configured, a remote caller reaches no device. The
// local surfaces are unaffected.
func TestRemoteCallersAreRefusedByDefault(t *testing.T) {
	rt, a, _ := policyRuntime(t)
	relay := Caller{Via: ViaRelay, AID: remoteAID}
	_, err := rt.Invoke(context.Background(), relay, lamp, "light.onoff", map[string]any{"on": true})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("default c1.remote must refuse a relay caller, got %v", err)
	}
	if a.n() != 0 {
		t.Fatal("a refused call reached the adapter")
	}
	for _, c := range []Caller{{Via: ViaLocal}, {Via: ViaLocalMCP}} {
		eff, err := rt.Invoke(context.Background(), c, lamp, "light.onoff", map[string]any{"on": true})
		if err != nil || eff.Status != effect.OK {
			t.Fatalf("%s caller must be served: %+v %v", c.Via, eff, err)
		}
	}
	if rt.RemoteAllowed(CapabilityID("light.onoff", lamp)) {
		t.Fatal("RemoteAllowed must be false with no patterns")
	}
}

func TestRemotePatternsMatchTheFullID(t *testing.T) {
	relay := Caller{Via: ViaRelay, AID: remoteAID}
	cases := []struct {
		pattern string
		allowed bool
	}{
		{"light.onoff@sim/lamp-1", true},
		{"light.onoff@sim/*", true},
		{"*@sim/*", true},
		{"*/*", true},
		{"light.*@sim/lamp-?", true},
		{"light.onoff@sim/lamp-2", false},
		{"sensor.*@sim/*", false},
		{"*@modbus/*", false},
		{"light.onoff", false}, // no adapter/device part: matches nothing
	}
	for _, c := range cases {
		rt, a, _ := policyRuntime(t, c.pattern)
		_, err := rt.Invoke(context.Background(), relay, lamp, "light.onoff", nil)
		if got := err == nil; got != c.allowed {
			t.Errorf("pattern %q: allowed=%v, want %v (err %v)", c.pattern, got, c.allowed, err)
		}
		if !c.allowed && a.n() != 0 {
			t.Errorf("pattern %q: refused call reached the adapter", c.pattern)
		}
		if rt.RemoteAllowed(CapabilityID("light.onoff", lamp)) != c.allowed {
			t.Errorf("pattern %q: RemoteAllowed disagrees with Invoke", c.pattern)
		}
	}
}

// A relay caller always names who it is; one that does not, and any caller
// kind nobody defined, is refused rather than guessed at.
func TestUnidentifiedOrUnknownCallersAreRefused(t *testing.T) {
	rt, a, _ := policyRuntime(t, "*/*")
	for _, c := range []Caller{
		{Via: ViaRelay},
		{Via: "voucher", AID: remoteAID},
		{Via: ""},
		{Via: "LOCAL"},
	} {
		_, err := rt.Invoke(context.Background(), c, lamp, "light.onoff", nil)
		if !errors.Is(err, ErrNotPermitted) {
			t.Errorf("caller %+v: want ErrNotPermitted, got %v", c, err)
		}
	}
	if a.n() != 0 {
		t.Fatal("a refused call reached the adapter")
	}
}

// The policy is decided before the device table is consulted, so a remote
// caller cannot learn which devices exist by probing ids it may not call.
func TestRemoteRefusalDoesNotRevealDevices(t *testing.T) {
	rt, _, _ := policyRuntime(t, "light.onoff@sim/lamp-1")
	relay := Caller{Via: ViaRelay, AID: remoteAID}
	_, errMissing := rt.Invoke(context.Background(), relay, DeviceKey{"sim", "ghost"}, "light.onoff", nil)
	_, errPresent := rt.Invoke(context.Background(), relay, lamp, "light.dim", nil)
	if !errors.Is(errMissing, ErrNotPermitted) || !errors.Is(errPresent, ErrNotPermitted) {
		t.Fatalf("both must be not_permitted: missing=%v present=%v", errMissing, errPresent)
	}
}

func TestUnknownCapabilityErrors(t *testing.T) {
	rt, a, _ := policyRuntime(t)
	for _, c := range []struct {
		key DeviceKey
		cap string
	}{
		{DeviceKey{"sim", "ghost"}, "light.onoff"},
		{DeviceKey{"nope", sim.DeviceID}, "light.onoff"},
		{lamp, "thermostat.set"},
		{DeviceKey{"sim", "meter-1"}, "sensor.power"}, // STATE-only: declared, not invokable
		{DeviceKey{"sim", "bare-1"}, "light.onoff"},   // no profile yet
	} {
		_, err := rt.Invoke(context.Background(), local, c.key, c.cap, nil)
		if !errors.Is(err, ErrUnknownCapability) {
			t.Errorf("%s on %s: want ErrUnknownCapability, got %v", c.cap, c.key, err)
		}
	}
	if a.n() != 0 {
		t.Fatal("an unknown capability reached the adapter")
	}
}

func TestValidateRemote(t *testing.T) {
	for _, ok := range [][]string{nil, {}, {"*/*"}, {"light.onoff@sim/*", "*@modbus/plc-1"}} {
		if err := ValidateRemote(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "[", "light.onoff@sim/[", "light.onoff", "*"} {
		if err := ValidateRemote([]string{bad}); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// AL-8: one line per invocation with who, what and how it ended — and
// never the arguments, which can carry anything a caller chose to send.
func TestEveryInvocationIsLoggedWithoutArgs(t *testing.T) {
	rt, _, logs := policyRuntime(t, "light.onoff@sim/*")
	const canary = "args-canary-7f3a"
	relay := Caller{Via: ViaRelay, AID: remoteAID}
	if _, err := rt.Invoke(context.Background(), relay, lamp, "light.onoff", map[string]any{"on": true, "note": canary}); err != nil {
		t.Fatal(err)
	}
	_, _ = rt.Invoke(context.Background(), relay, DeviceKey{"sim", "meter-1"}, "sensor.power", map[string]any{"note": canary})
	_, _ = rt.Invoke(context.Background(), Caller{Via: ViaLocalMCP}, DeviceKey{"sim", "ghost"}, "light.onoff", nil)

	out := logs.all()
	if strings.Contains(out, canary) {
		t.Fatalf("invocation log leaked args:\n%s", out)
	}
	for _, want := range []string{
		`via=relay aid="` + remoteAID + `" cap="light.onoff@sim/lamp-1" result=OK`,
		`via=relay aid="` + remoteAID + `" cap="sensor.power@sim/meter-1" result=not_permitted`,
		`via=local-mcp aid="" cap="light.onoff@sim/ghost" result=unknown_capability`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing log line %q in:\n%s", want, out)
		}
	}
}

// The key an id is published under must be the key Invoke resolves. An
// adapter's name and its devices' protocol differ for several adapters
// (mqttbridge/mqtt, habridge/homeassistant, hikvision/isapi), and a
// listing built from the protocol published ids that never resolved — and
// c1.remote would have been matched against one string when listing and
// another when invoking.
func TestInventoryKeysAreTheInvokeKeys(t *testing.T) {
	rt := New(Config{Remote: []string{"light.onoff@cloudsim/*"}, Logf: (&logSink{}).logf})
	if err := rt.Register(lowTrustAdapter{sim.New()}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	inv := rt.Inventory()
	if len(inv) != 1 || inv[0].Key != (DeviceKey{"cloudsim", sim.DeviceID}) {
		t.Fatalf("inventory key: %+v", inv)
	}
	relay := Caller{Via: ViaRelay, AID: remoteAID}
	for _, e := range inv {
		for _, c := range e.Device.Profile.AllCaps() {
			id := CapabilityID(c.ID, e.Key)
			if !rt.RemoteAllowed(id) {
				t.Fatalf("listed id %s is not the id the policy sees", id)
			}
			if _, err := rt.Invoke(context.Background(), relay, e.Key, c.ID, nil); err != nil {
				t.Fatalf("listed id %s does not resolve: %v", id, err)
			}
		}
	}
}
