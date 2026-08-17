package hikvision

import (
	"context"
	"fmt"
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
func (m *memSink) lastState() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.states) == 0 {
		return nil
	}
	return m.states[len(m.states)-1]
}

// fakeISAPI speaks the real ISAPI wire: digest auth, the vendor's XML, and
// the multipart alertStream.
type fakeISAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	hasPTZ   bool
	azimuth  int
	elev     int
	zoom     int
	stubborn bool // accepts moves but never actually moves
	// moves records the PTZData bodies received, so tests can assert the
	// velocity conversion rather than just that something was sent.
	moves     []string
	snapshot  []byte
	rejectAll bool
	alerts    []string // eventTypes to emit on the alert stream
}

func newFakeISAPI(t *testing.T, opts ...func(*fakeISAPI)) *fakeISAPI {
	t.Helper()
	f := &fakeISAPI{snapshot: []byte("\xff\xd8\xff\xe0hik-frame"), azimuth: 100, elev: 20, zoom: 10}
	for _, o := range opts {
		o(f)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeISAPI) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeISAPI) handle(w http.ResponseWriter, r *http.Request) {
	// Every ISAPI endpoint demands digest; answering without it is how a
	// camera tells you your password is wrong.
	if r.Header.Get("Authorization") == "" {
		w.Header().Set("WWW-Authenticate", `Digest realm="ISAPI", qop="auth", nonce="hiknonce"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// The alert stream is long-lived; holding the fake's lock for its whole
	// lifetime would deadlock every other request against it.
	if r.URL.Path == alertStreamPath {
		f.mu.Lock()
		alerts := append([]string(nil), f.alerts...)
		f.mu.Unlock()
		f.serveAlerts(w, alerts)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.rejectAll {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	p := r.URL.Path
	switch {
	case p == "/ISAPI/System/deviceInfo":
		fmt.Fprint(w, `<?xml version="1.0"?><DeviceInfo><deviceName>Front</deviceName>`+
			`<model>DS-2CD2143G0-I</model><serialNumber>SN123</serialNumber>`+
			`<firmwareVersion>V5.6.3</firmwareVersion></DeviceInfo>`)

	case strings.HasSuffix(p, "/capabilities"):
		if !f.hasPTZ {
			// A fixed camera answers 403 here — this is how PTZ is detected.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `<?xml version="1.0"?><PTZChanelCap></PTZChanelCap>`)

	case strings.HasSuffix(p, "/picture"):
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(f.snapshot)

	case strings.HasSuffix(p, "/continuous"):
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		body := string(buf)
		f.moves = append(f.moves, body)
		if !f.stubborn && !strings.Contains(body, "<pan>0</pan><tilt>0</tilt><zoom>0</zoom>") {
			f.azimuth += 10
			f.elev += 5
		}
		w.WriteHeader(http.StatusOK)

	case strings.HasSuffix(p, "/status"):
		fmt.Fprintf(w, `<?xml version="1.0"?><PTZStatus><AbsoluteHigh>`+
			`<elevation>%d</elevation><azimuth>%d</azimuth><absoluteZoom>%d</absoluteZoom>`+
			`</AbsoluteHigh></PTZStatus>`, f.elev, f.azimuth, f.zoom)

	case strings.Contains(p, "/presets/") && strings.HasSuffix(p, "/goto"):
		f.azimuth, f.elev, f.zoom = 1800, 450, 40
		w.WriteHeader(http.StatusOK)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveAlerts writes a real multipart stream, the shape the camera pushes.
//
// A part is terminated by the delimiter that follows it, not by its own
// Content-Length — on a real camera the periodic videoloss heartbeat is what
// closes the previous alert. The closing delimiter here plays that role.
func (f *fakeISAPI) serveAlerts(w http.ResponseWriter, alerts []string) {
	const boundary = "boundary"
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+boundary)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, et := range alerts {
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><EventNotificationAlert>`+
			`<channelID>1</channelID><eventType>%s</eventType><eventState>active</eventState>`+
			`<eventDescription>test</eventDescription></EventNotificationAlert>`, et)
		fmt.Fprintf(w, "--%s\r\nContent-Type: application/xml\r\nContent-Length: %d\r\n\r\n%s\r\n",
			boundary, len(body), body)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprintf(w, "--%s--\r\n", boundary)
	if flusher != nil {
		flusher.Flush()
	}
}

func withPTZ(f *fakeISAPI)      { f.hasPTZ = true }
func withStubborn(f *fakeISAPI) { f.hasPTZ = true; f.stubborn = true }

func startAdapter(t *testing.T, cam *fakeISAPI, mut ...func(*Config)) (*Adapter, *memSink) {
	t.Helper()
	cfg := Config{
		Devices: []DeviceConfig{{ID: "hik-1", Address: cam.host(), User: "admin", Pass: "pw"}},
		Timeout: 3 * time.Second,
	}
	for _, m := range mut {
		m(&cfg)
	}
	a := New(cfg)
	sink := newSink()
	if err := a.Start(context.Background(), nil, sink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })
	return a, sink
}

func TestIdentityComesFromTheCamera(t *testing.T) {
	cam := newFakeISAPI(t)
	_, sink := startAdapter(t, cam)
	d, ok := sink.get("hik-1")
	if !ok || !d.Online {
		t.Fatalf("camera should be online: %+v", d)
	}
	if d.Profile.Model != "DS-2CD2143G0-I" || d.Profile.Vendor != "Hikvision" {
		t.Fatalf("identity: %+v", d.Profile)
	}
	if d.Profile.Protocol != "isapi" {
		t.Fatalf("protocol must name the vendor API, got %q", d.Profile.Protocol)
	}
	if d.Profile.MaxTrust != profile.V2 || d.Profile.AuthTrust != profile.A1 {
		t.Fatalf("trust axes must be V2/A1: V%d/A%d", d.Profile.MaxTrust, d.Profile.AuthTrust)
	}
}

// TestPTZIsDiscoveredNotAssumed: a fixed camera must not advertise PTZ.
func TestPTZIsDiscoveredNotAssumed(t *testing.T) {
	fixed := newFakeISAPI(t)
	_, sink := startAdapter(t, fixed)
	d, _ := sink.get("hik-1")
	if _, ok := d.Profile.Find(profile.CapPTZMove); ok {
		t.Fatal("a fixed camera must not claim ptz.move")
	}

	ptz := newFakeISAPI(t, withPTZ)
	_, sink2 := startAdapter(t, ptz)
	d2, _ := sink2.get("hik-1")
	for _, id := range []string{profile.CapPTZMove, profile.CapPTZStop, profile.CapPTZPreset, profile.CapPTZPosition} {
		if _, ok := d2.Profile.Find(id); !ok {
			t.Fatalf("PTZ camera must expose %s", id)
		}
	}
}

func TestStreamURIFollowsTheChannelEncoding(t *testing.T) {
	cam := newFakeISAPI(t)
	cfg := Config{Devices: []DeviceConfig{{
		ID: "hik-1", Address: cam.host(), User: "a", Pass: "b", Channel: 2, Stream: 2, RTSPPort: 8554}},
		Timeout: 2 * time.Second}
	a := New(cfg)
	if err := a.Start(context.Background(), nil, newSink()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	d, _ := a.lookup("hik-1")
	// channel 2, sub stream → 202
	if !strings.HasSuffix(d.streamU, ":8554/Streaming/Channels/202") {
		t.Fatalf("ISAPI channel encoding wrong: %s", d.streamU)
	}
}

func TestSnapshotIsContentAddressed(t *testing.T) {
	cam := newFakeISAPI(t)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "hik-1", Capability: profile.CapStreamSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("real bytes are verification: %+v", eff)
	}
	art := eff.Record.Artifacts[0]
	if art.ContentCID == "" || art.SizeBytes != uint64(len(cam.snapshot)) {
		t.Fatalf("snapshot artifact: %+v", art)
	}
}

func TestUnprobedStreamIsUnverified(t *testing.T) {
	cam := newFakeISAPI(t)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "hik-1", Capability: profile.CapStreamRTSP})
	if eff.Status != effect.Unverified {
		t.Fatalf("a URI we constructed ourselves is the weakest possible claim: %+v", eff)
	}
	if len(eff.Record.Artifacts) != 1 || !strings.HasPrefix(eff.Record.Artifacts[0].Path, "rtsp://") {
		t.Fatalf("the uri must still be delivered: %+v", eff.Record)
	}
}

// TestVelocityIsConvertedToVendorScale: the ANetLink contract is [-1,1] and
// Hikvision's is [-100,100]. A caller must not have to know whose camera it
// is to ask for half speed.
func TestVelocityIsConvertedToVendorScale(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)

	_, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 0.5, "tilt": -0.25, "duration_ms": 50.0}})
	if err != nil {
		t.Fatal(err)
	}
	cam.mu.Lock()
	moves := append([]string(nil), cam.moves...)
	cam.mu.Unlock()
	if len(moves) < 2 {
		t.Fatalf("expected a move and a stop, got %d", len(moves))
	}
	if !strings.Contains(moves[0], "<pan>50</pan>") || !strings.Contains(moves[0], "<tilt>-25</tilt>") {
		t.Fatalf("velocity conversion wrong: %s", moves[0])
	}
	// The bounded move must always be followed by an explicit stop.
	if !strings.Contains(moves[len(moves)-1], "<pan>0</pan><tilt>0</tilt><zoom>0</zoom>") {
		t.Fatalf("a bounded move must end in a stop: %s", moves[len(moves)-1])
	}
}

func TestPTZMoveVerifiedByPositionChange(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 0.5, "duration_ms": 50.0}})
	if !eff.Verifiable() || eff.Evidence.VerifyTrust != uint8(profile.V2) {
		t.Fatalf("a position that changed is V2 verification: %+v", eff)
	}
}

func TestPTZMoveAtLimitIsNotSuccess(t *testing.T) {
	cam := newFakeISAPI(t, withStubborn)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 1.0, "duration_ms": 50.0}})
	if eff.Status == effect.OK {
		t.Fatalf("a camera that accepted the move but never moved is not a success: %+v", eff)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "unchanged") {
		t.Fatalf("the reason must say so: %+v", eff.Evidence)
	}
}

func TestVelocityOutOfRangeRefused(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)
	for _, v := range []float64{2, -1.5} {
		eff, _ := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "hik-1", Capability: profile.CapPTZMove, Args: map[string]any{"pan": v}})
		if eff.Status != effect.Failed {
			t.Fatalf("velocity %g must be refused, not clamped: %+v", v, eff)
		}
	}
}

// TestPresetTokenMustBeNumeric guards the URL path: the preset goes straight
// into it, and a non-numeric value is both a protocol error and something
// with no business being interpolated into a request line.
func TestPresetTokenMustBeNumeric(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)
	for _, bad := range []string{"../../System/reboot", "1;drop", "abc"} {
		eff, _ := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "hik-1", Capability: profile.CapPTZPreset,
			Args: map[string]any{"preset": bad}})
		if eff.Status != effect.Failed {
			t.Fatalf("preset %q must be refused: %+v", bad, eff)
		}
	}
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZPreset, Args: map[string]any{"preset": 3.0}})
	if !eff.Verifiable() || eff.Record.Metrics["azimuth"] != 1800 {
		t.Fatalf("a numeric preset must work and be read back: %+v", eff)
	}
}

func TestStopThatDidNotStopIsFailed(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)

	// Make the position keep changing between the two confirmation reads,
	// which is what a camera that ignored Stop looks like.
	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		for {
			select {
			case <-stopCh:
				return
			case <-time.After(50 * time.Millisecond):
				cam.mu.Lock()
				cam.azimuth += 7
				cam.mu.Unlock()
			}
		}
	}()

	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "hik-1", Capability: profile.CapPTZStop})
	if eff.Status != effect.Failed {
		t.Fatalf("a camera still moving after Stop must not report success: %+v", eff)
	}
}

func TestStopConfirmedBySteadyPosition(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "hik-1", Capability: profile.CapPTZStop})
	if !eff.Verifiable() {
		t.Fatalf("a position steady across two reads confirms the stop: %+v", eff)
	}
}

func TestCredentialRejectionIsDistinctFromUnreachable(t *testing.T) {
	cam := newFakeISAPI(t)
	cam.mu.Lock()
	cam.rejectAll = true
	cam.mu.Unlock()

	a := New(Config{Devices: []DeviceConfig{{ID: "hik-1", Address: cam.host(), User: "x", Pass: "y"}},
		Timeout: 2 * time.Second})
	sink := newSink()
	_ = a.Start(context.Background(), nil, sink)
	defer a.Stop(context.Background())
	d, ok := sink.get("hik-1")
	if !ok || d.Online {
		t.Fatalf("a camera refusing credentials is not usable: %+v", d)
	}

	av := nativeBackend{Config{Devices: []DeviceConfig{{ID: "x", Address: "127.0.0.1:1"}},
		Timeout: 300 * time.Millisecond}}.Probe(context.Background())
	if av.Available || !strings.Contains(av.Detail, "unreachable") {
		t.Fatalf("an unreachable camera must probe as such: %+v", av)
	}
}

func TestUnknownDeviceAndCapabilityFail(t *testing.T) {
	cam := newFakeISAPI(t)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "ghost", Capability: profile.CapStreamRTSP})
	if eff.Status != effect.Failed {
		t.Fatalf("unknown device must FAIL: %+v", eff)
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "hik-1", Capability: profile.CapPTZMove})
	if eff.Status != effect.Failed {
		t.Fatalf("PTZ on a fixed camera must FAIL: %+v", eff)
	}
}

func TestEventVocabularyMapping(t *testing.T) {
	for et, want := range map[string]string{
		"VMD":             profile.CapEventMotion,
		"motiondetection": profile.CapEventMotion,
		"shelteralarm":    profile.CapEventTamper,
		"linedetection":   profile.CapEventLineCross,
		"fielddetection":  profile.CapEventLineCross,
		"regionEntrance":  profile.CapEventLineCross,
	} {
		got, ok := eventCapability(et)
		if !ok || got != want {
			t.Fatalf("eventType %q → %q (ok=%v), want %q", et, got, ok, want)
		}
	}
	// Unknown types must be ignored rather than guessed into motion.
	for _, et := range []string{"videoloss", "diskfull", "facedetection", ""} {
		if id, ok := eventCapability(et); ok {
			t.Fatalf("eventType %q must not be mapped, got %s", et, id)
		}
	}
}

func TestAlertStreamReachesTheSink(t *testing.T) {
	cam := newFakeISAPI(t)
	cam.mu.Lock()
	cam.alerts = []string{"VMD"}
	cam.mu.Unlock()

	a := New(Config{Devices: []DeviceConfig{{
		ID: "hik-1", Address: cam.host(), User: "a", Pass: "b", Events: true}},
		Timeout: 3 * time.Second})
	sink := newSink()
	if err := a.Start(context.Background(), nil, sink); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if st := sink.lastState(); st != nil {
			if st["event_motion"] != 1 {
				t.Fatalf("motion state wrong: %+v", st)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a VMD alert must reach the sink")
}

// TestStopCutsTheAlertStream: the feed never ends by design, so Stop must
// cancel it rather than wait — otherwise the daemon cannot shut down.
func TestStopCutsTheAlertStream(t *testing.T) {
	cam := newFakeISAPI(t)
	a := New(Config{Devices: []DeviceConfig{{
		ID: "hik-1", Address: cam.host(), User: "a", Pass: "b", Events: true}},
		Timeout: 2 * time.Second})
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
		t.Fatal("Stop must cancel the alert stream, not wait for a feed that never ends")
	}
}
