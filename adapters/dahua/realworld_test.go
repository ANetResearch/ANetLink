//go:build realworld

// Real-hardware tier (L2): a REAL Dahua camera or NVR (or an Amcrest/Lorex
// unit on Dahua firmware). The CGI interface is proprietary and has no
// simulator, so this tier only runs against hardware. Point
// ANETLINK_DAHUA_ADDR / _USER / _PASS at one.
//
// TestRealPTZ moves a real camera head. Do not run it against a camera
// whose view someone depends on right now.
package dahua

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

func realCamera(t *testing.T) DeviceConfig {
	t.Helper()
	addr := os.Getenv("ANETLINK_DAHUA_ADDR")
	if addr == "" {
		t.Skip("set ANETLINK_DAHUA_ADDR/_USER/_PASS to a real Dahua camera (L2: no simulator exists for the Dahua CGI)")
	}
	// Cameras behind a port map, and the ANetMock rig, serve RTSP somewhere
	// other than 554. The vendor scheme only says where it usually is.
	rtspPort := 0
	if v := os.Getenv("ANETLINK_DAHUA_RTSP_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("ANETLINK_DAHUA_RTSP_PORT: %v", err)
		}
		rtspPort = n
	}
	return DeviceConfig{
		RTSPPort: rtspPort,
		ID:       "real-dh",
		Address:  addr,
		User:     os.Getenv("ANETLINK_DAHUA_USER"),
		Pass:     os.Getenv("ANETLINK_DAHUA_PASS"),
	}
}

func startReal(t *testing.T, verify bool) (*Adapter, *memSink) {
	t.Helper()
	a := New(Config{Devices: []DeviceConfig{realCamera(t)}, Timeout: 10 * time.Second, VerifyStream: verify})
	sink := newSink()
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatalf("probe against the real camera: %v", err)
	}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })
	return a, sink
}

func TestRealIdentityAndCapabilities(t *testing.T) {
	_, sink := startReal(t, false)
	d, ok := sink.get("real-dh")
	if !ok || !d.Online {
		t.Fatalf("a real camera that answered must be online: %+v", d)
	}
	if d.Profile.Model == "" {
		t.Fatal("a real camera must report its type over the CGI")
	}
	var ids []string
	for _, c := range d.Profile.AllCaps() {
		ids = append(ids, c.ID)
	}
	t.Logf("real Dahua: model=%q caps=%v", d.Profile.Model, ids)
}

func TestRealSnapshotIsRealBytes(t *testing.T) {
	a, _ := startReal(t, false)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "real-dh", Capability: profile.CapStreamSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("a real camera must return a real frame: %+v (%s)", eff, eff.Message)
	}
	art := eff.Record.Artifacts[0]
	if art.SizeBytes < 1000 {
		t.Fatalf("a real JPEG should not be %d bytes", art.SizeBytes)
	}
	t.Logf("real frame: %d bytes, cid=%s", art.SizeBytes, art.ContentCID)
}

func TestRealStreamProbe(t *testing.T) {
	a, _ := startReal(t, true)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "real-dh", Capability: profile.CapStreamRTSP})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("a real camera's main stream must verify: %+v (%s)", eff, eff.Message)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "H26") {
		t.Fatalf("the real codec must be reported: %q", eff.Evidence.ObservedState)
	}
	t.Logf("verified: %s", eff.Evidence.ObservedState)
}

func TestRealPTZ(t *testing.T) {
	a, sink := startReal(t, false)
	d, _ := sink.get("real-dh")
	if _, ok := d.Profile.Find(profile.CapPTZMove); !ok {
		t.Skip("this camera has no PTZ")
	}

	before, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "real-dh", Capability: profile.CapPTZPosition})
	if err != nil || !before.Verifiable() {
		t.Fatalf("position read: %+v %v", before, err)
	}
	t.Logf("before: %+v", before.Record.Metrics)

	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "real-dh", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 0.3, "duration_ms": 600.0}})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status == effect.Failed {
		t.Fatalf("the move failed: %s", eff.Message)
	}
	t.Logf("move: status=%s observed=%q", eff.Status, eff.Evidence.ObservedState)

	if _, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "real-dh", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": -0.3, "duration_ms": 600.0}}); err != nil {
		t.Logf("could not return the head to its original bearing: %v", err)
	}
}
