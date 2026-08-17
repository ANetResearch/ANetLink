package dahua

import (
	"bufio"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/ANetResearch/ANetLink/profile"
)

// eventCodes is what we ask the camera to push. Dahua only sends the codes
// you subscribe to, so this list is the event vocabulary of the adapter.
var eventCodes = []string{"VideoMotion", "VideoBlind", "CrossLineDetection", "CrossRegionDetection"}

const reconnectIn = 5 * time.Second

// eventCapability maps a Dahua event code to a TSIR-D capability.
// Unrecognised codes are ignored rather than guessed at.
func eventCapability(code string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "videomotion":
		return profile.CapEventMotion, true
	case "videoblind", "tampering":
		// VideoBlind is Dahua's name for a covered lens — tamper by another
		// name, and the closest honest mapping we have.
		return profile.CapEventTamper, true
	case "crosslinedetection", "crossregiondetection":
		return profile.CapEventLineCross, true
	}
	return "", false
}

// parseEventLine reads Dahua's event payload: "Code=VideoMotion;action=Start;index=0".
func parseEventLine(line string) (code, action string, ok bool) {
	for _, field := range strings.Split(strings.TrimSpace(line), ";") {
		k, v, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "code":
			code = strings.TrimSpace(v)
		case "action":
			action = strings.TrimSpace(v)
		}
	}
	return code, action, code != "" && action != ""
}

func (a *Adapter) pumpEvents(d *device, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		a.streamOnce(d, done)
		if !sleepOrDone(reconnectIn, done) {
			return
		}
	}
}

func (a *Adapter) streamOnce(d *device, done <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-done:
			cancel()
		case <-stop:
		}
	}()

	path := "/cgi-bin/eventManager.cgi?action=attach&codes=[" + strings.Join(eventCodes, ",") + "]"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return
	}
	// The attach feed must not inherit the per-request timeout: it is meant
	// to stay open indefinitely.
	cli := &http.Client{Transport: d.http.Transport}
	resp, err := cli.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}

	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err == nil && strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" {
		a.readMultipart(d, resp.Body, params["boundary"])
		return
	}
	// Some firmware answers with a bare line stream rather than multipart.
	a.readLines(d, resp.Body)
}

func (a *Adapter) readMultipart(d *device, body io.Reader, boundary string) {
	mr := multipart.NewReader(body, boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return
		}
		raw, err := io.ReadAll(io.LimitReader(part, maxText))
		part.Close()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(raw), "\n") {
			a.dispatchLine(d, line)
		}
	}
}

func (a *Adapter) readLines(d *device, body io.Reader) {
	sc := bufio.NewScanner(io.LimitReader(body, maxText))
	for sc.Scan() {
		a.dispatchLine(d, sc.Text())
	}
}

func (a *Adapter) dispatchLine(d *device, line string) {
	code, action, ok := parseEventLine(line)
	if !ok {
		return
	}
	capID, ok := eventCapability(code)
	if !ok {
		return
	}
	if _, declared := d.caps[capID]; !declared {
		return
	}
	// Dahua brackets an event with Start and Stop; "Pulse" is a momentary
	// event that has no Stop, so it reports as present.
	v := 0.0
	switch strings.ToLower(action) {
	case "start", "pulse":
		v = 1
	}

	a.mu.Lock()
	sink := a.sink
	a.mu.Unlock()
	if sink == nil {
		return
	}
	sink.State(d.cfg.ID, map[string]float64{strings.ReplaceAll(capID, ".", "_"): v})
}

func sleepOrDone(d time.Duration, done <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-done:
		return false
	}
}
