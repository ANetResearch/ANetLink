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
// STATUS: PASSING (D54 closed). A pure-Go commissioner completes the full
// PASE handshake against a real Matter device and both sides derive the same
// session key. Getting here required pinning the primitive against
// connectedhomeip's own SPAKE2+ vectors, which exposed three deviations from
// the RFC-literal reading: K_main splits Ka‖Ke (Matter) rather than Ke‖Ka,
// the confirmation KDF takes Ka (not K_main) and yields 16-byte keys, and
// cA = MAC(KcA, shareV) with cB = MAC(KcB, shareP).
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
