package adap

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANetLink/internal/peercred"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// peerCredOf is peercred.Of; a variable only so a test can stand in for a
// connection from another user.
var peerCredOf = peercred.Of

// Host runs one out-of-tree adapter process and presents it to the runtime
// as an ordinary sdk.Adapter.
//
// That equivalence is the whole design. The runtime resolves capabilities,
// applies quirks, builds effects and signs evidence exactly as it does for
// a compiled-in adapter; nothing in it learns that this one lives in
// another process, another language, or another trust domain. If the
// escape hatch had its own path through the runtime we would be maintaining
// two architectures, and the second one would rot.
type Host struct {
	cfg HostConfig

	mu       sync.Mutex
	conn     *Conn
	ln       net.Listener
	sink     sdk.Sink
	info     sdk.Info
	pending  map[string]chan *Message
	devices  map[string]sdk.Device
	stopping bool
	stopped  chan struct{}
}

// HostConfig configures one hosted adapter.
type HostConfig struct {
	// Name is the adapter name the runtime knows it by.
	Name string
	// Socket is the UDS path. Created 0600 in a directory nobody else can
	// write (spec §6) — the socket is the authorisation boundary. Only
	// anetlinkd's own user may connect: the adapter process runs as that
	// user, and the kernel's word on the peer is checked at every accept.
	Socket string
	// Timeout bounds one request/response exchange.
	Timeout time.Duration
}

// NewHost prepares a host. Nothing is created until Start.
func NewHost(cfg HostConfig) *Host {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Host{
		cfg:     cfg,
		pending: map[string]chan *Message{},
		devices: map[string]sdk.Device{},
		stopped: make(chan struct{}),
	}
}

func (h *Host) Info() sdk.Info {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.info.Name != "" {
		return h.info
	}
	return sdk.Info{Name: h.cfg.Name, Protocol: "adap", Version: fmt.Sprint(Version)}
}

// socketBackend reports whether an adapter process is actually there.
type socketBackend struct{ h *Host }

func (b socketBackend) Name() string { return "adap" }

func (b socketBackend) Probe(ctx context.Context) sdk.Availability {
	// The adapter connects to us, so before Start there is nothing to probe
	// but the ability to own the socket path. Reporting available here and
	// failing in Start would be the wrong shape: a path we cannot bind is a
	// configuration error an operator can fix, and it should say so now.
	if !peercred.Supported() {
		return sdk.Availability{Available: false,
			Detail: fmt.Sprintf("%s cannot report who connects to a Unix socket; out-of-tree adapters need Linux, macOS or FreeBSD", goruntime.GOOS)}
	}
	if _, err := peercred.PrepareDir(filepath.Dir(b.h.cfg.Socket)); err != nil {
		return sdk.Availability{Available: false, Detail: err.Error()}
	}
	return sdk.Availability{Available: true,
		Detail: fmt.Sprintf("out-of-tree adapter over ADAP v%d at %s", Version, b.h.cfg.Socket)}
}

func (h *Host) Backends() []sdk.Backend { return []sdk.Backend{socketBackend{h}} }

// Start binds the socket and waits for the adapter to say hello.
func (h *Host) Start(ctx context.Context, _ sdk.Backend, sink sdk.Sink) error {
	// The directory is checked again here, not only in Probe: Start is the
	// moment that matters. Then only a stale socket (nobody listening) is
	// removed — a path that is a live socket, a symlink or anything else
	// belongs to someone, and is refused rather than taken over.
	if !peercred.Supported() {
		return fmt.Errorf("adap: %s cannot report who connects to a Unix socket; out-of-tree adapters need Linux, macOS or FreeBSD", goruntime.GOOS)
	}
	if err := peercred.CheckPathLen(h.cfg.Socket); err != nil {
		return fmt.Errorf("adap: %w", err)
	}
	if _, err := peercred.PrepareDir(filepath.Dir(h.cfg.Socket)); err != nil {
		return fmt.Errorf("adap: %w", err)
	}
	if err := peercred.ClearPath(h.cfg.Socket); err != nil {
		return fmt.Errorf("adap: %w", err)
	}
	ln, err := net.Listen("unix", h.cfg.Socket)
	if err != nil {
		return fmt.Errorf("adap: listen %s: %w", h.cfg.Socket, err)
	}
	if err := os.Chmod(h.cfg.Socket, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("adap: secure %s: %w", h.cfg.Socket, err)
	}
	h.mu.Lock()
	h.ln, h.sink = ln, sink
	h.mu.Unlock()

	// The goroutine owns the listener it was started with rather than
	// re-reading the field: Stop() nils that field under the lock, and a
	// loop that reads it on every iteration is a data race with the close
	// that is meant to end it. Closing the listener is what stops the loop.
	go h.accept(ln)
	return nil
}

func (h *Host) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			h.mu.Lock()
			stopping := h.stopping
			h.mu.Unlock()
			if stopping {
				return
			}
			log.Printf("adap %s: accept: %v", h.cfg.Name, err)
			return
		}
		if !h.permitted(c) {
			c.Close()
			continue
		}
		h.mu.Lock()
		if h.conn != nil {
			// One adapter per socket. A second connection is either a
			// misconfiguration or someone else reaching for the socket, and
			// silently multiplexing them would make device ownership
			// ambiguous.
			h.mu.Unlock()
			log.Printf("adap %s: refusing a second connection", h.cfg.Name)
			c.Close()
			continue
		}
		conn := NewConn(c)
		h.conn = conn
		h.mu.Unlock()
		go h.serve(conn)
	}
}

// permitted enforces the peer-credential check (spec §6, AL-10): the
// adapter process must run as anetlinkd's own user. Nothing is read from
// a connection that fails it.
func (h *Host) permitted(c net.Conn) bool {
	cred, err := peerCredOf(c)
	if err != nil {
		log.Printf("adap %s: closed a connection whose peer could not be identified: %v", h.cfg.Name, err)
		return false
	}
	if self := os.Getuid(); cred.UID != self {
		log.Printf("adap %s: refused uid %d on %s; the adapter process must run as anetlinkd's own user (uid %d)",
			h.cfg.Name, cred.UID, h.cfg.Socket, self)
		return false
	}
	return true
}

// serve reads frames until the adapter goes away.
func (h *Host) serve(conn *Conn) {
	defer func() {
		h.mu.Lock()
		if h.conn == conn {
			h.conn = nil
			// Every device this adapter claimed is gone with it. Leaving
			// them registered would let the runtime keep offering
			// capabilities nothing can serve.
			for id := range h.devices {
				if h.sink != nil {
					h.sink.RemoveDevice(id)
				}
				delete(h.devices, id)
			}
		}
		h.mu.Unlock()
		conn.Close()
	}()

	for {
		m, err := conn.Recv()
		if err != nil {
			if !isClosed(err) {
				log.Printf("adap %s: %v", h.cfg.Name, err)
			}
			return
		}
		h.dispatch(conn, m)
	}
}

func (h *Host) dispatch(conn *Conn, m *Message) {
	switch m.Op {
	case OpHello:
		h.onHello(conn, m)
	case OpClaim:
		h.onClaim(conn, m)
	case OpDescribe:
		h.onDescribe(conn, m)
	case OpState:
		h.onState(m)
	case OpEvent:
		h.onEvent(m)
	case OpPing:
		_ = conn.Send(&Message{Op: OpPing, RID: m.RID})
	case OpResult, OpError:
		h.deliver(m)
	case OpBye:
		conn.Close()
	default:
		// An op we do not know is not fatal: the sender must have declared
		// it in hello, and ignoring an undeclared one is safer than
		// guessing (spec §8.4).
		_ = conn.Send(&Message{Op: OpError, RID: m.RID,
			Payload: map[string]any{"tag": ErrUnsupported, "detail": "unknown op " + m.Op}})
	}
}

func (h *Host) onHello(conn *Conn, m *Message) {
	name, _ := m.Payload["adapter"].(string)
	proto, _ := m.Payload["protocol"].(string)
	ver, _ := m.Payload["sdk_version"].(string)
	if name == "" {
		name = h.cfg.Name
	}
	h.mu.Lock()
	h.info = sdk.Info{Name: name, Protocol: proto, Version: ver}
	h.mu.Unlock()
	log.Printf("adap %s: adapter connected (protocol=%q sdk=%q)", h.cfg.Name, proto, ver)
	_ = conn.Send(&Message{Op: OpHello, RID: m.RID,
		Payload: map[string]any{"session": m.RID, "adap": uint64(Version)}})
}

// onClaim registers a device the adapter discovered.
func (h *Host) onClaim(conn *Conn, m *Message) {
	id := m.DeviceID
	if id == "" {
		id, _ = m.Payload["native_id"].(string)
	}
	if id == "" {
		_ = conn.Send(&Message{Op: OpError, RID: m.RID,
			Payload: map[string]any{"tag": ErrInvalidArgs, "detail": "claim without a device id"}})
		return
	}
	h.upsert(sdk.Device{ID: id, Online: true})
	_ = conn.Send(&Message{Op: OpClaim, RID: m.RID, DeviceID: id,
		Payload: map[string]any{"device_id": id}})
}

// onDescribe accepts a DeviceProfile.
//
// The profile arrives as CBOR-decoded generic values; rather than hand-roll
// a second decoder for a type the profile package already owns, it is
// round-tripped through JSON into the real struct. That keeps one
// definition of what a profile is, which is the point of having the type.
func (h *Host) onDescribe(conn *Conn, m *Message) {
	raw, ok := m.Payload["profile"]
	if !ok {
		_ = conn.Send(&Message{Op: OpError, RID: m.RID,
			Payload: map[string]any{"tag": ErrInvalidArgs, "detail": "describe without a profile"}})
		return
	}
	b, err := json.Marshal(normalise(raw))
	if err == nil {
		var p profile.DeviceProfile
		if err = json.Unmarshal(b, &p); err == nil {
			h.upsert(sdk.Device{ID: m.DeviceID, Profile: &p, Online: true})
			_ = conn.Send(&Message{Op: OpDescribe, RID: m.RID, DeviceID: m.DeviceID,
				Payload: map[string]any{"accepted": true}})
			return
		}
	}
	_ = conn.Send(&Message{Op: OpError, RID: m.RID,
		Payload: map[string]any{"tag": ErrMalformed, "detail": "profile: " + err.Error()}})
}

func (h *Host) onState(m *Message) {
	metrics := floatMap(m.Payload["metrics"])
	if len(metrics) == 0 || m.DeviceID == "" {
		return
	}
	h.mu.Lock()
	sink := h.sink
	h.mu.Unlock()
	if sink != nil {
		sink.State(m.DeviceID, metrics)
	}
}

// onEvent is state that must not be coalesced.
//
// The backpressure policy collapses successive property values — 22.1,
// 22.3, 22.5 only needs the last. An event may never be collapsed that way
// (spec §5.3): "the door was opened" is not superseded by whatever the door
// reported next, and dropping it loses the fact rather than an old value.
func (h *Host) onEvent(m *Message) {
	h.mu.Lock()
	sink := h.sink
	h.mu.Unlock()
	if sink == nil || m.DeviceID == "" {
		return
	}
	if metrics := floatMap(m.Payload["metrics"]); len(metrics) > 0 {
		sink.State(m.DeviceID, metrics)
	}
}

func (h *Host) upsert(d sdk.Device) {
	h.mu.Lock()
	if old, ok := h.devices[d.ID]; ok && d.Profile == nil {
		d.Profile = old.Profile
	}
	h.devices[d.ID] = d
	sink := h.sink
	h.mu.Unlock()
	if sink != nil {
		sink.UpsertDevice(d)
	}
}

func (h *Host) deliver(m *Message) {
	h.mu.Lock()
	ch, ok := h.pending[m.RID]
	if ok {
		delete(h.pending, m.RID)
	}
	h.mu.Unlock()
	if ok {
		ch <- m
	}
}

// Invoke calls one capability on the hosted adapter.
//
// Retry is not attempted here for any reason. The spec allows retrying an
// idempotent action, but the adapter is the only party that knows whether
// the action it exposes is idempotent, and a runtime that retries on its
// own guess is how a door gets unlocked twice. An out-of-tree adapter that
// wants retries performs them itself, where the knowledge is.
func (h *Host) Invoke(ctx context.Context, call sdk.Call) (effect.Effect, error) {
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()
	if conn == nil {
		return effect.Effect{}, errf(ErrDeviceOffline, "no adapter connected on %s", h.cfg.Socket)
	}

	rid := newRID()
	ch := make(chan *Message, 1)
	h.mu.Lock()
	h.pending[rid] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, rid)
		h.mu.Unlock()
	}()

	ttl := h.cfg.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d < ttl {
			ttl = d
		}
	}
	req := &Message{
		Op: OpInvoke, RID: rid, DeviceID: call.DeviceID, Capability: call.Capability,
		TTL:     uint64(ttl.Milliseconds()),
		Payload: map[string]any{"args": call.Args},
	}
	if err := conn.Send(req); err != nil {
		return effect.Effect{}, err
	}

	select {
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	case <-time.After(ttl):
		return effect.Effect{}, errf(ErrTimeout, "%s did not answer within %s", call.Capability, ttl)
	case resp := <-ch:
		return h.toEffect(call, resp)
	}
}

// toEffect turns a result frame into an honest effect.
//
// The `observed` flag is the whole reason this is not a straight decode: an
// adapter that could not read the value back has not verified anything, and
// the effect must say UNVERIFIED however cleanly the command was accepted
// (spec §4.2). "I sent it" is not "I confirmed it", and the boundary
// between processes is exactly where that distinction gets lost.
func (h *Host) toEffect(call sdk.Call, resp *Message) (effect.Effect, error) {
	if resp.Op == OpError {
		tag, _ := resp.Payload["tag"].(string)
		detail, _ := resp.Payload["detail"].(string)
		if tag == "" {
			tag = ErrAdapterInternal
		}
		return effect.Effect{}, &Error{Tag: tag, Detail: detail}
	}
	metrics := floatMap(resp.Payload["metrics"])
	observed, _ := resp.Payload["observed"].(bool)

	ev := &effect.Evidence{
		Protocol:  h.Info().Protocol,
		NativeAck: true,
	}
	if s, ok := resp.Payload["observed_state"].(string); ok {
		ev.ObservedState = s
	}
	if observed {
		ev.VerifyTrust = 2 // read back from the device itself
	} else {
		ev.VerifyTrust = 1 // the protocol accepted it, nothing more
	}

	eff := effect.Effect{Status: effect.OK, Evidence: ev}
	if len(metrics) > 0 {
		eff.Record = &tsir.EffectRecord{Metrics: metrics}
	}
	if !observed {
		eff.Status = effect.Unverified
		eff.Message = "the adapter could not read the value back"
	}
	return eff, nil
}

// Stop closes the session and releases the socket.
func (h *Host) Stop(context.Context) error {
	h.mu.Lock()
	h.stopping = true
	ln, conn := h.ln, h.conn
	h.ln, h.conn = nil, nil
	h.mu.Unlock()

	if conn != nil {
		_ = conn.Send(&Message{Op: OpBye, RID: newRID()})
		conn.Close()
	}
	if ln != nil {
		// Closing the listener unlinks the socket it created; nothing else
		// at the path is ours to remove.
		ln.Close()
	}
	return nil
}

var _ sdk.Adapter = (*Host)(nil)
