//go:build realworld

package exchange

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRealPASEHandshake runs our pure-Go PASE commissioner against a REAL
// Matter device (the testbed matter.js light). Cross-implementation at the
// hardest layer.
//
// STATUS (D54): the exchange reaches Pake2 — framing, TLV, MRP, PBKDF
// parameter negotiation and share exchange all interoperate with a real
// device. The device's confirmation MAC does not yet verify. The SPAKE2+
// primitive itself is proven correct against the RFC 9383 P-256 vector
// (see matter/spake2p/vectors_test.go), and the PASE context construction
// matches connectedhomeip's PASESession.cpp (SHA-256 over prefix ‖ request
// ‖ response). The remaining delta is a Matter-specific detail that needs a
// connectedhomeip-derived PASE vector (w0/w1 or Z/V convention) to pin —
// deliberately NOT chased by further live trial-and-error.
//
// Set ANETLINK_MATTER_ADDR=<ip:5540>; the testbed passcode is 20202021.
func TestRealPASEHandshake(t *testing.T) {
	addr := os.Getenv("ANETLINK_MATTER_ADDR")
	if addr == "" {
		t.Skip("set ANETLINK_MATTER_ADDR=<ip>:5540 (testbed matter.js light) + ANETLINK_MATTER_PASSCODE")
	}
	passcode := uint32(20202021)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ke, err := Commission(ctx, addr, passcode)
	if err != nil {
		t.Fatalf("PASE against real device failed: %v", err)
	}
	if len(ke) != 16 {
		t.Fatalf("session key length %d, want 16", len(ke))
	}
	t.Logf("PASE succeeded against real Matter device: Ke=%x", ke)
}
