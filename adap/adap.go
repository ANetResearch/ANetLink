// Package adap implements ADAP — the ANetLink Adapter Protocol (C4) over a
// Unix domain socket.
//
// C4's primary form is the Go interface in sdk/: adapters are packages,
// composed at compile time, selected by build tag. That is the right shape
// for the fourteen adapters that live in this repository and it is not
// changing. ADAP is the escape hatch for the adapters that cannot be Go
// packages in our binary:
//
//   - another language — a Matter controller is matter.js, and rewriting it
//     in Go to reach a light bulb is not a trade anyone should make;
//   - a vendor SDK that ships as a shared library with its own threads,
//     allocator and opinions about signal handling;
//   - a surface we would rather not host in-process at all, because it
//     parses hostile input or links code we cannot audit.
//
// The design decision that keeps this from becoming a second architecture:
// the escape hatch terminates at the same contract. A Host wraps one
// out-of-tree adapter process and presents it to the runtime as an
// sdk.Adapter — the runtime cannot tell the difference, and nothing in the
// runtime learns that out-of-process adapters exist.
//
// Wire format is CoreDet-CBOR, the same encoding as EffectRecord and every
// other ANet object, so an effect crosses the socket without a translation
// step (spec §9). Messages are not signed: the adapter holds no key, and
// the runtime signs the EffectRecord it produces with the bridge's own AID.
// A third-party adapter therefore cannot forge evidence — that is the point
// of the split, not an omission from it.
package adap

import (
	"errors"
	"fmt"
)

// Version is the ADAP major version this implementation speaks. An
// incompatible change increments it (spec §8.1).
const Version = 1

// Ops. The first version is deliberately fourteen (spec §1: "协议做大是灾难的开始").
const (
	OpHello       = "hello"
	OpClaim       = "claim"
	OpDescribe    = "describe"
	OpBye         = "bye"
	OpGet         = "get"
	OpSet         = "set"
	OpState       = "state"
	OpInvoke      = "invoke"
	OpResult      = "result"
	OpSubscribe   = "subscribe"
	OpUnsubscribe = "unsubscribe"
	OpEvent       = "event"
	OpPing        = "ping"
	OpError       = "error"
)

// Error tags, SCREAMING_CASE per _CONVENTIONS §7.1. The Go identifiers may
// be Go-shaped; the tag on the wire may not.
const (
	ErrMalformed       = "MALFORMED"
	ErrUnsupported     = "UNSUPPORTED"
	ErrNotFound        = "NOT_FOUND"
	ErrInvalidArgs     = "INVALID_ARGS"
	ErrAccessDenied    = "ACCESS_DENIED"
	ErrDeviceOffline   = "DEVICE_OFFLINE"
	ErrDeviceBusy      = "DEVICE_BUSY"
	ErrTimeout         = "TIMEOUT"
	ErrExpired         = "EXPIRED"
	ErrBackpressure    = "BACKPRESSURE"
	ErrAdapterInternal = "ADAPTER_INTERNAL"
)

// retryable reports whether a failure of this kind may be retried at all.
// Retrying is still forbidden for non-idempotent actions regardless (spec
// §5.1.2) — this only says the error itself is transient.
func retryable(tag string) bool {
	switch tag {
	case ErrDeviceOffline, ErrDeviceBusy, ErrTimeout, ErrBackpressure, ErrAdapterInternal:
		return true
	}
	return false
}

// Message is one ADAP frame. Keys are integers per the CDDL in spec §2;
// CoreDet-CBOR requires deterministic encoding, which keyasint gives us.
type Message struct {
	V          uint64 `cbor:"1,keyasint"`
	Op         string `cbor:"2,keyasint"`
	RID        string `cbor:"3,keyasint"`
	TID        string `cbor:"4,keyasint,omitempty"`
	TS         uint64 `cbor:"5,keyasint"`
	TTL        uint64 `cbor:"6,keyasint,omitempty"`
	DeviceID   string `cbor:"7,keyasint,omitempty"`
	Endpoint   string `cbor:"8,keyasint,omitempty"`
	Capability string `cbor:"9,keyasint,omitempty"`
	// Payload carries the op-specific body. Keeping it one field rather than
	// a union of typed fields is what lets a receiver ignore an op it does
	// not know without failing to decode the frame (spec §8.2).
	Payload map[string]any `cbor:"10,keyasint,omitempty"`
}

// Error is an ADAP error response carried in an OpError frame.
type Error struct {
	Tag    string `cbor:"1,keyasint"`
	Detail string `cbor:"2,keyasint,omitempty"`
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Tag
	}
	return e.Tag + ": " + e.Detail
}

// Retryable reports whether this failure may be retried, subject to the
// caller's own idempotency rule.
func (e *Error) Retryable() bool { return retryable(e.Tag) }

// AsError extracts an *Error from an error chain, if there is one.
func AsError(err error) (*Error, bool) {
	var ae *Error
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

func errf(tag, format string, args ...any) *Error {
	return &Error{Tag: tag, Detail: fmt.Sprintf(format, args...)}
}
