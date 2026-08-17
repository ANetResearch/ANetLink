package digest

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// Transport is an http.RoundTripper that answers Digest challenges. Cameras
// (Hikvision ISAPI, Dahua CGI, ONVIF snapshot endpoints) universally reject
// Basic auth, and Go ships no Digest client.
//
// The learned challenge is cached and reused, so steady-state traffic costs
// one round trip rather than two. A server that rotates its nonce answers
// 401 with stale=true and the next attempt re-learns it.
type Transport struct {
	Base  http.RoundTripper
	Creds *Credentials

	mu   sync.Mutex
	last *Challenge
}

// NewTransport builds a digest-authenticating transport over base.
func NewTransport(base http.RoundTripper, username, password string) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{Base: base, Creds: &Credentials{Username: username, Password: password}}
}

func (t *Transport) cached() *Challenge {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

func (t *Transport) remember(c Challenge) {
	t.mu.Lock()
	t.last = &c
	t.mu.Unlock()
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body once: a 401 means the request must be replayed, and a
	// consumed stream cannot be replayed. Without this, every authenticated
	// POST would silently send an empty body on the second attempt.
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	attempt := func(auth string) (*http.Response, error) {
		r := req.Clone(req.Context())
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return t.Base.RoundTrip(r)
	}

	// Pre-authenticate with the challenge already learned, if any.
	auth := ""
	if ch := t.cached(); ch != nil {
		if a, err := t.Creds.Authorize(*ch, req.Method, req.URL.RequestURI()); err == nil {
			auth = a
		}
	}

	resp, err := attempt(auth)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	ch, ok := ParseChallenge(resp.Header.Get("WWW-Authenticate"))
	if !ok {
		return resp, nil // not a digest challenge; hand the 401 back verbatim
	}
	// Drain and close so the connection can be reused for the retry.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	t.remember(ch)
	a, err := t.Creds.Authorize(ch, req.Method, req.URL.RequestURI())
	if err != nil {
		return nil, fmt.Errorf("digest: %w", err)
	}
	return attempt(a)
}
