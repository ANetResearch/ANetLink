package onvif

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// nsImaging is the ONVIF imaging service — brightness, contrast, saturation,
// sharpness, focus and the IR cut filter.
const nsImaging = "http://www.onvif.org/ver20/imaging/wsdl"

// imagingOptions carries the vendor's actual ranges. ONVIF does not
// normalise these: one camera's brightness runs 0–100, another's 0–255, a
// third's -128–127. Every value in the ANetLink contract is [0,1] and the
// conversion happens here, because a caller asking for "half brightness"
// should not have to know whose sensor it is.
type imagingOptions struct {
	Body struct {
		Options struct {
			Brightness struct {
				Min float64 `xml:"Min"`
				Max float64 `xml:"Max"`
			} `xml:"Brightness"`
			Contrast struct {
				Min float64 `xml:"Min"`
				Max float64 `xml:"Max"`
			} `xml:"Contrast"`
			ColorSaturation struct {
				Min float64 `xml:"Min"`
				Max float64 `xml:"Max"`
			} `xml:"ColorSaturation"`
			Sharpness struct {
				Min float64 `xml:"Min"`
				Max float64 `xml:"Max"`
			} `xml:"Sharpness"`
		} `xml:"ImagingOptions"`
	} `xml:"Body>GetOptionsResponse"`
}

type imagingSettings struct {
	Body struct {
		Settings struct {
			Brightness      *float64 `xml:"Brightness"`
			Contrast        *float64 `xml:"Contrast"`
			ColorSaturation *float64 `xml:"ColorSaturation"`
			Sharpness       *float64 `xml:"Sharpness"`
			IrCutFilter     string   `xml:"IrCutFilter"`
		} `xml:"ImagingSettings"`
	} `xml:"Body>GetImagingSettingsResponse"`
}

type videoSourcesResponse struct {
	Body struct {
		Sources []struct {
			Token string `xml:"token,attr"`
		} `xml:"VideoSources"`
	} `xml:"Body>GetVideoSourcesResponse"`
}

type presetsResponse struct {
	Body struct {
		Presets []struct {
			Token string `xml:"token,attr"`
			Name  string `xml:"Name"`
		} `xml:"Preset"`
	} `xml:"Body>GetPresetsResponse"`
}

type setPresetResponse struct {
	Body struct {
		Token string `xml:"PresetToken"`
	} `xml:"Body>SetPresetResponse"`
}

// vendorRange is one imaging parameter's native span.
type vendorRange struct{ min, max float64 }

// toNative maps [0,1] onto the vendor's own range.
func (r vendorRange) toNative(v float64) float64 {
	if r.max <= r.min {
		return v // no usable range reported; pass the value through
	}
	return r.min + v*(r.max-r.min)
}

// toNormal maps a vendor value back to [0,1] so a readback is comparable to
// what was asked for.
func (r vendorRange) toNormal(v float64) float64 {
	if r.max <= r.min {
		return v
	}
	return (v - r.min) / (r.max - r.min)
}

// imagingParam names one settable imaging field and how to reach it.
type imagingParam struct {
	cap     string
	element string // the ONVIF element name inside ImagingSettings
	rng     func(*device) vendorRange
	read    func(imagingSettings) *float64
}

var imagingParams = []imagingParam{
	{profile.CapImagingBrightness, "Brightness",
		func(d *device) vendorRange { return d.imgRange.brightness },
		func(s imagingSettings) *float64 { return s.Body.Settings.Brightness }},
	{profile.CapImagingContrast, "Contrast",
		func(d *device) vendorRange { return d.imgRange.contrast },
		func(s imagingSettings) *float64 { return s.Body.Settings.Contrast }},
	{profile.CapImagingSaturation, "ColorSaturation",
		func(d *device) vendorRange { return d.imgRange.saturation },
		func(s imagingSettings) *float64 { return s.Body.Settings.ColorSaturation }},
	{profile.CapImagingSharpness, "Sharpness",
		func(d *device) vendorRange { return d.imgRange.sharpness },
		func(s imagingSettings) *float64 { return s.Body.Settings.Sharpness }},
}

func lookupImagingParam(capID string) (imagingParam, bool) {
	for _, p := range imagingParams {
		if p.cap == capID {
			return p, true
		}
	}
	return imagingParam{}, false
}

// resolveImaging finds the video source the imaging service addresses and
// learns the vendor's parameter ranges. Absence of the service is not an
// error: plenty of cameras expose media and PTZ but no imaging control.
func (a *Adapter) resolveImaging(ctx context.Context, d *device) {
	if d.imagingX == "" {
		return
	}
	var srcs videoSourcesResponse
	if err := d.cli.call(ctx, d.mediaX, nsMedia+"/GetVideoSources",
		`<trt:GetVideoSources/>`, &srcs); err != nil || len(srcs.Body.Sources) == 0 {
		d.imagingX = "" // no source token means nothing to address
		return
	}
	d.videoSrc = srcs.Body.Sources[0].Token

	var opts imagingOptions
	if err := d.cli.call(ctx, d.imagingX, nsImaging+"/GetOptions",
		`<timg:GetOptions><timg:VideoSourceToken>`+d.videoSrc+
			`</timg:VideoSourceToken></timg:GetOptions>`, &opts); err != nil {
		d.imagingX = ""
		return
	}
	o := opts.Body.Options
	d.imgRange = imagingRanges{
		brightness: vendorRange{o.Brightness.Min, o.Brightness.Max},
		contrast:   vendorRange{o.Contrast.Min, o.Contrast.Max},
		saturation: vendorRange{o.ColorSaturation.Min, o.ColorSaturation.Max},
		sharpness:  vendorRange{o.Sharpness.Min, o.Sharpness.Max},
	}
	// Which parameters the camera actually offers is exactly which ones came
	// back with a usable range.
	d.imgAvail = map[string]bool{}
	for _, p := range imagingParams {
		r := p.rng(d)
		if r.max > r.min {
			d.imgAvail[p.cap] = true
		}
	}
}

func (a *Adapter) imagingSettings(ctx context.Context, d *device) (imagingSettings, error) {
	var s imagingSettings
	err := d.cli.call(ctx, d.imagingX, nsImaging+"/GetImagingSettings",
		`<timg:GetImagingSettings><timg:VideoSourceToken>`+d.videoSrc+
			`</timg:VideoSourceToken></timg:GetImagingSettings>`, &s)
	return s, err
}

// invokeImaging sets or reads one imaging parameter, verifying a set by
// reading the value back — the camera may clamp, round to its own step, or
// ignore the request entirely.
func (a *Adapter) invokeImaging(ctx context.Context, d *device, capID string, args map[string]any) (effect.Effect, error) {
	p, ok := lookupImagingParam(capID)
	if !ok {
		return sdk.NewEffect().Fail("unknown imaging parameter " + capID).Build(), nil
	}
	rng := p.rng(d)

	want, isSet := argFloat(args, "value")
	if isSet {
		if want < 0 || want > 1 {
			return sdk.NewEffect().Fail(
				fmt.Sprintf("%s value %v out of range [0,1]", capID, want)).Build(), nil
		}
		body := fmt.Sprintf(
			`<timg:SetImagingSettings><timg:VideoSourceToken>%s</timg:VideoSourceToken>`+
				`<timg:ImagingSettings><tt:%s>%g</tt:%s></timg:ImagingSettings>`+
				`</timg:SetImagingSettings>`,
			d.videoSrc, p.element, rng.toNative(want), p.element)
		if err := d.cli.call(ctx, d.imagingX, nsImaging+"/SetImagingSettings", body, nil); err != nil {
			return ptzFault(err, "set "+capID), nil
		}
	}

	s, err := a.imagingSettings(ctx, d)
	if err != nil {
		if isSet {
			// Sent, but nothing read it back. Not a failure and not a success.
			return sdk.NewEffect().
				Requested(fmt.Sprintf("%s=%g", capID, want)).Protocol("onvif").NativeAck(true).
				Observed("camera accepted the setting but does not report it back").
				Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
		}
		return ptzFault(err, "read "+capID), nil
	}
	got := p.read(s)
	if got == nil {
		return sdk.NewEffect().Unavailable(capID + ": camera reports no value").Build(), nil
	}
	norm := rng.toNormal(*got)

	b := sdk.NewEffect().
		Metric(strings.ReplaceAll(capID, ".", "_"), norm).
		Protocol("onvif").NativeAck(true).
		Trust(uint8(profile.V2), uint8(profile.A1))
	if !isSet {
		return b.Requested(capID).Observed(fmt.Sprintf("%g (native %g)", norm, *got)).
			Verified().Build(), nil
	}
	b = b.Requested(fmt.Sprintf("%s=%g", capID, want))
	// A camera legitimately quantises to its own step, so exact equality is
	// the wrong test; a value nowhere near the request means it refused.
	if math.Abs(norm-want) > 0.05 {
		return b.Observed(fmt.Sprintf("asked %g, camera settled at %g — clamped or quantised", want, norm)).
			Build(), nil
	}
	return b.Observed(fmt.Sprintf("%g (native %g)", norm, *got)).Verified().Build(), nil
}

// invokeIRCut switches the infrared cut filter. Unlike the other imaging
// parameters this one moves a physical part, so the readback is genuine
// evidence that something happened in the world.
func (a *Adapter) invokeIRCut(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	mode, _ := args["mode"].(string)
	if mode != "" {
		onvifMode, ok := ircutMode(mode)
		if !ok {
			return sdk.NewEffect().Fail(
				`imaging.ircut mode must be "on", "off" or "auto", got ` + mode).Build(), nil
		}
		body := fmt.Sprintf(
			`<timg:SetImagingSettings><timg:VideoSourceToken>%s</timg:VideoSourceToken>`+
				`<timg:ImagingSettings><tt:IrCutFilter>%s</tt:IrCutFilter></timg:ImagingSettings>`+
				`</timg:SetImagingSettings>`, d.videoSrc, onvifMode)
		if err := d.cli.call(ctx, d.imagingX, nsImaging+"/SetImagingSettings", body, nil); err != nil {
			return ptzFault(err, "set imaging.ircut"), nil
		}
	}

	s, err := a.imagingSettings(ctx, d)
	if err != nil {
		return ptzFault(err, "read imaging.ircut"), nil
	}
	got := s.Body.Settings.IrCutFilter
	b := sdk.NewEffect().
		Metric("imaging_ircut", ircutMetric(got)).
		Protocol("onvif").NativeAck(true).
		Observed(got).
		Trust(uint8(profile.V2), uint8(profile.A1))
	if mode == "" {
		return b.Requested(profile.CapImagingIRCut).Verified().Build(), nil
	}
	b = b.Requested("imaging.ircut=" + mode)
	if want, _ := ircutMode(mode); !strings.EqualFold(got, want) {
		return b.Fail(fmt.Sprintf("asked for %s, camera reports %s", want, got)).Build(), nil
	}
	return b.Verified().Build(), nil
}

// ircutMode maps our vocabulary to ONVIF's. ONVIF names the filter, not the
// night mode: ON means the IR-cut filter is engaged, which is *day* mode.
// Getting this backwards would flip every camera it touched.
func ircutMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "on", "day":
		return "ON", true
	case "off", "night":
		return "OFF", true
	case "auto":
		return "AUTO", true
	}
	return "", false
}

func ircutMetric(mode string) float64 {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "ON":
		return 1
	case "OFF":
		return 0
	case "AUTO":
		return 2
	}
	return -1
}

// invokeFocus drives the lens. Focus has no position readback in ONVIF's
// imaging service beyond a status word, so a manual move is honestly
// UNVERIFIED unless the camera reports it moved.
func (a *Adapter) invokeFocus(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	if auto, ok := args["auto"].(bool); ok && auto {
		body := fmt.Sprintf(
			`<timg:SetImagingSettings><timg:VideoSourceToken>%s</timg:VideoSourceToken>`+
				`<timg:ImagingSettings><tt:Focus><tt:AutoFocusMode>AUTO</tt:AutoFocusMode></tt:Focus>`+
				`</timg:ImagingSettings></timg:SetImagingSettings>`, d.videoSrc)
		if err := d.cli.call(ctx, d.imagingX, nsImaging+"/SetImagingSettings", body, nil); err != nil {
			return ptzFault(err, "set autofocus"), nil
		}
		return sdk.NewEffect().Requested("lens.focus auto").Protocol("onvif").NativeAck(true).
			Observed("autofocus enabled; the camera decides where focus lands").
			Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
	}

	speed, ok := argFloat(args, "focus")
	if !ok || speed == 0 {
		return sdk.NewEffect().Fail("lens.focus needs {focus: -1..1} or {auto: true}").Build(), nil
	}
	if speed < -1 || speed > 1 {
		return sdk.NewEffect().Fail(
			fmt.Sprintf("focus speed %v out of range [-1,1]", speed)).Build(), nil
	}
	// Manual focus first — a continuous focus move on a lens still in
	// autofocus is fought by the camera and silently undone.
	manual := fmt.Sprintf(
		`<timg:SetImagingSettings><timg:VideoSourceToken>%s</timg:VideoSourceToken>`+
			`<timg:ImagingSettings><tt:Focus><tt:AutoFocusMode>MANUAL</tt:AutoFocusMode></tt:Focus>`+
			`</timg:ImagingSettings></timg:SetImagingSettings>`, d.videoSrc)
	if err := d.cli.call(ctx, d.imagingX, nsImaging+"/SetImagingSettings", manual, nil); err != nil {
		return ptzFault(err, "set manual focus"), nil
	}

	body := fmt.Sprintf(
		`<timg:Move><timg:VideoSourceToken>%s</timg:VideoSourceToken>`+
			`<timg:Focus><tt:Continuous><tt:Speed>%g</tt:Speed></tt:Continuous></timg:Focus>`+
			`</timg:Move>`, d.videoSrc, speed)
	if err := d.cli.call(ctx, d.imagingX, nsImaging+"/Move", body, nil); err != nil {
		return ptzFault(err, "focus move"), nil
	}
	// A continuous focus move runs until stopped, exactly like PTZ.
	if err := d.cli.call(ctx, d.imagingX, nsImaging+"/Stop",
		`<timg:Stop><timg:VideoSourceToken>`+d.videoSrc+`</timg:VideoSourceToken></timg:Stop>`,
		nil); err != nil {
		return sdk.NewEffect().Protocol("onvif").
			Fail("focus move issued but stop failed, the lens may still be driving: " + err.Error()).Build(), nil
	}
	return sdk.NewEffect().
		Requested(fmt.Sprintf("lens.focus=%g", speed)).Protocol("onvif").NativeAck(true).
		Observed("focus driven; ONVIF reports no focus position to verify against").
		Trust(uint8(profile.V1), uint8(profile.A1)).Build(), nil
}
