package c1serv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANetLink/adapters/sim"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/runtime"
	"github.com/ANetResearch/ANetLink/sdk"
)

const testAID = "bafyreifcvw7axfcj45itg7ggk5pf33dtq53kgpf47ltxnjl746b45n62j4"

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

func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// fakeAdapter serves "fake/dev-1" (x.do, invokable), "fake/off-1"
// (offline) and answers with whatever the test put in reply/fail.
type fakeAdapter struct {
	mu    sync.Mutex
	reply effect.Effect
	fail  error
	calls int
	args  map[string]any // of the last call
}

func (f *fakeAdapter) Info() sdk.Info          { return sdk.Info{Name: "fake", Protocol: "fakeproto"} }
func (f *fakeAdapter) Backends() []sdk.Backend { return []sdk.Backend{okBackend{}} }
func (f *fakeAdapter) Stop(context.Context) error {
	return nil
}
func (f *fakeAdapter) Start(_ context.Context, _ sdk.Backend, s sdk.Sink) error {
	caps := []profile.Capability{{ID: "x.do", Access: profile.AccessSet | profile.AccessGet}}
	s.UpsertDevice(sdk.Device{ID: "dev-1", Online: true,
		Profile: &profile.DeviceProfile{Schema: 1, Protocol: "fakeproto", Caps: caps, MaxTrust: profile.V4}})
	s.UpsertDevice(sdk.Device{ID: "off-1", Online: false,
		Profile: &profile.DeviceProfile{Schema: 1, Protocol: "fakeproto", Caps: caps, MaxTrust: profile.V4}})
	return nil
}
func (f *fakeAdapter) Invoke(_ context.Context, call sdk.Call) (effect.Effect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.args = call.Args
	if f.fail != nil {
		return effect.Effect{}, f.fail
	}
	if f.reply.Status == "" {
		return effect.Effect{Status: effect.OK, Record: &tsir.EffectRecord{Metrics: map[string]float64{"done": 1}}}, nil
	}
	return f.reply, nil
}
func (f *fakeAdapter) lastArgs() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.args
}
func (f *fakeAdapter) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type okBackend struct{}

func (okBackend) Name() string { return "native" }
func (okBackend) Probe(context.Context) sdk.Availability {
	return sdk.Availability{Available: true, Detail: "test"}
}

type rig struct {
	cli  *http.Client
	sock string
	rt   *runtime.Runtime
	fake *fakeAdapter
	logs *logSink
}

// newRig starts sim + fake behind a real c1.sock in a fresh directory.
func newRig(t *testing.T, remote ...string) *rig {
	t.Helper()
	logs := &logSink{}
	rt := runtime.New(runtime.Config{Remote: remote, Logf: logs.logf})
	fake := &fakeAdapter{}
	for _, a := range []sdk.Adapter{sim.New(), fake} {
		if err := rt.Register(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "run", "c1.sock")
	ln, err := Listen(Options{Socket: sock, Logf: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = Serve(ctx, rt, ln, logs.logf); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &rig{cli: udsClient(sock), sock: sock, rt: rt, fake: fake, logs: logs}
}

func udsClient(sock string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

type answer struct {
	status int
	ctype  string
	body   []byte
}

func (r *rig) do(t *testing.T, method, path, ctype string, body []byte) answer {
	t.Helper()
	req, err := http.NewRequest(method, "http://anetlink"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := r.cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{resp.StatusCode, resp.Header.Get("Content-Type"), raw}
}

func (r *rig) invoke(t *testing.T, req invokeReq) answer {
	t.Helper()
	b, err := coredet.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return r.do(t, http.MethodPost, "/v1/invoke", contentCBOR, b)
}

// refusalOf asserts a is a C1 refusal with the given HTTP status and reason.
func refusalOf(t *testing.T, a answer, status int, reason string) {
	t.Helper()
	if a.status != status {
		t.Fatalf("status %d, want %d (body %x)", a.status, status, a.body)
	}
	if a.ctype != contentCBOR {
		t.Fatalf("refusal content type %q", a.ctype)
	}
	var rf refusal
	if err := c1DecMode.Unmarshal(a.body, &rf); err != nil {
		t.Fatalf("refusal body does not decode strictly: %v (%x)", err, a.body)
	}
	if rf.Reason != reason {
		t.Fatalf("reason %q, want %q", rf.Reason, reason)
	}
}

func okResp(t *testing.T, a answer) invokeResp {
	t.Helper()
	if a.status != http.StatusOK || a.ctype != contentCBOR {
		t.Fatalf("want 200 CBOR, got %d %q %x", a.status, a.ctype, a.body)
	}
	var out invokeResp
	if err := c1DecMode.Unmarshal(a.body, &out); err != nil {
		t.Fatalf("response does not decode strictly: %v", err)
	}
	return out
}

func sp(s string) *string { return &s }
func up(u uint64) *uint64 { return &u }

func localCaller() *caller { return &caller{Via: up(viaLocal)} }
func relayCaller() *caller { return &caller{Via: up(viaRelay), AID: sp(testAID)} }
