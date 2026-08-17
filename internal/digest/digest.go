// Package digest implements HTTP Digest Access Authentication (RFC 7616,
// superseding RFC 2617) as a challenge/response pair, without binding it to
// net/http.
//
// Three ANetLink adapters need this and none of them can use the same
// transport: ONVIF's RTSP probe speaks digest over an RTSP socket, Hikvision
// ISAPI and Dahua's CGI speak it over HTTP. The algorithm is identical in
// all three; only the envelope differs. Go's standard library ships Basic
// auth but not Digest, and every camera worth talking to refuses Basic.
package digest

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
	"sync/atomic"
)

// Challenge is a parsed WWW-Authenticate header.
type Challenge struct {
	Realm     string
	Nonce     string
	Opaque    string
	Algorithm string // "MD5", "MD5-sess", "SHA-256", "SHA-256-sess"
	QOP       string // "auth" when the server offered it
	Stale     bool
}

// Credentials answer a challenge. The nonce counter is per-credential state:
// RFC 7616 requires it to strictly increase for a given nonce, and a
// repeated count is exactly what replay protection is looking for.
type Credentials struct {
	Username string
	Password string
	nc       atomic.Uint32
}

// ParseChallenge extracts the Digest challenge from a WWW-Authenticate
// header value. Servers that offer several schemes get scanned for Digest;
// a Basic-only server yields ok=false rather than a silently wrong header.
func ParseChallenge(header string) (Challenge, bool) {
	idx := strings.Index(strings.ToLower(header), "digest ")
	if idx < 0 {
		return Challenge{}, false
	}
	c := Challenge{Algorithm: "MD5"}
	for _, part := range splitParams(header[idx+len("digest "):]) {
		key, val, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch key {
		case "realm":
			c.Realm = val
		case "nonce":
			c.Nonce = val
		case "opaque":
			c.Opaque = val
		case "algorithm":
			c.Algorithm = val
		case "qop":
			// A server may offer "auth,auth-int". We implement auth only,
			// and must not claim auth-int we cannot compute.
			for _, q := range strings.Split(val, ",") {
				if strings.TrimSpace(q) == "auth" {
					c.QOP = "auth"
				}
			}
		case "stale":
			c.Stale = strings.EqualFold(val, "true")
		}
	}
	if c.Nonce == "" {
		return Challenge{}, false
	}
	return c, true
}

// splitParams splits on commas that are not inside quoted strings — nonces
// and opaque values routinely contain commas, and a naive Split corrupts
// them into an authentication failure that looks like a wrong password.
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case ch == '"':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case ch == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func newHash(algorithm string) (func() hash.Hash, bool) {
	switch strings.ToUpper(strings.TrimSuffix(strings.ToUpper(algorithm), "-SESS")) {
	case "", "MD5":
		return md5.New, true
	case "SHA-256":
		return sha256.New, true
	default:
		return nil, false
	}
}

func sum(h func() hash.Hash, parts ...string) string {
	d := h()
	for i, p := range parts {
		if i > 0 {
			d.Write([]byte(":"))
		}
		d.Write([]byte(p))
	}
	return hex.EncodeToString(d.Sum(nil))
}

// Authorize computes the Authorization header value for one request.
// method is the verb ("GET", "DESCRIBE"); uri is the request target exactly
// as it appears on the request line — digest covers the literal string, so
// any normalisation here would break the hash.
func (c *Credentials) Authorize(ch Challenge, method, uri string) (string, error) {
	cnonce := ""
	if ch.QOP == "auth" || strings.HasSuffix(strings.ToUpper(ch.Algorithm), "-SESS") {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		cnonce = hex.EncodeToString(b)
	}
	return c.authorize(ch, method, uri, cnonce, fmt.Sprintf("%08x", c.nc.Add(1)))
}

// authorize is Authorize with the two client-chosen nonces supplied rather
// than generated, so the computation can be pinned against the published
// RFC 7616 vectors instead of only being tested against itself.
func (c *Credentials) authorize(ch Challenge, method, uri, cnonce, ncValue string) (string, error) {
	h, ok := newHash(ch.Algorithm)
	if !ok {
		return "", fmt.Errorf("digest: unsupported algorithm %q", ch.Algorithm)
	}

	ha1 := sum(h, c.Username, ch.Realm, c.Password)
	if strings.HasSuffix(strings.ToUpper(ch.Algorithm), "-SESS") {
		ha1 = sum(h, ha1, ch.Nonce, cnonce)
	}
	ha2 := sum(h, method, uri)

	var response string
	if ch.QOP == "auth" {
		response = sum(h, ha1, ch.Nonce, ncValue, cnonce, "auth", ha2)
	} else {
		response = sum(h, ha1, ch.Nonce, ha2)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
		c.Username, ch.Realm, ch.Nonce, uri, response)
	if ch.Algorithm != "" {
		fmt.Fprintf(&b, `, algorithm=%s`, ch.Algorithm)
	}
	if ch.QOP == "auth" {
		fmt.Fprintf(&b, `, qop=auth, nc=%s, cnonce="%s"`, ncValue, cnonce)
	}
	if ch.Opaque != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, ch.Opaque)
	}
	return b.String(), nil
}
