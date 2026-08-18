// Package onvif is the IP-camera adapter — ANetLink's reach into video
// surveillance, the one large smart-building device class the protocol
// adapters could not touch. Hikvision, Dahua, Uniview, Axis, Bosch, Reolink
// and most Chinese OEM cameras all speak ONVIF; vendor HTTP APIs
// (adapters/hikvision, adapters/dahua) exist for what ONVIF leaves out.
//
// Pure Go: SOAP 1.2 over net/http with encoding/xml, WS-Discovery over UDP
// multicast, and a minimal RTSP client used only to verify that an
// advertised stream is actually live. No gSOAP, no ffmpeg, no cgo.
//
// R02 §4 mapping: ONVIF Device -> Node, Media Profile -> Endpoint,
// stream/PTZ/event -> Capability. Honest trust: V2 (snapshot bytes and PTZ
// position readback are real verification) / A1 (WS-UsernameToken digest
// authenticates an account over the network, not a device identity — a
// camera that answers is not a camera that proved who it is).
//
// A note on the single backend: A5 requires every adapter to carry a
// self-contained fallback rather than depend on a host facility. This
// adapter depends on nothing outside the distribution, so native *is* the
// fallback; there is no second backend to degrade to.
//
// Scope (v0): WS-Discovery, device information, media profiles, stream and
// snapshot URIs, PTZ continuous move/stop/preset/position, and PullPoint
// event subscription for motion, tamper and line-cross. Recording (Profile
// G), access control (Profile C) and analytics configuration are follow-ups.
package onvif

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// DefaultPort is the ONVIF device service port cameras ship with.
const DefaultPort = 80

// DeviceConfig is one configured camera.
type DeviceConfig struct {
	ID      string `json:"id"`
	Address string `json:"address"` // "host" or "host:port"
	// XAddr overrides Address with a full device-service URL, for cameras
	// that do not serve ONVIF at the conventional /onvif/device_service.
	XAddr string `json:"xaddr,omitempty"`
	User  string `json:"user,omitempty"`
	Pass  string `json:"pass,omitempty"`
	// ProfileToken selects the media profile. Empty means the first one the
	// camera lists, which on nearly every camera is the main stream.
	ProfileToken string `json:"profile_token,omitempty"`
	Vendor       string `json:"vendor,omitempty"`
	Model        string `json:"model,omitempty"`
	// Events enables the PullPoint subscription for this camera. Off by
	// default: it holds a long-poll open, and not every camera survives one.
	Events bool `json:"events,omitempty"`
}

// Config configures the adapter.
type Config struct {
	Devices []DeviceConfig `json:"devices"`
	Timeout time.Duration  `json:"timeout,omitempty"`
	// DiscoveryInterface restricts WS-Discovery to one interface by name.
	// Empty probes every multicast-capable interface.
	DiscoveryInterface string `json:"discovery_interface,omitempty"`
	// VerifyStream makes stream.rtsp perform an RTSP DESCRIBE before
	// reporting success. Without it a stream URI is only what the camera
	// claims, and the effect stays UNVERIFIED — see Invoke.
	VerifyStream bool `json:"verify_stream,omitempty"`
}

// client holds the per-device HTTP and credential state.
type client struct {
	http *http.Client
	user string
	pass string
	// skewNS is the device clock offset in nanoseconds, learned from
	// GetSystemDateAndTime. Stored atomically: the event loop and Invoke
	// both build tokens with it.
	skewNS atomic.Int64
}

func (c *client) skew() time.Duration { return time.Duration(c.skewNS.Load()) }

// device is one live camera: its config, transport, resolved service URLs
// and the media profile in use.
// imagingRanges holds the vendor's native spans for the imaging parameters,
// learned once from GetOptions. ONVIF does not normalise these.
type imagingRanges struct {
	brightness vendorRange
	contrast   vendorRange
	saturation vendorRange
	sharpness  vendorRange
}

type device struct {
	cfg      DeviceConfig
	cli      *client
	deviceX  string // device service URL
	mediaX   string // media service URL
	ptzX     string // PTZ service URL ("" when the camera has no PTZ)
	eventsX  string // events service URL ("" when unsupported)
	imagingX string // imaging service URL ("" when unsupported)
	videoSrc string // video source token the imaging service addresses
	profTok  string // media profile token
	streamU  string // cached RTSP URI
	snapshot string // cached snapshot URI
	imgRange imagingRanges
	imgAvail map[string]bool // which imaging parameters have a usable range
	caps     map[string]profile.Capability
}

type Adapter struct {
	cfg  Config
	mu   sync.Mutex
	devs map[string]*device
	sink sdk.Sink
	done chan struct{}
	wg   sync.WaitGroup
}

func New(cfg Config) *Adapter {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Adapter{cfg: cfg, devs: map[string]*device{}}
}

func (a *Adapter) Info() sdk.Info {
	return sdk.Info{Name: "onvif", Protocol: "onvif", Version: "0.1.0"}
}

type nativeBackend struct{ cfg Config }

func (b nativeBackend) Name() string { return "native" }

func (b nativeBackend) Probe(ctx context.Context) sdk.Availability {
	if len(b.cfg.Devices) == 0 {
		return sdk.Availability{
			Available: true,
			Detail:    "pure-Go ONVIF; no cameras configured — use --scan to find them",
		}
	}
	timeout := b.cfg.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	// Ask each camera in turn, and stop at the first that answers.
	//
	// This used to probe only Devices[0], which made the whole adapter —
	// and therefore the daemon — unavailable whenever that one camera was
	// down, however many others were answering. A camera going offline is
	// the most ordinary event in a camera deployment, and it is a device
	// state, not a startup failure: the unreachable ones surface as
	// offline devices, which is what the effect model is for.
	//
	// The backend is unavailable only when nothing answers at all, which is
	// the case that really does mean "this adapter cannot do anything".
	var lastErr string
	for i, d := range b.cfg.Devices {
		url := deviceServiceURL(d)
		if url == "" {
			lastErr = fmt.Sprintf("camera %s has no address", d.ID)
			continue
		}
		// GetSystemDateAndTime is the one ONVIF call defined to work
		// unauthenticated, so it probes the transport without depending on
		// credentials being right yet — but firmware in the field does not
		// always honour that, so a refusal falls back to an authenticated
		// call rather than condemning the camera.
		anon := &client{http: &http.Client{Timeout: timeout}}
		var sdt systemDateAndTime
		if err := anon.call(ctx, url, nsDevice+"/GetSystemDateAndTime",
			`<tds:GetSystemDateAndTime/>`, &sdt); err != nil {
			authed := &client{http: &http.Client{Timeout: timeout}, user: d.User, pass: d.Pass}
			var caps capabilitiesResponse
			if err2 := authed.call(ctx, url, nsDevice+"/GetCapabilities",
				`<tds:GetCapabilities><tds:Category>All</tds:Category></tds:GetCapabilities>`, &caps); err2 != nil {
				lastErr = fmt.Sprintf("camera %s unreachable: %v", d.ID, err2)
				continue
			}
		}
		detail := fmt.Sprintf("pure-Go ONVIF; %d camera(s), %s answered", len(b.cfg.Devices), d.ID)
		if i > 0 {
			// Say so: starting with some cameras down is a fact an operator
			// wants in the log, not something to discover later.
			detail += fmt.Sprintf(" (%d ahead of it did not; last: %s)", i, lastErr)
		}
		return sdk.Availability{Available: true, Detail: detail}
	}
	return sdk.Availability{
		Available: false,
		Detail:    fmt.Sprintf("no configured camera answered (%d tried; last: %s)", len(b.cfg.Devices), lastErr),
	}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{nativeBackend{a.cfg}} }

// deviceServiceURL resolves a device config to its device service URL.
func deviceServiceURL(d DeviceConfig) string {
	if d.XAddr != "" {
		return d.XAddr
	}
	host := d.Address
	if host == "" {
		return ""
	}
	if !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", host, DefaultPort)
	}
	return "http://" + host + "/onvif/device_service"
}

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	a.mu.Lock()
	a.sink = sink
	a.done = make(chan struct{})
	done := a.done
	a.mu.Unlock()

	for _, dc := range a.cfg.Devices {
		d := &device{
			cfg:     dc,
			cli:     &client{http: &http.Client{Timeout: a.cfg.Timeout}, user: dc.User, pass: dc.Pass},
			deviceX: deviceServiceURL(dc),
			caps:    map[string]profile.Capability{},
		}
		// A camera that cannot be interrogated is reported offline rather
		// than omitted: an operator who configured it needs to see that it
		// is there and broken, not watch it silently vanish.
		online := a.introspect(ctx, d) == nil

		a.mu.Lock()
		a.devs[dc.ID] = d
		a.mu.Unlock()

		sink.UpsertDevice(sdk.Device{
			ID:      dc.ID,
			Profile: d.deviceProfile(),
			Online:  online,
		})

		if online && dc.Events && d.eventsX != "" {
			a.wg.Add(1)
			go func(d *device) {
				defer a.wg.Done()
				a.pumpEvents(d, done)
			}(d)
		}
	}
	return nil
}

// introspect learns everything about a camera that its own answers can tell
// us: clock offset, identity, which services it hosts, which media profile
// to use, and the stream and snapshot URIs for that profile.
func (a *Adapter) introspect(ctx context.Context, d *device) error {
	// 1. Clock offset first — every authenticated call below depends on it.
	//
	// Not fatal when it fails. ONVIF Core mandates this call and mandates it
	// be unauthenticated, but firmware in the field disagrees: some units
	// require auth for it, some omit it entirely. Since its only purpose is
	// correcting the WS-Security timestamp, a camera that refuses it is
	// still perfectly usable — it just gets an uncorrected clock, and the
	// authenticated calls below will say so if that turns out to matter.
	// Treating this as fatal would strand working cameras on a technicality.
	var sdt systemDateAndTime
	if err := d.cli.call(ctx, d.deviceX, nsDevice+"/GetSystemDateAndTime",
		`<tds:GetSystemDateAndTime/>`, &sdt); err == nil {
		if t, ok := sdt.utc(); ok {
			d.cli.skewNS.Store(int64(time.Until(t)))
		}
	}

	// 2. Identity. Non-fatal: a camera with strict ACLs may refuse this to a
	// low-privilege account yet still stream perfectly well.
	var info deviceInformation
	if err := d.cli.call(ctx, d.deviceX, nsDevice+"/GetDeviceInformation",
		`<tds:GetDeviceInformation/>`, &info); err == nil {
		if d.cfg.Vendor == "" {
			d.cfg.Vendor = info.Body.Manufacturer
		}
		if d.cfg.Model == "" {
			d.cfg.Model = info.Body.Model
		}
	}

	// 3. Service endpoints. Cameras place media/PTZ/events/imaging on their
	// own URLs.
	d.mediaX, d.ptzX, d.eventsX, d.imagingX = d.deviceX, "", "", ""
	var caps capabilitiesResponse
	if err := d.cli.call(ctx, d.deviceX, nsDevice+"/GetCapabilities",
		`<tds:GetCapabilities><tds:Category>All</tds:Category></tds:GetCapabilities>`, &caps); err == nil {
		if x := caps.Body.Capabilities.Media.XAddr; x != "" {
			d.mediaX = x
		}
		d.ptzX = caps.Body.Capabilities.PTZ.XAddr
		d.eventsX = caps.Body.Capabilities.Events.XAddr
		d.imagingX = caps.Body.Capabilities.Imaging.XAddr
	}

	// 4. Media profile and its URIs.
	if err := a.resolveMedia(ctx, d); err != nil {
		return err
	}

	// 5. Imaging ranges, if the camera offers the service at all. Never
	// fatal: plenty of cameras stream and pan but expose no imaging control.
	a.resolveImaging(ctx, d)

	d.buildCaps()
	return nil
}

// deviceProfile projects the camera into TSIR-D.
func (d *device) deviceProfile() *profile.DeviceProfile {
	caps := make([]profile.Capability, 0, len(d.caps))
	// Fixed order so the profile CID is stable across restarts: capability
	// arrays are author-significant in CoreDet, and map range order is not.
	for _, id := range capOrder {
		if c, ok := d.caps[id]; ok {
			caps = append(caps, c)
		}
	}
	return &profile.DeviceProfile{
		Schema: 1, Vendor: d.cfg.Vendor, Model: d.cfg.Model, Protocol: "onvif",
		Caps:      caps,
		MaxTrust:  profile.V2, // snapshot bytes / PTZ position readback
		AuthTrust: profile.A1, // WS-UsernameToken: account, not device identity
	}
}

// capOrder fixes capability ordering in emitted profiles.
var capOrder = []string{
	profile.CapStreamRTSP, profile.CapStreamSnapshot,
	profile.CapPTZMove, profile.CapPTZStop, profile.CapPTZPosition,
	profile.CapPTZAbsolute, profile.CapPTZHome,
	profile.CapPTZPreset, profile.CapPTZPresetList, profile.CapPTZPresetSave, profile.CapPTZTour,
	profile.CapLensFocus,
	profile.CapImagingBrightness, profile.CapImagingContrast,
	profile.CapImagingSaturation, profile.CapImagingSharpness, profile.CapImagingIRCut,
	profile.CapSystemInfo, profile.CapSystemReboot,
	profile.CapEventMotion, profile.CapEventTamper, profile.CapEventLineCross,
}

// buildCaps declares only what this camera actually demonstrated: a stream
// URI it returned, a PTZ service it advertised, an events service it hosts.
// Declaring a capability the camera does not have converts a clean "no such
// capability" into a confusing runtime failure.
func (d *device) buildCaps() {
	if d.streamU != "" {
		d.caps[profile.CapStreamRTSP] = profile.Capability{
			ID: profile.CapStreamRTSP, Access: profile.AccessGet}
	}
	if d.snapshot != "" {
		d.caps[profile.CapStreamSnapshot] = profile.Capability{
			ID: profile.CapStreamSnapshot, Access: profile.AccessGet}
	}
	if d.ptzX != "" {
		for _, id := range []string{
			profile.CapPTZMove, profile.CapPTZStop, profile.CapPTZPreset,
			profile.CapPTZAbsolute, profile.CapPTZHome,
			profile.CapPTZPresetSave, profile.CapPTZTour,
		} {
			d.caps[id] = profile.Capability{ID: id, Access: profile.AccessSet}
		}
		d.caps[profile.CapPTZPosition] = profile.Capability{
			ID: profile.CapPTZPosition, Access: profile.AccessGet | profile.AccessState}
		d.caps[profile.CapPTZPresetList] = profile.Capability{
			ID: profile.CapPTZPresetList, Access: profile.AccessGet}
	}
	// Imaging parameters are declared one by one, and only the ones whose
	// range the camera actually reported. A sensor with no contrast control
	// should not advertise one.
	if d.imagingX != "" {
		for id := range d.imgAvail {
			d.caps[id] = profile.Capability{
				ID: id, Access: profile.AccessSet | profile.AccessGet}
		}
		d.caps[profile.CapImagingIRCut] = profile.Capability{
			ID: profile.CapImagingIRCut, Access: profile.AccessSet | profile.AccessGet}
		d.caps[profile.CapLensFocus] = profile.Capability{
			ID: profile.CapLensFocus, Access: profile.AccessSet}
	}
	// Every ONVIF device answers these two.
	d.caps[profile.CapSystemInfo] = profile.Capability{
		ID: profile.CapSystemInfo, Access: profile.AccessGet}
	d.caps[profile.CapSystemReboot] = profile.Capability{
		ID: profile.CapSystemReboot, Access: profile.AccessSet}

	if d.eventsX != "" && d.cfg.Events {
		for _, id := range []string{profile.CapEventMotion, profile.CapEventTamper, profile.CapEventLineCross} {
			d.caps[id] = profile.Capability{ID: id, Access: profile.AccessState}
		}
	}
}

func (a *Adapter) Stop(context.Context) error {
	a.mu.Lock()
	if a.done != nil {
		close(a.done)
		a.done = nil
	}
	a.mu.Unlock()
	a.wg.Wait()
	return nil
}

func (a *Adapter) lookup(id string) (*device, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.devs[id]
	return d, ok
}

func (a *Adapter) Invoke(ctx context.Context, call sdk.Call) (effect.Effect, error) {
	d, ok := a.lookup(call.DeviceID)
	if !ok {
		return sdk.NewEffect().Fail(fmt.Sprintf("unknown device %q", call.DeviceID)).Build(), nil
	}
	if _, ok := d.caps[call.Capability]; !ok {
		return sdk.NewEffect().Fail(
			fmt.Sprintf("device %q does not expose %q", call.DeviceID, call.Capability)).Build(), nil
	}

	switch call.Capability {
	case profile.CapStreamRTSP:
		return a.invokeStream(ctx, d)
	case profile.CapStreamSnapshot:
		return a.invokeSnapshot(ctx, d)
	case profile.CapPTZMove:
		return a.invokePTZMove(ctx, d, call.Args)
	case profile.CapPTZStop:
		return a.invokePTZStop(ctx, d)
	case profile.CapPTZPreset:
		return a.invokePTZPreset(ctx, d, call.Args)
	case profile.CapPTZPosition:
		return a.invokePTZPosition(ctx, d)
	case profile.CapPTZAbsolute:
		return a.invokeAbsolute(ctx, d, call.Args)
	case profile.CapPTZHome:
		return a.invokeHome(ctx, d)
	case profile.CapPTZPresetList:
		return a.invokePresetList(ctx, d)
	case profile.CapPTZPresetSave:
		return a.invokePresetSave(ctx, d, call.Args)
	case profile.CapPTZTour:
		return a.invokeTour(ctx, d, call.Args)
	case profile.CapLensFocus:
		return a.invokeFocus(ctx, d, call.Args)
	case profile.CapImagingIRCut:
		return a.invokeIRCut(ctx, d, call.Args)
	case profile.CapImagingBrightness, profile.CapImagingContrast,
		profile.CapImagingSaturation, profile.CapImagingSharpness:
		return a.invokeImaging(ctx, d, call.Capability, call.Args)
	case profile.CapSystemInfo:
		return a.invokeSystemInfo(ctx, d)
	case profile.CapSystemReboot:
		return a.invokeReboot(ctx, d)
	default:
		// Event capabilities are STATE-only: they arrive on the sink, they
		// are not something you can pull on demand.
		return sdk.NewEffect().Fail(
			fmt.Sprintf("%q is a state-only capability; subscribe instead of invoking", call.Capability)).Build(), nil
	}
}

// dialTimeout is used by the RTSP probe; kept separate from the SOAP timeout
// because a camera's media plane and control plane fail independently.
func (a *Adapter) dialer() *net.Dialer { return &net.Dialer{Timeout: a.cfg.Timeout} }
