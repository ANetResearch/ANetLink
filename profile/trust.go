package profile

// ANetLink tracks TWO orthogonal trust axes (K210 D49):
//
//   VerifyTrust — "how strongly can I verify the effect HAPPENED"
//   AuthTrust   — "how strongly do I know WHO this device is"
//
// They are independent: a Modbus PLC has weak identity (AuthGateway) but can
// have strong effect verification (VerifyReadback), while a cloud API may
// authenticate a device well yet only ack the command (VerifyAck). Conflating
// them loses that distinction — the D3 lesson ("money ≠ credit") applied to
// devices.

// VerifyTrust is the effect-verification ladder: how strongly an execution
// effect on this device can be verified.
type VerifyTrust uint8

const (
	// V0/T0: no verification possible — command disappears into a black box.
	V0 VerifyTrust = iota
	// V1/T1: transport-level ack only (cloud APIs top out here).
	V1
	// V2/T2: device-reported state readback over a local link.
	V2
	// V3/T3: independent state confirmation (separate read path or sensor).
	V3
	// V4/T4: physically closed loop — the effect is measured, not reported.
	V4
)

// Legacy aliases: the effect-verification ladder was historically named
// T0–T4 / Trust. Kept so existing adapters and MaxTrust keep working.
type Trust = VerifyTrust

const (
	T0 = V0
	T1 = V1
	T2 = V2
	T3 = V3
	T4 = V4
)

// AuthTrust is the identity-authentication ladder (R02 §13): how strongly the
// device's identity is established.
type AuthTrust uint8

const (
	// A0: unknown — no identity.
	A0 AuthTrust = iota
	// A1: gateway-attested — the bridge vouches for it (e.g. a Modbus unit on
	// a trusted RS485 segment). No device-side credential.
	A1
	// A2: protocol-authenticated — the device protocol authenticated the
	// session (e.g. an OPC UA secure channel, a signed MQTT client).
	A2
	// A3: device-key authenticated — a per-device key proved possession
	// (e.g. a Matter operational credential).
	A3
	// A4: hardware-rooted — attestation chained to a secure element / TEE.
	A4
)

// VerifiableFrom is the minimum VerifyTrust at which an effect may claim
// effect.OK (verifiable): device-reported readback or better.
const VerifiableFrom = V2

// Clamp applies the link-minimum rule to the verify axis: nothing downstream
// of a transport can exceed the transport's ceiling, and nothing (quirks
// included) can raise it.
func Clamp(declared, ceiling VerifyTrust) VerifyTrust {
	if declared > ceiling {
		return ceiling
	}
	return declared
}

// ClampAuth applies the same link-minimum rule to the auth axis.
func ClampAuth(declared, ceiling AuthTrust) AuthTrust {
	if declared > ceiling {
		return ceiling
	}
	return declared
}
