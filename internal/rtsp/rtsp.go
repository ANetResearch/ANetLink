// Package rtsp implements just enough RTSP to answer one question: is there
// actually a live stream at this URI?
//
// Every camera adapter faces the same honesty problem. ONVIF's GetStreamUri,
// Hikvision's channel list and Dahua's realmonitor path all hand out a URI
// for a stream that may be misconfigured, whose encoder never started, or
// which the camera's own firewall blocks. Reporting that URI as a verified
// effect on the strength of the control-plane call alone is exactly the
// "I sent a command" ≠ "I verified the effect" confusion the effect model
// exists to prevent. A DESCRIBE that comes back with a session description
// is proof there is something on the other end.
//
// Only OPTIONS and DESCRIBE are implemented. ANetLink never carries video,
// so there is nothing here that would SETUP or PLAY a stream.
package rtsp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANetLink/internal/digest"
)

// maxSDP caps a session description. Cameras are untrusted input; a
// Content-Length header is a claim, not a promise.
const maxSDP = 1 << 20

// DefaultPort is the RTSP port cameras serve on.
const DefaultPort = 554

// Describe performs an RTSP DESCRIBE against uri and returns the SDP body.
// Credentials embedded in the URI take precedence over the user/pass
// arguments — some cameras only accept the account they minted the URI for.
func Describe(ctx context.Context, dialer *net.Dialer, uri, user, pass string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("stream uri: %w", err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", host, DefaultPort)
	}

	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if u.User != nil {
		user = u.User.Username()
		if p, ok := u.User.Password(); ok {
			pass = p
		}
	}
	// The digest URI is the stream URL without userinfo; leaving credentials
	// in it changes the hashed string and fails authentication.
	bare := *u
	bare.User = nil
	target := bare.String()

	br := bufio.NewReader(conn)
	creds := &digest.Credentials{Username: user, Password: pass}
	cseq := 0

	send := func(auth string) (int, textproto.MIMEHeader, string, error) {
		cseq++
		var req strings.Builder
		fmt.Fprintf(&req, "DESCRIBE %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: ANetLink/0.1\r\n", target, cseq)
		req.WriteString("Accept: application/sdp\r\n")
		if auth != "" {
			req.WriteString("Authorization: " + auth + "\r\n")
		}
		req.WriteString("\r\n")
		if _, err := io.WriteString(conn, req.String()); err != nil {
			return 0, nil, "", err
		}
		return readResponse(br)
	}

	code, hdr, body, err := send("")
	if err != nil {
		return "", err
	}
	if code == 401 {
		ch, ok := digest.ParseChallenge(hdr.Get("Www-Authenticate"))
		if !ok {
			return "", fmt.Errorf("rtsp: camera demands auth but offered no digest challenge")
		}
		auth, err := creds.Authorize(ch, "DESCRIBE", target)
		if err != nil {
			return "", err
		}
		code, _, body, err = send(auth)
		if err != nil {
			return "", err
		}
	}
	if code != 200 {
		return "", fmt.Errorf("rtsp: DESCRIBE returned %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "v=") {
		return "", fmt.Errorf("rtsp: DESCRIBE returned no SDP")
	}
	return body, nil
}

// readResponse parses one RTSP/1.0 response. RTSP framing is HTTP's, but
// net/http cannot parse it (the version token differs), so it is done here.
func readResponse(br *bufio.Reader) (int, textproto.MIMEHeader, string, error) {
	tp := textproto.NewReader(br)
	statusLine, err := tp.ReadLine()
	if err != nil {
		return 0, nil, "", err
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "RTSP/") {
		return 0, nil, "", fmt.Errorf("rtsp: malformed status line %q", statusLine)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, "", fmt.Errorf("rtsp: malformed status code in %q", statusLine)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return 0, nil, "", err
	}
	body := ""
	if n, err := strconv.Atoi(hdr.Get("Content-Length")); err == nil && n > 0 {
		if n > maxSDP {
			return 0, nil, "", fmt.Errorf("rtsp: SDP of %d bytes exceeds cap", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return 0, nil, "", err
		}
		body = string(buf)
	}
	return code, hdr, body, nil
}

// VideoCodec extracts the video codec name from an SDP body, so a verified
// stream reports what it actually carries rather than just "ok".
func VideoCodec(sdp string) string {
	inVideo := false
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			inVideo = strings.HasPrefix(line, "m=video")
		case inVideo && strings.HasPrefix(line, "a=rtpmap:"):
			if _, enc, ok := strings.Cut(line, " "); ok {
				if name, _, ok := strings.Cut(enc, "/"); ok {
					return name
				}
				return enc
			}
		}
	}
	return ""
}
