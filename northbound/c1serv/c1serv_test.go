package c1serv

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

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
