package dahua

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

// fakeDahua speaks the real CGI wire: digest auth, key=value bodies, "OK"
// command acknowledgements and the attach event stream.
type fakeDahua struct {
	srv *httptest.Server

	mu       sync.Mutex
	hasPTZ   bool
	pan      float64
	tilt     float64
	zoom     float64
	stubborn bool
	// cmds records the PTZ CGI query strings received.
	cmds      []string
	snapshot  []byte
	refuseCmd bool
	events    []string // "Code=X;action=Start;index=0" lines
}

func newFakeDahua(t *testing.T, opts ...func(*fakeDahua)) *fakeDahua {
	t.Helper()
	f := &fakeDahua{snapshot: []byte("\xff\xd8\xff\xe0dahua-frame"), pan: 10, tilt: 5, zoom: 1}
	for _, o := range opts {
		o(f)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDahua) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeDahua) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		w.Header().Set("WWW-Authenticate", `Digest realm="Login to camera", qop="auth", nonce="dhnonce"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/cgi-bin/eventManager.cgi" {
		f.mu.Lock()
		lines := append([]string(nil), f.events...)
		f.mu.Unlock()
		f.serveEvents(w, lines)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	action := q.Get("action")

	switch r.URL.Path {
	case "/cgi-bin/magicBox.cgi":
		if action == "getDeviceType" {
			fmt.Fprint(w, "type=IPC-HFW4431R-Z\r\n")
			return
		}
		w.WriteHeader(http.StatusBadRequest)

	case "/cgi-bin/snapshot.cgi":
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(f.snapshot)

	case "/cgi-bin/ptz.cgi":
		switch action {
		case "getCurrentProtocolCaps":
			if !f.hasPTZ {
				// A fixed camera has no PTZ protocol to report.
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, "caps.Preset=true\r\ncaps.Tour=true\r\n")
		case "getStatus":
			// Note the vendor's misspelling: "Postion". Every firmware has it.
			fmt.Fprintf(w, "status.Postion[0]=%g\r\nstatus.Postion[1]=%g\r\nstatus.Postion[2]=%g\r\n"+
				"status.ActionState=Idle\r\n", f.pan, f.tilt, f.zoom)
		case "start", "stop":
			f.cmds = append(f.cmds, r.URL.RawQuery)
			if f.refuseCmd {
				fmt.Fprint(w, "Error")
				return
			}
			if action == "start" && !f.stubborn {
				switch q.Get("code") {
				case "GotoPreset":
					f.pan, f.tilt, f.zoom = 180, 45, 4
				default:
					f.pan += 1
					f.tilt += 1
				}
			}
			fmt.Fprint(w, "OK")
		default:
			w.WriteHeader(http.StatusBadRequest)
		}

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeDahua) serveEvents(w http.ResponseWriter, lines []string) {
	const boundary = "myboundary"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, line := range lines {
		fmt.Fprintf(w, "--%s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s\r\n",
			boundary, len(line), line)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprintf(w, "--%s--\r\n", boundary)
	if flusher != nil {
		flusher.Flush()
	}
}

func withPTZ(f *fakeDahua)      { f.hasPTZ = true }
func withStubborn(f *fakeDahua) { f.hasPTZ = true; f.stubborn = true }

func startAdapter(t *testing.T, cam *fakeDahua, mut ...func(*Config)) (*Adapter, *memSink) {
	t.Helper()
	cfg := Config{
		Devices: []DeviceConfig{{ID: "dh-1", Address: cam.host(), User: "admin", Pass: "pw"}},
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

func TestIdentityAndStreamURI(t *testing.T) {
	cam := newFakeDahua(t)
	a, sink := startAdapter(t, cam)
	d, ok := sink.get("dh-1")
	if !ok || !d.Online {
		t.Fatalf("camera should be online: %+v", d)
	}
	if d.Profile.Model != "IPC-HFW4431R-Z" || d.Profile.Vendor != "Dahua" {
		t.Fatalf("identity: %+v", d.Profile)
	}
	dev, _ := a.lookup("dh-1")
	if !strings.Contains(dev.streamU, "/cam/realmonitor?channel=1&subtype=0") {
		t.Fatalf("stream uri must follow the vendor scheme: %s", dev.streamU)
	}
}

func TestPTZIsDiscoveredNotAssumed(t *testing.T) {
	fixed := newFakeDahua(t)
	_, sink := startAdapter(t, fixed)
	d, _ := sink.get("dh-1")
	if _, ok := d.Profile.Find(profile.CapPTZMove); ok {
		t.Fatal("a fixed camera must not claim ptz.move")
	}

	ptz := newFakeDahua(t, withPTZ)
	_, sink2 := startAdapter(t, ptz)
	d2, _ := sink2.get("dh-1")
	if _, ok := d2.Profile.Find(profile.CapPTZMove); !ok {
		t.Fatal("a PTZ camera must expose ptz.move")
	}
}

// TestVelocityBecomesDirectionAndSpeed is the impedance mismatch this
// adapter exists to absorb: our contract is a continuous vector, Dahua's is
// a compass code plus a 1..8 speed.
func TestVelocityBecomesDirectionAndSpeed(t *testing.T) {
	for _, tc := range []struct {
		pan, tilt float64
		wantCode  string
		wantSpeed string
	}{
		{0.5, 0, "Right", "arg2=4"},
		{-0.5, 0, "Left", "arg2=4"},
		{0, 1.0, "Up", "arg2=8"},
		{0, -0.25, "Down", "arg2=2"},
		{0.5, 0.5, "RightUp", "arg2=4"},
		{-0.9, -0.9, "LeftDown", "arg2=7"},
	} {
		cam := newFakeDahua(t, withPTZ)
		a, _ := startAdapter(t, cam)
		if _, err := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "dh-1", Capability: profile.CapPTZMove,
			Args: map[string]any{"pan": tc.pan, "tilt": tc.tilt, "duration_ms": 30.0}}); err != nil {
			t.Fatal(err)
		}
		cam.mu.Lock()
		cmds := append([]string(nil), cam.cmds...)
		cam.mu.Unlock()
		if len(cmds) < 2 {
			t.Fatalf("pan=%g tilt=%g: expected start and stop, got %v", tc.pan, tc.tilt, cmds)
		}
		if !strings.Contains(cmds[0], "code="+tc.wantCode) {
			t.Fatalf("pan=%g tilt=%g: want code %s, got %s", tc.pan, tc.tilt, tc.wantCode, cmds[0])
		}
		if !strings.Contains(cmds[0], tc.wantSpeed) {
			t.Fatalf("pan=%g tilt=%g: want %s, got %s", tc.pan, tc.tilt, tc.wantSpeed, cmds[0])
		}
		if !strings.Contains(cmds[len(cmds)-1], "action=stop") {
			t.Fatalf("a bounded move must end in a stop: %s", cmds[len(cmds)-1])
		}
	}
}

// TestTinyVelocityStillMoves: rounding a deliberate small request down to
// speed 0 would be a silent no-op reported as success.
func TestTinyVelocityStillMoves(t *testing.T) {
	if got := speedOf(0.01); got < 1 {
		t.Fatalf("a non-zero request must map to at least speed 1, got %d", got)
	}
	if got := speedOf(1.0); got != maxSpeed {
		t.Fatalf("full speed: %d", got)
	}
	if got := speedOf(-0.5); got != 4 {
		t.Fatalf("speed uses magnitude: %d", got)
	}
}

// TestZoomIsASeparateAxisAndAlsoStopped: pan and zoom are different Dahua
// commands, and stopping only the first leaves the lens still zooming.
func TestZoomIsASeparateAxisAndAlsoStopped(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)
	if _, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 0.5, "zoom": 0.5, "duration_ms": 30.0}}); err != nil {
		t.Fatal(err)
	}
	cam.mu.Lock()
	cmds := append([]string(nil), cam.cmds...)
	cam.mu.Unlock()

	var startedZoom, stoppedZoom bool
	for _, c := range cmds {
		if strings.Contains(c, "code=ZoomTele") {
			if strings.Contains(c, "action=start") {
				startedZoom = true
			}
			if strings.Contains(c, "action=stop") {
				stoppedZoom = true
			}
		}
	}
	if !startedZoom {
		t.Fatalf("zoom must be issued as its own command: %v", cmds)
	}
	if !stoppedZoom {
		t.Fatalf("every started axis must be stopped, or the lens keeps zooming: %v", cmds)
	}
}

func TestPositionParsingHandlesTheVendorTypo(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{DeviceID: "dh-1", Capability: profile.CapPTZPosition})
	if err != nil {
		t.Fatal(err)
	}
	// "status.Postion" is misspelled in the firmware; matching the correct
	// spelling would silently read zeroes and report them as a position.
	if !eff.Verifiable() || eff.Record.Metrics["pan"] != 10 || eff.Record.Metrics["tilt"] != 5 {
		t.Fatalf("position must parse the key the camera actually sends: %+v", eff.Record)
	}
}

func TestMoveVerifiedByPositionChange(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 0.5, "duration_ms": 30.0}})
	if !eff.Verifiable() || eff.Evidence.VerifyTrust != uint8(profile.V2) {
		t.Fatalf("a position that changed is V2 verification: %+v", eff)
	}
}

func TestMoveAtLimitIsNotSuccess(t *testing.T) {
	cam := newFakeDahua(t, withStubborn)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 1.0, "duration_ms": 30.0}})
	if eff.Status == effect.OK {
		t.Fatalf("a camera that never moved is not a success: %+v", eff)
	}
}

// TestRefusedCommandIsNotSuccess: Dahua reports application failure in the
// body while still answering HTTP 200, so a status check alone would read a
// refusal as success.
func TestRefusedCommandIsNotSuccess(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	cam.mu.Lock()
	cam.refuseCmd = true
	cam.mu.Unlock()
	a, _ := startAdapter(t, cam)

	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZMove,
		Args: map[string]any{"pan": 0.5, "duration_ms": 30.0}})
	if eff.Status == effect.OK {
		t.Fatalf("HTTP 200 with an error body must not read as success: %+v", eff)
	}
}

func TestVelocityOutOfRangeRefused(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZMove, Args: map[string]any{"pan": 3.0}})
	if eff.Status != effect.Failed {
		t.Fatalf("out-of-range velocity must be refused: %+v", eff)
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZMove, Args: map[string]any{}})
	if eff.Status != effect.Failed {
		t.Fatal("a move with no axis must be refused")
	}
}

func TestPresetMustBeAPositiveNumber(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)
	for _, bad := range []any{"abc", 0.0, -1.0} {
		eff, _ := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "dh-1", Capability: profile.CapPTZPreset, Args: map[string]any{"preset": bad}})
		if eff.Status != effect.Failed {
			t.Fatalf("preset %v must be refused: %+v", bad, eff)
		}
	}
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZPreset, Args: map[string]any{"preset": 3.0}})
	if !eff.Verifiable() || eff.Record.Metrics["pan"] != 180 {
		t.Fatalf("a valid preset must work and be read back: %+v", eff)
	}
}

func TestSnapshotIsContentAddressed(t *testing.T) {
	cam := newFakeDahua(t)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "dh-1", Capability: profile.CapStreamSnapshot})
	if !eff.Verifiable() {
		t.Fatalf("real bytes are verification: %+v", eff)
	}
	if eff.Record.Artifacts[0].SizeBytes != uint64(len(cam.snapshot)) {
		t.Fatalf("artifact: %+v", eff.Record.Artifacts[0])
	}
}

func TestEmptyKVResponseIsAnError(t *testing.T) {
	// A CGI that answers 200 with nothing parseable did not do what we
	// asked; treating that as success would mark a broken camera online.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="c", qop="auth", nonce="n"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK) // empty body
	}))
	defer srv.Close()

	a := New(Config{Devices: []DeviceConfig{{
		ID: "dh-1", Address: strings.TrimPrefix(srv.URL, "http://"), User: "u", Pass: "p"}},
		Timeout: 2 * time.Second})
	sink := newSink()
	_ = a.Start(context.Background(), nil, sink)
	defer a.Stop(context.Background())
	if d, ok := sink.get("dh-1"); !ok || d.Online {
		t.Fatalf("a camera that answered nothing must not be online: %+v", d)
	}
}

func TestEventLineParsing(t *testing.T) {
	code, action, ok := parseEventLine("Code=VideoMotion;action=Start;index=0")
	if !ok || code != "VideoMotion" || action != "Start" {
		t.Fatalf("parse: %q %q %v", code, action, ok)
	}
	if _, _, ok := parseEventLine("heartbeat"); ok {
		t.Fatal("a line with no code/action must not parse as an event")
	}
	if _, _, ok := parseEventLine(""); ok {
		t.Fatal("an empty line must not parse")
	}
}

func TestEventVocabularyMapping(t *testing.T) {
	for code, want := range map[string]string{
		"VideoMotion":          profile.CapEventMotion,
		"VideoBlind":           profile.CapEventTamper,
		"CrossLineDetection":   profile.CapEventLineCross,
		"CrossRegionDetection": profile.CapEventLineCross,
	} {
		got, ok := eventCapability(code)
		if !ok || got != want {
			t.Fatalf("code %q → %q (ok=%v), want %q", code, got, ok, want)
		}
	}
	for _, code := range []string{"VideoLoss", "StorageFailure", ""} {
		if id, ok := eventCapability(code); ok {
			t.Fatalf("code %q must not be mapped, got %s", code, id)
		}
	}
}

func TestEventStreamReachesTheSink(t *testing.T) {
	cam := newFakeDahua(t)
	cam.mu.Lock()
	cam.events = []string{"Code=VideoMotion;action=Start;index=0"}
	cam.mu.Unlock()

	a := New(Config{Devices: []DeviceConfig{{
		ID: "dh-1", Address: cam.host(), User: "a", Pass: "b", Events: true}},
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
	t.Fatal("a VideoMotion event must reach the sink")
}

func TestStopCutsTheEventStream(t *testing.T) {
	cam := newFakeDahua(t)
	a := New(Config{Devices: []DeviceConfig{{
		ID: "dh-1", Address: cam.host(), User: "a", Pass: "b", Events: true}},
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
		t.Fatal("Stop must cancel the attach stream")
	}
}

func TestUnknownDeviceAndCapabilityFail(t *testing.T) {
	cam := newFakeDahua(t)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{DeviceID: "ghost", Capability: profile.CapStreamRTSP})
	if eff.Status != effect.Failed {
		t.Fatalf("unknown device must FAIL: %+v", eff)
	}
	eff, _ = a.Invoke(context.Background(), sdk.Call{DeviceID: "dh-1", Capability: profile.CapPTZMove})
	if eff.Status != effect.Failed {
		t.Fatalf("PTZ on a fixed camera must FAIL: %+v", eff)
	}
}

func TestProbeReportsUnreachable(t *testing.T) {
	av := nativeBackend{Config{Devices: []DeviceConfig{{ID: "x", Address: "127.0.0.1:1"}},
		Timeout: 300 * time.Millisecond}}.Probe(context.Background())
	if av.Available || !strings.Contains(av.Detail, "unreachable") {
		t.Fatalf("unreachable camera: %+v", av)
	}
}
