package hikvision

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// settleAfterAbsolute lets the head finish travelling before the position is
// judged. A dome crossing 180° takes real time.
const settleAfterAbsolute = 2 * time.Second

// ISAPI absolute coordinates: azimuth 0..3600 (tenths of a degree),
// elevation -900..900, zoom 1..100. The ANetLink contract is [-1,1] for all
// three, so the mapping lives here and nowhere else.
const (
	isapiAzimuthMax   = 3600.0
	isapiElevationMax = 900.0
	isapiZoomMax      = 100.0
)

type presetList struct {
	Presets []struct {
		ID   int    `xml:"id"`
		Name string `xml:"presetName"`
	} `xml:"PTZPreset"`
}

// panToAzimuth maps [-1,1] onto ISAPI's 0..3600 tenths-of-a-degree circle.
// Pan is circular, so -1 and +1 are the same bearing; the wrap is deliberate.
func panToAzimuth(pan float64) int {
	deg := (pan + 1) / 2 * isapiAzimuthMax
	return int(deg) % int(isapiAzimuthMax)
}

func azimuthToPan(az int) float64 { return float64(az)/isapiAzimuthMax*2 - 1 }

func tiltToElevation(tilt float64) int { return int(tilt * isapiElevationMax) }

func elevationToTilt(el int) float64 { return float64(el) / isapiElevationMax }

func (a *Adapter) invokeAbsolute(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	pan, hasPan := argFloat(args, "pan")
	tilt, hasTilt := argFloat(args, "tilt")
	zoom, hasZoom := argFloat(args, "zoom")
	if !hasPan && !hasTilt && !hasZoom {
		return sdk.NewEffect().Fail("ptz.absolute needs at least one of pan/tilt/zoom").Build(), nil
	}
	for name, v := range map[string]float64{"pan": pan, "tilt": tilt, "zoom": zoom} {
		if v < -1 || v > 1 {
			return sdk.NewEffect().Fail(
				fmt.Sprintf("%s position %v out of range [-1,1]", name, v)).Build(), nil
		}
	}

	// Unspecified axes hold: asking to zoom must not also re-aim the camera.
	before, beforeErr := a.ptzPosition(ctx, d)
	azimuth, elevation := panToAzimuth(pan), tiltToElevation(tilt)
	zoomVal := int(zoom * isapiZoomMax)
	if beforeErr == nil {
		if !hasPan {
			azimuth = before.AbsoluteHigh.Azimuth
		}
		if !hasTilt {
			elevation = before.AbsoluteHigh.Elevation
		}
		if !hasZoom {
			zoomVal = before.AbsoluteHigh.AbsoluteZoom
		}
	}

	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><PTZData><AbsoluteHigh>`+
		`<elevation>%d</elevation><azimuth>%d</azimuth><absoluteZoom>%d</absoluteZoom>`+
		`</AbsoluteHigh></PTZData>`, elevation, azimuth, zoomVal)
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/absolute", d.cfg.Channel)
	if err := d.put(ctx, path, body); err != nil {
		return ptzFault(err, "absolute move"), nil
	}

	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.absolute pan=%g tilt=%g zoom=%g", pan, tilt, zoom)).
		Protocol("isapi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	select {
	case <-time.After(settleAfterAbsolute):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	after, err := a.ptzPosition(ctx, d)
	if err != nil {
		return b.Observed("move accepted; camera does not report position").Build(), nil
	}
	p := after.AbsoluteHigh
	b = b.Metric("azimuth", float64(p.Azimuth)).
		Metric("elevation", float64(p.Elevation)).
		Metric("zoom", float64(p.AbsoluteZoom)).
		Metric("pan", azimuthToPan(p.Azimuth)).
		Metric("tilt", elevationToTilt(p.Elevation))

	// 50 tenths of a degree is 5°, well inside a dome's own repeatability.
	if abs(p.Azimuth-azimuth) > 50 || abs(p.Elevation-elevation) > 50 {
		return b.Observed(fmt.Sprintf(
			"asked azimuth=%d elevation=%d, camera reports %d/%d — did not arrive",
			azimuth, elevation, p.Azimuth, p.Elevation)).Build(), nil
	}
	return b.Observed(fmt.Sprintf("arrived at azimuth=%d elevation=%d zoom=%d",
		p.Azimuth, p.Elevation, p.AbsoluteZoom)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (a *Adapter) invokeHome(ctx context.Context, d *device) (effect.Effect, error) {
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/homeposition/goto", d.cfg.Channel)
	if err := d.put(ctx, path, ""); err != nil {
		return ptzFault(err, "goto home"), nil
	}
	b := sdk.NewEffect().Requested(profile.CapPTZHome).Protocol("isapi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	select {
	case <-time.After(settleAfterAbsolute):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	st, err := a.ptzPosition(ctx, d)
	if err != nil {
		return b.Observed("home recalled; camera does not report position").Build(), nil
	}
	p := st.AbsoluteHigh
	return b.Metric("azimuth", float64(p.Azimuth)).Metric("elevation", float64(p.Elevation)).
		Observed(fmt.Sprintf("at azimuth=%d elevation=%d", p.Azimuth, p.Elevation)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// invokePresetList enumerates stored presets with their names, so an agent
// can act on "the loading bay" rather than on preset 7.
func (a *Adapter) invokePresetList(ctx context.Context, d *device) (effect.Effect, error) {
	var pl presetList
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/presets", d.cfg.Channel)
	if err := d.getXML(ctx, path, &pl); err != nil {
		return ptzFault(err, "preset list"), nil
	}
	type preset struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	list := make([]preset, 0, len(pl.Presets))
	for _, p := range pl.Presets {
		list = append(list, preset{Token: strconv.Itoa(p.ID), Name: strings.TrimSpace(p.Name)})
	}
	blob, err := json.Marshal(list)
	if err != nil {
		return sdk.NewEffect().Fail(err.Error()).Build(), nil
	}
	return sdk.NewEffect().
		Snapshot(blob, "application/json").
		Metric("preset_count", float64(len(list))).
		Requested(profile.CapPTZPresetList).Protocol("isapi").NativeAck(true).
		Observed(fmt.Sprintf("%d preset(s)", len(list))).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) invokePresetSave(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	var id int
	if n, ok := argFloat(args, "preset"); ok {
		id = int(n)
	} else if s, ok := args["preset"].(string); ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			return sdk.NewEffect().Fail("ISAPI presets are numbered; got " + s).Build(), nil
		}
		id = v
	}
	if id <= 0 {
		return sdk.NewEffect().Fail("ptz.preset.save needs a positive preset number").Build(), nil
	}
	name, _ := args["name"].(string)
	if name == "" {
		name = fmt.Sprintf("preset %d", id)
	}

	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><PTZPreset>`+
		`<id>%d</id><presetName>%s</presetName></PTZPreset>`, id, xmlEscape(name))
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/presets/%d", d.cfg.Channel, id)
	if err := d.put(ctx, path, body); err != nil {
		return ptzFault(err, "save preset"), nil
	}

	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.preset.save %d=%q", id, name)).
		Protocol("isapi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	// Read the list back: a full preset store accepts the write and keeps
	// nothing, which the acknowledgement alone would hide.
	var pl presetList
	if err := d.getXML(ctx, fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/presets", d.cfg.Channel), &pl); err != nil {
		return b.Observed("preset accepted; could not read the list back").Build(), nil
	}
	for _, p := range pl.Presets {
		if p.ID == id {
			return b.Metric("preset_saved", 1).
				Observed(fmt.Sprintf("stored as %d %q", p.ID, strings.TrimSpace(p.Name))).
				Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
		}
	}
	return b.Fail("camera accepted the preset but it is not in the list — store may be full").Build(), nil
}

// invokeIRCut switches the infrared cut filter. ISAPI names the *mode*
// (day/night/auto) rather than the filter, the opposite of ONVIF — mapping
// them the same way would flip every Hikvision camera.
func (a *Adapter) invokeIRCut(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	mode, _ := args["mode"].(string)
	path := fmt.Sprintf("/ISAPI/Image/channels/%d/ircutFilter", d.cfg.Channel)

	if mode != "" {
		isapiMode, ok := ircutMode(mode)
		if !ok {
			return sdk.NewEffect().Fail(
				`imaging.ircut mode must be "on", "off" or "auto", got ` + mode).Build(), nil
		}
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><IrcutFilter>`+
			`<IrcutFilterType>%s</IrcutFilterType></IrcutFilter>`, isapiMode)
		if err := d.put(ctx, path, body); err != nil {
			return ptzFault(err, "set imaging.ircut"), nil
		}
	}

	var cur struct {
		Type string `xml:"IrcutFilterType"`
	}
	if err := d.getXML(ctx, path, &cur); err != nil {
		return ptzFault(err, "read imaging.ircut"), nil
	}
	b := sdk.NewEffect().
		Metric("imaging_ircut", ircutMetric(cur.Type)).
		Protocol("isapi").NativeAck(true).Observed(cur.Type).
		Trust(uint8(profile.V2), uint8(profile.A1))
	if mode == "" {
		return b.Requested(profile.CapImagingIRCut).Verified().Build(), nil
	}
	b = b.Requested("imaging.ircut=" + mode)
	if want, _ := ircutMode(mode); !strings.EqualFold(cur.Type, want) {
		return b.Fail(fmt.Sprintf("asked for %s, camera reports %s", want, cur.Type)).Build(), nil
	}
	return b.Verified().Build(), nil
}

// ircutMode maps our vocabulary to ISAPI's. Note the inversion against
// ONVIF: ISAPI's "day" means daylight mode (filter engaged), which ONVIF
// calls IrCutFilter=ON. Same physical state, opposite naming convention.
func ircutMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "on", "day":
		return "day", true
	case "off", "night":
		return "night", true
	case "auto":
		return "auto", true
	}
	return "", false
}

func ircutMetric(mode string) float64 {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "day":
		return 1
	case "night":
		return 0
	case "auto":
		return 2
	}
	return -1
}

func (a *Adapter) invokeReboot(ctx context.Context, d *device) (effect.Effect, error) {
	if err := d.put(ctx, "/ISAPI/System/reboot", ""); err != nil {
		return ptzFault(err, "reboot"), nil
	}
	// Unverifiable by construction: the camera is on its way down.
	return sdk.NewEffect().
		Requested(profile.CapSystemReboot).Protocol("isapi").NativeAck(true).
		Observed("reboot accepted; the next successful call is the only real confirmation").
		Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
}

func (a *Adapter) invokeSystemInfo(ctx context.Context, d *device) (effect.Effect, error) {
	var info deviceInfo
	if err := d.getXML(ctx, "/ISAPI/System/deviceInfo", &info); err != nil {
		return ptzFault(err, "system info"), nil
	}
	blob, err := json.Marshal(map[string]string{
		"name":     info.DeviceName,
		"model":    info.Model,
		"firmware": info.Firmware,
		"serial":   info.SerialNumber,
		"type":     info.DeviceType,
	})
	if err != nil {
		return sdk.NewEffect().Fail(err.Error()).Build(), nil
	}
	return sdk.NewEffect().
		Snapshot(blob, "application/json").
		Requested(profile.CapSystemInfo).Protocol("isapi").NativeAck(true).
		Observed(info.Model+" fw "+info.Firmware).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// xmlEscape guards operator-supplied strings in request bodies — preset
// names come from humans and routinely contain ampersands.
func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
