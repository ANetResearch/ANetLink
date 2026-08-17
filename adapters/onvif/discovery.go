package onvif

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetLink/fingerprint"
	"github.com/ANetResearch/ANetLink/sdk"
)

// WS-Discovery multicast group (WS-Discovery 1.0 / ONVIF Core §7).
var (
	wsdGroupV4 = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}
)

const (
	nsDiscovery = "http://schemas.xmlsoap.org/ws/2005/04/discovery"
	nsWSA04     = "http://schemas.xmlsoap.org/ws/2004/08/addressing"
	// wsdTarget is the device type every ONVIF camera answers to.
	wsdTarget = "dn:NetworkVideoTransmitter"
	// wsdWait is how long to listen after probing. Cameras stagger their
	// replies by design (the spec asks for a random delay) to avoid a storm,
	// so a short window silently loses the slower half of the network.
	wsdWait = 3 * time.Second
)

// probeMessage builds the WS-Discovery Probe. The MessageID must be unique
// per probe; devices use it to suppress duplicate replies.
func probeMessage() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := "uuid:" + hex.EncodeToString(b)
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<e:Envelope xmlns:e="` + nsSOAP + `" xmlns:w="` + nsWSA04 + `"` +
		` xmlns:d="` + nsDiscovery + `" xmlns:dn="` + nsDevice + `">` +
		`<e:Header>` +
		`<w:MessageID>` + id + `</w:MessageID>` +
		`<w:To e:mustUnderstand="true">urn:schemas-xmlsoap-org:ws:2005:04:discovery</w:To>` +
		`<w:Action e:mustUnderstand="true">` + nsDiscovery + `/Probe</w:Action>` +
		`</e:Header>` +
		`<e:Body><d:Probe><d:Types>` + wsdTarget + `</d:Types></d:Probe></e:Body>` +
		`</e:Envelope>`, nil
}

// Compile-time proof that this adapter satisfies C4 discovery. Without
// it a renamed method makes the adapter silently undiscoverable rather
// than a build failure — which is exactly how the ONVIF adapter first
// reported an empty network on a network full of cameras.
var _ sdk.Discoverer = (*Adapter)(nil)

// DiscoverDevices implements sdk.Discoverer: it asks the network what cameras are
// on it. This is the difference between "ANetLink supports ONVIF" and
// "adding a camera is convenient" — a user should never have to find an IP
// address in a router's DHCP table to onboard a device that is broadcasting
// its own address.
func (a *Adapter) DiscoverDevices(ctx context.Context) ([]sdk.Found, error) {
	ifaces, err := multicastInterfaces(a.cfg.DiscoveryInterface)
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("onvif: no multicast-capable interface available")
	}

	msg, err := probeMessage()
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(wsdWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	// One socket per interface: a camera answers on the interface it heard
	// the probe on, and a single wildcard socket loses replies on hosts with
	// several networks — which is the normal case for a hub appliance.
	//
	// Concurrently, and this is not an optimisation. Every probe listens
	// until the same absolute deadline, so probing sequentially means the
	// first interface consumes the whole window and every later one gets
	// nothing. On a host with a dozen docker bridges ahead of the real NIC
	// that reliably discovers zero cameras on a network full of them —
	// which is exactly how this was found.
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		found = map[string]sdk.Found{}
	)
	for _, ifi := range ifaces {
		wg.Add(1)
		go func(ifi net.Interface) {
			defer wg.Done()
			matches, err := probeOn(ifi, msg, deadline)
			if err != nil {
				return // a down or unprivileged interface is not fatal
			}
			mu.Lock()
			defer mu.Unlock()
			for _, m := range matches {
				f, ok := foundFromMatch(m)
				if !ok {
					continue
				}
				if _, dup := found[f.SuggestedID]; !dup {
					found[f.SuggestedID] = f
				}
			}
		}(ifi)
	}
	wg.Wait()

	out := make([]sdk.Found, 0, len(found))
	for _, f := range found {
		// Name the manufacturer before handing the list over. A scan that
		// says "192.0.2.71" is an address list; one that says "Hikvision
		// camera, driveable via onvif/hikvision" is an answer.
		if host := hostOf(f.Address); host != "" {
			if id, ok := fingerprint.IdentifyIP(ctx, host); ok {
				f.Vendor = id.Vendor.Name
				f.VendorNote = id.Describe()
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// hostOf extracts the bare host from a service URL.
func hostOf(addr string) string {
	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func multicastInterfaces(only string) ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if only != "" && ifi.Name != only {
			continue
		}
		out = append(out, ifi)
	}
	return out, nil
}

// probeOn sends the probe from one interface and collects replies until the
// deadline.
func probeOn(ifi net.Interface, msg string, deadline time.Time) ([]probeMatch, error) {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	var src net.IP
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			src = ipn.IP.To4()
			break
		}
	}
	if src == nil {
		return nil, fmt.Errorf("no IPv4 address on %s", ifi.Name)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: src, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP([]byte(msg), wsdGroupV4); err != nil {
		return nil, err
	}

	var matches []probeMatch
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline reached: this is the normal end of a probe
		}
		var pm probeMatches
		if xml.Unmarshal(buf[:n], &pm) != nil {
			continue
		}
		matches = append(matches, pm.Body.Matches...)
	}
	return matches, nil
}

// foundFromMatch turns a ProbeMatch into an onboarding suggestion complete
// enough to paste into a config.
func foundFromMatch(m probeMatch) (sdk.Found, bool) {
	xaddr := firstHTTPAddr(m.XAddrs)
	if xaddr == "" {
		return sdk.Found{}, false
	}
	u, err := url.Parse(xaddr)
	if err != nil {
		return sdk.Found{}, false
	}

	name, hardware := parseScopes(m.Scopes)
	host := u.Hostname()
	id := "cam-" + strings.ReplaceAll(host, ".", "-")
	label := name
	if label == "" {
		label = hardware
	}
	if label == "" {
		label = host
	}

	// The hint carries xaddr rather than address: the camera just told us
	// exactly where its service lives, and guessing /onvif/device_service
	// from a host is how integrations break on the cameras that differ.
	hint := fmt.Sprintf(
		`{"id":%q,"xaddr":%q,"user":"<username>","pass":"<password>","events":true}`,
		id, xaddr)

	return sdk.Found{
		SuggestedID: id,
		Protocol:    "onvif",
		Label:       label,
		Address:     xaddr,
		// Capabilities are what ONVIF guarantees for a NetworkVideoTransmitter.
		// PTZ and events are not claimed here: only interrogating the camera
		// reveals those, and Discover has not authenticated yet.
		Capabilities: []string{"stream.rtsp", "stream.snapshot"},
		ConfigHint:   hint,
	}, true
}

// firstHTTPAddr picks a usable service URL from the space-separated XAddrs.
func firstHTTPAddr(xaddrs string) string {
	for _, a := range strings.Fields(xaddrs) {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			return a
		}
	}
	return ""
}

// parseScopes extracts the human-facing name and hardware model from ONVIF
// discovery scopes, which are URIs like onvif://www.onvif.org/name/Camera1.
func parseScopes(scopes string) (name, hardware string) {
	for _, s := range strings.Fields(scopes) {
		switch {
		case strings.Contains(s, "/name/"):
			name = unescapeScope(s[strings.Index(s, "/name/")+len("/name/"):])
		case strings.Contains(s, "/hardware/"):
			hardware = unescapeScope(s[strings.Index(s, "/hardware/")+len("/hardware/"):])
		}
	}
	return name, hardware
}

func unescapeScope(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}
