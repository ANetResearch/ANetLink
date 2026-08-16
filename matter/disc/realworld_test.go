//go:build realworld

package disc

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRealDiscoverMatterJS discovers the live matter.js virtual light on the
// testbed — cross-implementation at the discovery layer: their advertiser,
// our browser.
func TestRealDiscoverMatterJS(t *testing.T) {
	if os.Getenv("ANETLINK_MATTER_EXPECT") == "" {
		t.Skip("set ANETLINK_MATTER_EXPECT=<discriminator> with a live commissionable device (testbed matter.js: 3840)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	nodes, err := Browse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		t.Logf("found %s D=%d CM=%d port=%d ips=%v", n.Instance, n.Discriminator, n.Commissioning, n.Port, n.IPs)
		if n.Discriminator == 3840 && n.Port != 0 {
			return
		}
	}
	t.Fatalf("matter.js light (D=3840) not found among %d nodes", len(nodes))
}
