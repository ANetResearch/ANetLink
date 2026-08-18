// Package hikvision is the ISAPI adapter — Hikvision's own HTTP API, and by
// inheritance that of the many OEM brands built on Hikvision firmware.
//
// Why this exists when adapters/onvif already reaches these cameras: ONVIF
// is the interoperable floor, not the whole device. Installers routinely
// ship cameras with ONVIF disabled, the ONVIF account separate from the web
// account, or Profile S only. ISAPI is always on, always has the full event
// vocabulary (line crossing, intrusion, face, region entry), and reports the
// identity the operator sees in the vendor's own app. Where both work, ONVIF
// is preferable precisely because it is the standard; where ONVIF is off or
// crippled, this is the difference between a supported camera and a brick.
//
// Pure Go: HTTP with digest auth (internal/digest) and the vendor's XML.
// Honest trust: V2 (snapshot bytes and PTZ position readback) / A1 (digest
// authenticates an account over the network, not a device identity).
//
// Scope (v0): identity, stream URI, snapshot, PTZ continuous/stop/preset/
// position, and the alertStream event feed. Recording playback, storage
// management and configuration writes are out of scope by choice — this
// adapter reads and actuates, it does not administer cameras.
package hikvision

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/internal/digest"
	"github.com/ANetResearch/ANetLink/internal/rtsp"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

const (
	// DefaultPort is the ISAPI HTTP port.
	DefaultPort = 80
	// DefaultRTSPPort is where Hikvision serves RTSP.
	DefaultRTSPPort = 554
	// maxSnapshot caps a still frame; see adapters/onvif for the reasoning.
	maxSnapshot = 16 << 20
	// maxXML caps a control-plane response.
	maxXML = 1 << 20
	// ptzScale converts the ANetLink [-1,1] velocity contract to the
	// [-100,100] Hikvision uses. The capability contract is uniform across
	// adapters on purpose: a caller should not have to know whose camera it
	// is to ask for half speed.
	ptzScale = 100
)

type DeviceConfig struct {
	ID      string `json:"id"`
	Address string `json:"address"` // "host" or "host:port"
	User    string `json:"user"`
	Pass    string `json:"pass"`
	// Channel is the camera channel, 1 for a standalone camera and 1..N on
	// an NVR. ISAPI stream ids are channel*100 + stream, so channel 2's main
	// stream is 201.
	Channel int `json:"channel,omitempty"`
	// Stream selects main (1) or sub (2) stream.
	Stream   int    `json:"stream,omitempty"`
	RTSPPort int    `json:"rtsp_port,omitempty"`
	Vendor   string `json:"vendor,omitempty"`
	Model    string `json:"model,omitempty"`
	Events   bool   `json:"events,omitempty"`
	UseHTTPS bool   `json:"use_https,omitempty"`
}

type Config struct {
	Devices      []DeviceConfig `json:"devices"`
	Timeout      time.Duration  `json:"timeout,omitempty"`
	VerifyStream bool           `json:"verify_stream,omitempty"`
}

type device struct {
	cfg      DeviceConfig
	http     *http.Client
	base     string // "http://host:port"
	streamU  string
	caps     map[string]profile.Capability
	hasPTZ   bool
	hasIRCut bool
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
	return sdk.Info{Name: "hikvision", Protocol: "isapi", Version: "0.1.0"}
}

type nativeBackend struct{ cfg Config }

func (b nativeBackend) Name() string { return "native" }

func (b nativeBackend) Probe(ctx context.Context) sdk.Availability {
	if len(b.cfg.Devices) == 0 {
		return sdk.Availability{Available: true, Detail: "pure-Go ISAPI; no cameras configured"}
	}
	// Ask each camera in turn and stop at the first that answers.
	//
	// Probing only Devices[0] made the whole adapter — and with it the
	// daemon — unavailable whenever that one camera was down or had moved,
	// however many others were answering. A camera going offline is the most
	// ordinary event in a camera deployment: it is a device state, to be
	// surfaced as an offline device, not a startup failure. Unavailable is
	// reserved for nothing answering at all, which really does mean this
	// adapter can do nothing.
	var lastErr string
	for i, dc := range b.cfg.Devices {
		d := newDevice(dc, b.cfg.Timeout)
		var info deviceInfo
		if err := d.getXML(ctx, "/ISAPI/System/deviceInfo", &info); err != nil {
			lastErr = fmt.Sprintf("camera %s unreachable: %v", d.cfg.ID, err)
			continue
		}
		detail := fmt.Sprintf("pure-Go ISAPI; %s is a %s", d.cfg.ID, info.Model)
		if i > 0 {
			detail += fmt.Sprintf(" (%d ahead of it did not answer; last: %s)", i, lastErr)
		}
		return sdk.Availability{Available: true, Detail: detail}
	}
	return sdk.Availability{Available: false,
		Detail: fmt.Sprintf("no configured camera answered (%d tried; last: %s)",
			len(b.cfg.Devices), lastErr)}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{nativeBackend{a.cfg}} }

func newDevice(dc DeviceConfig, timeout time.Duration) *device {
	if dc.Channel == 0 {
		dc.Channel = 1
	}
	if dc.Stream == 0 {
		dc.Stream = 1
	}
	if dc.RTSPPort == 0 {
		dc.RTSPPort = DefaultRTSPPort
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	scheme := "http"
	port := DefaultPort
	if dc.UseHTTPS {
		scheme, port = "https", 443
	}
	host := dc.Address
	if !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", host, port)
	}
	return &device{
		cfg:  dc,
		base: scheme + "://" + host,
		http: &http.Client{
			Timeout:   timeout,
			Transport: digest.NewTransport(http.DefaultTransport, dc.User, dc.Pass),
		},
		caps: map[string]profile.Capability{},
	}
}

// streamID is ISAPI's channel*100+stream encoding.
func (d *device) streamID() int { return d.cfg.Channel*100 + d.cfg.Stream }

func (d *device) do(ctx context.Context, method, path, body string) (*http.Response, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/xml")
	}
	return d.http.Do(req)
}

// getXML performs a GET and unmarshals the vendor's XML.
func (d *device) getXML(ctx context.Context, path string, out any) error {
	resp, err := d.do(ctx, http.MethodGet, path, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ISAPI %s returned HTTP %d", path, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxXML))
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return xml.Unmarshal(raw, out)
}

// put sends a control command and reports whether the camera accepted it.
func (d *device) put(ctx context.Context, path, body string) error {
	resp, err := d.do(ctx, http.MethodPut, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxXML))
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ISAPI %s returned HTTP %d", path, resp.StatusCode)
	}
	return nil
}

var errUnauthorized = fmt.Errorf("camera did not accept the credentials")

// --- ISAPI response shapes ---

type deviceInfo struct {
	DeviceName   string `xml:"deviceName"`
	Model        string `xml:"model"`
	SerialNumber string `xml:"serialNumber"`
	Firmware     string `xml:"firmwareVersion"`
	DeviceType   string `xml:"deviceType"`
}

type ptzStatus struct {
	AbsoluteHigh struct {
		Elevation    int `xml:"elevation"`
		Azimuth      int `xml:"azimuth"`
		AbsoluteZoom int `xml:"absoluteZoom"`
	} `xml:"AbsoluteHigh"`
}

func (a *Adapter) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	a.mu.Lock()
	a.sink = sink
	a.done = make(chan struct{})
	done := a.done
	a.mu.Unlock()

	for _, dc := range a.cfg.Devices {
		d := newDevice(dc, a.cfg.Timeout)
		online := a.introspect(ctx, d) == nil

		a.mu.Lock()
		a.devs[dc.ID] = d
		a.mu.Unlock()

		sink.UpsertDevice(sdk.Device{ID: dc.ID, Profile: d.deviceProfile(), Online: online})

		if online && dc.Events {
			a.wg.Add(1)
			go func(d *device) {
				defer a.wg.Done()
				a.pumpEvents(d, done)
			}(d)
		}
	}
	return nil
}

func (a *Adapter) introspect(ctx context.Context, d *device) error {
	var info deviceInfo
	if err := d.getXML(ctx, "/ISAPI/System/deviceInfo", &info); err != nil {
		return err
	}
	if d.cfg.Vendor == "" {
		d.cfg.Vendor = "Hikvision"
	}
	if d.cfg.Model == "" {
		d.cfg.Model = info.Model
	}

	// A camera that answers deviceInfo serves RTSP at the conventional path.
	host, _, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(d.base, "https://"), "http://"))
	if err != nil {
		return err
	}
	d.streamU = fmt.Sprintf("rtsp://%s:%d/Streaming/Channels/%d", host, d.cfg.RTSPPort, d.streamID())

	// PTZ is discovered, not assumed: a fixed dome answers 4xx here, and
	// claiming ptz.move on it would turn a clean "no such capability" into a
	// runtime failure the operator has to chase.
	d.hasPTZ = d.getXML(ctx, fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/capabilities", d.cfg.Channel), nil) == nil
	// Same discipline for the image pipeline: ask, do not assume.
	d.hasIRCut = d.getXML(ctx, fmt.Sprintf("/ISAPI/Image/channels/%d/ircutFilter", d.cfg.Channel), nil) == nil

	d.buildCaps()
	return nil
}

var capOrder = []string{
	profile.CapStreamRTSP, profile.CapStreamSnapshot,
	profile.CapPTZMove, profile.CapPTZStop, profile.CapPTZPosition,
	profile.CapPTZAbsolute, profile.CapPTZHome,
	profile.CapPTZPreset, profile.CapPTZPresetList, profile.CapPTZPresetSave,
	profile.CapImagingIRCut,
	profile.CapSystemInfo, profile.CapSystemReboot,
	profile.CapEventMotion, profile.CapEventTamper, profile.CapEventLineCross,
}

func (d *device) buildCaps() {
	d.caps[profile.CapStreamRTSP] = profile.Capability{ID: profile.CapStreamRTSP, Access: profile.AccessGet}
	d.caps[profile.CapStreamSnapshot] = profile.Capability{ID: profile.CapStreamSnapshot, Access: profile.AccessGet}
	if d.hasPTZ {
		for _, id := range []string{
			profile.CapPTZMove, profile.CapPTZStop, profile.CapPTZPreset,
			profile.CapPTZAbsolute, profile.CapPTZHome, profile.CapPTZPresetSave,
		} {
			d.caps[id] = profile.Capability{ID: id, Access: profile.AccessSet}
		}
		d.caps[profile.CapPTZPosition] = profile.Capability{ID: profile.CapPTZPosition,
			Access: profile.AccessGet | profile.AccessState}
		d.caps[profile.CapPTZPresetList] = profile.Capability{ID: profile.CapPTZPresetList,
			Access: profile.AccessGet}
	}
	// The IR-cut filter is on the image pipeline, not the PTZ head — fixed
	// cameras have one too.
	if d.hasIRCut {
		d.caps[profile.CapImagingIRCut] = profile.Capability{ID: profile.CapImagingIRCut,
			Access: profile.AccessSet | profile.AccessGet}
	}
	// Every ISAPI device answers these.
	d.caps[profile.CapSystemInfo] = profile.Capability{ID: profile.CapSystemInfo, Access: profile.AccessGet}
	d.caps[profile.CapSystemReboot] = profile.Capability{ID: profile.CapSystemReboot, Access: profile.AccessSet}
	if d.cfg.Events {
		for _, id := range []string{profile.CapEventMotion, profile.CapEventTamper, profile.CapEventLineCross} {
			d.caps[id] = profile.Capability{ID: id, Access: profile.AccessState}
		}
	}
}

func (d *device) deviceProfile() *profile.DeviceProfile {
	caps := make([]profile.Capability, 0, len(d.caps))
	for _, id := range capOrder {
		if c, ok := d.caps[id]; ok {
			caps = append(caps, c)
		}
	}
	return &profile.DeviceProfile{
		Schema: 1, Vendor: d.cfg.Vendor, Model: d.cfg.Model, Protocol: "isapi",
		Caps:      caps,
		MaxTrust:  profile.V2,
		AuthTrust: profile.A1,
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
	case profile.CapImagingIRCut:
		return a.invokeIRCut(ctx, d, call.Args)
	case profile.CapSystemInfo:
		return a.invokeSystemInfo(ctx, d)
	case profile.CapSystemReboot:
		return a.invokeReboot(ctx, d)
	default:
		return sdk.NewEffect().Fail(
			fmt.Sprintf("%q is a state-only capability; subscribe instead of invoking", call.Capability)).Build(), nil
	}
}

func (a *Adapter) invokeStream(ctx context.Context, d *device) (effect.Effect, error) {
	b := sdk.NewEffect().StreamURI(d.streamU).
		Requested(profile.CapStreamRTSP).Protocol("isapi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	if !a.cfg.VerifyStream {
		// The URI is constructed from the vendor's documented scheme, not
		// returned by the camera — even less of a measurement than ONVIF's.
		return b.Observed("uri derived from ISAPI channel, stream not probed").Build(), nil
	}
	sdp, err := rtsp.Describe(ctx, &net.Dialer{Timeout: a.cfg.Timeout}, d.streamU, d.cfg.User, d.cfg.Pass)
	if err != nil {
		return sdk.NewEffect().StreamURI(d.streamU).
			Requested(profile.CapStreamRTSP).Protocol("isapi").
			Unavailable("stream probe failed: " + err.Error()).Build(), nil
	}
	codec := rtsp.VideoCodec(sdp)
	if codec == "" {
		codec = "unknown"
	}
	return b.Metric("stream_live", 1).
		Observed("RTSP DESCRIBE ok, video codec "+codec).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) invokeSnapshot(ctx context.Context, d *device) (effect.Effect, error) {
	path := fmt.Sprintf("/ISAPI/Streaming/channels/%d/picture", d.streamID())
	resp, err := d.do(ctx, http.MethodGet, path, "")
	if err != nil {
		return sdk.NewEffect().Unavailable("snapshot fetch: " + err.Error()).Build(), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return sdk.NewEffect().Fail("snapshot rejected: " + errUnauthorized.Error()).Build(), nil
	}
	if resp.StatusCode != http.StatusOK {
		return sdk.NewEffect().Fail(fmt.Sprintf("snapshot returned HTTP %d", resp.StatusCode)).Build(), nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshot))
	if err != nil {
		return sdk.NewEffect().Unavailable("snapshot read: " + err.Error()).Build(), nil
	}
	if len(data) == 0 {
		return sdk.NewEffect().Fail("camera returned an empty frame").Build(), nil
	}
	mt := resp.Header.Get("Content-Type")
	if mt == "" {
		mt = profile.MediaTypeJPEG
	}
	return sdk.NewEffect().Snapshot(data, mt).
		Metric("snapshot_bytes", float64(len(data))).
		Requested(profile.CapStreamSnapshot).Protocol("isapi").NativeAck(true).
		Observed(fmt.Sprintf("%d bytes of %s", len(data), mt)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) ptzPosition(ctx context.Context, d *device) (ptzStatus, error) {
	var st ptzStatus
	err := d.getXML(ctx, fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/status", d.cfg.Channel), &st)
	return st, err
}

func argFloat(args map[string]any, key string) (float64, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

func (a *Adapter) invokePTZMove(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	pan, _ := argFloat(args, "pan")
	tilt, _ := argFloat(args, "tilt")
	zoom, _ := argFloat(args, "zoom")
	if pan == 0 && tilt == 0 && zoom == 0 {
		return sdk.NewEffect().Fail("ptz.move needs at least one of pan/tilt/zoom").Build(), nil
	}
	for name, v := range map[string]float64{"pan": pan, "tilt": tilt, "zoom": zoom} {
		if v < -1 || v > 1 {
			return sdk.NewEffect().Fail(
				fmt.Sprintf("%s velocity %v out of range [-1,1]", name, v)).Build(), nil
		}
	}
	durMS := 500
	if v, ok := argFloat(args, "duration_ms"); ok && v > 0 {
		durMS = int(min(v, 10_000))
	}

	before, beforeErr := a.ptzPosition(ctx, d)

	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><PTZData><pan>%d</pan><tilt>%d</tilt><zoom>%d</zoom></PTZData>`,
		int(pan*ptzScale), int(tilt*ptzScale), int(zoom*ptzScale))
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/continuous", d.cfg.Channel)
	if err := d.put(ctx, path, body); err != nil {
		return ptzFault(err, "continuous move"), nil
	}

	select {
	case <-time.After(time.Duration(durMS) * time.Millisecond):
	case <-ctx.Done():
		// An abandoned continuous move keeps the motor running.
		_ = d.put(context.WithoutCancel(ctx), path, ptzStopBody)
		return effect.Effect{}, ctx.Err()
	}
	if err := d.put(ctx, path, ptzStopBody); err != nil {
		return sdk.NewEffect().Protocol("isapi").
			Fail("move issued but stop failed, camera may still be moving: " + err.Error()).Build(), nil
	}

	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.move pan=%g tilt=%g zoom=%g for %dms", pan, tilt, zoom, durMS)).
		Protocol("isapi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	after, afterErr := a.ptzPosition(ctx, d)
	if beforeErr != nil || afterErr != nil {
		return b.Observed("camera does not report PTZ position; movement unverified").Build(), nil
	}
	bp, ap := before.AbsoluteHigh, after.AbsoluteHigh
	b = b.Metric("azimuth", float64(ap.Azimuth)).
		Metric("elevation", float64(ap.Elevation)).
		Metric("zoom", float64(ap.AbsoluteZoom))
	if bp == ap {
		return b.Observed(fmt.Sprintf("position unchanged at azimuth=%d elevation=%d (at limit?)",
			ap.Azimuth, ap.Elevation)).Build(), nil
	}
	return b.Observed(fmt.Sprintf("azimuth %d→%d elevation %d→%d",
		bp.Azimuth, ap.Azimuth, bp.Elevation, ap.Elevation)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

const ptzStopBody = `<?xml version="1.0" encoding="UTF-8"?><PTZData><pan>0</pan><tilt>0</tilt><zoom>0</zoom></PTZData>`

func (a *Adapter) invokePTZStop(ctx context.Context, d *device) (effect.Effect, error) {
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/continuous", d.cfg.Channel)
	if err := d.put(ctx, path, ptzStopBody); err != nil {
		return ptzFault(err, "stop"), nil
	}
	b := sdk.NewEffect().Requested(profile.CapPTZStop).Protocol("isapi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	// ISAPI has no move-status field, so a stop is confirmed by the position
	// holding still across two reads rather than by a status word.
	first, err1 := a.ptzPosition(ctx, d)
	if err1 != nil {
		return b.Observed("stop acknowledged; camera does not report position").Build(), nil
	}
	select {
	case <-time.After(400 * time.Millisecond):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	second, err2 := a.ptzPosition(ctx, d)
	if err2 != nil {
		return b.Observed("stop acknowledged; second position read failed").Build(), nil
	}
	b = b.Metric("azimuth", float64(second.AbsoluteHigh.Azimuth)).
		Metric("elevation", float64(second.AbsoluteHigh.Elevation)).
		Metric("zoom", float64(second.AbsoluteHigh.AbsoluteZoom))
	if first.AbsoluteHigh != second.AbsoluteHigh {
		return b.Fail(fmt.Sprintf("stop acknowledged but camera is still moving: azimuth %d→%d",
			first.AbsoluteHigh.Azimuth, second.AbsoluteHigh.Azimuth)).Build(), nil
	}
	return b.Observed("position steady across two reads").
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) invokePTZPreset(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	token, _ := args["preset"].(string)
	if token == "" {
		if n, ok := argFloat(args, "preset"); ok {
			token = strconv.Itoa(int(n))
		}
	}
	if token == "" {
		return sdk.NewEffect().Fail("ptz.preset needs a preset number").Build(), nil
	}
	if _, err := strconv.Atoi(token); err != nil {
		// The token goes into a URL path; a non-numeric value is both a
		// protocol error and something that has no business being there.
		return sdk.NewEffect().Fail("ISAPI presets are numbered; got " + token).Build(), nil
	}

	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/presets/%s/goto", d.cfg.Channel, token)
	if err := d.put(ctx, path, ""); err != nil {
		return ptzFault(err, "goto preset"), nil
	}
	b := sdk.NewEffect().Requested("ptz.preset="+token).Protocol("isapi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	st, err := a.ptzPosition(ctx, d)
	if err != nil {
		return b.Observed("preset recalled; camera does not report position").Build(), nil
	}
	p := st.AbsoluteHigh
	return b.Metric("azimuth", float64(p.Azimuth)).
		Metric("elevation", float64(p.Elevation)).
		Metric("zoom", float64(p.AbsoluteZoom)).
		Observed(fmt.Sprintf("at azimuth=%d elevation=%d", p.Azimuth, p.Elevation)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) invokePTZPosition(ctx context.Context, d *device) (effect.Effect, error) {
	st, err := a.ptzPosition(ctx, d)
	if err != nil {
		return ptzFault(err, "position"), nil
	}
	p := st.AbsoluteHigh
	return sdk.NewEffect().
		Metric("azimuth", float64(p.Azimuth)).
		Metric("elevation", float64(p.Elevation)).
		Metric("zoom", float64(p.AbsoluteZoom)).
		Requested(profile.CapPTZPosition).Protocol("isapi").NativeAck(true).
		Observed(fmt.Sprintf("azimuth=%d elevation=%d zoom=%d", p.Azimuth, p.Elevation, p.AbsoluteZoom)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// ptzFault keeps "the camera refused me" distinct from "the camera is not
// there" — the two need different fixes.
func ptzFault(err error, op string) effect.Effect {
	if err == errUnauthorized {
		return sdk.NewEffect().Fail(op + " rejected: " + errUnauthorized.Error()).Build()
	}
	return sdk.NewEffect().Unavailable(op + ": " + err.Error()).Build()
}
