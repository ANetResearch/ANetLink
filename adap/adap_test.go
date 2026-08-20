package adap

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANetLink/sdk"
)

// memSink records what the runtime would have been told.
type memSink struct {
	mu      sync.Mutex
	devices map[string]sdk.Device
	states  []stateUpdate
}

type stateUpdate struct {
	id      string
	metrics map[string]float64
}

func newSink() *memSink { return &memSink{devices: map[string]sdk.Device{}} }

func (s *memSink) UpsertDevice(d sdk.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[d.ID] = d
}
func (s *memSink) RemoveDevice(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, id)
}
func (s *memSink) State(id string, m map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, stateUpdate{id, m})
}
func (s *memSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.devices)
}

// rig starts a host and connects a plugin to it over a real socket.
func rig(t *testing.T, p *Plugin) (*Host, *memSink) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "adap.sock")
	h := NewHost(HostConfig{Name: "test", Socket: sock, Timeout: 3 * time.Second})
	sink := newSink()

	b, err := sdk.Select(context.Background(), "", h.Backends()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background()) })

	if err := p.Dial(context.Background(), sock); err != nil {
		t.Fatal(err)
	}
	go p.Serve(context.Background())
	t.Cleanup(func() { p.Close() })
	return h, sink
}

func waitFor(t *testing.T, cond func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// An out-of-tree adapter must be indistinguishable from a compiled-in one.
// That equivalence is the design: the runtime resolves, quirks, verifies and
// signs the same way, and nothing in it learns that this adapter lives in
// another process.
func TestHostSatisfiesTheAdapterContract(t *testing.T) {
	p := &Plugin{Name: "lamp", Protocol: "demo", Version: "1.0",
		Invoke: func(_ context.Context, _, _ string, args map[string]any) (map[string]float64, bool, error) {
			on := 0.0
			if v, _ := args["on"].(bool); v {
				on = 1
			}
			return map[string]float64{"power_state": on}, true, nil
		}}
	h, sink := rig(t, p)

	if err := p.Claim("lamp-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return sink.count() == 1 }, "the claimed device to reach the sink")

	eff, err := h.Invoke(context.Background(), sdk.Call{
		DeviceID: "lamp-1", Capability: "light.onoff", Args: map[string]any{"on": true}})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("status = %v, want OK: %s", eff.Status, eff.Message)
	}
	if eff.Record == nil || eff.Record.Metrics["power_state"] != 1 {
		t.Fatalf("the effect must carry the readback: %+v", eff.Record)
	}
	if eff.Evidence == nil || eff.Evidence.VerifyTrust != 2 {
		t.Fatalf("a readback is V2 evidence, got %+v", eff.Evidence)
	}
	// The host reports the adapter's own identity, learned at hello.
	if got := h.Info(); got.Name != "lamp" || got.Protocol != "demo" {
		t.Errorf("info = %+v, want the adapter's own", got)
	}
}

// An adapter that cannot read the value back has verified nothing, however
// cleanly the command was accepted. "I sent it" is not "I confirmed it",
// and a process boundary is exactly where that distinction goes missing.
func TestUnobservedActionIsUnverifiedNotOK(t *testing.T) {
	p := &Plugin{Name: "blind", Protocol: "demo",
		Invoke: func(context.Context, string, string, map[string]any) (map[string]float64, bool, error) {
			return nil, false, nil // accepted, not observed
		}}
	h, _ := rig(t, p)
	if err := p.Claim("d-1"); err != nil {
		t.Fatal(err)
	}

	eff, err := h.Invoke(context.Background(), sdk.Call{DeviceID: "d-1", Capability: "x.do"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unverified {
		t.Fatalf("status = %v, want UNVERIFIED", eff.Status)
	}
	if eff.Evidence.VerifyTrust != 1 {
		t.Errorf("an unobserved action is V1 at best, got V%d", eff.Evidence.VerifyTrust)
	}
}

// A message that outlives its ttl must be dropped, not executed late.
//
// This is a safety property rather than an optimisation: a command to open
// a door that arrives 45 seconds after it was issued belongs to a world
// that is gone. The adapter must refuse it, and must not run the action and
// report the lateness afterwards.
func TestExpiredMessageIsNeverExecuted(t *testing.T) {
	var ran bool
	var mu sync.Mutex
	p := &Plugin{Name: "door", Protocol: "demo",
		Invoke: func(context.Context, string, string, map[string]any) (map[string]float64, bool, error) {
			mu.Lock()
			ran = true
			mu.Unlock()
			return nil, true, nil
		}}
	h, _ := rig(t, p)
	if err := p.Claim("door-1"); err != nil {
		t.Fatal(err)
	}

	// A frame stamped in the past with a short ttl: already expired on arrival.
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()
	rid := newRID()
	ch := make(chan *Message, 1)
	h.mu.Lock()
	h.pending[rid] = ch
	h.mu.Unlock()
	if err := conn.Send(&Message{
		Op: OpInvoke, RID: rid, DeviceID: "door-1", Capability: "lock.unlock",
		TS:  uint64(time.Now().Add(-30 * time.Second).UnixMilli()),
		TTL: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case resp := <-ch:
		if resp.Op != OpError || resp.Payload["tag"] != ErrExpired {
			t.Fatalf("an expired command must be refused as EXPIRED, got %+v", resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no answer to the expired command")
	}
	mu.Lock()
	defer mu.Unlock()
	if ran {
		t.Fatal("the action ran despite having expired — this is the door-opening case")
	}
}

// When the adapter process goes away its devices must go with it: leaving
// them registered would let the runtime keep offering capabilities that
// nothing can serve.
func TestDevicesLeaveWithTheAdapter(t *testing.T) {
	p := &Plugin{Name: "gone", Protocol: "demo"}
	_, sink := rig(t, p)
	if err := p.Claim("d-1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return sink.count() == 1 }, "the device to register")

	p.Close()
	waitFor(t, func() bool { return sink.count() == 0 }, "the device to be withdrawn")
}

// An unknown op is answered, not fatal — a receiver must tolerate what it
// does not know rather than drop the session (spec §8).
func TestUnknownOpIsRefusedNotFatal(t *testing.T) {
	p := &Plugin{Name: "odd", Protocol: "demo"}
	h, _ := rig(t, p)

	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()
	rid := newRID()
	ch := make(chan *Message, 1)
	h.mu.Lock()
	h.pending[rid] = ch
	h.mu.Unlock()
	if err := conn.Send(&Message{Op: "teleport", RID: rid}); err != nil {
		t.Fatal(err)
	}
	select {
	case resp := <-ch:
		if resp.Payload["tag"] != ErrUnsupported {
			t.Fatalf("want UNSUPPORTED, got %+v", resp.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no answer to an unknown op")
	}
	// The session must still work afterwards.
	if err := p.Claim("still-here"); err != nil {
		t.Fatalf("session died on an unknown op: %v", err)
	}
}

// Events carry facts and must not be coalesced the way property values are.
func TestEventsReachTheSink(t *testing.T) {
	p := &Plugin{Name: "sensor", Protocol: "demo"}
	_, sink := rig(t, p)
	if err := p.Claim("s-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Event("s-1", map[string]float64{"contact": 0}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.states) == 1 && sink.states[0].metrics["contact"] == 0
	}, "the event to reach the sink")
}
