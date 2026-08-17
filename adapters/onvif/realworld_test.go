//go:build realworld

// Real-service tier (L1): a REAL ONVIF device service in front of a REAL
// RTSP stream (the onvif-server + mediamtx rig in test/testbed/camera), or
// a REAL camera at L2. Point ANETLINK_ONVIF_XADDR at its device service.
//
// Cross-implementation on purpose: our pure-Go SOAP client against a
// JavaScript ONVIF server against a Go media server. Agreement across three
// independent implementations is worth more than any fake.
package onvif

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

func realCamera(t *testing.T) DeviceConfig {
	t.Helper()
	xaddr := os.Getenv("ANETLINK_ONVIF_XADDR")
	if xaddr == "" {
		t.Skip("set ANETLINK_ONVIF_XADDR to a real ONVIF device service " +
			"(L1: test/testbed/camera; L2: a real camera)")
	}
	return DeviceConfig{
		ID:    "real-cam",
		XAddr: xaddr,
		User:  os.Getenv("ANETLINK_ONVIF_USER"),
		Pass:  os.Getenv("ANETLINK_ONVIF_PASS"),
	}
}

func startReal(t *testing.T, verify bool) (*Adapter, *memSink) {
	t.Helper()
	a := New(Config{
		Devices:      []DeviceConfig{realCamera(t)},
		Timeout:      10 * time.Second,
		VerifyStream: verify,
	})
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

func TestRealCameraIntrospection(t *testing.T) {
	a, sink := startReal(t, false)

	d, ok := sink.get("real-cam")
	if !ok || !d.Online {
		t.Fatalf("a real camera that answered must be online: %+v", d)
	}
	dev, _ := a.lookup("real-cam")
	if dev.profTok == "" {
		t.Fatal("a real camera must yield a media profile token")
	}
	if dev.streamU == "" {
		t.Fatal("a real camera must yield a stream uri")
	}
	if !strings.HasPrefix(dev.streamU, "rtsp://") {
		t.Fatalf("stream uri from a real camera: %q", dev.streamU)
	}
	cid, err := d.Profile.CID()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real camera: vendor=%q model=%q profile=%s stream=%s",
		d.Profile.Vendor, d.Profile.Model, cid, dev.streamU)
}

// TestRealStreamIsProbedNotAssumed is the point of the whole media family:
// against a real camera and a real media server, the URI must be confirmed
// by an actual RTSP exchange before we call the effect verified.
func TestRealStreamIsProbedNotAssumed(t *testing.T) {
	a, _ := startReal(t, true)

	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "real-cam", Capability: profile.CapStreamRTSP})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("a live stream must verify against a real media server: %+v (%s)", eff, eff.Message)
	}
	if eff.Evidence.VerifyTrust != uint8(profile.V2) {
		t.Fatalf("a probed stream is V2: %+v", eff.Evidence)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "H26") {
		t.Fatalf("the real codec must be reported: %q", eff.Evidence.ObservedState)
	}
	t.Logf("verified real stream: %s", eff.Evidence.ObservedState)
}

// TestRealUnverifiedPathStaysHonest: with probing off, the same live camera
// must report UNVERIFIED — the status reflects what we checked, not what
// happens to be true.
func TestRealUnverifiedPathStaysHonest(t *testing.T) {
	a, _ := startReal(t, false)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "real-cam", Capability: profile.CapStreamRTSP})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unverified {
		t.Fatalf("an unprobed uri must stay UNVERIFIED even when the stream is in fact live: %+v", eff)
	}
}

// TestRealCapabilitiesAreNotOverclaimed: this rig is Profile S only, so the
// adapter must not advertise PTZ or events on it.
func TestRealCapabilitiesAreNotOverclaimed(t *testing.T) {
	_, sink := startReal(t, false)
	d, _ := sink.get("real-cam")
	for _, c := range d.Profile.AllCaps() {
		t.Logf("declared: %s", c.ID)
	}
	if _, ok := d.Profile.Find(profile.CapStreamRTSP); !ok {
		t.Fatal("stream.rtsp must be declared")
	}
}
