package dahua

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
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
	presets   []fakePreset
	rebooted  bool
}

type fakePreset struct {
	index int
	name  string
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
		switch action {
		case "getDeviceType":
			fmt.Fprint(w, "type=IPC-HFW4431R-Z\r\n")
		case "getSerialNo":
			fmt.Fprint(w, "sn=DH123456\r\n")
		case "getSoftwareVersion":
			fmt.Fprint(w, "version=2.800.0000\r\n")
		case "reboot":
			f.rebooted = true
			fmt.Fprint(w, "OK")
		default:
			w.WriteHeader(http.StatusBadRequest)
		}

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
		case "getPresets":
			for i, pr := range f.presets {
				fmt.Fprintf(w, "table.Preset[%d].Index=%d\r\ntable.Preset[%d].Name=%s\r\n",
					i, pr.index, i, pr.name)
			}
			if len(f.presets) == 0 {
				fmt.Fprint(w, "table.Preset[0].Index=0\r\n")
			}
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
				case "SetPreset":
					n, _ := strconv.Atoi(q.Get("arg2"))
					f.presets = append(f.presets, fakePreset{index: n, name: fmt.Sprintf("preset %d", n)})
				case "PositionABS":
					// Dahua takes degrees here, not the speed codes the
					// continuous API uses.
					p, _ := strconv.ParseFloat(q.Get("arg1"), 64)
					t, _ := strconv.ParseFloat(q.Get("arg2"), 64)
					z, _ := strconv.ParseFloat(q.Get("arg3"), 64)
					f.pan, f.tilt, f.zoom = p, t, z
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

// --- extended control surface ---

// TestAbsoluteSendsDegreesNotSpeedCodes: PositionABS takes degrees, while
// the continuous API next door takes a compass code and a 1..8 speed. Using
// the wrong units here points the camera somewhere arbitrary.
func TestAbsoluteSendsDegreesNotSpeedCodes(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)

	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZAbsolute,
		Args: map[string]any{"pan": 0, "tilt": 0.5, "zoom": 0.1}})
	if err != nil {
		t.Fatal(err)
	}
	cam.mu.Lock()
	cmds := append([]string(nil), cam.cmds...)
	cam.mu.Unlock()
	if len(cmds) == 0 || !strings.Contains(cmds[0], "code=PositionABS") {
		t.Fatalf("absolute must use PositionABS: %v", cmds)
	}
	// pan 0 → 180°, tilt 0.5 → 45°
	if !strings.Contains(cmds[0], "arg1=180.0") || !strings.Contains(cmds[0], "arg2=45.0") {
		t.Fatalf("degree conversion wrong: %s", cmds[0])
	}
	if !eff.Verifiable() {
		t.Fatalf("arriving verifies: %+v", eff)
	}
	if p := eff.Record.Metrics["pan"]; p < -0.01 || p > 0.01 {
		t.Fatalf("the normalised pan must round-trip to ~0, got %g", p)
	}
}

func TestAbsoluteHoldsUnspecifiedAxes(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)
	cam.mu.Lock()
	cam.pan, cam.tilt = 90, 30
	cam.mu.Unlock()

	if _, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZAbsolute,
		Args: map[string]any{"zoom": 0.5}}); err != nil {
		t.Fatal(err)
	}
	cam.mu.Lock()
	pan, tilt := cam.pan, cam.tilt
	cam.mu.Unlock()
	if pan != 90 || tilt != 30 {
		t.Fatalf("a zoom-only request re-aimed the camera to %g/%g", pan, tilt)
	}
}

func TestAbsoluteRejectsOutOfRange(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, _ := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZAbsolute, Args: map[string]any{"pan": 2.0}})
	if eff.Status != effect.Failed {
		t.Fatalf("out-of-range absolute must be refused: %+v", eff)
	}
}

// TestPresetListParsesTheTableFormat: Dahua reports presets as indexed
// key=value lines, and the index in the key is not the preset number.
func TestPresetListParsesTheTableFormat(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	cam.mu.Lock()
	cam.presets = []fakePreset{{3, "loading bay"}, {1, "front door"}}
	cam.mu.Unlock()
	a, _ := startAdapter(t, cam)

	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZPresetList})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["preset_count"] != 2 {
		t.Fatalf("preset list: %+v", eff)
	}
	if len(eff.Record.Artifacts) != 1 {
		t.Fatalf("names must ride as an artifact: %+v", eff.Record)
	}
	// The index in the key (0,1) is not the preset number (3,1) — reading
	// the wrong one would recall the wrong bearing.
	var got []preset
	if err := json.Unmarshal(presetBlob(t, a), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Token != "1" || got[0].Name != "front door" {
		t.Fatalf("presets must carry the vendor Index and Name: %+v", got)
	}
}

// presetBlob re-runs the listing and returns the JSON artifact bytes.
func presetBlob(t *testing.T, a *Adapter) []byte {
	t.Helper()
	// The artifact is content-addressed, not carried by value, so the test
	// reads the store the same way the adapter does.
	kv, err := func() (map[string]string, error) {
		d, _ := a.lookup("dh-1")
		return d.getKV(context.Background(), "/cgi-bin/ptz.cgi?action=getPresets&channel=0")
	}()
	if err != nil {
		t.Fatal(err)
	}
	byIndex := map[string]map[string]string{}
	for k, v := range kv {
		m := presetLine.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		if byIndex[m[1]] == nil {
			byIndex[m[1]] = map[string]string{}
		}
		byIndex[m[1]][strings.ToLower(m[2])] = v
	}
	list := []preset{}
	for _, f := range byIndex {
		if f["index"] != "" {
			list = append(list, preset{Token: f["index"], Name: strings.TrimSpace(f["name"])})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		x, _ := strconv.Atoi(list[i].Token)
		y, _ := strconv.Atoi(list[j].Token)
		return x < y
	})
	b, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPresetOrderIsStable: the parse goes through a map, and an agent
// comparing two listings must not see them shuffle.
func TestPresetOrderIsStable(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	cam.mu.Lock()
	cam.presets = []fakePreset{{9, "i"}, {2, "ii"}, {5, "iii"}, {1, "iv"}}
	cam.mu.Unlock()
	a, _ := startAdapter(t, cam)

	var first string
	for i := 0; i < 5; i++ {
		eff, err := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "dh-1", Capability: profile.CapPTZPresetList})
		if err != nil {
			t.Fatal(err)
		}
		cid := eff.Record.Artifacts[0].ContentCID
		if i == 0 {
			first = cid
			continue
		}
		if cid != first {
			t.Fatalf("the same preset store must serialise identically every time: %s vs %s", cid, first)
		}
	}
}

func TestPresetSaveThenList(t *testing.T) {
	cam := newFakeDahua(t, withPTZ)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapPTZPresetSave,
		Args: map[string]any{"preset": 7.0}})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("a saved preset that reads back verifies: %+v", eff)
	}
	for _, bad := range []map[string]any{{}, {"preset": 0.0}, {"preset": "x"}} {
		eff, _ := a.Invoke(context.Background(), sdk.Call{
			DeviceID: "dh-1", Capability: profile.CapPTZPresetSave, Args: bad})
		if eff.Status != effect.Failed {
			t.Fatalf("args %v must be refused: %+v", bad, eff)
		}
	}
}

func TestRebootCannotBeVerified(t *testing.T) {
	cam := newFakeDahua(t)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapSystemReboot})
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

// TestSystemInfoSurvivesAPartialAnswer: one unsupported magicBox query must
// not sink the whole report.
func TestSystemInfoSurvivesAPartialAnswer(t *testing.T) {
	cam := newFakeDahua(t)
	a, _ := startAdapter(t, cam)
	eff, err := a.Invoke(context.Background(), sdk.Call{
		DeviceID: "dh-1", Capability: profile.CapSystemInfo})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() {
		t.Fatalf("system.info: %+v", eff)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "IPC-HFW4431R-Z") {
		t.Fatalf("evidence should name the model: %q", eff.Evidence.ObservedState)
	}
}

// A camera that is down or has moved must not take the adapter — and with
// it the daemon — out of service while other cameras are answering. The
// deployed testbed daemon sat in a restart loop thousands deep because the
// first camera in its config had moved to a different port.
func TestProbeSurvivesADeadCameraAheadOfALiveOne(t *testing.T) {
	cam := newFakeDahua(t)

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
