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
// hardest layer: if our TLV split, SPAKE2+ M/N points, PBKDF params, or
// frame layout are off by a byte, the device rejects us and this fails.
// Set ANETLINK_MATTER_ADDR=<ip:5540> and ANETLINK_MATTER_PASSCODE=20202021.
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
