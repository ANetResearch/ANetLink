// Package c1serv is ANetLink's C1 surface: HTTP over a Unix socket that the
// ANet daemon's provider shim consumes. The wire carries capabilities as
// opaque ids of the form "<capability>@<adapter>/<device>" — the daemon
// never parses them (red line: no device concept crosses C1). The runtime
// mints the form (it matches c1.remote against it); this package resolves
// it.
//
// Wire (C1 v1; key numbers in core §2.11, reasons in core §2.15.3):
//
//	GET  /v1/health        → 204
//	GET  /v1/capabilities  → 200 CapList   {1: [{1: id, 2: remote}, ...]}
//	GET  /v1/describe      → 200 Describe  {1: cid}  ("" = no catalog yet)
//	POST /v1/invoke        InvokeReq {1: capability, 2?: args, 3?: call_id,
//	                                  5: caller {1: via, 2?: aid}}
//	                       → 200 InvokeResp {1: status, 2?: metrics,
//	                                         3?: message, 4?: evidence}
//	                       → 4xx/5xx Refusal {1: reason}
//	any  /v0/*             → 426 Refusal{c1_version}
//	anything else          → 404 Refusal{not_found}
//
// CoreDet-CBOR is the only encoding, both ways. Requests are decoded with
// c1DecMode, the same options as the daemon's coredet.UnmarshalStrict, so
// an unregistered key, a retired key, a duplicate key, a tag or invalid
// UTF-8 is refused the same way on both ends. A refusal carries the reason
// and nothing else; the detail goes to the local log only.
package c1serv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/fxamacker/cbor/v2"

	"github.com/ANetResearch/ANetLink/runtime"
)

const (
	// contentCBOR is the one media type C1 speaks.
	contentCBOR = "application/cbor"
	// maxBody bounds a request body (LEC §2.1.2).
	maxBody = 1 << 20
)

// The C1 refusal reasons and their HTTP status (core §2.15.3). The set is
// closed: a reason outside it is a wire change.
var refusalStatus = map[string]int{
	"not_permitted":      http.StatusForbidden,
	"bad_request":        http.StatusBadRequest,
	"too_large":          http.StatusRequestEntityTooLarge,
	"unsupported_media":  http.StatusUnsupportedMediaType,
	"unknown_capability": http.StatusNotFound,
	"c1_version":         http.StatusUpgradeRequired,
	"not_found":          http.StatusNotFound,
	"internal":           http.StatusInternalServerError,
}

// c1DecMode is the strict decoder for everything C1 receives (AL-5): the
// options of ANetCore v0.4.2 coredet (no tags, duplicate keys refused)
// plus unknown-field errors. That is exactly the option set of the
// daemon's coredet.UnmarshalStrict (core CORE-E4); ANetLink stays on
// v0.4.2, which has no UnmarshalStrict, so it builds the mode here.
var c1DecMode cbor.DecMode

func init() {
	dm, err := cbor.DecOptions{
		TagsMd:            cbor.TagsForbidden,
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
	}.DecMode()
	if err != nil {
		panic(fmt.Sprintf("c1serv: decode options: %v", err))
	}
	c1DecMode = dm
	refusalBodies = map[string][]byte{}
	for reason := range refusalStatus {
		b, err := coredet.Marshal(refusal{Reason: reason})
		if err != nil {
			panic(fmt.Sprintf("c1serv: refusal %s: %v", reason, err))
		}
		refusalBodies[reason] = b
	}
}

// Wire values of Caller.via.
const (
	viaLocal uint64 = 0
	viaRelay uint64 = 1
	// 2 is retired (the v0 voucher path) and refused like any other value.
)

type capList struct {
	Caps []capEntry `cbor:"1,keyasint"`
}

type capEntry struct {
	ID     string `cbor:"1,keyasint"`
	Remote uint64 `cbor:"2,keyasint"` // 0 = none, 1 = relay
}

type describe struct {
	CID string `cbor:"1,keyasint"`
}

// invokeReq is the request as received. Required members are pointers so
// "absent" is visible; there is deliberately no field for retired key 4,
// so the unknown-field rule refuses it.
type invokeReq struct {
	Capability *string        `cbor:"1,keyasint,omitempty"`
	Args       map[string]any `cbor:"2,keyasint,omitempty"`
	CallID     *string        `cbor:"3,keyasint,omitempty"`
	Caller     *caller        `cbor:"5,keyasint,omitempty"`
}

// caller has no field for key 3 (reserved; it was witnessed), for the same
// reason.
type caller struct {
	Via *uint64 `cbor:"1,keyasint,omitempty"`
	AID *string `cbor:"2,keyasint,omitempty"`
}

type invokeResp struct {
	Status   string             `cbor:"1,keyasint"`
	Metrics  map[string]float64 `cbor:"2,keyasint,omitempty"`
	Message  string             `cbor:"3,keyasint,omitempty"`
	Evidence *evidenceWire      `cbor:"4,keyasint,omitempty"`
}

type refusal struct {
	Reason string `cbor:"1,keyasint"`
}

// evidenceWire mirrors effect.Evidence on the C1 wire.
//
// Every field, keyed to match effect.Evidence, and Quirk is the one that
// makes the list load-bearing. The runtime stamps it whenever a correction
// changed a reading, because a corrected value is not the value the device
// put on the wire — and this struct was missing it, so the label stopped
// here. The consumer north of C1 saw the corrected number and no way to
// learn it had been corrected, which is the single thing the quirk layer
// is built to prevent: a correction reaches every surface or none.
//
// Adding a field to effect.Evidence means adding it here. There is no
// mechanical guard for that; TestEveryEvidenceFieldCrossesTheWire is what
// catches it.
type evidenceWire struct {
	Requested     string `cbor:"1,keyasint,omitempty"`
	Protocol      string `cbor:"2,keyasint,omitempty"`
	NativeAck     bool   `cbor:"3,keyasint,omitempty"`
	ObservedState string `cbor:"4,keyasint,omitempty"`
	LatencyMS     int64  `cbor:"5,keyasint,omitempty"`
	VerifyTrust   uint8  `cbor:"6,keyasint,omitempty"`
	AuthTrust     uint8  `cbor:"7,keyasint,omitempty"`
	Quirk         string `cbor:"8,keyasint,omitempty"`
}

// wireEvidence maps provenance onto the wire. A function rather than an
// inline literal so a test can hold it against effect.Evidence field by
// field: this mapping silently dropped Quirk, and an inline literal is
// exactly the shape nothing can check.
func wireEvidence(ev *effect.Evidence) *evidenceWire {
	if ev == nil {
		return nil
	}
	return &evidenceWire{
		Requested: clean(ev.Requested), Protocol: clean(ev.Protocol), NativeAck: ev.NativeAck,
		ObservedState: clean(ev.ObservedState), LatencyMS: ev.LatencyMS,
		VerifyTrust: ev.VerifyTrust, AuthTrust: ev.AuthTrust,
		Quirk: clean(ev.Quirk),
	}
}

// clean makes device-supplied text valid UTF-8 (AL-5). v0.4.2's Marshal
// writes a Go string as CBOR text without looking, and the daemon's
// strict decoder refuses invalid UTF-8; one stray byte in a device's
// message must not turn an action that happened into a failed call.
func clean(s string) string { return strings.ToValidUTF8(s, string(utf8.RuneError)) }

// CapabilityID mints the C1 wire id for one device capability.
func CapabilityID(capID string, key runtime.DeviceKey) string {
	return runtime.CapabilityID(capID, key)
}

// ParseCapabilityID resolves a C1 wire id back to (capability, device key).
func ParseCapabilityID(s string) (capID string, key runtime.DeviceKey, err error) {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return "", runtime.DeviceKey{}, fmt.Errorf("c1serv: malformed capability id %q", s)
	}
	capID = s[:at]
	rest := s[at+1:]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash == len(rest)-1 {
		return "", runtime.DeviceKey{}, fmt.Errorf("c1serv: malformed device key in %q", s)
	}
	return capID, runtime.DeviceKey{Adapter: rest[:slash], Device: rest[slash+1:]}, nil
}

// decodeInvokeReq applies every rule of LEC §2.1.1 to a request body and
// returns the request with the caller it names.
func decodeInvokeReq(b []byte) (invokeReq, runtime.Caller, error) {
	var req invokeReq
	if err := c1DecMode.Unmarshal(b, &req); err != nil {
		return invokeReq{}, runtime.Caller{}, err
	}
	if req.Capability == nil {
		return invokeReq{}, runtime.Caller{}, errors.New("missing capability (key 1)")
	}
	if req.Caller == nil {
		return invokeReq{}, runtime.Caller{}, errors.New("missing caller (key 5)")
	}
	c := req.Caller
	if c.Via == nil {
		return invokeReq{}, runtime.Caller{}, errors.New("missing caller.via (key 5.1)")
	}
	switch *c.Via {
	case viaLocal:
		if c.AID != nil {
			return invokeReq{}, runtime.Caller{}, errors.New("caller.via is 0 (local) but carries an aid")
		}
		return req, runtime.Caller{Via: runtime.ViaLocal}, nil
	case viaRelay:
		if c.AID == nil || *c.AID == "" {
			return invokeReq{}, runtime.Caller{}, errors.New("caller.via is 1 (relay) but carries no aid")
		}
		return req, runtime.Caller{Via: runtime.ViaRelay, AID: *c.AID}, nil
	default:
		return invokeReq{}, runtime.Caller{}, fmt.Errorf("caller.via %d is not 0 (local) or 1 (relay)", *c.Via)
	}
}

// marshal is the encoder; a variable only so a test can prove the
// read-back check below catches what an encoder lets through.
var marshal = coredet.Marshal

// encodeChecked encodes v and reads it back with c1DecMode before it is
// sent (AL-5). v0.4.2's Marshal lacks the read-back the daemon's v0.16
// has, so without this ANetLink could answer with bytes the daemon refuses
// — the failure would surface there, as a puzzle, instead of here.
func encodeChecked[T any](v T) ([]byte, error) {
	b, err := marshal(v)
	if err != nil {
		return nil, err
	}
	var back T
	if err := c1DecMode.Unmarshal(b, &back); err != nil {
		return nil, fmt.Errorf("encoded response will not decode: %w", err)
	}
	return b, nil
}

var refusalBodies map[string][]byte

// refusalBody is the fixed encoding of Refusal{reason}.
func refusalBody(reason string) []byte { return refusalBodies[reason] }

// Handler builds the C1 v1 handler over a started runtime. logf receives
// the detail behind every refusal; nil means log.Printf.
func Handler(rt *runtime.Runtime, logf func(format string, args ...any)) http.Handler {
	if logf == nil {
		logf = log.Printf
	}
	return &server{rt: rt, logf: logf}
}

type server struct {
	rt   *runtime.Runtime
	logf func(format string, args ...any)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/v0" || strings.HasPrefix(p, "/v0/") {
		s.refuse(w, "c1_version", "%s %s: the client speaks C1 v0; upgrade anet and ANetLink to the same release", r.Method, p)
		return
	}
	switch {
	case r.Method == http.MethodGet && p == "/v1/health":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && p == "/v1/capabilities":
		s.reply(w, capList{Caps: capEntries(s.rt, s.logf)})
	case r.Method == http.MethodGet && p == "/v1/describe":
		// The DeviceCatalog CAS object comes later; "" says there is none.
		s.reply(w, describe{CID: ""})
	case r.Method == http.MethodPost && p == "/v1/invoke":
		s.invoke(w, r)
	default:
		s.refuse(w, "not_found", "%s %s: no such route", r.Method, p)
	}
}

// capEntries lists every capability with whether remote callers may use
// it. An id that is not valid UTF-8 cannot cross the wire (the daemon's
// decoder refuses it), so it is left out and logged instead of failing the
// whole listing.
func capEntries(rt *runtime.Runtime, logf func(string, ...any)) []capEntry {
	caps := make([]capEntry, 0) // an empty list is [], never null
	for _, e := range rt.Inventory() {
		if e.Device.Profile == nil {
			continue
		}
		for _, c := range e.Device.Profile.AllCaps() { // endpoints included (D50)
			id := runtime.CapabilityID(c.ID, e.Key)
			if !utf8.ValidString(id) {
				if logf != nil {
					logf("c1: capability id %q is not valid UTF-8 and is not published", id)
				}
				continue
			}
			ce := capEntry{ID: id}
			if rt.RemoteAllowed(id) {
				ce.Remote = 1
			}
			caps = append(caps, ce)
		}
	}
	return caps
}

func (s *server) invoke(w http.ResponseWriter, r *http.Request) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != contentCBOR {
		s.refuse(w, "unsupported_media", "invoke with Content-Type %q; C1 v1 speaks only %s", r.Header.Get("Content-Type"), contentCBOR)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.refuse(w, "too_large", "invoke body over %d bytes", maxBody)
			return
		}
		s.refuse(w, "bad_request", "reading the invoke body: %v", err)
		return
	}
	req, who, err := decodeInvokeReq(body)
	if err != nil {
		s.refuse(w, "bad_request", "invoke: %v", err)
		return
	}
	capID, key, err := ParseCapabilityID(*req.Capability)
	if err != nil {
		s.refuse(w, "unknown_capability", "invoke: %v", err)
		return
	}
	eff, err := s.rt.Invoke(r.Context(), who, key, capID, req.Args)
	switch {
	case errors.Is(err, runtime.ErrNotPermitted):
		s.refuse(w, "not_permitted", "invoke %s: %v", *req.Capability, err)
		return
	case errors.Is(err, runtime.ErrUnknownCapability):
		s.refuse(w, "unknown_capability", "invoke %s: %v", *req.Capability, err)
		return
	case err != nil:
		s.refuse(w, "internal", "invoke %s: %v", *req.Capability, err)
		return
	}
	resp := invokeResp{Status: clean(string(eff.Status)), Message: clean(eff.Message)}
	if eff.Record != nil && len(eff.Record.Metrics) > 0 {
		resp.Metrics = make(map[string]float64, len(eff.Record.Metrics))
		for k, v := range eff.Record.Metrics {
			resp.Metrics[clean(k)] = v
		}
	}
	resp.Evidence = wireEvidence(eff.Evidence)
	s.reply(w, resp)
}

// reply writes a 200 CBOR answer, or 500 internal if the answer would not
// decode on the other side.
func (s *server) reply(w http.ResponseWriter, v any) {
	var b []byte
	var err error
	switch v := v.(type) {
	case capList:
		b, err = encodeChecked(v)
	case describe:
		b, err = encodeChecked(v)
	case invokeResp:
		b, err = encodeChecked(v)
	default:
		err = fmt.Errorf("no encoder for %T", v)
	}
	if err != nil {
		s.refuse(w, "internal", "response: %v", err)
		return
	}
	w.Header().Set("Content-Type", contentCBOR)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// refuse answers with the fixed Refusal for reason; detail goes to the
// local log only (AL-4).
func (s *server) refuse(w http.ResponseWriter, reason, format string, args ...any) {
	s.logf("c1: %s: %s", reason, fmt.Sprintf(format, args...))
	w.Header().Set("Content-Type", contentCBOR)
	w.WriteHeader(refusalStatus[reason])
	_, _ = w.Write(refusalBody(reason))
}

// Serve serves C1 v1 on ln (from Listen) until ctx is done; the socket is
// removed when it stops. logf as for Handler.
func Serve(ctx context.Context, rt *runtime.Runtime, ln net.Listener, logf func(format string, args ...any)) error {
	srv := &http.Server{
		Handler: Handler(rt, logf),
		// A peer that opens a connection and never finishes its headers
		// holds a goroutine; bound it.
		ReadHeaderTimeout: 10 * time.Second,
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-stopped:
		}
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
