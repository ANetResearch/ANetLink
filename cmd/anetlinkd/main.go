// anetlinkd is the ANetLink runtime daemon. It boots the runtime with the
// configured adapters and serves them north:
//
//	anetlinkd [--config F]          service mode: C1 v1 for the anet daemon
//	                                on $XDG_RUNTIME_DIR/anetlink/c1.sock
//	anetlinkd --mcp                 MCP on stdio (C1 too, if a socket is set)
//	anetlinkd --demo|--scan|--pair  one-shot commands
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANetLink/northbound/c1serv"
	"github.com/ANetResearch/ANetLink/northbound/mcpserv"
	"github.com/ANetResearch/ANetLink/runtime"
)

// version 0.2.0 speaks C1 v1 (CBOR only, /v1/*); a C1 v0 client gets 426.
const version = "0.2.0"

func main() {
	demo := flag.Bool("demo", false, "run one demo invocation and exit")
	serveMCP := flag.Bool("mcp", false, "serve the MCP northbound on stdio (for Claude Code / IDE clients)")
	configPath := flag.String("config", "", "path to anetlinkd JSON config")
	scan := flag.Bool("scan", false, "discover devices on the connected networks and print a suggested config")
	pair := flag.Duration("pair", 0, "open pairing-capable networks for this long (e.g. 60s) and report what joins")
	scanOut := flag.String("scan-out", "", "with --scan, write the suggested config to this file")
	var c1f c1Flags
	flag.StringVar(&c1f.socket, "c1-socket", "", "serve C1 for the anet daemon on this unix socket (default $XDG_RUNTIME_DIR/anetlink/c1.sock; config c1.socket)")
	flag.Var(&c1f.allowUsers, "c1-allow-user", "let this user (name or uid) connect to the C1 socket, besides anetlinkd's own; repeatable (config c1.allow_users)")
	flag.StringVar(&c1f.group, "c1-group", "", "group that opens the C1 socket to the allowed users (config c1.group)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("anetlinkd: %v", err)
	}
	rtCfg, err := cfg.runtimeConfig()
	if err != nil {
		log.Fatalf("anetlinkd: %v", err)
	}
	rt := runtime.New(rtCfg)
	if rtCfg.Quirks != nil {
		log.Printf("vendor corrections loaded: %d quirk(s)", rtCfg.Quirks.Len())
	}
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

	// C1 serves the daemon in service mode, and in --mcp mode only when a
	// socket is configured: an IDE's anetlinkd --mcp must not fight a
	// service anetlinkd for the default socket. The socket is prepared
	// before the adapters start, so a refusal (untrusted directory,
	// unknown user, a socket in use) stops the start at once; connections
	// wait in the backlog until the runtime is ready.
	c1opts := cfg.c1Options(c1f)
	var c1ln net.Listener
	if !*demo && !*scan && *pair == 0 && (c1opts.Socket != "" || !*serveMCP) {
		if c1opts.Socket == "" {
			if c1opts.Socket, err = c1serv.DefaultSocket(); err != nil {
				log.Fatalf("anetlinkd: %v", err)
			}
		}
		if c1ln, err = c1serv.Listen(c1opts); err != nil {
			log.Fatalf("anetlinkd: %v", err)
		}
	}

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

	if *pair > 0 {
		joined, problems := rt.PairAll(ctx, *pair)
		fmt.Printf("pairing window %s — %d device(s) joined\n", *pair, len(joined))
		for _, f := range joined {
			fmt.Printf("  %-24s %-10s %v\n", f.SuggestedID, f.Protocol, f.Capabilities)
			if f.ConfigHint != "" {
				fmt.Printf("    config: %s\n", f.ConfigHint)
			}
		}
		for name, err := range problems {
			fmt.Printf("! %s: %v\n", name, err)
		}
		if len(joined) == 0 && len(problems) == 0 {
			fmt.Println("  (no adapter supports pairing, or nothing joined in the window)")
		}
		return
	}

	if *scan {
		if err := runScan(rt, *scanOut); err != nil {
			log.Fatalf("anetlinkd: scan: %v", err)
		}
		return
	}

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c1done := make(chan error, 1)
	if c1ln != nil {
		go func() { c1done <- c1serv.Serve(sigCtx, rt, c1ln, nil) }()
		log.Printf("c1 v1 on %s (other users allowed: %d; capabilities open to remote callers: %d pattern(s))",
			c1opts.Socket, len(c1opts.AllowUsers), len(rtCfg.Remote))
	}

	if *serveMCP {
		// stdio belongs to the MCP session; logs already go to stderr.
		err := mcpserv.New(rt, version).Run(sigCtx, &mcp.StdioTransport{})
		interrupted := sigCtx.Err() != nil
		stop()
		if c1ln != nil {
			<-c1done // the socket is removed before we exit
		}
		if err != nil && !interrupted {
			log.Fatalf("anetlinkd: mcp: %v", err)
		}
		return
	}

	if *demo {
		if len(rt.Devices()) == 0 {
			fmt.Println("demo: no devices (sim compiled out or disabled) — inventory empty, exiting 0")
			return
		}
		e0 := rt.Inventory()[0]
		d0, key := e0.Device, e0.Key
		capID := d0.Profile.Caps[0].ID
		eff, err := rt.Invoke(ctx, runtime.Caller{Via: runtime.ViaLocal}, key, capID, map[string]any{"on": true})
		if err != nil {
			log.Fatalf("anetlinkd: demo invoke: %v", err)
		}
		fmt.Printf("demo: %s on=true → status=%s verifiable=%v metrics=%v\n",
			capID, eff.Status, eff.Verifiable(), eff.Record.Metrics)
		return
	}

	if c1ln == nil {
		<-sigCtx.Done()
		return
	}
	select {
	case <-sigCtx.Done():
		<-c1done // the socket is removed before we exit
	case err := <-c1done:
		// Service mode exists to serve C1; without it there is nothing to do.
		log.Fatalf("anetlinkd: c1 stopped: %v", err)
	}
}
