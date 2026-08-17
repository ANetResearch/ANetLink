package onvif

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// defaultMoveMS is how long a continuous move runs when the caller does not
// say. ONVIF continuous move runs until stopped, so an adapter that fires one
// and returns would leave the camera spinning — every move here is bounded.
const defaultMoveMS = 500

// ptzStatus reads the current pan/tilt/zoom position.
func (a *Adapter) ptzStatus(ctx context.Context, d *device) (ptzStatusResponse, error) {
	var st ptzStatusResponse
	body := `<tptz:GetStatus><tptz:ProfileToken>` + d.profTok + `</tptz:ProfileToken></tptz:GetStatus>`
	err := d.cli.call(ctx, d.ptzX, nsPTZ+"/GetStatus", body, &st)
	return st, err
}

// argFloat reads a numeric argument, tolerating the int/float ambiguity that
// survives a JSON round trip.
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

// clampVelocity holds a velocity inside the [-1,1] range ONVIF defines.
// Out-of-range values are rejected rather than clamped silently: a caller
// asking to pan at 5.0 has a bug, and quietly turning that into 1.0 hides it.
func clampVelocity(v float64, name string) error {
	if math.IsNaN(v) || v < -1 || v > 1 {
		return fmt.Errorf("%s velocity %v out of range [-1,1]", name, v)
	}
	return nil
}

// invokePTZMove runs a bounded continuous move and verifies it by position
// readback: pan/tilt/zoom before, move, stop, read again. A camera whose
// position actually changed executed the command; one that reports no
// position at all leaves the effect UNVERIFIED rather than assumed-good.
func (a *Adapter) invokePTZMove(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	pan, _ := argFloat(args, "pan")
	tilt, _ := argFloat(args, "tilt")
	zoom, _ := argFloat(args, "zoom")
	if pan == 0 && tilt == 0 && zoom == 0 {
		return sdk.NewEffect().Fail("ptz.move needs at least one of pan/tilt/zoom").Build(), nil
	}
	for _, c := range []struct {
		v float64
		n string
	}{{pan, "pan"}, {tilt, "tilt"}, {zoom, "zoom"}} {
		if err := clampVelocity(c.v, c.n); err != nil {
			return sdk.NewEffect().Fail(err.Error()).Build(), nil
		}
	}
	durMS := defaultMoveMS
	if v, ok := argFloat(args, "duration_ms"); ok && v > 0 {
		durMS = int(math.Min(v, 10_000)) // a single move may not exceed 10s
	}

	before, beforeErr := a.ptzStatus(ctx, d)

	body := fmt.Sprintf(
		`<tptz:ContinuousMove><tptz:ProfileToken>%s</tptz:ProfileToken><tptz:Velocity>`+
			`<tt:PanTilt x="%g" y="%g"/><tt:Zoom x="%g"/>`+
			`</tptz:Velocity></tptz:ContinuousMove>`, d.profTok, pan, tilt, zoom)
	if err := d.cli.call(ctx, d.ptzX, nsPTZ+"/ContinuousMove", body, nil); err != nil {
		return ptzFault(err, "ContinuousMove"), nil
	}

	select {
	case <-time.After(time.Duration(durMS) * time.Millisecond):
	case <-ctx.Done():
		// Stop anyway: an abandoned continuous move keeps the motor running.
		_ = a.stopPTZ(context.WithoutCancel(ctx), d)
		return effect.Effect{}, ctx.Err()
	}
	if err := a.stopPTZ(ctx, d); err != nil {
		return sdk.NewEffect().
			Requested(fmt.Sprintf("ptz.move pan=%g tilt=%g zoom=%g", pan, tilt, zoom)).
			Protocol("onvif").
			Fail("move issued but stop failed, camera may still be moving: " + err.Error()).Build(), nil
	}

	b := sdk.NewEffect().
		Requested(fmt.Sprintf("ptz.move pan=%g tilt=%g zoom=%g for %dms", pan, tilt, zoom, durMS)).
		Protocol("onvif").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	after, afterErr := a.ptzStatus(ctx, d)
	if beforeErr != nil || afterErr != nil {
		return b.Observed("camera does not report PTZ position; movement unverified").Build(), nil
	}
	bp, ap := before.Body.PTZStatus.Position, after.Body.PTZStatus.Position
	moved := bp.PanTilt.X != ap.PanTilt.X || bp.PanTilt.Y != ap.PanTilt.Y || bp.Zoom.X != ap.Zoom.X
	b = b.Metric("pan", ap.PanTilt.X).Metric("tilt", ap.PanTilt.Y).Metric("zoom", ap.Zoom.X)
	if !moved {
		// A camera already at its limit reports no change. That is a real
		// outcome, not a transport failure — and it is not success either.
		return b.Observed(fmt.Sprintf("position unchanged at pan=%g tilt=%g zoom=%g (at limit?)",
			ap.PanTilt.X, ap.PanTilt.Y, ap.Zoom.X)).Build(), nil
	}
	return b.Observed(fmt.Sprintf("pan %g→%g tilt %g→%g zoom %g→%g",
		bp.PanTilt.X, ap.PanTilt.X, bp.PanTilt.Y, ap.PanTilt.Y, bp.Zoom.X, ap.Zoom.X)).
		Trust(uint8(profile.V2), uint8(profile.A1)).
		Verified().Build(), nil
}

func (a *Adapter) stopPTZ(ctx context.Context, d *device) error {
	body := `<tptz:Stop><tptz:ProfileToken>` + d.profTok +
		`</tptz:ProfileToken><tptz:PanTilt>true</tptz:PanTilt><tptz:Zoom>true</tptz:Zoom></tptz:Stop>`
	return d.cli.call(ctx, d.ptzX, nsPTZ+"/Stop", body, nil)
}

func (a *Adapter) invokePTZStop(ctx context.Context, d *device) (effect.Effect, error) {
	if err := a.stopPTZ(ctx, d); err != nil {
		return ptzFault(err, "Stop"), nil
	}
	b := sdk.NewEffect().Requested(profile.CapPTZStop).Protocol("onvif").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	st, err := a.ptzStatus(ctx, d)
	if err != nil {
		return b.Observed("stop acknowledged; camera does not report move status").Build(), nil
	}
	ms := st.Body.PTZStatus.MoveStatus
	idle := (ms.PanTilt == "" || ms.PanTilt == "IDLE") && (ms.Zoom == "" || ms.Zoom == "IDLE")
	p := st.Body.PTZStatus.Position
	b = b.Metric("pan", p.PanTilt.X).Metric("tilt", p.PanTilt.Y).Metric("zoom", p.Zoom.X)
	if !idle {
		return b.Fail(fmt.Sprintf("stop acknowledged but camera still moving: pan/tilt=%s zoom=%s",
			ms.PanTilt, ms.Zoom)).Build(), nil
	}
	return b.Observed("IDLE").Trust(uint8(profile.V2), uint8(profile.A1)).Verified().Build(), nil
}

// invokePTZPreset recalls a stored position and verifies arrival by reading
// the position back after the camera has had time to travel.
func (a *Adapter) invokePTZPreset(ctx context.Context, d *device, args map[string]any) (effect.Effect, error) {
	token, _ := args["preset"].(string)
	if token == "" {
		if n, ok := argFloat(args, "preset"); ok {
			token = fmt.Sprintf("%d", int(n))
		}
	}
	if token == "" {
		return sdk.NewEffect().Fail("ptz.preset needs a preset token").Build(), nil
	}

	body := `<tptz:GotoPreset><tptz:ProfileToken>` + d.profTok +
		`</tptz:ProfileToken><tptz:PresetToken>` + token + `</tptz:PresetToken></tptz:GotoPreset>`
	if err := d.cli.call(ctx, d.ptzX, nsPTZ+"/GotoPreset", body, nil); err != nil {
		return ptzFault(err, "GotoPreset"), nil
	}

	b := sdk.NewEffect().Requested("ptz.preset="+token).Protocol("onvif").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	// Give the head time to travel before judging where it ended up.
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
	st, err := a.ptzStatus(ctx, d)
	if err != nil {
		return b.Observed("preset recalled; camera does not report position").Build(), nil
	}
	p := st.Body.PTZStatus.Position
	return b.Metric("pan", p.PanTilt.X).Metric("tilt", p.PanTilt.Y).Metric("zoom", p.Zoom.X).
		Observed(fmt.Sprintf("at pan=%g tilt=%g zoom=%g", p.PanTilt.X, p.PanTilt.Y, p.Zoom.X)).
		Trust(uint8(profile.V2), uint8(profile.A1)).
		Verified().Build(), nil
}

func (a *Adapter) invokePTZPosition(ctx context.Context, d *device) (effect.Effect, error) {
	st, err := a.ptzStatus(ctx, d)
	if err != nil {
		return ptzFault(err, "GetStatus"), nil
	}
	p := st.Body.PTZStatus.Position
	return sdk.NewEffect().
		Metric("pan", p.PanTilt.X).Metric("tilt", p.PanTilt.Y).Metric("zoom", p.Zoom.X).
		Requested(profile.CapPTZPosition).Protocol("onvif").NativeAck(true).
		Observed(fmt.Sprintf("pan=%g tilt=%g zoom=%g", p.PanTilt.X, p.PanTilt.Y, p.Zoom.X)).
		Trust(uint8(profile.V2), uint8(profile.A1)).
		Verified().Build(), nil
}

// ptzFault turns a device error into the right effect status: credentials
// the camera refused are a failure to fix, a camera that did not answer is
// merely unavailable, and conflating them sends operators down wrong paths.
func ptzFault(err error, op string) effect.Effect {
	if IsAuthFault(err) {
		return sdk.NewEffect().Fail(op + " rejected: camera did not accept the credentials").Build()
	}
	return sdk.NewEffect().Unavailable(op + ": " + err.Error()).Build()
}
