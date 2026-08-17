package onvif

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// settleAfterAbsolute is how long to let the head travel before judging
// where it ended up. A dome crossing 180° takes real time, and reading the
// position immediately would report the starting bearing as the result.
const settleAfterAbsolute = 2 * time.Second

// invokeAbsolute drives to an absolute bearing and verifies arrival.
//
// This is the capability that makes a camera useful to an agent. "Point at
// the loading bay" is a position; expressing it as a timed velocity nudge —
// all ptz.move can do — makes the outcome depend on how loaded the camera
// happens to be at that moment.
func (a *Adapter) invokeAbsolute(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	pan, hasPan := argFloat(args, "pan")
	tilt, hasTilt := argFloat(args, "tilt")
	zoom, hasZoom := argFloat(args, "zoom")
	if !hasPan && !hasTilt && !hasZoom {
		return sdk.NewEffect().Fail("ptz.absolute needs at least one of pan/tilt/zoom").Build(), nil
	}
	for name, v := range map[string]float64{"pan": pan, "tilt": tilt, "zoom": zoom} {
		if v < -1 || v > 1 || math.IsNaN(v) {
			return sdk.NewEffect().Fail(
				fmt.Sprintf("%s position %v out of range [-1,1]", name, v)).Build(), nil
		}
	}

	// Unspecified axes hold their current bearing rather than snapping to
	// zero: asking to zoom should not also re-aim the camera.
	cur, curErr := a.ptzStatus(ctx, d)
	if curErr == nil {
		p := cur.Body.PTZStatus.Position
		if !hasPan {
			pan = p.PanTilt.X
		}
		if !hasTilt {
			tilt = p.PanTilt.Y
		}
		if !hasZoom {
			zoom = p.Zoom.X
		}
	}

	speedAttr := ""
	if s, ok := argFloat(args, "speed"); ok && s > 0 && s <= 1 {
		speedAttr = fmt.Sprintf(`<tptz:Speed><tt:PanTilt x="%g" y="%g"/><tt:Zoom x="%g"/></tptz:Speed>`, s, s, s)
	}
	body := fmt.Sprintf(
		`<tptz:AbsoluteMove><tptz:ProfileToken>%s</tptz:ProfileToken>`+
			`<tptz:Position><tt:PanTilt x="%g" y="%g"/><tt:Zoom x="%g"/></tptz:Position>%s`+
			`</tptz:AbsoluteMove>`, d.profTok, pan, tilt, zoom, speedAttr)
	if err := d.cli.call(ctx, d.ptzX, nsPTZ+"/AbsoluteMove", body, nil); err != nil {
		return ptzFault(err, "AbsoluteMove"), nil
	}

	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.absolute pan=%g tilt=%g zoom=%g", pan, tilt, zoom)).
		Protocol("onvif").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	select {
	case <-time.After(settleAfterAbsolute):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	st, err := a.ptzStatus(ctx, d)
	if err != nil {
		return b.Observed("move accepted; camera does not report position").Build(), nil
	}
	p := st.Body.PTZStatus.Position
	b = b.Metric("pan", p.PanTilt.X).Metric("tilt", p.PanTilt.Y).Metric("zoom", p.Zoom.X)

	// Absolute positioning is the one PTZ operation where "did it arrive"
	// has a real answer: compare where we asked against where it is.
	const tol = 0.05
	off := math.Abs(p.PanTilt.X-pan) + math.Abs(p.PanTilt.Y-tilt)
	if off > tol*2 {
		return b.Observed(fmt.Sprintf(
			"asked pan=%g tilt=%g, camera reports pan=%g tilt=%g — did not arrive",
			pan, tilt, p.PanTilt.X, p.PanTilt.Y)).Build(), nil
	}
	return b.Observed(fmt.Sprintf("arrived at pan=%g tilt=%g zoom=%g",
		p.PanTilt.X, p.PanTilt.Y, p.Zoom.X)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// invokeHome sends the head to its configured home bearing.
func (a *Adapter) invokeHome(ctx context.Context, d *device) (effect.Effect, error) {
	body := `<tptz:GotoHomePosition><tptz:ProfileToken>` + d.profTok +
		`</tptz:ProfileToken></tptz:GotoHomePosition>`
	if err := d.cli.call(ctx, d.ptzX, nsPTZ+"/GotoHomePosition", body, nil); err != nil {
		return ptzFault(err, "GotoHomePosition"), nil
	}
	b := sdk.NewEffect().Requested(profile.CapPTZHome).Protocol("onvif").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	select {
	case <-time.After(settleAfterAbsolute):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	st, err := a.ptzStatus(ctx, d)
	if err != nil {
		return b.Observed("home recalled; camera does not report position").Build(), nil
	}
	p := st.Body.PTZStatus.Position
	return b.Metric("pan", p.PanTilt.X).Metric("tilt", p.PanTilt.Y).Metric("zoom", p.Zoom.X).
		Observed(fmt.Sprintf("at pan=%g tilt=%g zoom=%g", p.PanTilt.X, p.PanTilt.Y, p.Zoom.X)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// invokePresetList enumerates stored presets with their names.
//
// Names are the point. An agent should be able to act on "the loading bay"
// without anyone having told it that the loading bay is preset 7 — the
// operator already named it when they stored it.
func (a *Adapter) invokePresetList(ctx context.Context, d *device) (effect.Effect, error) {
	var pr presetsResponse
	body := `<tptz:GetPresets><tptz:ProfileToken>` + d.profTok + `</tptz:ProfileToken></tptz:GetPresets>`
	if err := d.cli.call(ctx, d.ptzX, nsPTZ+"/GetPresets", body, &pr); err != nil {
		return ptzFault(err, "GetPresets"), nil
	}
	type preset struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	list := make([]preset, 0, len(pr.Body.Presets))
	for _, p := range pr.Body.Presets {
		list = append(list, preset{Token: p.Token, Name: strings.TrimSpace(p.Name)})
	}
	blob, err := json.Marshal(list)
	if err != nil {
		return sdk.NewEffect().Fail(err.Error()).Build(), nil
	}
	// The list is content, not a measurement — it rides as an artifact so
	// the caller gets the names, while the count is the metric.
	return sdk.NewEffect().
		Snapshot(blob, "application/json").
		Metric("preset_count", float64(len(list))).
		Requested(profile.CapPTZPresetList).Protocol("onvif").NativeAck(true).
		Observed(fmt.Sprintf("%d preset(s)", len(list))).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// invokePresetSave stores the current bearing under a name.
func (a *Adapter) invokePresetSave(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	name, _ := args["name"].(string)
	token, _ := args["preset"].(string)
	if name == "" && token == "" {
		return sdk.NewEffect().Fail("ptz.preset.save needs a name (to create) or a preset token (to overwrite)").Build(), nil
	}

	body := `<tptz:SetPreset><tptz:ProfileToken>` + d.profTok + `</tptz:ProfileToken>`
	if name != "" {
		body += `<tptz:PresetName>` + xmlEscape(name) + `</tptz:PresetName>`
	}
	if token != "" {
		body += `<tptz:PresetToken>` + xmlEscape(token) + `</tptz:PresetToken>`
	}
	body += `</tptz:SetPreset>`

	var res setPresetResponse
	if err := d.cli.call(ctx, d.ptzX, nsPTZ+"/SetPreset", body, &res); err != nil {
		return ptzFault(err, "SetPreset"), nil
	}
	assigned := strings.TrimSpace(res.Body.Token)
	if assigned == "" {
		assigned = token
	}

	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.preset.save name=%q token=%q", name, token)).
		Protocol("onvif").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	// Verify by reading the preset list back: a camera whose preset store is
	// full accepts SetPreset and quietly keeps nothing.
	var pr presetsResponse
	if err := d.cli.call(ctx, d.ptzX, nsPTZ+"/GetPresets",
		`<tptz:GetPresets><tptz:ProfileToken>`+d.profTok+`</tptz:ProfileToken></tptz:GetPresets>`,
		&pr); err != nil {
		return b.Observed("preset accepted; could not read the list back").Build(), nil
	}
	for _, p := range pr.Body.Presets {
		if (assigned != "" && p.Token == assigned) || (name != "" && strings.EqualFold(strings.TrimSpace(p.Name), name)) {
			return b.Metric("preset_saved", 1).
				Observed(fmt.Sprintf("stored as token %q name %q", p.Token, strings.TrimSpace(p.Name))).
				Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
		}
	}
	return b.Fail("camera accepted the preset but it is not in the preset list — store may be full").Build(), nil
}

// invokeTour starts or stops a patrol. ONVIF's preset tours are optional and
// vendor coverage is thin, so an unsupported camera says so plainly.
func (a *Adapter) invokeTour(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	tour, _ := args["tour"].(string)
	if tour == "" {
		tour = "1"
	}
	on := true
	if v, ok := args["on"].(bool); ok {
		on = v
	}
	op, verb := "Start", "start"
	if !on {
		op, verb = "Stop", "stop"
	}
	body := fmt.Sprintf(
		`<tptz:OperatePresetTour><tptz:ProfileToken>%s</tptz:ProfileToken>`+
			`<tptz:PresetTourToken>%s</tptz:PresetTourToken>`+
			`<tptz:Operation>%s</tptz:Operation></tptz:OperatePresetTour>`,
		d.profTok, xmlEscape(tour), op)
	if err := d.cli.call(ctx, d.ptzX, nsPTZ+"/OperatePresetTour", body, nil); err != nil {
		return ptzFault(err, verb+" preset tour"), nil
	}
	// There is no tour-state readback in ONVIF, so this is honestly
	// UNVERIFIED: the camera acknowledged, nothing confirmed motion.
	return sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.tour %s=%s", tour, verb)).
		Protocol("onvif").NativeAck(true).
		Observed("tour "+verb+" accepted; ONVIF reports no tour state to verify against").
		Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
}

// invokeReboot restarts the device.
func (a *Adapter) invokeReboot(ctx context.Context, d *device) (effect.Effect, error) {
	var res struct {
		Body struct {
			Message string `xml:"Message"`
		} `xml:"Body>SystemRebootResponse"`
	}
	if err := d.cli.call(ctx, d.deviceX, nsDevice+"/SystemReboot",
		`<tds:SystemReboot/>`, &res); err != nil {
		return ptzFault(err, "SystemReboot"), nil
	}
	// A reboot cannot be verified from here — the camera is on its way down,
	// and the next successful call is the only real confirmation. Claiming
	// OK because the request was accepted would be exactly the confusion the
	// effect model exists to prevent.
	return sdk.NewEffect().
		Requested(profile.CapSystemReboot).Protocol("onvif").NativeAck(true).
		Observed("reboot accepted: "+strings.TrimSpace(res.Body.Message)).
		Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
}

// invokeSystemInfo reports what the device says about itself.
func (a *Adapter) invokeSystemInfo(ctx context.Context, d *device) (effect.Effect, error) {
	var info deviceInformation
	if err := d.cli.call(ctx, d.deviceX, nsDevice+"/GetDeviceInformation",
		`<tds:GetDeviceInformation/>`, &info); err != nil {
		return ptzFault(err, "GetDeviceInformation"), nil
	}
	blob, err := json.Marshal(map[string]string{
		"manufacturer": info.Body.Manufacturer,
		"model":        info.Body.Model,
		"firmware":     info.Body.FirmwareVersion,
		"serial":       info.Body.SerialNumber,
		"hardware":     info.Body.HardwareID,
	})
	if err != nil {
		return sdk.NewEffect().Fail(err.Error()).Build(), nil
	}
	return sdk.NewEffect().
		Snapshot(blob, "application/json").
		Requested(profile.CapSystemInfo).Protocol("onvif").NativeAck(true).
		Observed(info.Body.Manufacturer+" "+info.Body.Model+" fw "+info.Body.FirmwareVersion).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// xmlEscape guards operator-supplied strings that go into request bodies.
// Preset names come from humans and routinely contain ampersands.
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
