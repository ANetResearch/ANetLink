package profile

// Trust is the ANetLink trust ladder (DocsANetLink 05 / K207): how strongly an
// execution effect on this device can be verified.
type Trust uint8

const (
	// T0: no verification possible — command disappears into a black box.
	T0 Trust = iota
	// T1: transport-level ack only (cloud APIs top out here — link-minimum rule).
	T1
	// T2: device-reported state readback over a local link.
	T2
	// T3: independent state confirmation (separate read path or second sensor).
	T3
	// T4: physically closed loop — the effect is measured, not reported.
	T4
)

// VerifiableFrom is the minimum trust at which an effect may claim
// effect.OK (verifiable): device-reported readback or better.
const VerifiableFrom = T2

// Clamp applies the link-minimum rule: nothing downstream of a transport can
// exceed the transport's ceiling, and nothing (quirks included) can raise it.
func Clamp(declared, ceiling Trust) Trust {
	if declared > ceiling {
		return ceiling
	}
	return declared
}
