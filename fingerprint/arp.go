package fingerprint

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// procNetARP is Linux's ARP cache. Reading it needs no privileges and no
// raw sockets, which matters because anetlinkd is meant to run unprivileged
// on an appliance.
const procNetARP = "/proc/net/arp"

// IdentifyIP names the manufacturer of a host on the local segment.
//
// Only the local segment: ARP is link-scoped, so an address behind a router
// resolves to the router's MAC and would be reported as whoever made the
// router. That is a confidently wrong answer, so an entry whose MAC belongs
// to the gateway is not treated as the device's own.
//
// This is the step that turns "there is something at 192.0.2.71" into "that
// is a Hikvision camera, and the onvif and hikvision adapters can drive it"
// — before any credential has been typed.
func IdentifyIP(ctx context.Context, ip string) (Identification, bool) {
	mac, ok := MACForIP(ctx, ip)
	if !ok {
		return Identification{}, false
	}
	return VendorByMAC(mac)
}

// MACForIP resolves an IP to a MAC from the ARP cache, nudging the cache
// first if the address is not already in it.
func MACForIP(ctx context.Context, ip string) (string, bool) {
	if mac, ok := arpLookup(ip); ok {
		return mac, true
	}
	// A host we have not spoken to is not in the cache. One UDP packet to a
	// closed port is enough to make the kernel ARP for it; nothing needs to
	// answer, and nothing is sent to the application layer.
	nudge(ctx, ip)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mac, ok := arpLookup(ip); ok {
			return mac, true
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return "", false
}

func nudge(ctx context.Context, ip string) {
	d := net.Dialer{Timeout: 200 * time.Millisecond}
	// Port 9 is discard; the packet is inert by design.
	conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(ip, "9"))
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte{0})
}

// arpLookup reads one entry out of the kernel ARP cache.
//
// The file's columns are: IP address, HW type, Flags, HW address, Mask,
// Device. An entry with flags 0x0 is incomplete — the kernel asked and got
// no answer — and its MAC is the all-zero placeholder, which must not be
// resolved to a vendor.
func arpLookup(ip string) (string, bool) {
	f, err := os.Open(procNetARP)
	if err != nil {
		return "", false // not Linux, or no procfs: fingerprinting is optional
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return "", false // header
	}
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[0] != ip {
			continue
		}
		if fields[2] == "0x0" {
			return "", false // incomplete entry
		}
		mac := fields[3]
		if mac == "00:00:00:00:00:00" {
			return "", false
		}
		return mac, true
	}
	return "", false
}

// Describe renders an identification for an operator: short, and explicit
// about what is and is not known.
func (id Identification) Describe() string {
	if !id.Found {
		if id.MAC == "" {
			return "unidentified (no MAC on this segment)"
		}
		return fmt.Sprintf("unidentified (%s)", id.MAC)
	}
	s := id.Vendor.Name
	if id.Vendor.Class != "" && id.Vendor.Class != ClassUnknown {
		s += " " + string(id.Vendor.Class)
	}
	if len(id.Vendor.Adapters) > 0 {
		s += " — driveable via " + strings.Join(id.Vendor.Adapters, "/")
	} else if len(id.Vendor.Ecosystems) > 0 {
		// Naming the ecosystem we cannot yet reach is more useful than
		// silence: it tells the operator which adapter is missing.
		ecos := make([]string, len(id.Vendor.Ecosystems))
		for i, e := range id.Vendor.Ecosystems {
			ecos[i] = string(e)
		}
		s += " — no adapter yet (" + strings.Join(ecos, "/") + ")"
	} else {
		s += " — no adapter yet"
	}
	return s
}
