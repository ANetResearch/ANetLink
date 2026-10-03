package c1serv

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"

	"github.com/ANetResearch/ANetLink/runtime"
)

// F-C1 (LEC §7.2): the C1 v1 decoders never panic, and whatever the
// request decoder accepts obeys §2.1.1. Seeded with every C1-1 vector,
// positive and negative.
//
//	go test ./northbound/c1serv -run '^$' -fuzz FuzzC1Decode -fuzztime 10m
func FuzzC1Decode(f *testing.F) {
	seeds, _ := filepath.Glob(filepath.Join(goldenDir, "*.cbor"))
	for _, p := range seeds {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		// the response-side objects: decode or refuse, never panic
		var cl capList
		_ = c1DecMode.Unmarshal(b, &cl)
		var ir invokeResp
		_ = c1DecMode.Unmarshal(b, &ir)
		var d describe
		_ = c1DecMode.Unmarshal(b, &d)
		var rf refusal
		_ = c1DecMode.Unmarshal(b, &rf)

		req, who, err := decodeInvokeReq(b)
		if err != nil {
			return
		}
		if req.Capability == nil || req.Caller == nil || req.Caller.Via == nil {
			t.Fatalf("accepted without a required field: %#v", req)
		}
		switch *req.Caller.Via {
		case viaLocal:
			if req.Caller.AID != nil {
				t.Fatalf("accepted via 0 with an aid: %x", b)
			}
			if who != (runtime.Caller{Via: runtime.ViaLocal}) {
				t.Fatalf("via 0 mapped to %+v", who)
			}
		case viaRelay:
			if req.Caller.AID == nil || *req.Caller.AID == "" {
				t.Fatalf("accepted via 1 without an aid: %x", b)
			}
			if who != (runtime.Caller{Via: runtime.ViaRelay, AID: *req.Caller.AID}) {
				t.Fatalf("via 1 mapped to %+v", who)
			}
		default:
			t.Fatalf("accepted via %d", *req.Caller.Via)
		}

		// An accepted request survives a round trip through the encoder
		// unchanged. The encoder may refuse (NaN in args is decodable but
		// not encodable under CoreDet); it may not change the meaning.
		again, err := coredet.Marshal(req)
		if err != nil {
			return
		}
		req2, who2, err := decodeInvokeReq(again)
		if err != nil {
			t.Fatalf("re-encoded request refused: %v\n in %x\nout %x", err, b, again)
		}
		if who2 != who || !reflect.DeepEqual(req2, req) {
			t.Fatalf("round trip changed the request:\n%#v\n%#v", req, req2)
		}
	})
}
