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
	// hasIRCut gates the image pipeline; ircut is day|night|auto.
	hasIRCut bool
	ircut    string
	// presets is the store; presetCap caps it so the silent "store full"
	// path can be exercised.
	presets   []fakePreset
	presetCap int
	rebooted  bool
	// absolute records the AbsoluteHigh bodies received.
	absolute []string
}

type fakePreset struct {
	id   int
	name string
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

	case strings.HasSuffix(p, "/ircutFilter"):
		if !f.hasIRCut {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPut {
			buf := make([]byte, r.ContentLength)
			r.Body.Read(buf)
			body := string(buf)
			for _, m := range []string{"day", "night", "auto"} {
				if strings.Contains(body, ">"+m+"<") {
					f.ircut = m
				}
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		fmt.Fprintf(w, `<?xml version="1.0"?><IrcutFilter><IrcutFilterType>%s</IrcutFilterType></IrcutFilter>`, f.ircut)

	case strings.HasSuffix(p, "/absolute"):
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		body := string(buf)
		f.absolute = append(f.absolute, body)
		if !f.stubborn {
			var el, az, zm int
			fmt.Sscanf(body[strings.Index(body, "<elevation>"):],
				"<elevation>%d</elevation><azimuth>%d</azimuth><absoluteZoom>%d</absoluteZoom>", &el, &az, &zm)
			f.elev, f.azimuth, f.zoom = el, az, zm
		}
		w.WriteHeader(http.StatusOK)

	case strings.HasSuffix(p, "/homeposition/goto"):
		f.azimuth, f.elev = 0, 0
		w.WriteHeader(http.StatusOK)

	case strings.HasSuffix(p, "/presets"):
		b := `<?xml version="1.0"?><PTZPresetList>`
		for _, pr := range f.presets {
			b += fmt.Sprintf(`<PTZPreset><id>%d</id><presetName>%s</presetName></PTZPreset>`, pr.id, pr.name)
		}
		fmt.Fprint(w, b+`</PTZPresetList>`)

	// Recall must be matched before the generic preset PUT below, or a
	// /presets/3/goto is parsed as a preset write with an empty body.
	case strings.Contains(p, "/presets/") && strings.HasSuffix(p, "/goto"):
		f.azimuth, f.elev, f.zoom = 1800, 450, 40
		w.WriteHeader(http.StatusOK)

	case strings.Contains(p, "/presets/") && r.Method == http.MethodPut:
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		body := string(buf)
		var id int
		name := ""
		idAt := strings.Index(body, "<id>")
		if idAt < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Sscanf(body[idAt:], "<id>%d</id>", &id)
		if i := strings.Index(body, "<presetName>"); i >= 0 {
			rest := body[i+len("<presetName>"):]
			if j := strings.Index(rest, "<"); j > 0 {
				name = rest[:j]
			}
		}
		// A full store accepts the write and keeps nothing.
		if f.presetCap == 0 || len(f.presets) < f.presetCap {
			f.presets = append(f.presets, fakePreset{id: id, name: name})
		}
		w.WriteHeader(http.StatusOK)

	case p == "/ISAPI/System/reboot":
		f.rebooted = true
		w.WriteHeader(http.StatusOK)

	case strings.HasSuffix(p, "/status"):
		fmt.Fprintf(w, `<?xml version="1.0"?><PTZStatus><AbsoluteHigh>`+
			`<elevation>%d</elevation><azimuth>%d</azimuth><absoluteZoom>%d</absoluteZoom>`+
			`</AbsoluteHigh></PTZStatus>`, f.elev, f.azimuth, f.zoom)

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
func withIRCut(f *fakeISAPI)    { f.hasIRCut = true; f.ircut = "auto" }
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

// --- extended control surface ---

// TestAbsoluteMapsToISAPICoordinates: our contract is [-1,1] on all three
// axes; ISAPI wants azimuth in tenths of a degree (0..3600) and elevation
// -900..900. The caller must never see that.
func TestAbsoluteMapsToISAPICoordinates(t *testing.T) {
	for _, tc := range []struct {
		pan, tilt float64
		wantAz    int
		wantElev  int
	}{
		{-1, 0, 0, 0},   // -1 is the origin of the circle
		{0, 0, 1800, 0}, // straight round the back
		{0.5, 0.5, 2700, 450},
		{0, -1, 1800, -900}, // full down
	} {
		cam := newFakeISAPI(t, withPTZ)
		a, _ := startAdapter(t, cam)
		if _, err := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "hik-1", Capability: profile.CapPTZAbsolute,
			Args: map[string]any{"pan": tc.pan, "tilt": tc.tilt, "zoom": 0.1}}); err != nil {
			t.Fatal(err)
		}
		cam.mu.Lock()
		got := append([]string(nil), cam.absolute...)
		cam.mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("pan=%g tilt=%g: expected one absolute write, got %v", tc.pan, tc.tilt, got)
		}
		wantAz := fmt.Sprintf("<azimuth>%d</azimuth>", tc.wantAz)
		wantEl := fmt.Sprintf("<elevation>%d</elevation>", tc.wantElev)
		if !strings.Contains(got[0], wantAz) || !strings.Contains(got[0], wantEl) {
			t.Fatalf("pan=%g tilt=%g: want %s and %s, got %s", tc.pan, tc.tilt, wantAz, wantEl, got[0])
		}
	}
}

func TestAbsoluteRoundTripsThroughTheContract(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZAbsolute,
		Args: map[string]any{"pan": 0.5, "tilt": 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("arriving verifies: %+v", eff)
	}
	// Both the vendor units and the normalised contract values are reported.
	if eff.Record.Metrics["azimuth"] != 2700 {
		t.Fatalf("native azimuth: %+v", eff.Record.Metrics)
	}
	if p := eff.Record.Metrics["pan"]; p < 0.49 || p > 0.51 {
		t.Fatalf("the normalised pan must round-trip, got %g", p)
	}
}

func TestAbsoluteThatDidNotArriveIsUnverified(t *testing.T) {
	cam := newFakeISAPI(t, withStubborn)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZAbsolute,
		Args: map[string]any{"pan": 0.9, "tilt": 0.9}})
	if eff.Status == effect.OK {
		t.Fatalf("a head that never arrived is not a success: %+v", eff)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "did not arrive") {
		t.Fatalf("the reason must say so: %q", eff.Evidence.ObservedState)
	}
}

func TestAbsoluteHoldsUnspecifiedAxes(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)
	cam.mu.Lock()
	cam.azimuth, cam.elev = 1234, 321
	cam.mu.Unlock()

	if _, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZAbsolute,
		Args: map[string]any{"zoom": 0.5}}); err != nil {
		t.Fatal(err)
	}
	cam.mu.Lock()
	az, el := cam.azimuth, cam.elev
	cam.mu.Unlock()
	if az != 1234 || el != 321 {
		t.Fatalf("a zoom-only request re-aimed the camera to az=%d el=%d", az, el)
	}
}

// TestIRCutNamingIsVendorSpecific: ISAPI names the *mode* (day/night) while
// ONVIF names the *filter* (ON/OFF). Same physical state, opposite words —
// mapping them identically would flip every Hikvision camera.
func TestIRCutNamingIsVendorSpecific(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"on", "day"}, {"day", "day"},
		{"off", "night"}, {"night", "night"},
		{"auto", "auto"},
	} {
		got, ok := ircutMode(tc.in)
		if !ok || got != tc.want {
			t.Fatalf("ircut %q → %q (ok=%v), want %q", tc.in, got, ok, tc.want)
		}
	}
	if _, ok := ircutMode("dusk"); ok {
		t.Fatal("an unknown mode must be refused, not guessed")
	}

	cam := newFakeISAPI(t, withIRCut)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapImagingIRCut,
		Args: map[string]any{"mode": "night"}})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("the filter position is readable, so this verifies: %+v", eff)
	}
	cam.mu.Lock()
	got := cam.ircut
	cam.mu.Unlock()
	if got != "night" {
		t.Fatalf("ISAPI expects the mode name, camera got %q", got)
	}
}

func TestIRCutIsDiscoveredNotAssumed(t *testing.T) {
	plain := newFakeISAPI(t)
	_, sink := startAdapter(t, plain)
	d, _ := sink.get("hik-1")
	if _, ok := d.Profile.Find(profile.CapImagingIRCut); ok {
		t.Fatal("a camera with no ircutFilter endpoint must not claim imaging.ircut")
	}
}

func TestPresetListCarriesNames(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	cam.mu.Lock()
	cam.presets = []fakePreset{{1, "front door"}, {2, "loading bay"}}
	cam.mu.Unlock()
	a, _ := startAdapter(t, cam)

	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZPresetList})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["preset_count"] != 2 {
		t.Fatalf("preset list: %+v", eff)
	}
	if len(eff.Record.Artifacts) != 1 || eff.Record.Artifacts[0].MediaType != "application/json" {
		t.Fatalf("names must ride as a JSON artifact: %+v", eff.Record)
	}
}

func TestPresetSaveVerifiedByReadback(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZPresetSave,
		Args: map[string]any{"preset": 4.0, "name": "loading bay"}})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("a preset that shows up in the list verifies: %+v", eff)
	}
	for _, bad := range []map[string]any{{}, {"preset": 0.0}, {"preset": "abc"}} {
		eff, _ := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "hik-1", Capability: profile.CapPTZPresetSave, Args: bad})
		if eff.Status != effect.Failed {
			t.Fatalf("args %v must be refused: %+v", bad, eff)
		}
	}
}

func TestPresetSaveIntoAFullStoreFails(t *testing.T) {
	cam := newFakeISAPI(t, withPTZ)
	cam.mu.Lock()
	cam.presetCap = 1
	cam.presets = []fakePreset{{1, "taken"}}
	cam.mu.Unlock()
	a, _ := startAdapter(t, cam)

	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapPTZPresetSave,
		Args: map[string]any{"preset": 9.0, "name": "nowhere"}})
	if eff.Status != effect.Failed {
		t.Fatalf("a preset the camera did not keep must fail: %+v", eff)
	}
}

func TestRebootCannotBeVerified(t *testing.T) {
	cam := newFakeISAPI(t)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapSystemReboot})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unverified {
		t.Fatalf("a reboot cannot be verified from here: %+v", eff)
	}
	cam.mu.Lock()
	rebooted := cam.rebooted
	cam.mu.Unlock()
	if !rebooted {
		t.Fatal("the reboot must actually have been sent")
	}
}

func TestSystemInfoIsReported(t *testing.T) {
	cam := newFakeISAPI(t)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "hik-1", Capability: profile.CapSystemInfo})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || len(eff.Record.Artifacts) != 1 {
		t.Fatalf("system.info: %+v", eff)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "DS-2CD2143G0-I") {
		t.Fatalf("evidence should name the model: %q", eff.Evidence.ObservedState)
	}
}

func TestPresetNameIsEscaped(t *testing.T) {
	if got := xmlEscape("R&D"); !strings.Contains(got, "&amp;") {
		t.Fatalf("ampersands in operator-supplied names must be escaped: %q", got)
	}
}

// A camera that is down or has moved must not take the adapter — and with
// it the daemon — out of service while other cameras are answering. The
// deployed testbed daemon sat in a restart loop thousands deep because the
// first camera in its config had moved to a different port.
func TestProbeSurvivesADeadCameraAheadOfALiveOne(t *testing.T) {
	cam := newFakeISAPI(t)

	av := nativeBackend{Config{
		Timeout: 2 * time.Second,
		Devices: []DeviceConfig{
			// Port 1 is reserved and refuses immediately.
			{ID: "dead", Address: "127.0.0.1:1", User: "admin", Pass: "pw"},
			{ID: "live", Address: cam.host(), User: "admin", Pass: "pw"},
		},
	}}.Probe(context.Background())

	if !av.Available {
		t.Fatalf("a live camera behind a dead one must keep the adapter available: %s", av.Detail)
	}
	if !strings.Contains(av.Detail, "live") {
		t.Errorf("the detail should name the camera that answered: %q", av.Detail)
	}
	if !strings.Contains(av.Detail, "did not answer") {
		t.Errorf("the detail should report the ones that did not: %q", av.Detail)
	}
}
