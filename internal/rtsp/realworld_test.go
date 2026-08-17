//go:build realworld

// Real-service tier (L1): a REAL RTSP server carrying a REAL H.264 stream
// (mediamtx publishing an ffmpeg source; or any real camera at L2). Point
// ANETLINK_RTSP_URI at it.
//
// This tier is what proves the probe is worth having. The unit tests answer
// a hand-written socket; here the SDP is produced by a real media server and
// the codec line is whatever the encoder actually negotiated.
package rtsp

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func realURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("ANETLINK_RTSP_URI")
	if uri == "" {
		t.Skip("set ANETLINK_RTSP_URI to a real RTSP stream (L1: docker bluenviron/mediamtx; L2: a real camera)")
	}
	return uri
}

func TestRealStreamDescribe(t *testing.T) {
	uri := realURI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sdp, err := Describe(ctx, &net.Dialer{Timeout: 5 * time.Second}, uri,
		os.Getenv("ANETLINK_RTSP_USER"), os.Getenv("ANETLINK_RTSP_PASS"))
	if err != nil {
		t.Fatalf("DESCRIBE against a real media server: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(sdp), "v=") {
		t.Fatalf("a real SDP must start with a version line: %q", sdp[:min(60, len(sdp))])
	}
	codec := VideoCodec(sdp)
	if codec == "" {
		t.Fatalf("a real video stream must advertise a codec:\n%s", sdp)
	}
	t.Logf("real stream: codec=%s, %d bytes of SDP", codec, len(sdp))
}

// TestRealStreamWrongPathIsRejected: the media server is up, but this path
// carries nothing. The whole point of probing is telling those apart.
func TestRealStreamWrongPathIsRejected(t *testing.T) {
	uri := realURI(t)
	idx := strings.LastIndex(uri, "/")
	if idx < 0 {
		t.Skip("cannot derive a bogus path from " + uri)
	}
	bogus := uri[:idx] + "/definitely-not-a-real-path"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Describe(ctx, &net.Dialer{Timeout: 5 * time.Second}, bogus, "", ""); err == nil {
		t.Fatal("a path with no stream behind it must not report a live stream")
	}
}
