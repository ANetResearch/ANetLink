package sdk

import (
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"
	"github.com/ANetResearch/ANetLink/profile"
)

// EffectBuilder constructs invocation effects with honest defaults: an
// effect is UNVERIFIED until the adapter both attaches evidence-bearing
// content and calls Verified() — claiming OK without measurement is
// structurally impossible.
//
// "Evidence-bearing content" is a metric or an artifact. The media family
// (profile/media.go) produces the latter: a snapshot that came back as real
// JPEG bytes proves the camera responded just as surely as a number does.
type EffectBuilder struct {
	status    effect.Status
	metrics   map[string]float64
	artifacts []tsir.Artifact
	msg       string
	ev        effect.Evidence
	hasEv     bool
	started   time.Time
}

// NewEffect starts an UNVERIFIED effect. The clock starts now so Build can
// stamp latency into the Evidence.
func NewEffect() *EffectBuilder {
	return &EffectBuilder{status: effect.Unverified, started: time.Now()}
}

// Requested records what was asked for (e.g. "light.onoff=on").
func (b *EffectBuilder) Requested(s string) *EffectBuilder {
	b.ev.Requested, b.hasEv = s, true
	return b
}

// Protocol records which protocol carried the invocation.
func (b *EffectBuilder) Protocol(p string) *EffectBuilder {
	b.ev.Protocol, b.hasEv = p, true
	return b
}

// NativeAck records that the protocol itself acknowledged the command
// (distinct from verifying the effect).
func (b *EffectBuilder) NativeAck(ok bool) *EffectBuilder {
	b.ev.NativeAck, b.hasEv = ok, true
	return b
}

// Observed records the state a readback actually reported.
func (b *EffectBuilder) Observed(state string) *EffectBuilder {
	b.ev.ObservedState, b.hasEv = state, true
	return b
}

// Trust records both trust axes (K210 D49).
func (b *EffectBuilder) Trust(verify, auth uint8) *EffectBuilder {
	b.ev.VerifyTrust, b.ev.AuthTrust, b.hasEv = verify, auth, true
	return b
}

// Metric attaches one measured value.
func (b *EffectBuilder) Metric(name string, v float64) *EffectBuilder {
	if b.metrics == nil {
		b.metrics = map[string]float64{}
	}
	b.metrics[name] = v
	return b
}

// StreamURI attaches a live-stream reference (RTSP and friends). The URI is
// a handle, not content: ANetLink never proxies video through the narrow
// waist, so nothing here is content-addressed. On its own a URI is only a
// claim that the device advertised an endpoint — an adapter that has not
// actually reached the stream must leave the effect UNVERIFIED.
func (b *EffectBuilder) StreamURI(uri string) *EffectBuilder {
	b.artifacts = append(b.artifacts, tsir.Artifact{
		Path:      uri,
		MediaType: profile.MediaTypeRTSP,
	})
	return b
}

// Snapshot attaches a still frame by value: the bytes are content-addressed
// so the same frame reported twice carries the same id, and a truncated
// transfer cannot masquerade as a good one.
func (b *EffectBuilder) Snapshot(data []byte, mediaType string) *EffectBuilder {
	if len(data) == 0 {
		return b // an empty frame is not evidence; leave the effect bare
	}
	cid, err := anetcid.SumRaw(data)
	if err != nil {
		return b
	}
	if mediaType == "" {
		mediaType = profile.MediaTypeJPEG
	}
	b.artifacts = append(b.artifacts, tsir.Artifact{
		Path:       "snapshot",
		SizeBytes:  uint64(len(data)),
		ContentCID: cid,
		MediaType:  mediaType,
	})
	return b
}

// Verified marks the effect verifiable. Without at least one metric or
// artifact the claim is vacuous and Build keeps the effect UNVERIFIED.
func (b *EffectBuilder) Verified() *EffectBuilder {
	b.status = effect.OK
	return b
}

// Fail marks execution failure.
func (b *EffectBuilder) Fail(msg string) *EffectBuilder {
	b.status, b.msg = effect.Failed, msg
	return b
}

// Unavailable marks the target unreachable at invocation time.
func (b *EffectBuilder) Unavailable(msg string) *EffectBuilder {
	b.status, b.msg = effect.Unavailable, msg
	return b
}

// Build finalizes the effect, enforcing the honesty rules.
func (b *EffectBuilder) Build() effect.Effect {
	e := effect.Effect{Status: b.status, Message: b.msg}
	if len(b.metrics) > 0 || len(b.artifacts) > 0 {
		e.Record = &tsir.EffectRecord{Metrics: b.metrics, Artifacts: b.artifacts}
	}
	if e.Status == effect.OK && e.Record == nil {
		e.Status = effect.Unverified // OK without measurement is not OK
	}
	if b.hasEv {
		ev := b.ev
		if !b.started.IsZero() {
			ev.LatencyMS = time.Since(b.started).Milliseconds()
		}
		e.Evidence = &ev
	}
	return e
}
