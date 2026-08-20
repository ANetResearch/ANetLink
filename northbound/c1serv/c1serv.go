// Package c1serv is ANetLink's C1 wire surface: an HTTP-over-UDS API the
// ANet daemon's provider shim consumes. The wire carries capabilities as
// opaque ids of the form "<capability>@<adapter>/<device>" — the daemon
// never parses them (red line: no device concept crosses C1); only this
// package mints and resolves the form.
//
// Wire (v0, JSON):
//
//	GET  /v0/health        → 204
//	GET  /v0/capabilities  → {"capabilities": ["light.onoff@sim/lamp-1", ...]}
//	GET  /v0/describe      → {"cid": ""}            (DeviceCatalog CAS object: later)
//	POST /v0/invoke        {"capability": "...", "args": {...}, "call_id": "..."}
//	                       → {"status": "...", "metrics": {...}, "message": "...",
//	                          "evidence": {...}}
//
// Encoding (D52 / AELP §7): CoreDet-CBOR is the canonical wire form —
// request bodies with Content-Type: application/cbor are decoded as CBOR,
// and a client that sends Accept: application/cbor gets CBOR back. JSON
// remains available for debugging, per the AELP transport profile.
package c1serv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/runtime"
)

// contentCBOR is the AELP canonical media type.
const contentCBOR = "application/cbor"

// wantsCBOR reports whether the client asked for a CBOR response.
func wantsCBOR(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), contentCBOR)
}

// CapabilityID mints the C1 wire id for one device capability.
func CapabilityID(capID string, key runtime.DeviceKey) string {
	return capID + "@" + key.String()
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

type invokeReq struct {
	Capability string         `json:"capability" cbor:"1,keyasint"`
	Args       map[string]any `json:"args,omitempty" cbor:"2,keyasint,omitempty"`
	CallID     string         `json:"call_id,omitempty" cbor:"3,keyasint,omitempty"`
	CallerAID  string         `json:"caller_aid,omitempty" cbor:"4,keyasint,omitempty"`
}

type invokeResp struct {
	Status   string             `json:"status" cbor:"1,keyasint"`
	Metrics  map[string]float64 `json:"metrics,omitempty" cbor:"2,keyasint,omitempty"`
	Message  string             `json:"message,omitempty" cbor:"3,keyasint,omitempty"`
	Evidence *evidenceWire      `json:"evidence,omitempty" cbor:"4,keyasint,omitempty"`
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
	Requested     string `json:"requested,omitempty" cbor:"1,keyasint,omitempty"`
	Protocol      string `json:"protocol,omitempty" cbor:"2,keyasint,omitempty"`
	NativeAck     bool   `json:"native_ack,omitempty" cbor:"3,keyasint,omitempty"`
	ObservedState string `json:"observed_state,omitempty" cbor:"4,keyasint,omitempty"`
	LatencyMS     int64  `json:"latency_ms,omitempty" cbor:"5,keyasint,omitempty"`
	VerifyTrust   uint8  `json:"verify_trust,omitempty" cbor:"6,keyasint,omitempty"`
	AuthTrust     uint8  `json:"auth_trust,omitempty" cbor:"7,keyasint,omitempty"`
	Quirk         string `json:"quirk,omitempty" cbor:"8,keyasint,omitempty"`
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
		Requested: ev.Requested, Protocol: ev.Protocol, NativeAck: ev.NativeAck,
		ObservedState: ev.ObservedState, LatencyMS: ev.LatencyMS,
		VerifyTrust: ev.VerifyTrust, AuthTrust: ev.AuthTrust,
		Quirk: ev.Quirk,
	}
}

// Handler builds the C1 HTTP handler over a started runtime.
func Handler(rt *runtime.Runtime) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v0/capabilities", func(w http.ResponseWriter, r *http.Request) {
		var caps []string
		for _, d := range rt.Devices() {
			key := runtime.DeviceKey{Adapter: d.Profile.Protocol, Device: d.ID}
			for _, c := range d.Profile.AllCaps() { // endpoints included (D50)
				caps = append(caps, CapabilityID(c.ID, key))
			}
		}
		if wantsCBOR(r) {
			if b, err := coredet.Marshal(map[uint64]any{1: caps}); err == nil {
				w.Header().Set("Content-Type", contentCBOR)
				_, _ = w.Write(b)
				return
			}
		}
		writeJSON(w, map[string]any{"capabilities": caps})
	})
	mux.HandleFunc("GET /v0/describe", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"cid": ""})
	})
	mux.HandleFunc("POST /v0/invoke", func(w http.ResponseWriter, r *http.Request) {
		var req invokeReq
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.Contains(r.Header.Get("Content-Type"), contentCBOR) {
			if err := coredet.Unmarshal(body, &req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		} else if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		capID, key, err := ParseCapabilityID(req.Capability)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		eff, err := rt.Invoke(r.Context(), key, capID, req.Args)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		resp := invokeResp{Status: string(eff.Status), Message: eff.Message}
		if eff.Record != nil {
			resp.Metrics = eff.Record.Metrics
		}
		resp.Evidence = wireEvidence(eff.Evidence)
		if wantsCBOR(r) {
			b, err := coredet.Marshal(resp)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", contentCBOR)
			_, _ = w.Write(b)
			return
		}
		writeJSON(w, resp)
	})
	return mux
}

// Serve listens on a unix socket until ctx is done.
func Serve(ctx context.Context, rt *runtime.Runtime, socketPath string) error {
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("c1serv: listen %s: %w", socketPath, err)
	}
	srv := &http.Server{Handler: Handler(rt)}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
