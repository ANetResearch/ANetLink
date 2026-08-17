package onvif

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

type memSink struct {
	mu      sync.Mutex
	devices map[string]sdk.Device
	states  []map[string]float64
}

func newSink() *memSink { return &memSink{devices: map[string]sdk.Device{}} }

func (m *memSink) UpsertDevice(d sdk.Device) { m.mu.Lock(); m.devices[d.ID] = d; m.mu.Unlock() }
func (m *memSink) RemoveDevice(id string)    { m.mu.Lock(); delete(m.devices, id); m.mu.Unlock() }
func (m *memSink) State(_ string, metrics map[string]float64) {
	m.mu.Lock()
	m.states = append(m.states, metrics)
	m.mu.Unlock()
}
func (m *memSink) get(id string) (sdk.Device, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	return d, ok
}
func (m *memSink) stateCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.states)
}
func (m *memSink) lastState() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.states) == 0 {
		return nil
	}
	return m.states[len(m.states)-1]
}

// --- a fake camera that speaks the real ONVIF wire ---

type fakeCam struct {
	srv *httptest.Server

	mu       sync.Mutex
	hasPTZ   bool
	hasEvent bool
	pan      float64
	tilt     float64
	zoom     float64
	moving   bool
	// stubborn simulates a camera at its mechanical limit: it accepts a move
	// and reports success while its position never changes.
	stubborn bool
	// noClock simulates firmware that refuses GetSystemDateAndTime despite
	// the spec mandating it be available unauthenticated.
	noClock bool
	// authFault makes every media call answer NotAuthorized.
	authFault bool
	snapshot  []byte
	streamURI string
	events    []string // topics to emit on the next PullMessages
}

func soapOK(body string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="` + nsSOAP + `"><s:Body>` + body + `</s:Body></s:Envelope>`
}

func soapFaultXML(sub, reason string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="` + nsSOAP + `"><s:Body><s:Fault>` +
		`<s:Code><s:Value>s:Sender</s:Value><s:Subcode><s:Value>` + sub + `</s:Value></s:Subcode></s:Code>` +
		`<s:Reason><s:Text>` + reason + `</s:Text></s:Reason>` +
		`</s:Fault></s:Body></s:Envelope>`
}

func newFakeCam(t *testing.T, opts ...func(*fakeCam)) *fakeCam {
	t.Helper()
	c := &fakeCam{snapshot: []byte("\xff\xd8\xff\xe0JFIF-frame-bytes"), pan: 0.1, tilt: 0.2, zoom: 0.3}
	for _, o := range opts {
		o(c)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/onvif/device_service", c.handleSOAP)
	mux.HandleFunc("/onvif/snapshot", c.handleSnapshot)
	c.srv = httptest.NewServer(mux)
	if c.streamURI == "" {
		c.streamURI = "rtsp://" + strings.TrimPrefix(c.srv.URL, "http://") + "/Streaming/Channels/101"
	}
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCam) xaddr() string { return c.srv.URL + "/onvif/device_service" }

func (c *fakeCam) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	// Real cameras demand digest on the snapshot endpoint.
	if r.Header.Get("Authorization") == "" {
		w.Header().Set("WWW-Authenticate", `Digest realm="cam", qop="auth", nonce="snapnonce"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	c.mu.Lock()
	data := c.snapshot
	c.mu.Unlock()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(data)
}

func (c *fakeCam) handleSOAP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	req := string(raw)
	c.mu.Lock()
	defer c.mu.Unlock()

	has := func(op string) bool { return strings.Contains(req, op) }
	w.Header().Set("Content-Type", "application/soap+xml")

	switch {
	case has("GetSystemDateAndTime"):
		if c.noClock {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, soapFaultXML("ter:NotAuthorized", "Sender not authorized"))
			return
		}
		// Report a clock five minutes ahead — the drift a real camera has.
		t := time.Now().UTC().Add(5 * time.Minute)
		fmt.Fprint(w, soapOK(fmt.Sprintf(
			`<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime><tt:UTCDateTime>`+
				`<tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>`+
				`<tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date>`+
				`</tt:UTCDateTime></tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`,
			t.Hour(), t.Minute(), t.Second(), t.Year(), int(t.Month()), t.Day())))

	case has("GetDeviceInformation"):
		fmt.Fprint(w, soapOK(`<tds:GetDeviceInformationResponse>`+
			`<tds:Manufacturer>ACME</tds:Manufacturer><tds:Model>IPC-9000</tds:Model>`+
			`<tds:FirmwareVersion>V5.7.3</tds:FirmwareVersion></tds:GetDeviceInformationResponse>`))

	case has("GetCapabilities"):
		b := `<tds:GetCapabilitiesResponse><tds:Capabilities>` +
			`<tt:Media><tt:XAddr>` + c.srv.URL + `/onvif/device_service</tt:XAddr></tt:Media>`
		if c.hasPTZ {
			b += `<tt:PTZ><tt:XAddr>` + c.srv.URL + `/onvif/device_service</tt:XAddr></tt:PTZ>`
		}
		if c.hasEvent {
			b += `<tt:Events><tt:XAddr>` + c.srv.URL + `/onvif/device_service</tt:XAddr></tt:Events>`
		}
		fmt.Fprint(w, soapOK(b+`</tds:Capabilities></tds:GetCapabilitiesResponse>`))

	case has("GetProfiles"):
		if c.authFault {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, soapFaultXML("ter:NotAuthorized", "Sender not authorized"))
			return
		}
		fmt.Fprint(w, soapOK(`<trt:GetProfilesResponse>`+
			`<trt:Profiles token="MainStream"><tt:Name>main</tt:Name></trt:Profiles>`+
			`<trt:Profiles token="SubStream"><tt:Name>sub</tt:Name></trt:Profiles>`+
			`</trt:GetProfilesResponse>`))

	case has("GetStreamUri"):
		fmt.Fprint(w, soapOK(`<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>`+
			c.streamURI+`</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`))

	case has("GetSnapshotUri"):
		fmt.Fprint(w, soapOK(`<trt:GetSnapshotUriResponse><trt:MediaUri><tt:Uri>`+
			c.srv.URL+`/onvif/snapshot</tt:Uri></trt:MediaUri></trt:GetSnapshotUriResponse>`))

	case has("ContinuousMove"):
		if !c.stubborn {
			c.pan, c.tilt = c.pan+0.1, c.tilt+0.1
		}
		c.moving = true
		fmt.Fprint(w, soapOK(`<tptz:ContinuousMoveResponse/>`))

	case has("GotoPreset"):
		c.pan, c.tilt, c.zoom = 0.9, -0.4, 0.5
		fmt.Fprint(w, soapOK(`<tptz:GotoPresetResponse/>`))

	case has("tptz:Stop"), has("Stop>"):
		if !c.stubborn {
			c.moving = false
		}
		fmt.Fprint(w, soapOK(`<tptz:StopResponse/>`))

	case has("GetStatus"):
		state := "IDLE"
		if c.moving {
			state = "MOVING"
		}
		fmt.Fprint(w, soapOK(fmt.Sprintf(
			`<tptz:GetStatusResponse><tptz:PTZStatus><tt:Position>`+
				`<tt:PanTilt x="%g" y="%g"/><tt:Zoom x="%g"/></tt:Position>`+
				`<tt:MoveStatus><tt:PanTilt>%s</tt:PanTilt><tt:Zoom>%s</tt:Zoom></tt:MoveStatus>`+
				`</tptz:PTZStatus></tptz:GetStatusResponse>`, c.pan, c.tilt, c.zoom, state, state)))

	case has("CreatePullPointSubscription"):
		fmt.Fprint(w, soapOK(`<tev:CreatePullPointSubscriptionResponse><tev:SubscriptionReference>`+
			`<wsa:Address>`+c.srv.URL+`/onvif/device_service</wsa:Address>`+
			`</tev:SubscriptionReference></tev:CreatePullPointSubscriptionResponse>`))

	case has("PullMessages"):
		b := `<tev:PullMessagesResponse>`
		for _, topic := range c.events {
			b += `<wsnt:NotificationMessage><wsnt:Topic>` + topic + `</wsnt:Topic>` +
				`<wsnt:Message><tt:Message UtcTime="2026-08-17T00:00:00Z">` +
				`<tt:Data><tt:SimpleItem Name="State" Value="true"/></tt:Data>` +
				`</tt:Message></wsnt:Message></wsnt:NotificationMessage>`
		}
		c.events = nil
		fmt.Fprint(w, soapOK(b+`</tev:PullMessagesResponse>`))

	default:
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, soapFaultXML("ter:ActionNotSupported", "unknown operation"))
	}
}

func withNoClock(c *fakeCam)  { c.noClock = true }
func withPTZ(c *fakeCam)      { c.hasPTZ = true }
func withEvents(c *fakeCam)   { c.hasEvent = true }
func withStubborn(c *fakeCam) { c.stubborn = true; c.hasPTZ = true }

func startAdapter(t *testing.T, cam *fakeCam, mut ...func(*Config)) (*Adapter, *memSink) {
	t.Helper()
	cfg := Config{
		Devices: []DeviceConfig{{
			ID: "cam-1", XAddr: cam.xaddr(), User: "admin", Pass: "secret", Events: cam.hasEvent,
		}},
		Timeout: 3 * time.Second,
	}
	for _, m := range mut {
		m(&cfg)
	}
	a := New(cfg)
	sink := newSink()
	b, err := sdk.Select(context.Background(), "", a.Backends()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background(), b, sink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })
	return a, sink
}

// --- tests ---

func TestIntrospectionProjectsTheCamera(t *testing.T) {
	cam := newFakeCam(t)
	_, sink := startAdapter(t, cam)

	d, ok := sink.get("cam-1")
	if !ok || !d.Online {
		t.Fatalf("camera should be online: %+v", d)
	}
	if d.Profile.Vendor != "ACME" || d.Profile.Model != "IPC-9000" {
		t.Fatalf("identity must come from the camera: %+v", d.Profile)
	}
	if d.Profile.MaxTrust != profile.V2 || d.Profile.AuthTrust != profile.A1 {
		t.Fatalf("onvif trust axes must be V2/A1: V%d/A%d", d.Profile.MaxTrust, d.Profile.AuthTrust)
	}
	if _, ok := d.Profile.Find(profile.CapStreamRTSP); !ok {
		t.Fatal("a camera with a stream uri must expose stream.rtsp")
	}
	if _, ok := d.Profile.Find(profile.CapStreamSnapshot); !ok {
		t.Fatal("a camera with a snapshot uri must expose stream.snapshot")
	}
}

// TestCapabilitiesReflectTheCameraNotTheProtocol: declaring PTZ on a fixed
// camera converts a clean "no such capability" into a runtime failure the
// operator has to debug.
func TestCapabilitiesReflectTheCameraNotTheProtocol(t *testing.T) {
	fixed := newFakeCam(t)
	_, sink := startAdapter(t, fixed)
	d, _ := sink.get("cam-1")
	for _, id := range []string{profile.CapPTZMove, profile.CapPTZPosition, profile.CapEventMotion} {
		if _, ok := d.Profile.Find(id); ok {
			t.Fatalf("fixed camera must not claim %s", id)
		}
	}

	ptz := newFakeCam(t, withPTZ)
	_, sink2 := startAdapter(t, ptz)
	d2, _ := sink2.get("cam-1")
	for _, id := range []string{profile.CapPTZMove, profile.CapPTZStop, profile.CapPTZPreset, profile.CapPTZPosition} {
		if _, ok := d2.Profile.Find(id); !ok {
			t.Fatalf("PTZ camera must expose %s", id)
		}
	}
}

func TestProfileCIDIsStableAcrossRestarts(t *testing.T) {
	cam := newFakeCam(t, withPTZ, withEvents)
	_, s1 := startAdapter(t, cam)
	d1, _ := s1.get("cam-1")
	cid1, err := d1.Profile.CID()
	if err != nil {
		t.Fatal(err)
	}
	_, s2 := startAdapter(t, cam)
	d2, _ := s2.get("cam-1")
	cid2, err := d2.Profile.CID()
	if err != nil {
		t.Fatal(err)
	}
	// Capabilities are collected in a map; without a fixed emission order
	// the profile CID would churn on every restart.
	if cid1 != cid2 {
		t.Fatalf("profile CID must not depend on map order:\n %s\n %s", cid1, cid2)
	}
}

// TestCameraWithoutClockCallStillWorks: ONVIF Core mandates that
// GetSystemDateAndTime be available unauthenticated, and firmware in the
// field routinely does not honour that. Its only job here is correcting the
// WS-Security timestamp, so refusing it must not strand an otherwise
// perfectly usable camera.
func TestCameraWithoutClockCallStillWorks(t *testing.T) {
	cam := newFakeCam(t, withNoClock, withPTZ)
	a, sink := startAdapter(t, cam)

	d, ok := sink.get("cam-1")
	if !ok || !d.Online {
		t.Fatalf("a camera that refuses the clock call is still usable: %+v", d)
	}
	if _, ok := d.Profile.Find(profile.CapStreamRTSP); !ok {
		t.Fatal("its stream capability must still be discovered")
	}
	dev, _ := a.lookup("cam-1")
	if dev.cli.skew() != 0 {
		t.Fatalf("with no clock reading the skew must stay zero, got %v", dev.cli.skew())
	}

	// The probe must fall back to an authenticated call rather than
	// condemning the camera on the strength of one refused request.
	av := nativeBackend{Config{
		Devices: []DeviceConfig{{ID: "cam-1", XAddr: cam.xaddr(), User: "admin", Pass: "secret"}},
		Timeout: 2 * time.Second}}.Probe(context.Background())
	if !av.Available {
		t.Fatalf("probe must fall back when the clock call is refused: %s", av.Detail)
	}
}

func TestClockSkewIsLearned(t *testing.T) {
	cam := newFakeCam(t)
	a, _ := startAdapter(t, cam)
	d, _ := a.lookup("cam-1")
	// The fake reports a clock 5 minutes ahead; tokens must follow it or the
	// camera rejects every authenticated call as out of window.
	if skew := d.cli.skew(); skew < 4*time.Minute || skew > 6*time.Minute {
		t.Fatalf("clock skew should track the camera, got %v", skew)
	}
}

func TestConfiguredProfileTokenIsHonoured(t *testing.T) {
	cam := newFakeCam(t)
	cfg := Config{Devices: []DeviceConfig{{
		ID: "cam-1", XAddr: cam.xaddr(), User: "u", Pass: "p", ProfileToken: "SubStream",
	}}, Timeout: 2 * time.Second}
	a := New(cfg)
	sink := newSink()
	if err := a.Start(context.Background(), nil, sink); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	d, _ := a.lookup("cam-1")
	if d.profTok != "SubStream" {
		t.Fatalf("configured profile token must be used, got %q", d.profTok)
	}

	// A token the camera does not offer must fail loudly. Falling back to
	// the first profile would silently stream the wrong lens.
	cfg.Devices[0].ProfileToken = "NoSuchStream"
	a2 := New(cfg)
	sink2 := newSink()
	_ = a2.Start(context.Background(), nil, sink2)
	defer a2.Stop(context.Background())
	if d, ok := sink2.get("cam-1"); !ok || d.Online {
		t.Fatal("an unknown profile token must leave the camera offline, not silently substituted")
	}
}

func TestStreamURIWithoutProbeIsUnverified(t *testing.T) {
	cam := newFakeCam(t)
	a, _ := startAdapter(t, cam)

	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapStreamRTSP})
	if err != nil {
		t.Fatal(err)
	}
	// The camera returned a URI; nobody checked whether a stream is there.
	if eff.Status != effect.Unverified {
		t.Fatalf("an unprobed stream uri is a claim, not a verified effect: %+v", eff)
	}
	if eff.Record == nil || len(eff.Record.Artifacts) != 1 {
		t.Fatalf("the uri must still be delivered: %+v", eff.Record)
	}
	art := eff.Record.Artifacts[0]
	if art.MediaType != profile.MediaTypeRTSP || !strings.HasPrefix(art.Path, "rtsp://") {
		t.Fatalf("stream artifact wrong: %+v", art)
	}
	if art.ContentCID != "" {
		t.Fatal("a stream reference has no content to address — ANetLink never carries the video")
	}
}

func TestSnapshotIsContentAddressed(t *testing.T) {
	cam := newFakeCam(t)
	a, _ := startAdapter(t, cam)

	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapStreamSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("real bytes from the camera are verification: %+v", eff)
	}
	art := eff.Record.Artifacts[0]
	if art.ContentCID == "" || art.SizeBytes != uint64(len(cam.snapshot)) {
		t.Fatalf("snapshot must be content-addressed with a true size: %+v", art)
	}
	if art.MediaType != "image/jpeg" {
		t.Fatalf("media type must come from the camera: %q", art.MediaType)
	}
	if eff.Evidence == nil || eff.Evidence.AuthTrust != uint8(profile.A1) {
		t.Fatalf("evidence must record the auth axis: %+v", eff.Evidence)
	}

	// The same frame twice must carry the same id.
	eff2, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapStreamSnapshot})
	if eff2.Record.Artifacts[0].ContentCID != art.ContentCID {
		t.Fatal("identical frames must be identically addressed")
	}
}

func TestEmptyFrameIsNotEvidence(t *testing.T) {
	cam := newFakeCam(t)
	cam.mu.Lock()
	cam.snapshot = nil
	cam.mu.Unlock()
	a, _ := startAdapter(t, cam)

	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapStreamSnapshot})
	if eff.Status != effect.Failed {
		t.Fatalf("an empty frame must fail, never pass as a picture: %+v", eff)
	}
	if eff.Record != nil {
		t.Fatal("nothing was measured; no record may be attached")
	}
}

func TestPTZMoveVerifiedByPositionReadback(t *testing.T) {
	cam := newFakeCam(t, withPTZ)
	a, _ := startAdapter(t, cam)

	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "cam-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 0.5, "duration_ms": 50.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("a position that actually changed is verification: %+v", eff)
	}
	if eff.Evidence.VerifyTrust != uint8(profile.V2) {
		t.Fatalf("verified movement is V2: %+v", eff.Evidence)
	}
	if eff.Record.Metrics["pan"] == 0 {
		t.Fatalf("the new position must be reported: %+v", eff.Record.Metrics)
	}
	// The move must have been stopped — a continuous move left running is a
	// camera that never stops turning.
	cam.mu.Lock()
	moving := cam.moving
	cam.mu.Unlock()
	if moving {
		t.Fatal("a bounded move must issue Stop")
	}
}

// TestPTZMoveAtLimitIsNotSuccess: a camera already against its stop accepts
// the command and reports OK while nothing moves. Trusting the ack would
// report a pan that never happened.
func TestPTZMoveAtLimitIsNotSuccess(t *testing.T) {
	cam := newFakeCam(t, withStubborn)
	a, _ := startAdapter(t, cam)

	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "cam-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 1.0, "duration_ms": 50.0},
	})
	if eff.Status == effect.OK {
		t.Fatalf("unchanged position must not be reported as a successful move: %+v", eff)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "unchanged") {
		t.Fatalf("the reason must say the position never moved: %+v", eff.Evidence)
	}
}

func TestPTZVelocityOutOfRangeRefused(t *testing.T) {
	cam := newFakeCam(t, withPTZ)
	a, _ := startAdapter(t, cam)
	for _, v := range []float64{5, -3} {
		eff, _ := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "cam-1", Capability: profile.CapPTZMove, Args: map[string]any{"pan": v}})
		if eff.Status != effect.Failed {
			t.Fatalf("velocity %g is out of the ONVIF range and must be refused, not clamped: %+v", v, eff)
		}
	}
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "cam-1", Capability: profile.CapPTZMove, Args: map[string]any{}})
	if eff.Status != effect.Failed {
		t.Fatal("a move with no axis must be refused")
	}
}

func TestPTZStopThatDidNotStopIsFailed(t *testing.T) {
	cam := newFakeCam(t, withStubborn) // ignores Stop
	a, _ := startAdapter(t, cam)
	cam.mu.Lock()
	cam.moving = true
	cam.mu.Unlock()

	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapPTZStop})
	if eff.Status != effect.Failed {
		t.Fatalf("a camera still moving after Stop is a failure, not a success: %+v", eff)
	}
}

func TestPTZPresetReadsBackPosition(t *testing.T) {
	cam := newFakeCam(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "cam-1", Capability: profile.CapPTZPreset, Args: map[string]any{"preset": "3"}})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["pan"] != 0.9 {
		t.Fatalf("preset must be confirmed by reading the position: %+v", eff)
	}

	eff, _ = a.Invoke(context.Background(), sdk.Call{
		DeviceID: "cam-1", Capability: profile.CapPTZPreset, Args: map[string]any{}})
	if eff.Status != effect.Failed {
		t.Fatal("a preset recall with no preset must fail")
	}
}

func TestAuthRejectionIsDistinctFromUnreachable(t *testing.T) {
	cam := newFakeCam(t, withPTZ)
	cam.mu.Lock()
	cam.authFault = true
	cam.mu.Unlock()

	cfg := Config{Devices: []DeviceConfig{{ID: "cam-1", XAddr: cam.xaddr(), User: "admin", Pass: "wrong"}},
		Timeout: 2 * time.Second}
	a := New(cfg)
	sink := newSink()
	_ = a.Start(context.Background(), nil, sink)
	defer a.Stop(context.Background())

	// The camera answered — it just refused. It must appear offline but the
	// operator needs the real reason, not "unreachable".
	d, ok := sink.get("cam-1")
	if !ok || d.Online {
		t.Fatalf("a camera refusing credentials is not usable: %+v", d)
	}
}

func TestUnknownDeviceAndCapabilityFail(t *testing.T) {
	cam := newFakeCam(t)
	a, _ := startAdapter(t, cam)

	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "ghost", Capability: profile.CapStreamRTSP})
	if eff.Status != effect.Failed {
		t.Fatalf("unknown device must FAIL: %+v", eff)
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapPTZMove})
	if eff.Status != effect.Failed {
		t.Fatalf("a capability the camera never declared must FAIL: %+v", eff)
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: "sensor.temperature"})
	if eff.Status != effect.Failed {
		t.Fatalf("an unrelated capability must FAIL: %+v", eff)
	}
}

func TestEventsReachTheSink(t *testing.T) {
	cam := newFakeCam(t, withEvents)
	cam.mu.Lock()
	cam.events = []string{"tns1:VideoSource/MotionAlarm"}
	cam.mu.Unlock()

	_, sink := startAdapter(t, cam)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && sink.stateCount() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	st := sink.lastState()
	if st == nil {
		t.Fatal("a motion event must reach the sink")
	}
	if st["event_motion"] != 1 {
		t.Fatalf("motion state wrong: %+v", st)
	}
}

func TestUnknownTopicsAreIgnoredNotGuessed(t *testing.T) {
	for _, topic := range []string{
		"tns1:VideoSource/ImageTooDark", "tns1:Device/HardwareFailure/StorageFailure",
	} {
		if id, ok := topicCapability(topic); ok {
			t.Fatalf("topic %q must not be mapped, got %s", topic, id)
		}
	}
	for topic, want := range map[string]string{
		"tns1:VideoSource/MotionAlarm":              profile.CapEventMotion,
		"tns1:RuleEngine/CellMotionDetector/Motion": profile.CapEventMotion,
		"tns1:RuleEngine/TamperDetector/Tamper":     profile.CapEventTamper,
		"tns1:RuleEngine/LineDetector/Crossed":      profile.CapEventLineCross,
	} {
		got, ok := topicCapability(topic)
		if !ok || got != want {
			t.Fatalf("topic %q → %q (ok=%v), want %q", topic, got, ok, want)
		}
	}
}

func TestStopEndsTheEventPump(t *testing.T) {
	cam := newFakeCam(t, withEvents)
	cfg := Config{Devices: []DeviceConfig{{
		ID: "cam-1", XAddr: cam.xaddr(), User: "u", Pass: "p", Events: true}}, Timeout: time.Second}
	a := New(cfg)
	if err := a.Start(context.Background(), nil, newSink()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Stop(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		// A long poll that Stop cannot cut leaves the daemon unable to shut
		// down — the exact goroutine-leak shape the audit looks for.
		t.Fatal("Stop must cut an in-flight long poll, not wait it out")
	}
}

func TestProbeReportsUnreachableCamera(t *testing.T) {
	av := nativeBackend{Config{Devices: []DeviceConfig{{ID: "x", XAddr: "http://127.0.0.1:1/onvif"}},
		Timeout: 500 * time.Millisecond}}.Probe(context.Background())
	if av.Available {
		t.Fatal("an unreachable camera must not report available")
	}
	if !strings.Contains(av.Detail, "unreachable") {
		t.Fatalf("probe must say why: %q", av.Detail)
	}

	// With nothing configured the adapter is still usable — that is what
	// discovery is for — and the probe should say so rather than fail.
	av = nativeBackend{Config{}}.Probe(context.Background())
	if !av.Available || !strings.Contains(av.Detail, "scan") {
		t.Fatalf("an unconfigured onvif adapter should point at discovery: %+v", av)
	}
}

// --- RTSP verification against a real socket ---

// fakeRTSP answers DESCRIBE on a real TCP socket, demanding digest first so
// both halves of the probe meet a real wire.
type fakeRTSP struct {
	ln        net.Listener
	sdp       string
	rejectAll bool
}

func newFakeRTSP(t *testing.T, sdp string, rejectAll bool) *fakeRTSP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRTSP{ln: ln, sdp: sdp, rejectAll: rejectAll}
	go r.serve()
	t.Cleanup(func() { ln.Close() })
	return r
}

func (r *fakeRTSP) port() int { return r.ln.Addr().(*net.TCPAddr).Port }

func (r *fakeRTSP) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				req := string(buf[:n])
				cseq := "1"
				for _, line := range strings.Split(req, "\r\n") {
					if strings.HasPrefix(strings.ToLower(line), "cseq:") {
						cseq = strings.TrimSpace(line[len("cseq:"):])
					}
				}
				switch {
				case r.rejectAll:
					fmt.Fprintf(c, "RTSP/1.0 404 Not Found\r\nCSeq: %s\r\n\r\n", cseq)
				case !strings.Contains(req, "Authorization:"):
					fmt.Fprintf(c, "RTSP/1.0 401 Unauthorized\r\nCSeq: %s\r\n"+
						"WWW-Authenticate: Digest realm=\"cam\", nonce=\"rtspnonce\", qop=\"auth\"\r\n\r\n", cseq)
				default:
					fmt.Fprintf(c, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nContent-Type: application/sdp\r\n"+
						"Content-Length: %d\r\n\r\n%s", cseq, len(r.sdp), r.sdp)
				}
			}
		}(conn)
	}
}

const testSDP = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=Stream\r\n" +
	"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"

func TestStreamVerifiedByRealRTSPDescribe(t *testing.T) {
	rtsp := newFakeRTSP(t, testSDP, false)
	cam := newFakeCam(t)
	cam.mu.Lock()
	cam.streamURI = fmt.Sprintf("rtsp://127.0.0.1:%d/Streaming/Channels/101", rtsp.port())
	cam.mu.Unlock()

	a, _ := startAdapter(t, cam, func(c *Config) { c.VerifyStream = true })

	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapStreamRTSP})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("a DESCRIBE that returned SDP is verification: %+v", eff)
	}
	if eff.Evidence.VerifyTrust != uint8(profile.V2) {
		t.Fatalf("a probed stream is V2: %+v", eff.Evidence)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "H264") {
		t.Fatalf("the codec the camera actually offered must be reported: %q", eff.Evidence.ObservedState)
	}
}

// TestAdvertisedButDeadStreamIsUnavailable is the whole reason the probe
// exists: the camera hands out a perfectly good URI for a stream that is not
// there, and without probing we would report that as success.
func TestAdvertisedButDeadStreamIsUnavailable(t *testing.T) {
	rtsp := newFakeRTSP(t, "", true)
	cam := newFakeCam(t)
	cam.mu.Lock()
	cam.streamURI = fmt.Sprintf("rtsp://127.0.0.1:%d/dead", rtsp.port())
	cam.mu.Unlock()

	a, _ := startAdapter(t, cam, func(c *Config) { c.VerifyStream = true })
	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapStreamRTSP})
	if eff.Status != effect.Unavailable {
		t.Fatalf("a URI whose stream does not answer must be UNAVAILABLE: %+v", eff)
	}
	if eff.Record == nil || len(eff.Record.Artifacts) == 0 {
		t.Fatal("the uri should still be handed back so the operator can investigate")
	}
}

func TestUnreachableStreamProbeIsHonest(t *testing.T) {
	cam := newFakeCam(t)
	cam.mu.Lock()
	cam.streamURI = "rtsp://127.0.0.1:1/nothing"
	cam.mu.Unlock()
	a, _ := startAdapter(t, cam, func(c *Config) {
		c.VerifyStream = true
		c.Timeout = 500 * time.Millisecond
	})
	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "cam-1", Capability: profile.CapStreamRTSP})
	if eff.Status != effect.Unavailable {
		t.Fatalf("an unreachable media plane must be UNAVAILABLE: %+v", eff)
	}
}

// --- WS-Discovery ---

func TestProbeMatchBecomesUsableConfig(t *testing.T) {
	m := probeMatch{
		Address: "urn:uuid:abcd",
		XAddrs:  "http://192.168.1.64:80/onvif/device_service http://[fe80::1]/onvif/device_service",
		Scopes: "onvif://www.onvif.org/type/video_encoder " +
			"onvif://www.onvif.org/name/Front%20Door onvif://www.onvif.org/hardware/DS-2CD2143G0",
	}
	f, ok := foundFromMatch(m)
	if !ok {
		t.Fatal("a match with an http XAddr must be usable")
	}
	if f.SuggestedID != "cam-192-168-1-64" {
		t.Fatalf("suggested id: %q", f.SuggestedID)
	}
	if f.Label != "Front Door" {
		t.Fatalf("scope name must be unescaped: %q", f.Label)
	}
	// The hint must carry the exact xaddr the camera published — guessing
	// the path from the host is what breaks on non-standard cameras.
	if !strings.Contains(f.ConfigHint, `"xaddr":"http://192.168.1.64:80/onvif/device_service"`) {
		t.Fatalf("config hint must carry the published xaddr: %s", f.ConfigHint)
	}

	if _, ok := foundFromMatch(probeMatch{XAddrs: ""}); ok {
		t.Fatal("a match with no address is not actionable")
	}
}

func TestDiscoverProbesWithoutCrashingOnHostsWithoutMulticast(t *testing.T) {
	a := New(Config{DiscoveryInterface: "definitely-not-an-interface"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := a.DiscoverDevices(ctx)
	if err == nil {
		t.Fatal("naming an interface that does not exist must be an error, not an empty result")
	}
}

func TestScopeParsing(t *testing.T) {
	name, hw := parseScopes("onvif://www.onvif.org/name/Cam%201 onvif://www.onvif.org/hardware/X5")
	if name != "Cam 1" || hw != "X5" {
		t.Fatalf("scopes: name=%q hardware=%q", name, hw)
	}
	if n, h := parseScopes(""); n != "" || h != "" {
		t.Fatalf("empty scopes must yield empty fields, got %q/%q", n, h)
	}
}

func TestSOAPFaultClassification(t *testing.T) {
	authErr := &soapFault{Code: "ter:NotAuthorized", Reason: "Sender not authorized"}
	if !IsAuthFault(authErr) {
		t.Fatal("NotAuthorized must be recognised as an auth fault")
	}
	other := &soapFault{Code: "ter:ActionNotSupported", Reason: "no such operation"}
	if IsAuthFault(other) {
		t.Fatal("an unsupported action is not an auth problem")
	}
	if IsAuthFault(fmt.Errorf("connection refused")) {
		t.Fatal("a transport error is not an auth fault")
	}
	if !strings.Contains(authErr.Error(), "Sender not authorized") {
		t.Fatalf("the camera's own reason must survive: %s", authErr.Error())
	}
}
