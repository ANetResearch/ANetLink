package digest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The RFC 7616 §3.9.1 worked example. Pinning against the published vector
// is the only way to know our digest is right rather than merely
// self-consistent — a round-trip test against our own verifier would pass
// with the field order or the qop term wrong, and every camera would reject
// us for reasons no test could explain.
const (
	rfcRealm  = "http-auth@example.org"
	rfcNonce  = "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v"
	rfcOpaque = "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"
	rfcCnonce = "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"
	rfcURI    = "/dir/index.html"
	rfcUser   = "Mufasa"
	rfcPass   = "Circle of Life"
)

func responseOf(t *testing.T, header string) string {
	t.Helper()
	for _, p := range splitParams(header) {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "response") {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	t.Fatalf("no response parameter in %q", header)
	return ""
}

func TestRFC7616MD5Vector(t *testing.T) {
	c := &Credentials{Username: rfcUser, Password: rfcPass}
	ch := Challenge{Realm: rfcRealm, Nonce: rfcNonce, Opaque: rfcOpaque, Algorithm: "MD5", QOP: "auth"}

	got, err := c.authorize(ch, http.MethodGet, rfcURI, rfcCnonce, "00000001")
	if err != nil {
		t.Fatal(err)
	}
	const want = "8ca523f5e9506fed4657c9700eebdbec"
	if r := responseOf(t, got); r != want {
		t.Fatalf("RFC 7616 MD5 response:\n got  %s\n want %s", r, want)
	}
}

func TestRFC7616SHA256Vector(t *testing.T) {
	c := &Credentials{Username: rfcUser, Password: rfcPass}
	ch := Challenge{Realm: rfcRealm, Nonce: rfcNonce, Opaque: rfcOpaque, Algorithm: "SHA-256", QOP: "auth"}

	got, err := c.authorize(ch, http.MethodGet, rfcURI, rfcCnonce, "00000001")
	if err != nil {
		t.Fatal(err)
	}
	const want = "753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"
	if r := responseOf(t, got); r != want {
		t.Fatalf("RFC 7616 SHA-256 response:\n got  %s\n want %s", r, want)
	}
}

// TestRFC2069LegacyNoQOP: cameras predating qop are still deployed, and the
// response term is computed differently for them.
func TestRFC2069LegacyNoQOP(t *testing.T) {
	c := &Credentials{Username: "user", Password: "pass"}
	ch := Challenge{Realm: "test", Nonce: "abc", Algorithm: "MD5"}
	got, err := c.authorize(ch, http.MethodGet, "/x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "qop=") || strings.Contains(got, "cnonce=") {
		t.Fatalf("a challenge without qop must not produce qop/cnonce: %s", got)
	}
	// HA1=md5(user:test:pass), HA2=md5(GET:/x), response=md5(HA1:abc:HA2),
	// computed independently rather than captured from this implementation.
	const want = "5b69995755d2a865ca198513b2540236"
	if r := responseOf(t, got); r != want {
		t.Fatalf("RFC 2069 response:\n got  %s\n want %s", r, want)
	}
}

func TestParseChallenge(t *testing.T) {
	// A nonce containing a comma is the case a naive strings.Split corrupts
	// into an authentication failure that looks like a wrong password.
	hdr := `Digest realm="cam", qop="auth,auth-int", nonce="aa,bb", opaque="o1", algorithm=MD5, stale=TRUE`
	ch, ok := ParseChallenge(hdr)
	if !ok {
		t.Fatal("challenge must parse")
	}
	if ch.Nonce != "aa,bb" {
		t.Fatalf("quoted comma in nonce: %q", ch.Nonce)
	}
	if ch.QOP != "auth" {
		t.Fatalf("auth-int must not be claimed: %q", ch.QOP)
	}
	if ch.Realm != "cam" || ch.Opaque != "o1" || !ch.Stale {
		t.Fatalf("challenge fields: %+v", ch)
	}

	if _, ok := ParseChallenge(`Basic realm="cam"`); ok {
		t.Fatal("a Basic-only challenge must not parse as digest")
	}
	if _, ok := ParseChallenge(`Digest realm="cam"`); ok {
		t.Fatal("a challenge with no nonce is unusable and must be rejected")
	}
}

func TestNonceCountIncreases(t *testing.T) {
	c := &Credentials{Username: "u", Password: "p"}
	ch := Challenge{Realm: "r", Nonce: "n", Algorithm: "MD5", QOP: "auth"}
	first, _ := c.Authorize(ch, "GET", "/a")
	second, _ := c.Authorize(ch, "GET", "/a")
	if !strings.Contains(first, "nc=00000001") || !strings.Contains(second, "nc=00000002") {
		t.Fatalf("nc must strictly increase:\n %s\n %s", first, second)
	}
	if responseOf(t, first) == responseOf(t, second) {
		t.Fatal("a repeated response would defeat replay protection")
	}
}

// TestTransportAuthenticates drives the RoundTripper against a server that
// demands digest, proving the 401-retry path works end to end.
func TestTransportAuthenticates(t *testing.T) {
	var challenges, authed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			challenges++
			w.Header().Set("WWW-Authenticate",
				`Digest realm="cam", qop="auth", nonce="deadbeef", opaque="op"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authed++
		if !strings.Contains(r.Header.Get("Authorization"), `uri="/snap"`) {
			t.Errorf("digest uri must be the request target: %s", r.Header.Get("Authorization"))
		}
		w.Write([]byte("frame"))
	}))
	defer srv.Close()

	cli := &http.Client{Transport: NewTransport(nil, "admin", "secret")}
	for i := 0; i < 3; i++ {
		resp, err := cli.Get(srv.URL + "/snap")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}
	if authed != 3 {
		t.Fatalf("all three requests must authenticate, got %d", authed)
	}
	// Only the first should have needed a challenge; the rest reuse it.
	if challenges != 1 {
		t.Fatalf("challenge should be cached and reused, saw %d challenges", challenges)
	}
}

// TestTransportReplaysBody: a POST that gets challenged must resend its body.
// Without buffering, the retry sends an empty body and the camera stores
// nothing while reporting success.
func TestTransportReplaysBody(t *testing.T) {
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="c", qop="auth", nonce="n1"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		lastBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli := &http.Client{Transport: NewTransport(nil, "u", "p")}
	resp, err := cli.Post(srv.URL+"/cfg", "application/xml", strings.NewReader("<cfg>on</cfg>"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if lastBody != "<cfg>on</cfg>" {
		t.Fatalf("body must survive the 401 retry, server saw %q", lastBody)
	}
}

// TestNonDigestChallengePassesThrough: a Basic-only server must yield its
// 401 verbatim rather than being retried forever with a header it will
// never accept.
func TestNonDigestChallengePassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="cam"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	cli := &http.Client{Transport: NewTransport(nil, "u", "p")}
	resp, err := cli.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the 401 to pass through, got %d", resp.StatusCode)
	}
}
