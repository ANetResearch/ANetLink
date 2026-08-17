package rtsp

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

const testSDP = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=Stream\r\n" +
	"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n" +
	"m=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"

// fakeServer answers DESCRIBE on a real TCP socket, demanding digest first,
// so both halves of the exchange meet a real wire rather than a mock.
type fakeServer struct {
	ln     net.Listener
	sdp    string
	status int // when non-zero, always answer this instead
	// requireAuth makes the server challenge before answering.
	requireAuth bool
	// garbage makes the server answer with something that is not RTSP.
	garbage bool
}

func newFakeServer(t *testing.T, f *fakeServer) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeServer) uri(path string) string {
	return fmt.Sprintf("rtsp://127.0.0.1:%d%s", f.ln.Addr().(*net.TCPAddr).Port, path)
}

func (f *fakeServer) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				req := string(buf[:n])
				cseq := "1"
				for _, line := range strings.Split(req, "\r\n") {
					if strings.HasPrefix(strings.ToLower(line), "cseq:") {
						cseq = strings.TrimSpace(line[len("cseq:"):])
					}
				}
				switch {
				case f.garbage:
					fmt.Fprint(c, "HTTP/1.1 200 OK\r\n\r\n")
				case f.status != 0:
					fmt.Fprintf(c, "RTSP/1.0 %d Nope\r\nCSeq: %s\r\n\r\n", f.status, cseq)
				case f.requireAuth && !strings.Contains(req, "Authorization:"):
					fmt.Fprintf(c, "RTSP/1.0 401 Unauthorized\r\nCSeq: %s\r\n"+
						"WWW-Authenticate: Digest realm=\"cam\", nonce=\"n0\", qop=\"auth\"\r\n\r\n", cseq)
				default:
					fmt.Fprintf(c, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nContent-Type: application/sdp\r\n"+
						"Content-Length: %d\r\n\r\n%s", cseq, len(f.sdp), f.sdp)
				}
			}
		}(conn)
	}
}

func dialer() *net.Dialer { return &net.Dialer{Timeout: 2 * time.Second} }

func TestDescribeAgainstRealSocket(t *testing.T) {
	f := newFakeServer(t, &fakeServer{sdp: testSDP, requireAuth: true})
	sdp, err := Describe(context.Background(), dialer(), f.uri("/live"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sdp, "H264") {
		t.Fatalf("SDP did not survive the exchange: %q", sdp)
	}
}

// TestCredentialsInURIWin: a camera that minted the URI for one account may
// refuse any other, so userinfo in the URI takes precedence.
func TestCredentialsInURIWin(t *testing.T) {
	f := newFakeServer(t, &fakeServer{sdp: testSDP, requireAuth: true})
	uri := strings.Replace(f.uri("/live"), "rtsp://", "rtsp://user:pass@", 1)
	if _, err := Describe(context.Background(), dialer(), uri, "ignored", "ignored"); err != nil {
		t.Fatal(err)
	}
}

func TestDeadStreamIsAnError(t *testing.T) {
	f := newFakeServer(t, &fakeServer{status: 404})
	if _, err := Describe(context.Background(), dialer(), f.uri("/gone"), "u", "p"); err == nil {
		t.Fatal("a 404 stream must not look like a live one")
	}
}

func TestUnreachableIsAnError(t *testing.T) {
	_, err := Describe(context.Background(), &net.Dialer{Timeout: 300 * time.Millisecond},
		"rtsp://127.0.0.1:1/x", "u", "p")
	if err == nil {
		t.Fatal("an unreachable media plane must not appear to succeed")
	}
}

func TestNonRTSPResponseRejected(t *testing.T) {
	f := newFakeServer(t, &fakeServer{garbage: true})
	if _, err := Describe(context.Background(), dialer(), f.uri("/x"), "u", "p"); err == nil {
		t.Fatal("an HTTP server on the RTSP port must not pass as a stream")
	}
}

// TestEmptySDPRejected: a 200 with no session description is not a stream,
// and treating it as one is exactly the false positive this package exists
// to prevent.
func TestEmptySDPRejected(t *testing.T) {
	f := newFakeServer(t, &fakeServer{sdp: ""})
	if _, err := Describe(context.Background(), dialer(), f.uri("/x"), "u", "p"); err == nil {
		t.Fatal("a 200 with no SDP must not be reported as a live stream")
	}
}

func TestVideoCodec(t *testing.T) {
	if got := VideoCodec(testSDP); got != "H264" {
		t.Fatalf("codec: %q", got)
	}
	audioOnly := "v=0\r\nm=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"
	if got := VideoCodec(audioOnly); got != "" {
		t.Fatalf("an audio-only SDP has no video codec, got %q", got)
	}
	if got := VideoCodec(""); got != "" {
		t.Fatalf("empty sdp: %q", got)
	}
	h265 := "v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\n"
	if got := VideoCodec(h265); got != "H265" {
		t.Fatalf("h265: %q", got)
	}
}

func TestMalformedURI(t *testing.T) {
	if _, err := Describe(context.Background(), dialer(), "://not a uri", "u", "p"); err == nil {
		t.Fatal("a malformed uri must be an error")
	}
}
