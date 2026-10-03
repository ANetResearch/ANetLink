package c1serv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANetLink/adapters/sim"
	"github.com/ANetResearch/ANetLink/northbound/c1serv"
	"github.com/ANetResearch/ANetLink/northbound/mcpserv"
	"github.com/ANetResearch/ANetLink/runtime"
)

const aid = "bafyreifcvw7axfcj45itg7ggk5pf33dtq53kgpf47ltxnjl746b45n62j4"

type entrances struct {
	rt  *runtime.Runtime
	c1  *http.Client
	mcp *mcp.ClientSession
}

func open(t *testing.T, remote ...string) *entrances {
	t.Helper()
	rt := runtime.New(runtime.Config{Remote: remote, Logf: func(string, ...any) {}})
	if err := rt.Register(sim.New()); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "c1e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "run", "c1.sock")
	ln, err := c1serv.Listen(c1serv.Options{Socket: sock, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c1serv.Serve(ctx, rt, ln, func(string, ...any) {}); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	st, ct := mcp.NewInMemoryTransports()
	go func() { _ = mcpserv.New(rt, "test").Run(context.Background(), st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return &entrances{rt: rt, mcp: cs, c1: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

// viaC1 invokes the lamp over C1 as a local (via 0) or relay (via 1) caller
// and returns the HTTP status.
func (e *entrances) viaC1(t *testing.T, relay bool) int {
	t.Helper()
	who := map[any]any{uint64(1): uint64(0)}
	if relay {
		who = map[any]any{uint64(1): uint64(1), uint64(2): aid}
	}
	body, err := coredet.Marshal(map[any]any{
		uint64(1): "light.onoff@sim/lamp-1",
		uint64(2): map[any]any{"on": true},
		uint64(5): who,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.c1.Post("http://anetlink/v1/invoke", "application/cbor", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func (e *entrances) viaMCP(t *testing.T) bool {
	t.Helper()
	res, err := e.mcp.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "device_invoke",
		Arguments: map[string]any{"device": "sim/lamp-1", "capability": "light.onoff", "args": map[string]any{"on": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		return false
	}
	var out struct{ Status string }
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	return out.Status == "OK"
}

func (e *entrances) direct(relay bool) error {
	c := runtime.Caller{Via: runtime.ViaLocal}
	if relay {
		c = runtime.Caller{Via: runtime.ViaRelay, AID: aid}
	}
	_, err := e.rt.Invoke(context.Background(), c, runtime.DeviceKey{Adapter: "sim", Device: "lamp-1"}, "light.onoff", nil)
	return err
}

// AL-7, M-AL-6: the remote policy is decided once, in runtime.Invoke, so
// every entrance gives the same answer for the same caller — C1, the MCP
// tools, and any future surface that calls the runtime. A policy kept only
// in c1serv would pass the C1 half of this test and fail the runtime half.
func TestPolicyIsTheSameThroughEveryEntrance(t *testing.T) {
	closed := open(t)
	if s := closed.viaC1(t, false); s != http.StatusOK {
		t.Fatalf("C1 local: %d", s)
	}
	if !closed.viaMCP(t) {
		t.Fatal("MCP device_invoke refused")
	}
	if err := closed.direct(false); err != nil {
		t.Fatalf("runtime local: %v", err)
	}
	if s := closed.viaC1(t, true); s != http.StatusForbidden {
		t.Fatalf("C1 relay with default c1.remote: %d, want 403", s)
	}
	if err := closed.direct(true); !errors.Is(err, runtime.ErrNotPermitted) {
		t.Fatalf("runtime relay with default c1.remote: %v, want ErrNotPermitted", err)
	}

	opened := open(t, "light.onoff@sim/*")
	if s := opened.viaC1(t, true); s != http.StatusOK {
		t.Fatalf("C1 relay with c1.remote match: %d", s)
	}
	if err := opened.direct(true); err != nil {
		t.Fatalf("runtime relay with c1.remote match: %v", err)
	}
	if !opened.viaMCP(t) || opened.viaC1(t, false) != http.StatusOK {
		t.Fatal("opening a capability to remote callers must not change local access")
	}
}

// AL-7 structural rule: no northbound package can reach an adapter except
// through runtime.Invoke. Without the adapter contract or an adapter
// package in scope there is nothing to call Invoke on but the runtime.
func TestNorthboundReachesAdaptersOnlyThroughTheRuntime(t *testing.T) {
	forbidden := []string{
		"github.com/ANetResearch/ANetLink/sdk",
		"github.com/ANetResearch/ANetLink/adap",
		"github.com/ANetResearch/ANetLink/adapters/",
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if path == bad || (strings.HasSuffix(bad, "/") && strings.HasPrefix(path, bad)) {
					t.Errorf("%s imports %s: northbound code must go through runtime.Invoke", p, path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
