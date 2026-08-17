// Package dahua is the Dahua HTTP API adapter — the CGI interface Dahua
// cameras and NVRs expose, shared by Lorex, Amcrest and the other brands
// built on Dahua firmware.
//
// Why this exists alongside adapters/onvif: the same reasoning as Hikvision.
// ONVIF is the interoperable floor; the vendor API is always on, carries the
// full analytics event vocabulary, and works on units where ONVIF was
// disabled at commissioning.
//
// The wire is not XML or JSON but line-oriented key=value text, and the PTZ
// model is discrete direction codes rather than a velocity vector. Bridging
// that to the uniform ANetLink capability contract is most of what this
// package does — a caller asks for pan=0.5 and never learns whose camera it
// is talking to.
//
// Pure Go: HTTP with digest auth (internal/digest). Honest trust: V2
// (snapshot bytes and PTZ position readback) / A1 (digest authenticates an
// account over the network, not a device identity).
package dahua

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
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
	DefaultPort     = 80
	DefaultRTSPPort = 554
	maxSnapshot     = 16 << 20
	maxText         = 1 << 20
	// maxSpeed is the Dahua PTZ speed scale (1..8). Our [-1,1] velocity maps
	// onto it linearly.
	maxSpeed = 8
)

type DeviceConfig struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	User    string `json:"user"`
	Pass    string `json:"pass"`
	// Channel is 1-based in the media API and 0-based in the PTZ API — a
	// vendor inconsistency this adapter absorbs so callers never see it.
	Channel  int    `json:"channel,omitempty"`
	Subtype  int    `json:"subtype,omitempty"` // 0 main stream, 1 sub stream
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
	cfg     DeviceConfig
	http    *http.Client
	base    string
	streamU string
	caps    map[string]profile.Capability
	hasPTZ  bool
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
	return sdk.Info{Name: "dahua", Protocol: "dahua-cgi", Version: "0.1.0"}
}

type nativeBackend struct{ cfg Config }

func (b nativeBackend) Name() string { return "native" }

func (b nativeBackend) Probe(ctx context.Context) sdk.Availability {
	if len(b.cfg.Devices) == 0 {
		return sdk.Availability{Available: true, Detail: "pure-Go Dahua CGI; no cameras configured"}
	}
	d := newDevice(b.cfg.Devices[0], b.cfg.Timeout)
	kv, err := d.getKV(ctx, "/cgi-bin/magicBox.cgi?action=getDeviceType")
	if err != nil {
		return sdk.Availability{Available: false,
			Detail: fmt.Sprintf("camera %s unreachable: %v", d.cfg.ID, err)}
	}
	return sdk.Availability{Available: true,
		Detail: fmt.Sprintf("pure-Go Dahua CGI; %s is a %s", d.cfg.ID, kv["type"])}
}

func (a *Adapter) Backends() []sdk.Backend { return []sdk.Backend{nativeBackend{a.cfg}} }

func newDevice(dc DeviceConfig, timeout time.Duration) *device {
	if dc.Channel == 0 {
		dc.Channel = 1
	}
	if dc.RTSPPort == 0 {
		dc.RTSPPort = DefaultRTSPPort
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	scheme, port := "http", DefaultPort
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

// ptzChannel is the 0-based index the PTZ CGI wants.
func (d *device) ptzChannel() int { return d.cfg.Channel - 1 }

var errUnauthorized = fmt.Errorf("camera did not accept the credentials")

func (d *device) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return nil, err
	}
	return d.http.Do(req)
}

// getKV performs a GET and parses Dahua's line-oriented key=value response.
// Keys are returned verbatim, including the vendor's own misspellings.
func (d *device) getKV(ctx context.Context, path string) (map[string]string, error) {
	resp, err := d.get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dahua %s returned HTTP %d", path, resp.StatusCode)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxText))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// A CGI that answered 200 with nothing parseable did not do what we
	// asked; reporting success here would let empty responses look fine.
	if len(out) == 0 {
		return nil, fmt.Errorf("dahua %s returned no key=value pairs", path)
	}
	return out, nil
}

// command issues a control CGI and reports whether the camera accepted it.
// Dahua answers "OK" in the body rather than using status codes for
// application-level outcomes, so a 200 alone is not success.
func (d *device) command(ctx context.Context, path string) error {
	resp, err := d.get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxText))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dahua %s returned HTTP %d", path, resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(string(body)), "ok") {
		return fmt.Errorf("dahua refused the command: %s", strings.TrimSpace(string(body)))
	}
	return nil
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
	kv, err := d.getKV(ctx, "/cgi-bin/magicBox.cgi?action=getDeviceType")
	if err != nil {
		return err
	}
	if d.cfg.Vendor == "" {
		d.cfg.Vendor = "Dahua"
	}
	if d.cfg.Model == "" {
		d.cfg.Model = kv["type"]
	}

	host, _, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(d.base, "https://"), "http://"))
	if err != nil {
		return err
	}
	d.streamU = fmt.Sprintf("rtsp://%s:%d/cam/realmonitor?channel=%d&subtype=%d",
		host, d.cfg.RTSPPort, d.cfg.Channel, d.cfg.Subtype)

	// PTZ is discovered: a fixed camera has no protocol capabilities to report.
	_, perr := d.getKV(ctx, fmt.Sprintf(
		"/cgi-bin/ptz.cgi?action=getCurrentProtocolCaps&channel=%d", d.ptzChannel()))
	d.hasPTZ = perr == nil

	d.buildCaps()
	return nil
}

var capOrder = []string{
	profile.CapStreamRTSP, profile.CapStreamSnapshot,
	profile.CapPTZMove, profile.CapPTZStop, profile.CapPTZPreset, profile.CapPTZPosition,
	profile.CapEventMotion, profile.CapEventTamper, profile.CapEventLineCross,
}

func (d *device) buildCaps() {
	d.caps[profile.CapStreamRTSP] = profile.Capability{ID: profile.CapStreamRTSP, Access: profile.AccessGet}
	d.caps[profile.CapStreamSnapshot] = profile.Capability{ID: profile.CapStreamSnapshot, Access: profile.AccessGet}
	if d.hasPTZ {
		d.caps[profile.CapPTZMove] = profile.Capability{ID: profile.CapPTZMove, Access: profile.AccessSet}
		d.caps[profile.CapPTZStop] = profile.Capability{ID: profile.CapPTZStop, Access: profile.AccessSet}
		d.caps[profile.CapPTZPreset] = profile.Capability{ID: profile.CapPTZPreset, Access: profile.AccessSet}
		d.caps[profile.CapPTZPosition] = profile.Capability{ID: profile.CapPTZPosition,
			Access: profile.AccessGet | profile.AccessState}
	}
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
		Schema: 1, Vendor: d.cfg.Vendor, Model: d.cfg.Model, Protocol: "dahua-cgi",
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
	default:
		return sdk.NewEffect().Fail(
			fmt.Sprintf("%q is a state-only capability; subscribe instead of invoking", call.Capability)).Build(), nil
	}
}

func (a *Adapter) invokeStream(ctx context.Context, d *device) (effect.Effect, error) {
	b := sdk.NewEffect().StreamURI(d.streamU).
		Requested(profile.CapStreamRTSP).Protocol("dahua-cgi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))
	if !a.cfg.VerifyStream {
		return b.Observed("uri derived from the vendor scheme, stream not probed").Build(), nil
	}
	sdp, err := rtsp.Describe(ctx, &net.Dialer{Timeout: a.cfg.Timeout}, d.streamU, d.cfg.User, d.cfg.Pass)
	if err != nil {
		return sdk.NewEffect().StreamURI(d.streamU).
			Requested(profile.CapStreamRTSP).Protocol("dahua-cgi").
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
	resp, err := d.get(ctx, fmt.Sprintf("/cgi-bin/snapshot.cgi?channel=%d", d.cfg.Channel))
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
		Requested(profile.CapStreamSnapshot).Protocol("dahua-cgi").NativeAck(true).
		Observed(fmt.Sprintf("%d bytes of %s", len(data), mt)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// --- PTZ ---

// directionCode maps a velocity vector onto Dahua's discrete direction
// vocabulary. Dahua has no notion of a continuous pan/tilt vector: it takes
// one of eight compass codes plus a speed. Diagonals exist, so a combined
// pan and tilt is one command rather than two fighting each other.
func directionCode(pan, tilt float64) string {
	switch {
	case pan > 0 && tilt > 0:
		return "RightUp"
	case pan > 0 && tilt < 0:
		return "RightDown"
	case pan < 0 && tilt > 0:
		return "LeftUp"
	case pan < 0 && tilt < 0:
		return "LeftDown"
	case pan > 0:
		return "Right"
	case pan < 0:
		return "Left"
	case tilt > 0:
		return "Up"
	case tilt < 0:
		return "Down"
	}
	return ""
}

// speedOf converts an ANetLink velocity magnitude to Dahua's 1..8 scale.
// Any non-zero request must produce at least speed 1: rounding a deliberate
// small movement down to "do nothing" would be a silent no-op.
func speedOf(v float64) int {
	s := int(math.Round(math.Abs(v) * maxSpeed))
	if s < 1 {
		s = 1
	}
	if s > maxSpeed {
		s = maxSpeed
	}
	return s
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

// ptzPosition reads pan/tilt/zoom. The response key is "status.Postion" —
// the vendor's own misspelling, present in every firmware to date. Matching
// the correct spelling would silently read nothing.
func (a *Adapter) ptzPosition(ctx context.Context, d *device) (pan, tilt, zoom float64, err error) {
	kv, err := d.getKV(ctx, fmt.Sprintf("/cgi-bin/ptz.cgi?action=getStatus&channel=%d", d.ptzChannel()))
	if err != nil {
		return 0, 0, 0, err
	}
	get := func(i int) float64 {
		f, _ := strconv.ParseFloat(kv[fmt.Sprintf("status.Postion[%d]", i)], 64)
		return f
	}
	return get(0), get(1), get(2), nil
}

func (a *Adapter) ptzCommand(ctx context.Context, d *device, action, code string, arg1, arg2, arg3 int) error {
	return d.command(ctx, fmt.Sprintf(
		"/cgi-bin/ptz.cgi?action=%s&channel=%d&code=%s&arg1=%d&arg2=%d&arg3=%d",
		action, d.ptzChannel(), url.QueryEscape(code), arg1, arg2, arg3))
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
		durMS = int(math.Min(v, 10_000))
	}

	bp, bt, bz, beforeErr := a.ptzPosition(ctx, d)

	// Pan/tilt and zoom are separate axes in Dahua's vocabulary, so a
	// request touching both becomes two commands — and both must be stopped.
	var started []string
	if code := directionCode(pan, tilt); code != "" {
		if err := a.ptzCommand(ctx, d, "start", code, 0, speedOf(math.Max(math.Abs(pan), math.Abs(tilt))), 0); err != nil {
			return ptzFault(err, "ptz start "+code), nil
		}
		started = append(started, code)
	}
	if zoom != 0 {
		code := "ZoomTele"
		if zoom < 0 {
			code = "ZoomWide"
		}
		if err := a.ptzCommand(ctx, d, "start", code, 0, speedOf(zoom), 0); err != nil {
			a.stopAll(context.WithoutCancel(ctx), d, started)
			return ptzFault(err, "ptz start "+code), nil
		}
		started = append(started, code)
	}

	select {
	case <-time.After(time.Duration(durMS) * time.Millisecond):
	case <-ctx.Done():
		a.stopAll(context.WithoutCancel(ctx), d, started)
		return effect.Effect{}, ctx.Err()
	}
	if err := a.stopAll(ctx, d, started); err != nil {
		return sdk.NewEffect().Protocol("dahua-cgi").
			Fail("move issued but stop failed, camera may still be moving: " + err.Error()).Build(), nil
	}

	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.move pan=%g tilt=%g zoom=%g for %dms", pan, tilt, zoom, durMS)).
		Protocol("dahua-cgi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	ap, at, az, afterErr := a.ptzPosition(ctx, d)
	if beforeErr != nil || afterErr != nil {
		return b.Observed("camera does not report PTZ position; movement unverified").Build(), nil
	}
	b = b.Metric("pan", ap).Metric("tilt", at).Metric("zoom", az)
	if bp == ap && bt == at && bz == az {
		return b.Observed(fmt.Sprintf("position unchanged at pan=%g tilt=%g zoom=%g (at limit?)",
			ap, at, az)).Build(), nil
	}
	return b.Observed(fmt.Sprintf("pan %g→%g tilt %g→%g zoom %g→%g", bp, ap, bt, at, bz, az)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// stopAll halts every code that was started. Stopping only the first would
// leave a zoom running after a diagonal pan finished.
func (a *Adapter) stopAll(ctx context.Context, d *device, codes []string) error {
	var firstErr error
	for _, code := range codes {
		if err := a.ptzCommand(ctx, d, "stop", code, 0, 0, 0); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// allStopCodes are every motion code, used when a caller asks for a bare
// stop without saying what is moving.
var allStopCodes = []string{"Left", "Right", "Up", "Down",
	"LeftUp", "LeftDown", "RightUp", "RightDown", "ZoomTele", "ZoomWide"}

func (a *Adapter) invokePTZStop(ctx context.Context, d *device) (effect.Effect, error) {
	if err := a.stopAll(ctx, d, allStopCodes); err != nil {
		return ptzFault(err, "stop"), nil
	}
	b := sdk.NewEffect().Requested(profile.CapPTZStop).Protocol("dahua-cgi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	// Dahua has no move-status word, so a stop is confirmed by the position
	// holding still across two reads.
	p1, t1, z1, err1 := a.ptzPosition(ctx, d)
	if err1 != nil {
		return b.Observed("stop acknowledged; camera does not report position").Build(), nil
	}
	select {
	case <-time.After(400 * time.Millisecond):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	p2, t2, z2, err2 := a.ptzPosition(ctx, d)
	if err2 != nil {
		return b.Observed("stop acknowledged; second position read failed").Build(), nil
	}
	b = b.Metric("pan", p2).Metric("tilt", t2).Metric("zoom", z2)
	if p1 != p2 || t1 != t2 || z1 != z2 {
		return b.Fail(fmt.Sprintf("stop acknowledged but camera is still moving: pan %g→%g", p1, p2)).Build(), nil
	}
	return b.Observed("position steady across two reads").
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) invokePTZPreset(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	var preset int
	if s, ok := args["preset"].(string); ok && s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return sdk.NewEffect().Fail("Dahua presets are numbered; got " + s).Build(), nil
		}
		preset = n
	} else if n, ok := argFloat(args, "preset"); ok {
		preset = int(n)
	} else {
		return sdk.NewEffect().Fail("ptz.preset needs a preset number").Build(), nil
	}
	if preset <= 0 {
		return sdk.NewEffect().Fail(fmt.Sprintf("preset numbers start at 1, got %d", preset)).Build(), nil
	}

	if err := a.ptzCommand(ctx, d, "start", "GotoPreset", 0, preset, 0); err != nil {
		return ptzFault(err, "goto preset"), nil
	}
	b := sdk.NewEffect().Requested(fmt.Sprintf("ptz.preset=%d", preset)).
		Protocol("dahua-cgi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	p, t, z, err := a.ptzPosition(ctx, d)
	if err != nil {
		return b.Observed("preset recalled; camera does not report position").Build(), nil
	}
	return b.Metric("pan", p).Metric("tilt", t).Metric("zoom", z).
		Observed(fmt.Sprintf("at pan=%g tilt=%g zoom=%g", p, t, z)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) invokePTZPosition(ctx context.Context, d *device) (effect.Effect, error) {
	p, t, z, err := a.ptzPosition(ctx, d)
	if err != nil {
		return ptzFault(err, "position"), nil
	}
	return sdk.NewEffect().Metric("pan", p).Metric("tilt", t).Metric("zoom", z).
		Requested(profile.CapPTZPosition).Protocol("dahua-cgi").NativeAck(true).
		Observed(fmt.Sprintf("pan=%g tilt=%g zoom=%g", p, t, z)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func ptzFault(err error, op string) effect.Effect {
	if err == errUnauthorized {
		return sdk.NewEffect().Fail(op + " rejected: " + errUnauthorized.Error()).Build()
	}
	return sdk.NewEffect().Unavailable(op + ": " + err.Error()).Build()
}
