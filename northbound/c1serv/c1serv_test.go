package c1serv

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"
	"github.com/fxamacker/cbor/v2"

	"github.com/ANetResearch/ANetLink/runtime"
)

func TestCapabilityIDRoundTrip(t *testing.T) {
	id := CapabilityID("light.onoff", runtime.DeviceKey{Adapter: "sim", Device: "lamp-1"})
	if id != "light.onoff@sim/lamp-1" {
		t.Fatalf("mint: %s", id)
	}
	capID, key, err := ParseCapabilityID(id)
	if err != nil || capID != "light.onoff" || key.Adapter != "sim" || key.Device != "lamp-1" {
		t.Fatalf("parse: %v %v %v", capID, key, err)
	}
	for _, bad := range []string{"", "@", "x@", "@y", "x@nodash", "x@/y", "x@y/"} {
		if _, _, err := ParseCapabilityID(bad); err == nil {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

// The route table of §2.1.2, including the two catch-alls: every v0 path
// answers 426 so an old daemon fails with a reason instead of a puzzle,
// and everything else is 404 not_found.
func TestRoutes(t *testing.T) {
	r := newRig(t)

	a := r.do(t, http.MethodGet, "/v1/health", "", nil)
	if a.status != http.StatusNoContent || len(a.body) != 0 {
		t.Fatalf("health: %d %x", a.status, a.body)
	}

	a = r.do(t, http.MethodGet, "/v1/describe", "", nil)
	if a.status != http.StatusOK || a.ctype != contentCBOR {
		t.Fatalf("describe: %d %q", a.status, a.ctype)
	}
	if !bytes.Equal(a.body, []byte{0xa1, 0x01, 0x60}) { // {1: ""}
		t.Fatalf("describe body %x", a.body)
	}

	for _, p := range []string{"/v0/health", "/v0/capabilities", "/v0/describe", "/v0/invoke", "/v0", "/v0/anything"} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			refusalOf(t, r.do(t, m, p, "application/json", []byte(`{}`)), http.StatusUpgradeRequired, "c1_version")
		}
	}

	for _, c := range []struct{ m, p string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/v1"},
		{http.MethodGet, "/v1/nope"},
		{http.MethodGet, "/v2/health"},
		{http.MethodPost, "/v1/health"},
		{http.MethodGet, "/v1/invoke"},
		{http.MethodDelete, "/v1/capabilities"},
		{http.MethodGet, "/v1/health/"},
	} {
		refusalOf(t, r.do(t, c.m, c.p, "", nil), http.StatusNotFound, "not_found")
	}
}

// AL-6, M-AL-4: the listing says which ids a remote caller may use, and by
// default that is none.
func TestCapabilitiesReportRemote(t *testing.T) {
	list := func(r *rig) map[string]uint64 {
		t.Helper()
		a := r.do(t, http.MethodGet, "/v1/capabilities", "", nil)
		if a.status != http.StatusOK || a.ctype != contentCBOR {
			t.Fatalf("capabilities: %d %q", a.status, a.ctype)
		}
		var cl capList
		if err := c1DecMode.Unmarshal(a.body, &cl); err != nil {
			t.Fatal(err)
		}
		out := map[string]uint64{}
		for _, e := range cl.Caps {
			out[e.ID] = e.Remote
		}
		return out
	}

	got := list(newRig(t))
	want := map[string]uint64{"light.onoff@sim/lamp-1": 0, "x.do@fake/dev-1": 0, "x.do@fake/off-1": 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default listing: %v, want %v", got, want)
	}

	got = list(newRig(t, "light.onoff@sim/*", "x.do@fake/dev-?"))
	want = map[string]uint64{"light.onoff@sim/lamp-1": 1, "x.do@fake/dev-1": 1, "x.do@fake/off-1": 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listing with c1.remote: %v, want %v", got, want)
	}
}

// An empty inventory is an empty array, not null: the field is required.
func TestEmptyCapListIsAnArray(t *testing.T) {
	b, err := encodeChecked(capList{Caps: capEntries(runtime.New(runtime.Config{}), nil)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, []byte{0xa1, 0x01, 0x80}) {
		t.Fatalf("empty CapList %x, want a10180", b)
	}
}

func TestInvokeLocalAndRelay(t *testing.T) {
	r := newRig(t, "x.do@fake/*")

	out := okResp(t, r.invoke(t, invokeReq{Capability: sp("light.onoff@sim/lamp-1"),
		Args: map[string]any{"on": true}, Caller: localCaller()}))
	if out.Status != "OK" || out.Metrics["power_state"] != 1 {
		t.Fatalf("local invoke: %+v", out)
	}

	// relay to a capability c1.remote does not name: refused, adapter untouched
	refusalOf(t, r.invoke(t, invokeReq{Capability: sp("light.onoff@sim/lamp-1"),
		Args: map[string]any{"on": false}, Caller: relayCaller()}), http.StatusForbidden, "not_permitted")

	// relay to one it does name: served
	before := r.fake.n()
	out = okResp(t, r.invoke(t, invokeReq{Capability: sp("x.do@fake/dev-1"), Caller: relayCaller()}))
	if out.Status != "OK" || r.fake.n() != before+1 {
		t.Fatalf("relay invoke: %+v", out)
	}
	if !strings.Contains(r.logs.all(), `via=relay aid="`+testAID+`" cap="x.do@fake/dev-1" result=OK`) {
		t.Fatalf("AL-8 line missing:\n%s", r.logs.all())
	}
}

func TestInvokeRefusals(t *testing.T) {
	r := newRig(t)
	good, err := coredet.Marshal(invokeReq{Capability: sp("light.onoff@sim/lamp-1"), Caller: localCaller()})
	if err != nil {
		t.Fatal(err)
	}

	// only CBOR is spoken (415), whatever the body looks like
	for _, ct := range []string{"", "application/json", "text/plain", "application/cbor-seq", "application/cbor; x=\""} {
		refusalOf(t, r.do(t, http.MethodPost, "/v1/invoke", ct, good), http.StatusUnsupportedMediaType, "unsupported_media")
	}
	if a := r.do(t, http.MethodPost, "/v1/invoke", "application/cbor; charset=binary", good); a.status != http.StatusOK {
		t.Fatalf("media type parameters must not matter: %d", a.status)
	}
	if a := r.do(t, http.MethodPost, "/v1/invoke", "Application/CBOR", good); a.status != http.StatusOK {
		t.Fatalf("media type is case-insensitive: %d", a.status)
	}

	// JSON sent as CBOR, garbage, empty body: 400
	for _, body := range [][]byte{[]byte(`{"capability":"light.onoff@sim/lamp-1","caller":{"via":0}}`), {0xff}, nil} {
		refusalOf(t, r.do(t, http.MethodPost, "/v1/invoke", contentCBOR, body), http.StatusBadRequest, "bad_request")
	}

	// unknown or malformed ids: 404 unknown_capability
	for _, id := range []string{"light.onoff@sim/ghost", "thermostat.set@sim/lamp-1", "x@nope/dev", "nonsense", ""} {
		refusalOf(t, r.invoke(t, invokeReq{Capability: sp(id), Caller: localCaller()}), http.StatusNotFound, "unknown_capability")
	}

	// an offline device is an answer, not a refusal
	out := okResp(t, r.invoke(t, invokeReq{Capability: sp("x.do@fake/off-1"), Caller: localCaller()}))
	if out.Status != string(effect.Unavailable) {
		t.Fatalf("offline: %+v", out)
	}
}

// M-AL-3: a body over 1 MiB is refused with too_large before it is decoded.
func TestRequestBodyLimit(t *testing.T) {
	r := newRig(t)
	big := bytes.Repeat([]byte{'a'}, 2<<20)
	body, err := coredet.Marshal(invokeReq{Capability: sp("light.onoff@sim/lamp-1"),
		Args: map[string]any{"pad": string(big)}, Caller: localCaller()})
	if err != nil {
		t.Fatal(err)
	}
	refusalOf(t, r.do(t, http.MethodPost, "/v1/invoke", contentCBOR, body), http.StatusRequestEntityTooLarge, "too_large")

	// just under the limit is decoded (and then served)
	pad := maxBody - 200
	body, _ = coredet.Marshal(invokeReq{Capability: sp("light.onoff@sim/lamp-1"),
		Args: map[string]any{"pad": strings.Repeat("a", pad)}, Caller: localCaller()})
	if len(body) > maxBody {
		t.Fatalf("test setup: %d > %d", len(body), maxBody)
	}
	okResp(t, r.do(t, http.MethodPost, "/v1/invoke", contentCBOR, body))
}

// AL-4: an error's text goes to the local log, never into the answer.
func TestErrorTextIsNotEchoed(t *testing.T) {
	r := newRig(t)
	const canary = "secret-device-detail-91c2"
	r.fake.fail = errors.New("adapter blew up: " + canary)
	a := r.invoke(t, invokeReq{Capability: sp("x.do@fake/dev-1"), Caller: localCaller()})
	refusalOf(t, a, http.StatusInternalServerError, "internal")
	if bytes.Contains(a.body, []byte(canary)) {
		t.Fatal("adapter error text crossed C1")
	}
	if !strings.Contains(r.logs.all(), canary) {
		t.Fatal("adapter error text must reach the local log")
	}

	a = r.do(t, http.MethodPost, "/v1/invoke", contentCBOR, []byte{0xa1, 0x01, 0x63, 'a', 'b'})
	refusalOf(t, a, http.StatusBadRequest, "bad_request")
	if len(a.body) != len(mustRefusal(t, "bad_request")) {
		t.Fatalf("a refusal carries the reason and nothing else: %x", a.body)
	}
}

func mustRefusal(t *testing.T, reason string) []byte {
	t.Helper()
	b, err := coredet.Marshal(refusal{Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// M-AL-8: v0.4.2's Marshal writes a Go string as CBOR text without looking,
// and the daemon's strict decoder refuses invalid UTF-8. A device message
// with a stray byte must not turn a performed action into a failure.
func TestInvalidUTF8MessageIsCleaned(t *testing.T) {
	r := newRig(t)
	r.fake.reply = effect.Effect{Status: effect.OK, Message: "temp \xa2 ok",
		Evidence: &effect.Evidence{ObservedState: "on\xff", Protocol: "fakeproto"}}
	a := r.invoke(t, invokeReq{Capability: sp("x.do@fake/dev-1"), Caller: localCaller()})
	out := okResp(t, a)
	if !utf8.ValidString(out.Message) || !strings.Contains(out.Message, "\uFFFD") {
		t.Fatalf("message not cleaned: %q", out.Message)
	}
	if out.Message != "temp \uFFFD ok" {
		t.Fatalf("message changed beyond the bad byte: %q", out.Message)
	}
	if out.Evidence == nil || out.Evidence.ObservedState != "on\uFFFD" {
		t.Fatalf("evidence text not cleaned: %+v", out.Evidence)
	}
}

// The adapters see the same argument shape whichever entrance a call came
// through. MCP hands them JSON (float64 numbers, map[string]any objects);
// CBOR keeps integers as integers and nested maps as map[any]any, and an
// adapter that reads args.value with v.(float64) — modbus, CAN — would
// refuse {"value": 20} from C1 and accept it from MCP.
func TestArgsReachAdaptersInTheMCPShape(t *testing.T) {
	r := newRig(t)
	okResp(t, r.invoke(t, invokeReq{Capability: sp("x.do@fake/dev-1"), Caller: localCaller(), Args: map[string]any{
		"value": uint64(20), "offset": int64(-3), "ratio": 0.5, "on": true, "label": "前门",
		"rgb":  []any{uint64(255), int64(-1)},
		"opts": map[any]any{"fade_ms": uint64(300), "deep": map[any]any{"n": uint64(1)}},
		"ids":  map[any]any{uint64(7): uint64(8)},
	}}))
	want := map[string]any{
		"value": 20.0, "offset": -3.0, "ratio": 0.5, "on": true, "label": "前门",
		"rgb":  []any{255.0, -1.0},
		"opts": map[string]any{"fade_ms": 300.0, "deep": map[string]any{"n": 1.0}},
		"ids":  map[any]any{uint64(7): 8.0},
	}
	if got := r.fake.lastArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter got\n  %#v\nwant\n  %#v", got, want)
	}

	// a negative integer past int64 decodes as a big.Int; it too is a number
	body := cat(cMap(3), cUint(1), cText("x.do@fake/dev-1"),
		cUint(2), cMap(1), cText("v"), []byte{0x3b, 0x80, 0, 0, 0, 0, 0, 0, 0},
		cUint(5), cMap(1), cUint(1), cUint(0))
	okResp(t, r.do(t, http.MethodPost, "/v1/invoke", contentCBOR, body))
	if v, ok := r.fake.lastArgs()["v"].(float64); !ok || v != -9223372036854775809 {
		t.Fatalf("big negative integer reached the adapter as %#v", r.fake.lastArgs()["v"])
	}
}

// A refusal's detail goes to the log, and text that came off the wire must
// not be able to start a log line of its own.
func TestRefusalDetailStaysOnOneLogLine(t *testing.T) {
	r := newRig(t)
	refusalOf(t, r.invoke(t, invokeReq{Capability: sp("x.do@fake/ghost\nc1: forged line"), Caller: localCaller()}),
		http.StatusNotFound, "unknown_capability")
	r.fake.fail = errors.New("adapter says\r\nc1: forged too")
	refusalOf(t, r.invoke(t, invokeReq{Capability: sp("x.do@fake/dev-1"), Caller: localCaller()}),
		http.StatusInternalServerError, "internal")
	refusalOf(t, r.do(t, http.MethodGet, "/v1/x%0Ac1:%20forged", "", nil), http.StatusNotFound, "not_found")
	r.logs.mu.Lock()
	defer r.logs.mu.Unlock()
	for _, l := range r.logs.lines {
		if strings.ContainsAny(l, "\r\n") {
			t.Errorf("log entry spans lines: %q", l)
		}
	}
}

// CoreDet has no NaN or infinity. A device that reports one must not turn
// an action that happened into 500 internal; the reading is left out and
// the rest of the answer goes back.
func TestNonFiniteMetricIsLeftOut(t *testing.T) {
	r := newRig(t)
	r.fake.reply = effect.Effect{Status: effect.OK, Message: "done", Record: &tsir.EffectRecord{
		Metrics: map[string]float64{"temp": math.NaN(), "hum": math.Inf(1), "power_state": 1}}}
	out := okResp(t, r.invoke(t, invokeReq{Capability: sp("x.do@fake/dev-1"), Caller: localCaller()}))
	if out.Status != "OK" || !reflect.DeepEqual(out.Metrics, map[string]float64{"power_state": 1}) {
		t.Fatalf("answer %+v", out)
	}
	if r.logs.count(`metric "temp" is NaN`) != 1 || r.logs.count(`metric "hum" is +Inf`) != 1 {
		t.Fatalf("left-out readings must be logged:\n%s", r.logs.all())
	}

	r.fake.reply = effect.Effect{Status: effect.OK, Record: &tsir.EffectRecord{Metrics: map[string]float64{"temp": math.NaN()}}}
	out = okResp(t, r.invoke(t, invokeReq{Capability: sp("x.do@fake/dev-1"), Caller: localCaller()}))
	if out.Metrics != nil {
		t.Fatalf("no finite metric left must mean no metrics key: %+v", out.Metrics)
	}
}

// M-AL-8, the other half: whatever the encoder produces is read back with
// the strict decoder before it is sent, and a response the daemon could
// not decode becomes 500 internal here instead of a puzzle there.
func TestResponsesAreReadBackBeforeSending(t *testing.T) {
	r := newRig(t)
	saved := marshal
	t.Cleanup(func() { marshal = saved })
	marshal = func(v any) ([]byte, error) {
		b, err := saved(v)
		if err != nil {
			return nil, err
		}
		// smuggle an invalid UTF-8 byte into the first text string
		if i := bytes.IndexByte(b, 'O'); i >= 0 {
			b = append([]byte(nil), b...)
			b[i] = 0xff
		}
		return b, nil
	}
	a := r.invoke(t, invokeReq{Capability: sp("light.onoff@sim/lamp-1"), Caller: localCaller()})
	refusalOf(t, a, http.StatusInternalServerError, "internal")
}

// AL-5, M-AL-7: the decoding mode, option by option, and as a whole equal to
// the daemon's coredet.UnmarshalStrict (CORE-E4): v0.4.2 coredet's options
// plus unknown-field errors, nothing else.
func TestC1DecModeOptions(t *testing.T) {
	got := c1DecMode.DecOptions()
	if got.TagsMd != cbor.TagsForbidden {
		t.Errorf("TagsMd = %v, want TagsForbidden", got.TagsMd)
	}
	if got.DupMapKey != cbor.DupMapKeyEnforcedAPF {
		t.Errorf("DupMapKey = %v, want DupMapKeyEnforcedAPF", got.DupMapKey)
	}
	if got.ExtraReturnErrors != cbor.ExtraDecErrorUnknownField {
		t.Errorf("ExtraReturnErrors = %v, want ExtraDecErrorUnknownField", got.ExtraReturnErrors)
	}
	if got.UTF8 != cbor.UTF8RejectInvalid {
		t.Errorf("UTF8 = %v, want UTF8RejectInvalid", got.UTF8)
	}
	ref, err := cbor.DecOptions{
		TagsMd:            cbor.TagsForbidden,
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
	}.DecMode()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, ref.DecOptions()) {
		t.Errorf("c1DecMode options differ from the reference set:\n got %+v\nwant %+v", got, ref.DecOptions())
	}
	// and the two v0.4.2 coredet rules hold in both decoders
	for _, b := range [][]byte{{0xc0, 0x60}, {0xa2, 0x01, 0x01, 0x01, 0x02}} {
		var v any
		if coredet.Unmarshal(b, &v) == nil || c1DecMode.Unmarshal(b, &v) == nil {
			t.Errorf("%x must be refused by both decoders", b)
		}
	}
}

// AL-5 structural rule: c1serv decodes request bodies with c1DecMode only.
// JSON is gone from C1, and coredet.Unmarshal does not refuse unknown keys.
func TestC1ServDecodesOnlyWithC1DecMode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "encoding/json" {
				t.Errorf("%s imports encoding/json", name)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case pkg.Name == "coredet" && sel.Sel.Name != "Marshal":
				t.Errorf("%s: %s uses coredet.%s; decode with c1DecMode", name, fset.Position(sel.Pos()), sel.Sel.Name)
			case pkg.Name == "cbor" && (sel.Sel.Name == "Unmarshal" || sel.Sel.Name == "UnmarshalFirst" || sel.Sel.Name == "NewDecoder"):
				t.Errorf("%s: %s uses cbor.%s; decode with c1DecMode", name, fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
}

// Every field of effect.Evidence has to cross C1.
//
// One did not. Quirk names the vendor correction applied to a reading, and
// the wire struct simply had no field for it — so a consumer north of C1
// received a corrected value with nothing saying it had been corrected.
// That is the one outcome the quirk layer exists to prevent, and it was the
// northbound boundary of the very package that applies the corrections.
//
// Written by reflection on purpose. A test that names the seven fields it
// expects would have passed against the six-field struct that shipped: it
// would only ever check what someone already remembered. This one fails
// when effect.Evidence grows a field and the wire does not.
func TestEveryEvidenceFieldCrossesTheWire(t *testing.T) {
	var ev effect.Evidence
	src := reflect.ValueOf(&ev).Elem()
	for i := 0; i < src.NumField(); i++ {
		f := src.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("v-" + src.Type().Field(i).Name)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int64, reflect.Int:
			f.SetInt(7)
		case reflect.Uint8, reflect.Uint64, reflect.Uint:
			f.SetUint(4)
		default:
			t.Fatalf("effect.Evidence.%s is a %s — teach this test to fill it",
				src.Type().Field(i).Name, f.Kind())
		}
	}

	got := reflect.ValueOf(wireEvidence(&ev)).Elem()
	for i := 0; i < src.NumField(); i++ {
		name := src.Type().Field(i).Name
		out := got.FieldByName(name)
		if !out.IsValid() {
			t.Errorf("effect.Evidence.%s has no field on the C1 wire — provenance stops here", name)
			continue
		}
		if !reflect.DeepEqual(out.Interface(), src.Field(i).Interface()) {
			t.Errorf("%s: wire has %v, evidence had %v", name, out.Interface(), src.Field(i).Interface())
		}
	}
	if wireEvidence(nil) != nil {
		t.Error("no evidence must stay no evidence, not an empty block claiming A0/V0")
	}
}

// And it has to survive the encoding, not just the mapping.
func TestQuirkSurvivesTheWire(t *testing.T) {
	in := invokeResp{Status: "OK", Evidence: wireEvidence(&effect.Evidence{
		Quirk: "aqara.temp.scale100", ObservedState: "22.13", VerifyTrust: 2,
	})}
	c, err := encodeChecked(in)
	if err != nil {
		t.Fatal(err)
	}
	var viaCBOR invokeResp
	if err := c1DecMode.Unmarshal(c, &viaCBOR); err != nil {
		t.Fatal(err)
	}
	if viaCBOR.Evidence == nil || viaCBOR.Evidence.Quirk != "aqara.temp.scale100" {
		t.Errorf("the correction did not survive CBOR: %+v", viaCBOR.Evidence)
	}
}

func TestDefaultSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
	if p, err := DefaultSocket(); err != nil || p != "/run/user/4242/anetlink/c1.sock" {
		t.Fatalf("DefaultSocket = %q, %v", p, err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := DefaultSocket(); err == nil || !strings.Contains(err.Error(), "--c1-socket") {
		t.Fatalf("without XDG_RUNTIME_DIR the error must name the fix, got %v", err)
	}
}

func TestMain(m *testing.M) {
	// The rig's sockets live under t.TempDir(); keep the path short enough
	// for sun_path (104 bytes on macOS) by preferring a short TMPDIR.
	if len(os.TempDir()) > 40 {
		if d, err := os.MkdirTemp("/tmp", "c1t"); err == nil {
			os.Setenv("TMPDIR", d)
			code := m.Run()
			os.RemoveAll(d)
			os.Exit(code)
		}
	}
	os.Exit(m.Run())
}
