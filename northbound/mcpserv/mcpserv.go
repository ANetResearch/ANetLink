// Package mcpserv is ANetLink's MCP northbound: it exposes the runtime's
// devices as tools so any MCP client (Claude Code, IDEs) can inventory and
// actuate them — with zero hub and zero daemon. This surface is the
// cold-start hook: one machine, one adapter, and devices are callable.
package mcpserv

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANetLink/runtime"
)

// DeviceView is the tool-facing projection of one device.
type DeviceView struct {
	Key          string   `json:"key" jsonschema:"runtime device key, adapter/deviceID"`
	Protocol     string   `json:"protocol"`
	Online       bool     `json:"online"`
	Trust        uint8    `json:"trust" jsonschema:"effect-verification trust 0-4 (can I verify it happened)"`
	AuthTrust    uint8    `json:"auth_trust" jsonschema:"identity-authentication trust 0-4 (do I know who it is)"`
	Capabilities []string `json:"capabilities"`
	ProfileCID   string   `json:"profile_cid,omitempty"`
}

type listOut struct {
	Devices []DeviceView `json:"devices"`
}

type invokeIn struct {
	Device     string         `json:"device" jsonschema:"runtime device key as returned by devices_list, e.g. sim/lamp-1"`
	Capability string         `json:"capability" jsonschema:"capability id declared by the device, e.g. light.onoff"`
	Args       map[string]any `json:"args,omitempty" jsonschema:"capability arguments, e.g. {\"on\": true}"`
}

type invokeOut struct {
	Status     string             `json:"status" jsonschema:"OK | UNVERIFIED | FAILED | UNAVAILABLE"`
	Verifiable bool               `json:"verifiable"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Message    string             `json:"message,omitempty"`
	Evidence   *evidenceOut       `json:"evidence,omitempty" jsonschema:"structured provenance of the execution"`
}

// evidenceOut is the tool-facing projection of effect.Evidence.
type evidenceOut struct {
	Requested     string `json:"requested,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	NativeAck     bool   `json:"native_ack,omitempty"`
	ObservedState string `json:"observed_state,omitempty"`
	LatencyMS     int64  `json:"latency_ms,omitempty"`
	VerifyTrust   uint8  `json:"verify_trust,omitempty"`
	AuthTrust     uint8  `json:"auth_trust,omitempty"`
}

// New builds the MCP server over a started runtime.
func New(rt *runtime.Runtime, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "anetlink", Version: version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "devices_list",
		Description: "List all devices ANetLink currently exposes: key, protocol, online state, trust level (0-4) and callable capabilities.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOut, error) {
		var out listOut
		for _, d := range rt.Devices() {
			all := d.Profile.AllCaps() // includes endpoint capabilities (D50)
			caps := make([]string, 0, len(all))
			for _, c := range all {
				caps = append(caps, c.ID)
			}
			cid, _ := d.Profile.CID()
			out.Devices = append(out.Devices, DeviceView{
				Key:          d.Profile.Protocol + "/" + d.ID,
				Protocol:     d.Profile.Protocol,
				Online:       d.Online,
				Trust:        uint8(d.Profile.MaxTrust),
				AuthTrust:    uint8(d.Profile.AuthTrust),
				Capabilities: caps,
				ProfileCID:   cid,
			})
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "device_invoke",
		Description: "Invoke one capability on one device. Returns the execution effect: status is honest — UNVERIFIED means the command went out but the physical effect cannot be verified at this device's trust level; it is not a failure.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in invokeIn) (*mcp.CallToolResult, invokeOut, error) {
		key, err := parseKey(in.Device)
		if err != nil {
			return nil, invokeOut{}, err
		}
		eff, err := rt.Invoke(ctx, key, in.Capability, in.Args)
		if err != nil {
			return nil, invokeOut{}, err
		}
		out := invokeOut{Status: string(eff.Status), Verifiable: eff.Verifiable(), Message: eff.Message}
		if eff.Record != nil {
			out.Metrics = eff.Record.Metrics
		}
		if ev := eff.Evidence; ev != nil {
			out.Evidence = &evidenceOut{
				Requested: ev.Requested, Protocol: ev.Protocol, NativeAck: ev.NativeAck,
				ObservedState: ev.ObservedState, LatencyMS: ev.LatencyMS,
				VerifyTrust: ev.VerifyTrust, AuthTrust: ev.AuthTrust,
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "devices_discover",
		Description: "Scan the connected networks (Zigbee, BACnet, …) for devices that are present but not yet configured. Returns each find with a suggested id, its capabilities and a ready-to-paste config fragment — the fast path from 'I have a device' to 'the agent can call it'.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, discoverOut, error) {
		found, errs := rt.DiscoverAll(ctx)
		out := discoverOut{}
		for _, f := range found {
			out.Found = append(out.Found, foundView{
				SuggestedID: f.SuggestedID, Protocol: f.Protocol, Label: f.Label,
				Address: f.Address, Capabilities: f.Capabilities, ConfigHint: f.ConfigHint,
			})
		}
		for name, err := range errs {
			out.Problems = append(out.Problems, name+": "+err.Error())
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "devices_pair",
		Description: "Open the networks that support pairing (Zigbee permit-join, ...) for a window of seconds so new devices can join, then report what joined with its capabilities. Use this when the user says they bought or reset a device. Default window 60s, max 254s (a Zigbee limit).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in pairIn) (*mcp.CallToolResult, discoverOut, error) {
		secs := in.WindowSeconds
		if secs <= 0 {
			secs = 60
		}
		if secs > 254 {
			secs = 254
		}
		joined, errs := rt.PairAll(ctx, time.Duration(secs)*time.Second)
		out := discoverOut{}
		for _, f := range joined {
			out.Found = append(out.Found, foundView{
				SuggestedID: f.SuggestedID, Protocol: f.Protocol, Label: f.Label,
				Address: f.Address, Capabilities: f.Capabilities, ConfigHint: f.ConfigHint,
			})
		}
		for name, err := range errs {
			out.Problems = append(out.Problems, name+": "+err.Error())
		}
		return nil, out, nil
	})

	return s
}

// pairIn is the input to devices_pair.
type pairIn struct {
	WindowSeconds int `json:"window_seconds,omitempty" jsonschema:"how long to accept new devices, seconds (default 60, max 254)"`
}

// foundView is the tool-facing projection of a discovered device.
type foundView struct {
	SuggestedID  string   `json:"suggested_id"`
	Protocol     string   `json:"protocol"`
	Label        string   `json:"label,omitempty"`
	Address      string   `json:"address,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	ConfigHint   string   `json:"config_hint,omitempty" jsonschema:"JSON fragment to paste into anetlinkd config"`
}

type discoverOut struct {
	Found    []foundView `json:"found"`
	Problems []string    `json:"problems,omitempty" jsonschema:"adapters that could not scan, and why"`
}

func parseKey(s string) (runtime.DeviceKey, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			if i == 0 || i == len(s)-1 {
				break
			}
			return runtime.DeviceKey{Adapter: s[:i], Device: s[i+1:]}, nil
		}
	}
	return runtime.DeviceKey{}, fmt.Errorf("mcpserv: device key must be adapter/deviceID, got %q", s)
}
