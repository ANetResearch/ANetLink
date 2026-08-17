package hikvision

import (
	"context"
	"encoding/xml"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/ANetResearch/ANetLink/profile"
)

// alertStreamPath is Hikvision's push feed: one long-lived HTTP response
// that never ends, carrying a multipart stream of event XML. This is the
// reason the vendor adapter earns its place next to ONVIF — ONVIF's
// PullPoint gives motion and tamper, while this feed carries the analytics
// events (line crossing, intrusion, region entry) that customers actually
// bought the camera for.
const alertStreamPath = "/ISAPI/Event/notification/alertStream"

// reconnectIn is how long to wait after the feed drops. Cameras close the
// stream on their own schedule; treating that as fatal would mean events
// stop silently after a few minutes.
const reconnectIn = 5 * time.Second

// eventNotificationAlert is one alert from the feed.
type eventNotificationAlert struct {
	ChannelID   int    `xml:"channelID"`
	DynChannel  int    `xml:"dynChannelID"`
	EventType   string `xml:"eventType"`
	EventState  string `xml:"eventState"`
	Description string `xml:"eventDescription"`
}

// eventCapability maps a Hikvision eventType to a TSIR-D capability.
//
// Unmapped types are ignored rather than guessed at. The vendor vocabulary
// is long and grows with every firmware; inventing a motion event from an
// unrecognised type would be worse than reporting nothing.
func eventCapability(eventType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "vmd", "motiondetection":
		return profile.CapEventMotion, true
	case "shelteralarm", "tamperdetection":
		return profile.CapEventTamper, true
	case "linedetection", "fielddetection", "regionentrance", "regionexiting":
		// Field/region detection is a tripwire in all but name: a boundary
		// was crossed. Mapping them together keeps the capability vocabulary
		// honest about what it can distinguish.
		return profile.CapEventLineCross, true
	}
	return "", false
}

// pumpEvents holds the alertStream open and forwards what the camera
// reports. It returns when done closes.
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

// streamOnce consumes one alertStream connection until it breaks.
func (a *Adapter) streamOnce(d *device, done <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel the in-flight read when the adapter stops, rather than waiting
	// for a feed that by design never ends.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-done:
			cancel()
		case <-stop:
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+alertStreamPath, nil)
	if err != nil {
		return
	}
	// The alert stream must not inherit the per-request timeout: it is
	// supposed to stay open indefinitely.
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
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return
	}
	boundary := params["boundary"]
	if boundary == "" {
		return
	}

	mr := multipart.NewReader(resp.Body, boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return // stream ended or was cancelled; caller reconnects
		}
		raw, err := io.ReadAll(io.LimitReader(part, maxXML))
		part.Close()
		if err != nil {
			return
		}
		var alert eventNotificationAlert
		if xml.Unmarshal(raw, &alert) != nil {
			continue
		}
		a.dispatch(d, alert)
	}
}

func (a *Adapter) dispatch(d *device, alert eventNotificationAlert) {
	capID, ok := eventCapability(alert.EventType)
	if !ok {
		return
	}
	if _, declared := d.caps[capID]; !declared {
		return
	}
	// Hikvision sends "inactive" alerts as heartbeats as well as real
	// clears; both mean the same thing to a consumer — the condition is not
	// currently present.
	v := 0.0
	if strings.EqualFold(alert.EventState, "active") {
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
