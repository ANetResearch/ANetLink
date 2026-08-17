// Package thread is the Thread adapter. Thread is a bearer (IPv6 mesh), not
// an application protocol: ANetLink reaches Thread devices through a Border
// Router, and speaks CoAP over the resulting IPv6 network.
//
// Backends (A5 probe-first, fallback-always):
//
//	host-otbr — an OpenThread Border Router already running on the host, used
//	            through its REST API. Nothing is bundled.
//	managed   — the registered cross-language exemption (K207 A3′ ②): a
//	            bundled OTBR run as a managed child process, invisible
//	            externally. Enabled only when explicitly configured, so a
//	            plain build stays pure Go.
//
// Scope (v0): border-router discovery and health, Thread network state, and
// the CoAP client used to reach devices. Commissioning through the BR is a
// follow-up.
package thread

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// ResourceMap binds a capability to a CoAP resource on a Thread device.
type ResourceMap struct {
	ID     string // capability id
	Path   string // CoAP path, e.g. "/light"
	Access uint8
	Unit   string
	Scale  float64
}

// DeviceConfig is one Thread device reachable over the mesh.
type DeviceConfig struct {
	ID        string
	Address   string // IPv6 address on the Thread network
	Vendor    string
	Model     string
	Resources []ResourceMap
}

// ManagedOTBR configures the bundled border router (the exemption).
type ManagedOTBR struct {
	// Binary is the packaged otbr-agent path; empty disables the managed
	// backend entirely, keeping the build pure Go.
	Binary string
	// RadioURL is passed to otbr-agent, e.g. "spinel+hdlc+uart:///dev/ttyACM0".
	RadioURL string
	// RESTPort is where the managed agent serves its REST API.
	RESTPort int
}

// Config configures the adapter.
type Config struct {
	// BorderRouterURL is an existing OTBR REST endpoint, e.g.
	// "http://127.0.0.1:8081". Probed first.
	BorderRouterURL string
	// Managed describes the bundled OTBR fallback.
	Managed ManagedOTBR
	Devices []DeviceConfig
	Timeout time.Duration
}

type Adapter struct {
	cfg     Config
	mu      sync.Mutex
	baseURL string
	child   *exec.Cmd
	http    *http.Client
	sink    sdk.Sink
}

func New(cfg Config) *Adapter {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Adapter{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "thread", Protocol: "thread", Version: "0.1.0"}
}

// hostBackend uses an OTBR that is already running — nothing bundled.
type hostBackend struct{ cfg Config }

func (b hostBackend) Name() string { return "host-otbr" }

func (b hostBackend) Probe(ctx context.Context) sdk.Availability {
	if b.cfg.BorderRouterURL == "" {
		return sdk.Availability{Available: false, Detail: "no border router URL configured"}
	}
	state, err := fetchState(ctx, &http.Client{Timeout: 2 * time.Second}, b.cfg.BorderRouterURL)
	if err != nil {
		return sdk.Availability{Available: false, Detail: "border router unreachable: " + err.Error()}
	}
	return sdk.Availability{Available: true, Detail: "host OTBR, Thread state: " + state}
}

// managedBackend runs the bundled OTBR as a child process. This is the one
// registered cross-language exemption; it stays inert unless configured.
type managedBackend struct{ cfg Config }

func (b managedBackend) Name() string { return "managed-otbr" }

func (b managedBackend) Probe(ctx context.Context) sdk.Availability {
	if b.cfg.Managed.Binary == "" {
		return sdk.Availability{Available: false,
			Detail: "no bundled otbr-agent configured (managed backend disabled; build stays pure Go)"}
	}
	if _, err := exec.LookPath(b.cfg.Managed.Binary); err != nil {
		return sdk.Availability{Available: false, Detail: "bundled otbr-agent not executable: " + err.Error()}
	}
	if b.cfg.Managed.RadioURL == "" {
		return sdk.Availability{Available: false, Detail: "managed OTBR needs a radio URL (RCP device)"}
	}
	return sdk.Availability{Available: true, Detail: "bundled otbr-agent available (managed subprocess)"}
}

func (a *Adapter) Backends() []sdk.Backend {
	return []sdk.Backend{hostBackend{a.cfg}, managedBackend{a.cfg}}
}

// threadState is the subset of OTBR's REST /node response we use.
type threadState struct {
	State       string `json:"State"`
	NetworkName string `json:"NetworkName"`
	RLOC16      any    `json:"Rloc16"`
}

func fetchState(ctx context.Context, cli *http.Client, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/node", nil)
	if err != nil {
		return "", err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var st threadState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return "", err
	}
	if st.State == "" {
		return "unknown", nil
	}
	return st.State, nil
}

func (a *Adapter) Start(ctx context.Context, backend sdk.Backend, sink sdk.Sink) error {
	a.sink = sink
	switch backend.Name() {
	case "host-otbr":
		a.baseURL = a.cfg.BorderRouterURL
	case "managed-otbr":
		if err := a.startManaged(ctx); err != nil {
			return err
		}
	}
	for _, d := range a.cfg.Devices {
		caps := make([]profile.Capability, 0, len(d.Resources))
		for _, r := range d.Resources {
			caps = append(caps, profile.Capability{ID: r.ID, Access: r.Access, Unit: r.Unit})
		}
		sink.UpsertDevice(sdk.Device{
			ID: d.ID,
			Profile: &profile.DeviceProfile{
				Schema: 1, Vendor: d.Vendor, Model: d.Model, Protocol: "thread",
				Caps: caps,
				// Thread encrypts the mesh with a network key and devices are
				// commissioned with per-device credentials.
				MaxTrust:  profile.V2,
				AuthTrust: profile.A2,
			},
			Online: a.baseURL != "",
		})
	}
	return nil
}

// startManaged launches the bundled otbr-agent and waits for its REST API.
func (a *Adapter) startManaged(ctx context.Context) error {
	m := a.cfg.Managed
	port := m.RESTPort
	if port == 0 {
		port = 8081
	}
	cmd := exec.Command(m.Binary, "-I", "wpan0", "-B", "eth0",
		m.RadioURL, "--rest-listen-port", strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("thread: start bundled otbr-agent: %w", err)
	}
	a.mu.Lock()
	a.child = cmd
	a.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	a.mu.Unlock()

	deadline := time.Now().Add(a.cfg.Timeout)
	for time.Now().Before(deadline) {
		if _, err := fetchState(ctx, a.http, a.baseURL); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = a.Stop(ctx)
	return fmt.Errorf("thread: bundled otbr-agent did not become ready")
}

func (a *Adapter) Stop(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.child != nil && a.child.Process != nil {
		_ = a.child.Process.Kill()
		_ = a.child.Wait()
		a.child = nil
	}
	return nil
}

// State returns the border router's Thread state ("leader", "child", ...).
func (a *Adapter) State(ctx context.Context) (string, error) {
	a.mu.Lock()
	base := a.baseURL
	a.mu.Unlock()
	if base == "" {
		return "", fmt.Errorf("thread: no border router")
	}
	return fetchState(ctx, a.http, base)
}

func (a *Adapter) Invoke(ctx context.Context, call sdk.Call) (effect.Effect, error) {
	var dev *DeviceConfig
	for i := range a.cfg.Devices {
		if a.cfg.Devices[i].ID == call.DeviceID {
			dev = &a.cfg.Devices[i]
			break
		}
	}
	if dev == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown device %q", call.DeviceID)).Build(), nil
	}
	var rm *ResourceMap
	for i := range dev.Resources {
		if dev.Resources[i].ID == call.Capability {
			rm = &dev.Resources[i]
			break
		}
	}
	if rm == nil {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown capability %q", call.Capability)).Build(), nil
	}
	a.mu.Lock()
	base := a.baseURL
	a.mu.Unlock()
	if base == "" {
		return sdk.NewEffect().Unavailable("no border router available").Build(), nil
	}
	metric := strings.ReplaceAll(rm.ID, ".", "_")
	scale := rm.Scale
	if scale == 0 {
		scale = 1
	}

	// SET: PUT the value, then GET it back — readback is the metric.
	if raw, isSet := call.Args["value"]; isSet {
		body := fmt.Sprintf("%v", raw)
		resp, err := a.coapRequest(ctx, dev.Address, rm.Path, codePUT, []byte(body))
		if err != nil {
			return sdk.NewEffect().Unavailable("CoAP PUT: " + err.Error()).Build(), nil
		}
		if !coapClassOK(resp.Code) {
			return sdk.NewEffect().Fail(fmt.Sprintf("device rejected the write (CoAP %d.%02d)",
				resp.Code>>5, resp.Code&0x1F)).Build(), nil
		}
	}

	resp, err := a.coapRequest(ctx, dev.Address, rm.Path, codeGET, nil)
	if err != nil {
		return sdk.NewEffect().Unavailable("CoAP GET: " + err.Error()).Build(), nil
	}
	if !coapClassOK(resp.Code) {
		return sdk.NewEffect().Fail(fmt.Sprintf("CoAP %d.%02d from device",
			resp.Code>>5, resp.Code&0x1F)).Build(), nil
	}
	v, perr := strconv.ParseFloat(strings.TrimSpace(string(resp.Payload)), 64)
	if perr != nil {
		return sdk.NewEffect().Unavailable(fmt.Sprintf("non-numeric CoAP payload %q", resp.Payload)).Build(), nil
	}
	v *= scale
	return sdk.NewEffect().Metric(metric, v).
		Requested(fmt.Sprintf("%s=%v", rm.ID, call.Args["value"])).
		Protocol("thread").NativeAck(true).
		Observed(strconv.FormatFloat(v, 'f', -1, 64)).
		Trust(uint8(profile.V2), uint8(profile.A2)).
		Verified().Build(), nil
}
