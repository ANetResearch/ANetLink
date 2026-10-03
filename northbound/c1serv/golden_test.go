package c1serv

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
)

// C1-1 (core §5.2, LEC §2.1.3): the golden vectors for C1 v1. The same
// bytes sit in the daemon's repository (provider/anetlink/testdata/c1v1/)
// and its CI compares the two directories file by file. Each repository
// proves its own side:
//
//  1. its encoder (here: ANetCore v0.4.2 coredet.Marshal) turns the fixed Go
//     value of each positive vector into exactly the file's bytes;
//  2. its strict decoder (here: c1DecMode) turns each positive file back into
//     that value, and refuses every negative one.
//
// If either side's CBOR library or encoding options drift, (1) goes red.
// The fix is never to regenerate the files to match: the bytes are the
// contract between two programs built from different ANetCore versions.
//
// Regenerate only for a deliberate wire change (core CORE-F2):
//
//	C1_GOLDEN_UPDATE=1 go test ./northbound/c1serv -run TestC1Golden

const goldenDir = "testdata/c1v1"

// Public, label-derived values (core CORE-G5): nothing here is a real
// identity.
var (
	goldenAID = anetcid.MustSum([]byte("anet-golden/v016/C1-1/caller"))
	goldenCID = anetcid.MustSum([]byte("anet-golden/v016/C1-1/catalog"))
)

const goldenCap = "light.onoff@sim/lamp-1"

type vector struct {
	file string
	// kind is the object the bytes are decoded as: invokereq, invokeresp,
	// caplist, describe or refusal.
	kind string
	// value is a positive vector's Go value; nil for a negative one.
	value any
	// raw is a negative vector's bytes.
	raw []byte
	// why a negative vector is refused, for the reader.
	why string
}

func positives() []vector {
	return []vector{
		{file: "caplist.cbor", kind: "caplist", value: capList{Caps: []capEntry{
			{ID: goldenCap, Remote: 1},
			{ID: "sensor.temperature@modbus/plc-1", Remote: 0},
		}}},
		{file: "caplist-empty.cbor", kind: "caplist", value: capList{Caps: []capEntry{}}},
		{file: "describe-empty.cbor", kind: "describe", value: describe{CID: ""}},
		{file: "describe-cid.cbor", kind: "describe", value: describe{CID: goldenCID}},
		{file: "invokereq-local.cbor", kind: "invokereq", value: invokeReq{
			Capability: sp(goldenCap), Args: map[string]any{"on": true}, CallID: sp("call-1"),
			Caller: &caller{Via: up(0)},
		}},
		{file: "invokereq-relay.cbor", kind: "invokereq", value: invokeReq{
			Capability: sp(goldenCap),
			Args: map[string]any{
				"on": true, "level": uint64(40), "offset": int64(-3), "ratio": 0.5,
				"label": "前门", "rgb": []any{uint64(255), uint64(128), uint64(0)},
				"opts": map[any]any{"fade_ms": uint64(300)},
			},
			CallID: sp("call-2"),
			Caller: &caller{Via: up(1), AID: sp(goldenAID)},
		}},
		{file: "invokereq-minimal.cbor", kind: "invokereq", value: invokeReq{
			Capability: sp(goldenCap), Caller: &caller{Via: up(0)},
		}},
		{file: "invokeresp-full.cbor", kind: "invokeresp", value: invokeResp{
			Status:  "OK",
			Metrics: map[string]float64{"power_state": 1, "temperature": 22.13},
			Message: "done",
			Evidence: &evidenceWire{
				Requested: "on=true", Protocol: "zigbee", NativeAck: true, ObservedState: "on",
				LatencyMS: 12, VerifyTrust: 4, AuthTrust: 2, Quirk: "aqara.temp.scale100",
			},
		}},
		{file: "invokeresp-unavailable.cbor", kind: "invokeresp", value: invokeResp{
			Status: "UNAVAILABLE", Message: "device offline",
		}},
		{file: "refusal-not_permitted.cbor", kind: "refusal", value: refusal{Reason: "not_permitted"}},
		{file: "refusal-bad_request.cbor", kind: "refusal", value: refusal{Reason: "bad_request"}},
		{file: "refusal-too_large.cbor", kind: "refusal", value: refusal{Reason: "too_large"}},
		{file: "refusal-unsupported_media.cbor", kind: "refusal", value: refusal{Reason: "unsupported_media"}},
		{file: "refusal-unknown_capability.cbor", kind: "refusal", value: refusal{Reason: "unknown_capability"}},
		{file: "refusal-c1_version.cbor", kind: "refusal", value: refusal{Reason: "c1_version"}},
		{file: "refusal-not_found.cbor", kind: "refusal", value: refusal{Reason: "not_found"}},
		{file: "refusal-internal.cbor", kind: "refusal", value: refusal{Reason: "internal"}},
	}
}

// Hand-built CBOR, for the vectors a correct encoder refuses to produce.
func hdr(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 256:
		return []byte{major<<5 | 24, byte(n)}
	default:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
}
func cUint(n uint64) []byte   { return hdr(0, n) }
func cText(s string) []byte   { return append(hdr(3, uint64(len(s))), s...) }
func cMap(pairs int) []byte   { return hdr(5, uint64(pairs)) }
func cat(p ...[]byte) []byte  { return bytes.Join(p, nil) }
func gm(m map[any]any) []byte { b, err := coredet.Marshal(m); must(err); return b }

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func negatives() []vector {
	k := func(n uint64) any { return n }
	local := map[any]any{k(1): k(0)}
	relay := map[any]any{k(1): k(1), k(2): goldenAID}
	minimal, err := coredet.Marshal(invokeReq{Capability: sp(goldenCap), Caller: &caller{Via: up(0)}})
	must(err)
	return []vector{
		{file: "neg-invokereq-via2.cbor", kind: "invokereq", why: "via 2 (the retired voucher path)",
			raw: gm(map[any]any{k(1): goldenCap, k(5): map[any]any{k(1): k(2)}})},
		{file: "neg-invokereq-via3.cbor", kind: "invokereq", why: "via outside {0, 1}",
			raw: gm(map[any]any{k(1): goldenCap, k(5): map[any]any{k(1): k(3)}})},
		{file: "neg-invokereq-local-with-aid.cbor", kind: "invokereq", why: "via 0 carrying an aid",
			raw: gm(map[any]any{k(1): goldenCap, k(5): map[any]any{k(1): k(0), k(2): goldenAID}})},
		{file: "neg-invokereq-relay-no-aid.cbor", kind: "invokereq", why: "via 1 without an aid",
			raw: gm(map[any]any{k(1): goldenCap, k(5): map[any]any{k(1): k(1)}})},
		{file: "neg-invokereq-relay-empty-aid.cbor", kind: "invokereq", why: "via 1 with an empty aid",
			raw: gm(map[any]any{k(1): goldenCap, k(5): map[any]any{k(1): k(1), k(2): ""}})},
		{file: "neg-invokereq-key4.cbor", kind: "invokereq", why: "retired key 4 (v0 caller_aid)",
			raw: gm(map[any]any{k(1): goldenCap, k(4): goldenAID, k(5): relay})},
		{file: "neg-invokereq-caller-key3.cbor", kind: "invokereq", why: "reserved Caller key 3 (witnessed)",
			raw: gm(map[any]any{k(1): goldenCap, k(5): map[any]any{k(1): k(1), k(2): goldenAID, k(3): true}})},
		{file: "neg-invokereq-unknown-key.cbor", kind: "invokereq", why: "unregistered key 6",
			raw: gm(map[any]any{k(1): goldenCap, k(5): local, k(6): "x"})},
		{file: "neg-invokereq-dup-key.cbor", kind: "invokereq", why: "key 1 twice",
			raw: cat(cMap(3), cUint(1), cText(goldenCap), cUint(1), cText("x.do@fake/dev-1"),
				cUint(5), cMap(1), cUint(1), cUint(0))},
		{file: "neg-invokereq-tag.cbor", kind: "invokereq", why: "a CBOR tag (32, URI) around the capability",
			raw: cat(cMap(2), cUint(1), []byte{0xd8, 0x20}, cText(goldenCap), cUint(5), cMap(1), cUint(1), cUint(0))},
		{file: "neg-invokereq-utf8.cbor", kind: "invokereq", why: "text that is not valid UTF-8",
			raw: cat(cMap(2), cUint(1), cText("light\xff.onoff@sim/lamp-1"), cUint(5), cMap(1), cUint(1), cUint(0))},
		{file: "neg-invokereq-json.cbor", kind: "invokereq", why: "a JSON body",
			raw: []byte(`{"capability":"light.onoff@sim/lamp-1","caller":{"via":0}}`)},
		{file: "neg-invokereq-no-caller.cbor", kind: "invokereq", why: "required key 5 (caller) missing",
			raw: gm(map[any]any{k(1): goldenCap})},
		{file: "neg-invokereq-no-capability.cbor", kind: "invokereq", why: "required key 1 (capability) missing",
			raw: gm(map[any]any{k(5): local})},
		{file: "neg-invokereq-caller-no-via.cbor", kind: "invokereq", why: "required Caller key 1 (via) missing",
			raw: gm(map[any]any{k(1): goldenCap, k(5): map[any]any{}})},
		{file: "neg-invokereq-text-keys.cbor", kind: "invokereq", why: "text keys instead of integer keys",
			raw: gm(map[any]any{"capability": goldenCap, "caller": map[any]any{"via": k(0)}})},
		{file: "neg-invokereq-args-not-map.cbor", kind: "invokereq", why: "args that is not a map",
			raw: gm(map[any]any{k(1): goldenCap, k(2): "on", k(5): local})},
		{file: "neg-invokereq-trailing.cbor", kind: "invokereq", why: "bytes after the object",
			raw: append(append([]byte(nil), minimal...), 0x00)},
		{file: "neg-invokeresp-utf8.cbor", kind: "invokeresp", why: "message that is not valid UTF-8",
			raw: cat(cMap(2), cUint(1), cText("OK"), cUint(3), cText("bad \xa2"))},
		{file: "neg-invokeresp-unknown-key.cbor", kind: "invokeresp", why: "unregistered key 5",
			raw: gm(map[any]any{k(1): "OK", k(5): k(1)})},
		{file: "neg-invokeresp-evidence-unknown-key.cbor", kind: "invokeresp", why: "unregistered Evidence key 9",
			raw: gm(map[any]any{k(1): "OK", k(4): map[any]any{k(9): "x"}})},
		{file: "neg-caplist-unknown-key.cbor", kind: "caplist", why: "unregistered CapEntry key 3",
			raw: gm(map[any]any{k(1): []any{map[any]any{k(1): goldenCap, k(2): k(0), k(3): true}}})},
	}
}

func allVectors() []vector { return append(positives(), negatives()...) }

// decodeAs runs the receiving side's full check for one kind.
func decodeAs(kind string, b []byte) (any, error) {
	switch kind {
	case "invokereq":
		req, _, err := decodeInvokeReq(b)
		return req, err
	case "invokeresp":
		var v invokeResp
		return v, c1DecMode.Unmarshal(b, &v)
	case "caplist":
		var v capList
		return v, c1DecMode.Unmarshal(b, &v)
	case "describe":
		var v describe
		return v, c1DecMode.Unmarshal(b, &v)
	case "refusal":
		var v refusal
		return v, c1DecMode.Unmarshal(b, &v)
	}
	panic("unknown kind " + kind)
}

func vectorBytes(t *testing.T, v vector) []byte {
	t.Helper()
	if v.value == nil {
		return v.raw
	}
	b, err := coredet.Marshal(v.value)
	if err != nil {
		t.Fatalf("%s: encode: %v", v.file, err)
	}
	return b
}

func sums(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var out bytes.Buffer
	for _, n := range names {
		h := sha256.Sum256(files[n])
		fmt.Fprintf(&out, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	return out.Bytes()
}

func TestC1GoldenUpdate(t *testing.T) {
	if os.Getenv("C1_GOLDEN_UPDATE") != "1" {
		t.Skip("set C1_GOLDEN_UPDATE=1 to regenerate C1-1 (a wire change: core CORE-F2)")
	}
	if err := os.MkdirAll(goldenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old, _ := filepath.Glob(filepath.Join(goldenDir, "*.cbor"))
	for _, f := range old {
		must(os.Remove(f))
	}
	files := map[string][]byte{}
	for _, v := range allVectors() {
		b := vectorBytes(t, v)
		files[v.file] = b
		must(os.WriteFile(filepath.Join(goldenDir, v.file), b, 0o644))
	}
	must(os.WriteFile(filepath.Join(goldenDir, "SHA256SUMS"), sums(t, files), 0o644))
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("C1-1 %s: %v", name, err)
	}
	return b
}

// LEC §2.1.3 item 1 and 2 for every positive vector.
func TestC1GoldenPositive(t *testing.T) {
	for _, v := range positives() {
		want := readGolden(t, v.file)
		got := vectorBytes(t, v)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: ANetCore v0.4.2 coredet.Marshal gives\n  %x\nthe golden file holds\n  %x\n"+
				"(an encoder change: do not regenerate the file, fix the encoder)", v.file, got, want)
			continue
		}
		dec, err := decodeAs(v.kind, want)
		if err != nil {
			t.Errorf("%s: strict decode: %v", v.file, err)
			continue
		}
		if !reflect.DeepEqual(dec, v.value) {
			t.Errorf("%s: decodes to\n  %#v\nwant\n  %#v", v.file, dec, v.value)
		}
	}
}

// C1-1-neg: every negative vector is refused by the full receive path.
func TestC1GoldenNegative(t *testing.T) {
	for _, v := range negatives() {
		b := readGolden(t, v.file)
		if !bytes.Equal(b, v.raw) {
			t.Errorf("%s: file differs from the vector table", v.file)
		}
		_, err := decodeAs(v.kind, b)
		if err == nil {
			t.Errorf("%s: accepted, but must be refused (%s)", v.file, v.why)
			continue
		}
		t.Logf("%s (%s): %v", v.file, v.why, err)
	}
}

// The directory holds exactly the table, and SHA256SUMS describes it.
func TestC1GoldenSums(t *testing.T) {
	onDisk, err := filepath.Glob(filepath.Join(goldenDir, "*.cbor"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, p := range onDisk {
		files[filepath.Base(p)] = readGolden(t, filepath.Base(p))
	}
	table := map[string]bool{}
	for _, v := range allVectors() {
		table[v.file] = true
		if _, ok := files[v.file]; !ok {
			t.Errorf("vector %s has no file", v.file)
		}
	}
	for name := range files {
		if !table[name] {
			t.Errorf("%s is in %s but not in the vector table", name, goldenDir)
		}
	}
	if got, want := readGolden(t, "SHA256SUMS"), sums(t, files); !bytes.Equal(got, want) {
		t.Errorf("SHA256SUMS is stale:\n%s\nwant\n%s", got, want)
	}
}

// What the server actually writes is what the golden files say: the
// refusal bodies and the describe answer are byte-identical.
func TestServedBytesMatchGolden(t *testing.T) {
	for reason := range refusalStatus {
		if got, want := refusalBody(reason), readGolden(t, "refusal-"+reason+".cbor"); !bytes.Equal(got, want) {
			t.Errorf("refusal %s: served %x, golden %x", reason, got, want)
		}
	}
	if len(refusalStatus) != 8 {
		t.Errorf("core §2.15.3 has 8 C1 reasons, the server knows %d", len(refusalStatus))
	}
	r := newRig(t)
	a := r.do(t, "GET", "/v1/describe", "", nil)
	if !bytes.Equal(a.body, readGolden(t, "describe-empty.cbor")) {
		t.Errorf("describe: served %x", a.body)
	}
}

// The label-derived values really are what the comments say.
func TestGoldenValuesAreLabelDerived(t *testing.T) {
	if goldenAID != testAID {
		t.Fatalf("goldenAID %s != testAID %s", goldenAID, testAID)
	}
	if !strings.HasPrefix(goldenCID, "bafy") {
		t.Fatalf("goldenCID %s", goldenCID)
	}
}
