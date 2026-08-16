package sdk

import (
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"
)

// EffectBuilder constructs invocation effects with honest defaults: an
// effect is UNVERIFIED until the adapter both attaches metrics and calls
// Verified() — claiming OK without measurement is structurally impossible.
type EffectBuilder struct {
	status  effect.Status
	metrics map[string]float64
	msg     string
}

// NewEffect starts an UNVERIFIED effect.
func NewEffect() *EffectBuilder {
	return &EffectBuilder{status: effect.Unverified}
}

// Metric attaches one measured value.
func (b *EffectBuilder) Metric(name string, v float64) *EffectBuilder {
	if b.metrics == nil {
		b.metrics = map[string]float64{}
	}
	b.metrics[name] = v
	return b
}

// Verified marks the effect verifiable. Without at least one metric the
// claim is vacuous and Build keeps the effect UNVERIFIED.
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
	if len(b.metrics) > 0 {
		e.Record = &tsir.EffectRecord{Metrics: b.metrics}
	}
	if e.Status == effect.OK && e.Record == nil {
		e.Status = effect.Unverified // OK without measurement is not OK
	}
	return e
}
