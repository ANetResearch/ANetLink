package adap

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"
)

// Plugin is the adapter side of ADAP: everything an out-of-tree adapter
// needs beyond its own protocol knowledge.
//
// It exists so the Go answer to "how do I write an out-of-tree adapter" is
// thirty lines rather than a CBOR tutorial. Other languages hand-write the
// frames — that is the stated reason the wire is CBOR and not gRPC (spec
// §9): no toolchain, no generated code, and an EffectRecord that crosses
// the socket without a translation step.
type Plugin struct {
	Name     string
	Protocol string
	Version  string

	// Invoke performs one action. Returning observed=false is not a
	// failure: it says the action was accepted and could not be read back,
	// and the runtime will mark the effect UNVERIFIED rather than OK. An
	// adapter that cannot verify must say so — claiming success it did not
	// observe is the one thing this protocol exists to prevent.
	Invoke func(ctx context.Context, deviceID, capability string, args map[string]any) (metrics map[string]float64, observed bool, err error)

	conn *Conn
}

// Dial connects to the runtime's socket and completes the handshake.
func (p *Plugin) Dial(ctx context.Context, socket string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("adap: dial %s: %w", socket, err)
	}
	p.conn = NewConn(c)
	if err := p.conn.Send(&Message{Op: OpHello, RID: newRID(), Payload: map[string]any{
		"adapter": p.Name, "protocol": p.Protocol, "sdk_version": p.Version,
		"adap_versions": []any{uint64(Version)},
	}}); err != nil {
		return err
	}
	resp, err := p.conn.Recv()
	if err != nil {
		return err
	}
	if resp.Op != OpHello {
		return fmt.Errorf("adap: expected hello, got %s", resp.Op)
	}
	return nil
}

// Claim announces a device.
func (p *Plugin) Claim(deviceID string) error {
	return p.conn.Send(&Message{Op: OpClaim, RID: newRID(), DeviceID: deviceID,
		Payload: map[string]any{"native_id": deviceID}})
}

// Describe publishes a device's profile. The profile is passed as the
// decoded shape rather than the Go type so a plugin need not import
// ANetLink at all.
func (p *Plugin) Describe(deviceID string, profile map[string]any) error {
	return p.conn.Send(&Message{Op: OpDescribe, RID: newRID(), DeviceID: deviceID,
		Payload: map[string]any{"profile": profile}})
}

// State reports property values. These may be coalesced under backpressure.
func (p *Plugin) State(deviceID string, metrics map[string]float64) error {
	return p.conn.Send(&Message{Op: OpState, RID: newRID(), DeviceID: deviceID,
		Payload: map[string]any{"metrics": toAny(metrics)}})
}

// Event reports something that happened. Unlike State it must never be
// coalesced away: "the door was opened" is not superseded by the next
// reading (spec §5.3).
func (p *Plugin) Event(deviceID string, metrics map[string]float64) error {
	return p.conn.Send(&Message{Op: OpEvent, RID: newRID(), DeviceID: deviceID,
		Payload: map[string]any{"metrics": toAny(metrics)}})
}

// Serve handles requests until the socket closes.
func (p *Plugin) Serve(ctx context.Context) error {
	for {
		m, err := p.conn.Recv()
		if err != nil {
			if isClosed(err) {
				return nil
			}
			return err
		}
		switch m.Op {
		case OpPing:
			_ = p.conn.Send(&Message{Op: OpPing, RID: m.RID})
		case OpBye:
			return p.conn.Close()
		case OpInvoke:
			p.handleInvoke(ctx, m)
		default:
			_ = p.conn.Send(&Message{Op: OpError, RID: m.RID, Payload: map[string]any{
				"tag": ErrUnsupported, "detail": "this adapter does not implement " + m.Op}})
		}
	}
}

func (p *Plugin) handleInvoke(ctx context.Context, m *Message) {
	// The expiry check happens before the action, never after.
	//
	// A late "unlock the door" is not a slow success, it is a command for a
	// moment that has passed, and executing it then reporting the delay
	// would be exactly the wrong order (spec §5.1.4).
	if Expired(m, time.Now()) {
		_ = p.conn.Send(&Message{Op: OpError, RID: m.RID, Payload: map[string]any{
			"tag": ErrExpired, "detail": "message outlived its ttl; not executed"}})
		return
	}
	if p.Invoke == nil {
		_ = p.conn.Send(&Message{Op: OpError, RID: m.RID, Payload: map[string]any{
			"tag": ErrUnsupported, "detail": "this adapter takes no actions"}})
		return
	}
	args, _ := normalise(m.Payload["args"]).(map[string]any)
	metrics, observed, err := p.Invoke(ctx, m.DeviceID, m.Capability, args)
	if err != nil {
		tag := ErrAdapterInternal
		if ae, ok := AsError(err); ok {
			tag = ae.Tag
		}
		_ = p.conn.Send(&Message{Op: OpError, RID: m.RID,
			Payload: map[string]any{"tag": tag, "detail": err.Error()}})
		return
	}
	if err := p.conn.Send(&Message{Op: OpResult, RID: m.RID, DeviceID: m.DeviceID,
		Payload: map[string]any{"metrics": toAny(metrics), "observed": observed}}); err != nil {
		log.Printf("adap %s: result: %v", p.Name, err)
	}
}

// Close ends the session.
func (p *Plugin) Close() error {
	if p.conn == nil {
		return nil
	}
	_ = p.conn.Send(&Message{Op: OpBye, RID: newRID()})
	return p.conn.Close()
}

func toAny(m map[string]float64) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
