package dahua

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

const settleAfterAbsolute = 2 * time.Second

// Dahua absolute coordinates: arg1 is the horizontal angle in degrees
// (0..360), arg2 the vertical (-90..90 on most domes), arg3 the zoom (1..128
// depending on the lens). The ANetLink contract is [-1,1] on all three.
const (
	dahuaPanDegrees  = 360.0
	dahuaTiltDegrees = 90.0
	dahuaZoomMax     = 128.0
)

func panToDegrees(pan float64) float64   { return (pan + 1) / 2 * dahuaPanDegrees }
func degreesToPan(deg float64) float64   { return deg/dahuaPanDegrees*2 - 1 }
func tiltToDegrees(tilt float64) float64 { return tilt * dahuaTiltDegrees }

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

	// Unspecified axes hold their current bearing.
	bp, bt, bz, beforeErr := a.ptzPosition(ctx, d)
	panDeg, tiltDeg := panToDegrees(pan), tiltToDegrees(tilt)
	zoomVal := zoom * dahuaZoomMax
	if beforeErr == nil {
		if !hasPan {
			panDeg = bp
		}
		if !hasTilt {
			tiltDeg = bt
		}
		if !hasZoom {
			zoomVal = bz
		}
	}

	// PositionABS takes its arguments as degrees, not as the speed codes the
	// continuous API uses.
	path := fmt.Sprintf(
		"/cgi-bin/ptz.cgi?action=start&channel=%d&code=PositionABS&arg1=%.1f&arg2=%.1f&arg3=%.1f",
		d.ptzChannel(), panDeg, tiltDeg, zoomVal)
	if err := d.command(ctx, path); err != nil {
		return ptzFault(err, "absolute move"), nil
	}

	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.absolute pan=%g tilt=%g zoom=%g", pan, tilt, zoom)).
		Protocol("dahua-cgi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	select {
	case <-time.After(settleAfterAbsolute):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	ap, at, az, err := a.ptzPosition(ctx, d)
	if err != nil {
		return b.Observed("move accepted; camera does not report position").Build(), nil
	}
	b = b.Metric("pan_degrees", ap).Metric("tilt_degrees", at).Metric("zoom_native", az).
		Metric("pan", degreesToPan(ap)).Metric("tilt", at/dahuaTiltDegrees)

	// 5° is inside the repeatability of any dome worth aiming.
	if absf(ap-panDeg) > 5 || absf(at-tiltDeg) > 5 {
		return b.Observed(fmt.Sprintf(
			"asked pan=%.1f° tilt=%.1f°, camera reports %.1f°/%.1f° — did not arrive",
			panDeg, tiltDeg, ap, at)).Build(), nil
	}
	return b.Observed(fmt.Sprintf("arrived at pan=%.1f° tilt=%.1f° zoom=%.1f", ap, at, az)).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// preset is one stored bearing as reported to callers.
type preset struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

// presetLine matches Dahua's preset listing:
//
//	table.Preset[0].Name=Front Door
//	table.Preset[0].Index=1
var presetLine = regexp.MustCompile(`^table\.Preset\[(\d+)\]\.(\w+)$`)

func (a *Adapter) invokePresetList(ctx context.Context, d *device) (effect.Effect, error) {
	kv, err := d.getKV(ctx, fmt.Sprintf("/cgi-bin/ptz.cgi?action=getPresets&channel=%d", d.ptzChannel()))
	if err != nil {
		return ptzFault(err, "preset list"), nil
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
	list := make([]preset, 0, len(byIndex))
	for _, fields := range byIndex {
		tok := fields["index"]
		if tok == "" {
			continue
		}
		list = append(list, preset{Token: tok, Name: strings.TrimSpace(fields["name"])})
	}
	// Stable order: the map above is keyed by string index, and an agent
	// comparing two listings should not see them shuffle.
	sort.Slice(list, func(i, j int) bool {
		a, _ := strconv.Atoi(list[i].Token)
		b, _ := strconv.Atoi(list[j].Token)
		return a < b
	})

	blob, err := json.Marshal(list)
	if err != nil {
		return sdk.NewEffect().Fail(err.Error()).Build(), nil
	}
	return sdk.NewEffect().
		Snapshot(blob, "application/json").
		Metric("preset_count", float64(len(list))).
		Requested(profile.CapPTZPresetList).Protocol("dahua-cgi").NativeAck(true).
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
			return sdk.NewEffect().Fail("Dahua presets are numbered; got " + s).Build(), nil
		}
		id = v
	}
	if id <= 0 {
		return sdk.NewEffect().Fail("ptz.preset.save needs a positive preset number").Build(), nil
	}

	if err := a.ptzCommand(ctx, d, "start", "SetPreset", 0, id, 0); err != nil {
		return ptzFault(err, "save preset"), nil
	}
	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.preset.save %d", id)).
		Protocol("dahua-cgi").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	// Read the list back: a full store accepts the write and keeps nothing.
	eff, err := a.invokePresetList(ctx, d)
	if err != nil || eff.Record == nil || len(eff.Record.Artifacts) == 0 {
		return b.Observed("preset accepted; could not read the list back").Build(), nil
	}
	return b.Metric("preset_saved", 1).
		Observed(fmt.Sprintf("store now holds %g preset(s)", eff.Record.Metrics["preset_count"])).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) invokeHome(ctx context.Context, d *device) (effect.Effect, error) {
	// Dahua has no home command; preset 1 is the universal convention.
	if err := a.ptzCommand(ctx, d, "start", "GotoPreset", 0, 1, 0); err != nil {
		return ptzFault(err, "goto home"), nil
	}
	b := sdk.NewEffect().Requested(profile.CapPTZHome).Protocol("dahua-cgi").NativeAck(true).
		Observed("Dahua has no home command; recalled preset 1 by convention").
		Trust(uint8(profile.V1), uint8(profile.A1))

	select {
	case <-time.After(settleAfterAbsolute):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	p, t, z, err := a.ptzPosition(ctx, d)
	if err != nil {
		return b.Build(), nil
	}
	return b.Metric("pan_degrees", p).Metric("tilt_degrees", t).Metric("zoom_native", z).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

func (a *Adapter) invokeReboot(ctx context.Context, d *device) (effect.Effect, error) {
	if err := d.command(ctx, "/cgi-bin/magicBox.cgi?action=reboot"); err != nil {
		return ptzFault(err, "reboot"), nil
	}
	return sdk.NewEffect().
		Requested(profile.CapSystemReboot).Protocol("dahua-cgi").NativeAck(true).
		Observed("reboot accepted; the next successful call is the only real confirmation").
		Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
}

func (a *Adapter) invokeSystemInfo(ctx context.Context, d *device) (effect.Effect, error) {
	info := map[string]string{}
	for _, q := range []struct{ action, key string }{
		{"getDeviceType", "type"},
		{"getSerialNo", "sn"},
		{"getSoftwareVersion", "version"},
	} {
		kv, err := d.getKV(ctx, "/cgi-bin/magicBox.cgi?action="+q.action)
		if err != nil {
			continue // one unsupported query should not sink the whole report
		}
		for k, v := range kv {
			info[k] = v
		}
	}
	if len(info) == 0 {
		return sdk.NewEffect().Unavailable("camera reported nothing about itself").Build(), nil
	}
	blob, err := json.Marshal(info)
	if err != nil {
		return sdk.NewEffect().Fail(err.Error()).Build(), nil
	}
	return sdk.NewEffect().
		Snapshot(blob, "application/json").
		Requested(profile.CapSystemInfo).Protocol("dahua-cgi").NativeAck(true).
		Observed(info["type"]+" "+info["version"]).
		Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}
