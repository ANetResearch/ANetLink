package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ANetResearch/ANetLink/runtime"
)

// runScan is the onboarding fast path: probe every compiled-in adapter, ask
// the ones that can to enumerate their networks, and print a config the user
// can save — turning "I have devices" into a working anetlinkd in one step.
func runScan(rt *runtime.Runtime, writeTo string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	found, problems := rt.DiscoverAll(ctx)

	fmt.Printf("found %d device(s)\n\n", len(found))
	if len(found) == 0 {
		fmt.Println("no devices discovered.")
		if len(problems) == 0 {
			fmt.Println("  (the compiled-in adapters either need configuration first —")
			fmt.Println("   a coordinator port, a broker URL, a Home Assistant token —")
			fmt.Println("   or their networks are empty.)")
		}
	}
	byProto := map[string][]string{}
	fragments := map[string][]json.RawMessage{}
	for _, f := range found {
		line := fmt.Sprintf("  %-24s %-18s %s", f.SuggestedID, f.Address, strings.Join(f.Capabilities, ", "))
		if f.Label != "" {
			line += "  (" + f.Label + ")"
		}
		byProto[f.Protocol] = append(byProto[f.Protocol], line)
		if f.ConfigHint != "" {
			fragments[f.Protocol] = append(fragments[f.Protocol], json.RawMessage(f.ConfigHint))
		}
	}
	protos := make([]string, 0, len(byProto))
	for p := range byProto {
		protos = append(protos, p)
	}
	sort.Strings(protos)
	for _, p := range protos {
		fmt.Printf("%s:\n", p)
		for _, l := range byProto[p] {
			fmt.Println(l)
		}
		fmt.Println()
	}
	for name, err := range problems {
		fmt.Printf("! %s could not scan: %v\n", name, err)
	}

	if len(fragments) == 0 {
		return nil
	}
	adapters := map[string]any{}
	for proto, devs := range fragments {
		adapters[proto] = map[string]any{"devices": devs}
	}
	cfg, err := json.MarshalIndent(map[string]any{"adapters": adapters}, "", "  ")
	if err != nil {
		return err
	}
	if writeTo == "" {
		fmt.Println("suggested config (save it and pass --config):")
		fmt.Println(string(cfg))
		return nil
	}
	if _, err := os.Stat(writeTo); err == nil {
		return fmt.Errorf("refusing to overwrite %s (move it aside first)", writeTo)
	}
	if err := os.WriteFile(writeTo, append(cfg, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s — start with: anetlinkd --config %s --mcp\n", writeTo, writeTo)
	return nil
}
