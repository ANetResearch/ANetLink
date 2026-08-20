package c1serv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANetLink/adapters/sim"
	"github.com/ANetResearch/ANetLink/runtime"
)

func udsClient(t *testing.T) (*http.Client, string) {
	t.Helper()
	rt := runtime.New(runtime.Config{})
	if err := rt.Register(sim.New()); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "anetlink.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, rt, sock) }()
	cli := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if resp, err := cli.Get("http://uds/v0/health"); err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("c1serv did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cli, sock
}

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

func TestCapabilitiesAndInvokeOverUDS(t *testing.T) {
	cli, _ := udsClient(t)
	resp, err := cli.Get("http://uds/v0/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var caps struct{ Capabilities []string }
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if len(caps.Capabilities) != 1 || caps.Capabilities[0] != "light.onoff@sim/lamp-1" {
		t.Fatalf("capabilities: %v", caps)
	}

	body, _ := json.Marshal(map[string]any{
		"capability": "light.onoff@sim/lamp-1",
		"args":       map[string]any{"on": true},
	})
	resp2, err := cli.Post("http://uds/v0/invoke", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out struct {
		Status  string
		Metrics map[string]float64
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "OK" || out.Metrics["power_state"] != 1 {
		t.Fatalf("invoke: %+v", out)
	}
}

func TestInvokeMalformedID(t *testing.T) {
	cli, _ := udsClient(t)
	body, _ := json.Marshal(map[string]any{"capability": "nonsense"})
	resp, err := cli.Post("http://uds/v0/invoke", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// TestCBORWire pins D52: the AELP canonical encoding. A client that speaks
// CoreDet-CBOR gets CBOR back, byte-identical in meaning to the JSON form.
func TestCBORWire(t *testing.T) {
	cli, _ := udsClient(t)

	req := map[uint64]any{1: "light.onoff@sim/lamp-1", 2: map[string]any{"on": true}}
	body, err := coredet.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq, _ := http.NewRequest(http.MethodPost, "http://uds/v0/invoke", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/cbor")
	httpReq.Header.Set("Accept", "application/cbor")
	resp, err := cli.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/cbor" {
		t.Fatalf("expected CBOR response, got %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out invokeResp
	if err := coredet.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not CoreDet-CBOR: %v", err)
	}
	if out.Status != "OK" || out.Metrics["power_state"] != 1 {
		t.Fatalf("CBOR round-trip lost data: %+v", out)
	}

	// JSON must still work (debug profile)
	jbody, _ := json.Marshal(map[string]any{"capability": "light.onoff@sim/lamp-1", "args": map[string]any{"on": false}})
	jr, err := cli.Post("http://uds/v0/invoke", "application/json", bytes.NewReader(jbody))
	if err != nil {
		t.Fatal(err)
	}
	defer jr.Body.Close()
	var jout invokeResp
	if err := json.NewDecoder(jr.Body).Decode(&jout); err != nil {
		t.Fatal(err)
	}
	if jout.Status != "OK" {
		t.Fatalf("JSON path broken: %+v", jout)
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

// And it has to survive the encoding, not just the mapping — over both
// wire formats, since a CBOR key and a JSON name are two separate ways to
// lose a field.
func TestQuirkSurvivesBothEncodings(t *testing.T) {
	in := invokeResp{Status: "OK", Evidence: wireEvidence(&effect.Evidence{
		Quirk: "aqara.temp.scale100", ObservedState: "22.13", VerifyTrust: 2,
	})}
	j, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var viaJSON invokeResp
	if err := json.Unmarshal(j, &viaJSON); err != nil {
		t.Fatal(err)
	}
	if viaJSON.Evidence == nil || viaJSON.Evidence.Quirk != "aqara.temp.scale100" {
		t.Errorf("the correction did not survive JSON: %s", j)
	}
	c, err := coredet.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var viaCBOR invokeResp
	if err := coredet.Unmarshal(c, &viaCBOR); err != nil {
		t.Fatal(err)
	}
	if viaCBOR.Evidence == nil || viaCBOR.Evidence.Quirk != "aqara.temp.scale100" {
		t.Errorf("the correction did not survive CBOR: %+v", viaCBOR.Evidence)
	}
}
