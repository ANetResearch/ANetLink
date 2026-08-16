package disc

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// canned builds a realistic mDNS response like a Matter device would send:
// PTR + SRV + TXT + A across answer/additional sections.
func canned(t *testing.T) []byte {
	t.Helper()
	mk := func(s string) dnsmessage.Name {
		n, err := dnsmessage.NewName(s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	inst := "0123456789ABCDEF._matterc._udp.local."
	host := "lamp.local."
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{Response: true, Authoritative: true},
		Answers: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{Name: mk(ServiceCommissionable), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.PTRResource{PTR: mk(inst)},
			},
		},
		Additionals: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{Name: mk(inst), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.SRVResource{Target: mk(host), Port: 5540},
			},
			{
				Header: dnsmessage.ResourceHeader{Name: mk(inst), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.TXTResource{TXT: []string{"D=3840", "CM=1", "VP=65521+32768", "DN=TestLamp"}},
			},
			{
				Header: dnsmessage.ResourceHeader{Name: mk(host), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.AResource{A: [4]byte{10, 2, 2, 93}},
			},
		},
	}
	b, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAggregatesFullResponse(t *testing.T) {
	a := newAggregator()
	a.ingest(canned(t))
	nodes := a.result()
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(nodes))
	}
	n := nodes[0]
	if n.Discriminator != 3840 || n.Commissioning != 1 {
		t.Fatalf("TXT parse: %+v", n)
	}
	if n.VendorID != 65521 || n.ProductID != 32768 || n.DeviceName != "TestLamp" {
		t.Fatalf("VP/DN parse: %+v", n)
	}
	if n.Port != 5540 || len(n.IPs) != 1 || !n.IPs[0].Equal(net.IPv4(10, 2, 2, 93)) {
		t.Fatalf("SRV/A join: %+v", n)
	}
	if n.Instance != "0123456789ABCDEF" {
		t.Fatalf("instance: %q", n.Instance)
	}
}

func TestIgnoresGarbageAndForeignServices(t *testing.T) {
	a := newAggregator()
	a.ingest([]byte{0xde, 0xad})
	a.ingest(nil)
	// a query (not response) must be ignored
	q, _ := packQuery(ServiceCommissionable)
	a.ingest(q)
	if len(a.result()) != 0 {
		t.Fatal("garbage must not create nodes")
	}
}
