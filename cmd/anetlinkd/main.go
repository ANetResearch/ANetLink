// anetlinkd is the ANetLink runtime daemon. v0: boots the runtime with the
// sim adapter, prints the device inventory, and (with --demo) proves the
// invoke path end to end. The MCP surface and the C1 shim come next.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANetLink/northbound/c1serv"
	"github.com/ANetResearch/ANetLink/northbound/mcpserv"
	"github.com/ANetResearch/ANetLink/runtime"
)

const version = "0.1.0"

func main() {
	demo := flag.Bool("demo", false, "run one demo invocation and exit")
	serveMCP := flag.Bool("mcp", false, "serve the MCP northbound on stdio (for Claude Code / IDE clients)")
	configPath := flag.String("config", "", "path to anetlinkd JSON config")
	c1Socket := flag.String("c1-socket", "", "serve the C1 provider wire on this unix socket (for the ANet daemon shim)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("anetlinkd: %v", err)
	}
	rt := runtime.New(cfg.runtimeConfig())
	adapters, err := cfg.buildAdapters()
	if err != nil {
		log.Fatalf("anetlinkd: %v", err)
	}
	for _, na := range adapters {
		if err := rt.Register(na.Adapter); err != nil {
			log.Fatalf("anetlinkd: %v", err)
		}
	}
	log.Printf("adapters compiled+configured: %s", adapterNames(adapters))
	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		log.Fatalf("anetlinkd: %v", err)
	}
	defer rt.Stop(ctx)

	for _, d := range rt.Devices() {
		cid, _ := d.Profile.CID()
		log.Printf("device %s/%s online=%v trust=T%d profile=%s",
			d.Profile.Protocol, d.ID, d.Online, d.Profile.MaxTrust, cid)
	}

	if *c1Socket != "" {
		go func() {
			if err := c1serv.Serve(ctx, rt, *c1Socket); err != nil {
				log.Fatalf("anetlinkd: c1: %v", err)
			}
		}()
		log.Printf("c1 provider wire on %s", *c1Socket)
	}

	if *serveMCP {
		// stdio belongs to the MCP session; logs already go to stderr.
		if err := mcpserv.New(rt, version).Run(ctx, &mcp.StdioTransport{}); err != nil {
			log.Fatalf("anetlinkd: mcp: %v", err)
		}
		return
	}

	if *demo {
		if len(rt.Devices()) == 0 {
			fmt.Println("demo: no devices (sim compiled out or disabled) — inventory empty, exiting 0")
			return
		}
		d0 := rt.Devices()[0]
		key := runtime.DeviceKey{Adapter: d0.Profile.Protocol, Device: d0.ID}
		capID := d0.Profile.Caps[0].ID
		eff, err := rt.Invoke(ctx, key, capID, map[string]any{"on": true})
		if err != nil {
			log.Fatalf("anetlinkd: demo invoke: %v", err)
		}
		fmt.Printf("demo: %s on=true → status=%s verifiable=%v metrics=%v\n",
			capID, eff.Status, eff.Verifiable(), eff.Record.Metrics)
		return
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
