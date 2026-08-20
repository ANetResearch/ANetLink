package adap

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"syscall"
)

// newRID mints a request-correlation id. Random rather than a counter: a
// retry MUST reuse its original rid so the adapter can deduplicate (spec
// §5.1.3), and a counter would collide across a reconnect.
func newRID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "rid-fallback"
	}
	return hex.EncodeToString(b[:])
}

// peerUID reads the connecting process's uid via SO_PEERCRED (spec §6).
func peerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("adap: not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return cred.Uid, nil
}

// floatMap coerces a decoded CBOR map into metrics.
//
// CBOR gives back whichever numeric type the sender used, and a metric that
// arrives as an integer is still a metric. Rejecting it because it is not a
// float64 would make the wire format depend on how a third-party adapter's
// CBOR library happens to encode 1.
func floatMap(v any) map[string]float64 {
	m, ok := v.(map[any]any)
	if !ok {
		if sm, ok2 := v.(map[string]any); ok2 {
			m = make(map[any]any, len(sm))
			for k, val := range sm {
				m[k] = val
			}
		} else {
			return nil
		}
	}
	out := make(map[string]float64, len(m))
	for k, val := range m {
		ks, ok := k.(string)
		if !ok {
			continue
		}
		switch n := val.(type) {
		case float64:
			out[ks] = n
		case float32:
			out[ks] = float64(n)
		case int:
			out[ks] = float64(n)
		case int64:
			out[ks] = float64(n)
		case uint64:
			out[ks] = float64(n)
		case bool:
			// A boolean capability reports as 0/1 like every other metric —
			// EffectRecord holds numbers, and "on" is 1.
			if n {
				out[ks] = 1
			} else {
				out[ks] = 0
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalise converts CBOR's map[any]any into something json.Marshal accepts,
// recursively. CBOR keys may be any type; JSON keys must be strings, and a
// non-string key in a DeviceProfile is malformed anyway.
func normalise(v any) any {
	switch t := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if ks, ok := k.(string); ok {
				out[ks] = normalise(val)
			}
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalise(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalise(val)
		}
		return out
	}
	return v
}
