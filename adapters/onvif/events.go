package onvif

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetLink/profile"
)

// Event pump tuning. PullMessages is a long poll: the camera holds the
// request open until something happens or the timeout expires, which is why
// the poll window is long and the retry backoff is short.
const (
	pullTimeout   = 30 * time.Second
	pullLimit     = 100
	resubscribeIn = 5 * time.Second
)

// topicCapability maps an ONVIF event topic to a TSIR-D capability.
//
// Topics are matched by suffix because vendors prefix them inconsistently
// (tns1:, tnsaxis:, bare) and nest them differently, but agree on the leaf.
// An unmapped topic is ignored rather than guessed at — inventing a motion
// event from an unrecognised topic is worse than reporting nothing.
func topicCapability(topic string) (string, bool) {
	t := strings.ToLower(topic)
	switch {
	case strings.Contains(t, "motionalarm"), strings.Contains(t, "cellmotiondetector"),
		strings.Contains(t, "motiondetect"):
		return profile.CapEventMotion, true
	case strings.Contains(t, "tamper"):
		return profile.CapEventTamper, true
	case strings.Contains(t, "linedetector"), strings.Contains(t, "linecross"):
		return profile.CapEventLineCross, true
	}
	return "", false
}

// eventState reads the boolean an event carries. ONVIF puts it in a Data
// SimpleItem whose name varies by vendor and rule type (State, IsMotion,
// IsTamper, Crossed…), so the value is taken from the first data item that
// parses as a boolean.
func eventState(items []simpleItem) (float64, bool) {
	for _, it := range items {
		switch strings.ToLower(it.Value) {
		case "true", "1":
			return 1, true
		case "false", "0":
			return 0, true
		}
	}
	return 0, false
}

// pumpEvents holds a PullPoint subscription open and forwards what the
// camera reports to the sink. It returns when done closes; every wait in
// here selects on that channel so Stop never blocks on a long poll.
func (a *Adapter) pumpEvents(d *device, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}

		addr, err := a.subscribe(d, done)
		if err != nil {
			if !sleepOrDone(resubscribeIn, done) {
				return
			}
			continue
		}
		// Pull until the subscription breaks, then start a new one. Cameras
		// expire subscriptions on their own schedule; treating that as fatal
		// would mean events stop silently after a few minutes.
		a.pullLoop(d, addr, done)
	}
}

func (a *Adapter) subscribe(d *device, done <-chan struct{}) (string, error) {
	ctx, cancel := contextWithDone(a.cfg.Timeout, done)
	defer cancel()

	var sub subscriptionResponse
	body := `<tev:CreatePullPointSubscription>` +
		`<tev:InitialTerminationTime>PT600S</tev:InitialTerminationTime>` +
		`</tev:CreatePullPointSubscription>`
	if err := d.cli.call(ctx, d.eventsX, nsEvents+"/CreatePullPointSubscription", body, &sub); err != nil {
		return "", err
	}
	addr := strings.TrimSpace(sub.Body.SubscriptionReference.Address)
	if addr == "" {
		addr = d.eventsX // some cameras pull from the events service itself
	}
	return addr, nil
}

func (a *Adapter) pullLoop(d *device, addr string, done <-chan struct{}) {
	body := `<tev:PullMessages>` +
		`<tev:Timeout>PT30S</tev:Timeout>` +
		`<tev:MessageLimit>` + strconv.Itoa(pullLimit) + `</tev:MessageLimit>` +
		`</tev:PullMessages>`

	for {
		select {
		case <-done:
			return
		default:
		}

		// The poll deadline must exceed the camera's own hold time, or every
		// quiet interval would look like a broken subscription.
		ctx, cancel := contextWithDone(pullTimeout+a.cfg.Timeout, done)
		var msgs pullMessagesResponse
		err := d.cli.call(ctx, addr, nsEvents+"/PullMessages", body, &msgs)
		cancel()
		if err != nil {
			return // subscription is gone; caller re-subscribes
		}
		a.dispatch(d, msgs.Body.Messages)
	}
}

func (a *Adapter) dispatch(d *device, msgs []notificationMessage) {
	a.mu.Lock()
	sink := a.sink
	a.mu.Unlock()
	if sink == nil {
		return
	}
	for _, m := range msgs {
		capID, ok := topicCapability(m.Topic)
		if !ok {
			continue
		}
		if _, declared := d.caps[capID]; !declared {
			continue
		}
		v, ok := eventState(m.Message.Inner.Data)
		if !ok {
			continue
		}
		sink.State(d.cfg.ID, map[string]float64{strings.ReplaceAll(capID, ".", "_"): v})
	}
}

// contextWithDone builds a context that expires on timeout or when done
// closes, so a Stop cuts an in-flight long poll instead of waiting it out.
func contextWithDone(timeout time.Duration, done <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	stop := make(chan struct{})
	go func() {
		select {
		case <-done:
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}

// sleepOrDone waits, reporting false if the adapter is stopping.
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
