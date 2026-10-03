package mcpserv

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANetLink/adapters/sim"
	"github.com/ANetResearch/ANetLink/runtime"
)

func session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	rt := runtime.New(runtime.Config{})
	if err := rt.Register(sim.New()); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := New(rt, "test")
	st, ct := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), st) }()
	cli := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := cli.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestToolsExposed(t *testing.T) {
	cs := session(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	if !names["devices_list"] || !names["device_invoke"] {
		t.Fatalf("missing tools: %v", names)
	}
}

func TestListAndInvoke(t *testing.T) {
	cs := session(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "devices_list"})
	if err != nil {
		t.Fatal(err)
	}
	var lo listOut
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &lo); err != nil {
		t.Fatal(err)
	}
	if len(lo.Devices) != 1 || lo.Devices[0].Key != "sim/lamp-1" || lo.Devices[0].Trust != 4 {
		t.Fatalf("inventory wrong: %+v", lo)
	}

	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "device_invoke",
		Arguments: map[string]any{"device": "sim/lamp-1", "capability": "light.onoff", "args": map[string]any{"on": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var io invokeOut
	b, _ = json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &io); err != nil {
		t.Fatal(err)
	}
	if io.Status != "OK" || !io.Verifiable || io.Metrics["power_state"] != 1 {
		t.Fatalf("invoke effect wrong: %+v", io)
	}
}

func TestInvokeBadKey(t *testing.T) {
	cs := session(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "device_invoke",
		Arguments: map[string]any{"device": "nonsense", "capability": "x"},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("malformed device key must surface as tool error")
	}
}

// AL-9: the hints an MCP client uses to decide what needs a human's
// confirmation. Actuating and pairing change the physical world and reach
// beyond this process; discovery and listing change nothing.
func TestToolAnnotations(t *testing.T) {
	cs := session(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type hints struct{ readOnly, destructive, openWorld bool }
	want := map[string]hints{
		"device_invoke":    {readOnly: false, destructive: true, openWorld: true},
		"devices_pair":     {readOnly: false, destructive: true, openWorld: true},
		"devices_discover": {readOnly: true, destructive: false, openWorld: true},
		"devices_list":     {readOnly: true, destructive: false, openWorld: false},
	}
	seen := 0
	for _, tool := range res.Tools {
		w, ok := want[tool.Name]
		if !ok {
			continue
		}
		seen++
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		// DestructiveHint and OpenWorldHint default to true when absent, so
		// "false" must be written out and "true" may be either.
		destructive := a.DestructiveHint == nil || *a.DestructiveHint
		openWorld := a.OpenWorldHint == nil || *a.OpenWorldHint
		if a.ReadOnlyHint != w.readOnly {
			t.Errorf("%s readOnlyHint = %v, want %v", tool.Name, a.ReadOnlyHint, w.readOnly)
		}
		if !a.ReadOnlyHint && destructive != w.destructive {
			t.Errorf("%s destructiveHint = %v, want %v", tool.Name, destructive, w.destructive)
		}
		if openWorld != w.openWorld {
			t.Errorf("%s openWorldHint = %v, want %v", tool.Name, openWorld, w.openWorld)
		}
		if w.destructive && (a.DestructiveHint == nil || a.OpenWorldHint == nil) {
			t.Errorf("%s: spell the destructive and open-world hints out rather than rely on defaults", tool.Name)
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d tools", seen, len(want))
	}
}
