// Package disc implements Matter commissionable-node discovery over
// mDNS/DNS-SD (_matterc._udp.local, Matter Core Spec §4.3): the second
// brick of the native stack. Thin by design — we own the multicast and
// Matter-specific TXT semantics; x/net/dns/dnsmessage supplies the DNS wire
// primitives (pure Go, D46).
package disc

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// ServiceCommissionable is the DNS-SD service for nodes in commissioning mode.
const ServiceCommissionable = "_matterc._udp.local."

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Commissionable is one discovered commissionable Matter node.
type Commissionable struct {
	Instance      string // DNS-SD instance (usually the MAC-derived name)
	Host          string // SRV target
	Port          uint16
	IPs           []net.IP
	Discriminator uint16 // TXT D — the long discriminator
	VendorID      uint16 // TXT VP "vid+pid"
	ProductID     uint16
	Commissioning int               // TXT CM (1/2 = open window)
	DeviceName    string            // TXT DN (optional)
	TXT           map[string]string // raw TXT pairs
}

// Browse queries for commissionable nodes and collects answers until the
// context deadline (callers pass a ctx with timeout). It listens on BOTH an
// ephemeral socket (QU unicast replies) and the mDNS group socket
// (multicast replies — many responders, matter.js included, only ever
// multicast).
func Browse(ctx context.Context) ([]Commissionable, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("disc: %w", err)
	}
	defer conn.Close()

	mc, err := listenMulticast()
	if err != nil {
		mc = nil // degraded: QU-only (still useful); surfaced via result count
	} else {
		defer mc.Close()
	}

	query, err := packQuery(ServiceCommissionable)
	if err != nil {
		return nil, err
	}
	// Ask twice (mDNS is lossy); QU bit set → responders may unicast to us.
	for i := 0; i < 2; i++ {
		if _, err := conn.WriteToUDP(query, mdnsGroup); err != nil {
			return nil, fmt.Errorf("disc: query: %w", err)
		}
		time.Sleep(120 * time.Millisecond)
	}

	agg := newAggregator()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(3 * time.Second)
	}
	pkts := make(chan []byte, 64)
	done := make(chan struct{})
	readLoop := func(c net.PacketConn) {
		buf := make([]byte, 9000)
		for {
			_ = c.SetReadDeadline(deadline)
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			p := make([]byte, n)
			copy(p, buf[:n])
			select {
			case pkts <- p:
			case <-done:
				return
			}
		}
	}
	go readLoop(conn)
	if mc != nil {
		go readLoop(mc)
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case p := <-pkts:
			agg.ingest(p)
		case <-timer.C:
			close(done)
			return agg.result(), nil
		case <-ctx.Done():
			close(done)
			return agg.result(), nil
		}
	}
}

// listenMulticast opens 0.0.0.0:5353 with SO_REUSEADDR/SO_REUSEPORT (mDNS
// daemons like avahi may share the port) and joins 224.0.0.251 on every
// multicast-capable interface.
func listenMulticast() (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			if serr == nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0xf /*SO_REUSEPORT*/, 1)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:5353")
	if err != nil {
		return nil, err
	}
	p := ipv4.NewPacketConn(pc)
	ifaces, _ := net.Interfaces()
	joined := 0
	for i := range ifaces {
		ifc := &ifaces[i]
		if ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		if err := p.JoinGroup(ifc, &net.UDPAddr{IP: mdnsGroup.IP}); err == nil {
			joined++
		}
	}
	if joined == 0 {
		pc.Close()
		return nil, fmt.Errorf("disc: no multicast interface joined")
	}
	return pc, nil
}

func packQuery(service string) ([]byte, error) {
	name, err := dnsmessage.NewName(service)
	if err != nil {
		return nil, err
	}
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{}, // mDNS query: ID 0, QR 0
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypePTR,
			Class: dnsmessage.Class(0x8001), // IN | QU (unicast-response)
		}},
	}
	return msg.Pack()
}

// aggregator folds PTR/SRV/TXT/A/AAAA records (which may arrive across
// several packets) into Commissionable entries.
type aggregator struct {
	instances map[string]*Commissionable // instance FQDN → node
	hostIPs   map[string][]net.IP        // host FQDN → addresses
	hostOf    map[string]string          // instance FQDN → host FQDN
}

func newAggregator() *aggregator {
	return &aggregator{
		instances: map[string]*Commissionable{},
		hostIPs:   map[string][]net.IP{},
		hostOf:    map[string]string{},
	}
}

func (a *aggregator) node(instance string) *Commissionable {
	n, ok := a.instances[instance]
	if !ok {
		n = &Commissionable{Instance: strings.TrimSuffix(instance, "."+ServiceCommissionable), TXT: map[string]string{}}
		a.instances[instance] = n
	}
	return n
}

func (a *aggregator) ingest(pkt []byte) {
	var p dnsmessage.Parser
	h, err := p.Start(pkt)
	if err != nil || !h.Response {
		return
	}
	_ = p.SkipAllQuestions()
	for _, next := range []func() (dnsmessage.ResourceHeader, error){
		func() (dnsmessage.ResourceHeader, error) { return p.AnswerHeader() },
		func() (dnsmessage.ResourceHeader, error) { return p.AuthorityHeader() },
		func() (dnsmessage.ResourceHeader, error) { return p.AdditionalHeader() },
	} {
		for {
			rh, err := next()
			if err != nil {
				break
			}
			a.record(rh, &p)
		}
	}
	// join hosts → instances (deduplicated; answers repeat across packets)
	for inst, host := range a.hostOf {
		ips, ok := a.hostIPs[host]
		if !ok {
			continue
		}
		seen := map[string]bool{}
		var uniq []net.IP
		for _, ip := range ips {
			if k := ip.String(); !seen[k] {
				seen[k] = true
				uniq = append(uniq, ip)
			}
		}
		a.instances[inst].IPs = uniq
	}
}

func (a *aggregator) record(rh dnsmessage.ResourceHeader, p *dnsmessage.Parser) {
	owner := rh.Name.String()
	switch rh.Type {
	case dnsmessage.TypePTR:
		r, err := p.PTRResource()
		if err != nil {
			return
		}
		if strings.EqualFold(owner, ServiceCommissionable) {
			a.node(r.PTR.String())
		}
	case dnsmessage.TypeSRV:
		r, err := p.SRVResource()
		if err != nil {
			return
		}
		if strings.HasSuffix(strings.ToLower(owner), "._matterc._udp.local.") {
			n := a.node(owner)
			n.Port = r.Port
			n.Host = r.Target.String()
			a.hostOf[owner] = r.Target.String()
		}
	case dnsmessage.TypeTXT:
		r, err := p.TXTResource()
		if err != nil {
			return
		}
		if strings.HasSuffix(strings.ToLower(owner), "._matterc._udp.local.") {
			n := a.node(owner)
			for _, kv := range r.TXT {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				n.TXT[k] = v
			}
			applyTXT(n)
		}
	case dnsmessage.TypeA:
		r, err := p.AResource()
		if err != nil {
			return
		}
		a.hostIPs[owner] = append(a.hostIPs[owner], net.IP(r.A[:]))
	case dnsmessage.TypeAAAA:
		r, err := p.AAAAResource()
		if err != nil {
			return
		}
		a.hostIPs[owner] = append(a.hostIPs[owner], net.IP(r.AAAA[:]))
	default:
		_ = p.SkipAnswer()
	}
}

func applyTXT(n *Commissionable) {
	if d, err := strconv.ParseUint(n.TXT["D"], 10, 16); err == nil {
		n.Discriminator = uint16(d)
	}
	if cm, err := strconv.Atoi(n.TXT["CM"]); err == nil {
		n.Commissioning = cm
	}
	if vp := n.TXT["VP"]; vp != "" {
		vid, pid, _ := strings.Cut(vp, "+")
		if v, err := strconv.ParseUint(vid, 10, 16); err == nil {
			n.VendorID = uint16(v)
		}
		if pfound, err := strconv.ParseUint(pid, 10, 16); err == nil {
			n.ProductID = uint16(pfound)
		}
	}
	n.DeviceName = n.TXT["DN"]
}

func (a *aggregator) result() []Commissionable {
	out := make([]Commissionable, 0, len(a.instances))
	for _, n := range a.instances {
		out = append(out, *n)
	}
	return out
}
