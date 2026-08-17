package commission

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetLink/matter/im"
	"github.com/ANetResearch/ANetLink/matter/tlv"
)

// scriptedDevice plays the device side of commissioning: it records the
// commands it receives and answers each with a plausible response.
type scriptedDevice struct {
	seen      []im.CommandPath
	failAt    uint32 // cluster command id to fail (0 = never)
	failCode  uint64
	csr       []byte
	invokeErr error
}

func (d *scriptedDevice) Invoke(_ context.Context, path im.CommandPath, _ []byte) ([]byte, error) {
	d.seen = append(d.seen, path)
	if d.invokeErr != nil {
		return nil, d.invokeErr
	}
	if d.failAt != 0 && path.Command == d.failAt {
		return statusResponse(d.failCode), nil
	}
	if path.Cluster == ClusterOperationalCredentials && path.Command == CmdCSRRequest {
		return csrResponse(d.csr), nil
	}
	return statusResponse(0), nil
}

// statusResponse builds a response whose first context field is a status.
func statusResponse(code uint64) []byte {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutUint(tlv.ContextTag(0), code)
	_ = w.EndContainer()
	b, _ := w.Bytes()
	return b
}

// csrResponse builds a CSRResponse carrying the CSR bytes.
func csrResponse(csr []byte) []byte {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(0), csr)
	_ = w.EndContainer()
	b, _ := w.Bytes()
	return b
}

func testCreds(signed bool) Credentials {
	c := Credentials{
		TrustedRoot:  []byte("root-cert-bytes"),
		IPK:          []byte("0123456789abcdef"),
		AdminSubject: 0x0000000000000001,
		VendorID:     0xFFF1,
	}
	if signed {
		c.SignNOC = func(csr []byte) ([]byte, []byte, error) {
			if len(csr) == 0 {
				return nil, nil, errors.New("empty CSR")
			}
			return append([]byte("noc-for-"), csr...), nil, nil
		}
	}
	return c
}

// TestFullSequence pins the order — commissioning is a sequence, and doing
// it out of order is how devices get bricked.
func TestFullSequence(t *testing.T) {
	dev := &scriptedDevice{csr: []byte("csr-bytes")}
	c := New(dev, testCreds(true))

	res, err := c.Run(context.Background(), []byte("nonce-32-bytes-placeholder......"))
	if err != nil {
		t.Fatalf("commissioning failed: %v", err)
	}
	if strings.Join(res.Completed, ",") != strings.Join(Steps, ",") {
		t.Fatalf("steps out of order or incomplete:\n got  %v\n want %v", res.Completed, Steps)
	}
	// the very first command must be ArmFailSafe
	if len(dev.seen) == 0 || dev.seen[0].Command != CmdArmFailSafe {
		t.Fatalf("failsafe must be armed first, got %+v", dev.seen)
	}
	// the last must be CommissioningComplete
	last := dev.seen[len(dev.seen)-1]
	if last.Cluster != ClusterGeneralCommissioning || last.Command != CmdCommissioningComplete {
		t.Fatalf("sequence must end with CommissioningComplete, got %+v", last)
	}
	// every command addresses endpoint 0 (the root node)
	for _, p := range dev.seen {
		if p.Endpoint != 0 {
			t.Fatalf("commissioning commands live on endpoint 0, got %+v", p)
		}
	}
}

// TestDeviceRejectionAborts: an error code from the device stops the run and
// reports how far it got — the failsafe rolls the rest back.
func TestDeviceRejectionAborts(t *testing.T) {
	dev := &scriptedDevice{csr: []byte("csr"), failAt: CmdSetRegulatoryConfig, failCode: 3}
	c := New(dev, testCreds(true))

	res, err := c.Run(context.Background(), []byte("nonce"))
	if err == nil {
		t.Fatal("a device error code must abort the sequence")
	}
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("abort must be reported as such: %v", err)
	}
	if len(res.Completed) != 1 || res.Completed[0] != "arm-failsafe" {
		t.Fatalf("partial progress must be reported: %v", res.Completed)
	}
	// AddNOC must never have been attempted after the failure
	for _, p := range dev.seen {
		if p.Command == CmdAddNOC {
			t.Fatal("AddNOC ran after an earlier step failed")
		}
	}
}

// TestMissingCredentialsRefused: we refuse to start what we cannot finish.
func TestMissingCredentialsRefused(t *testing.T) {
	dev := &scriptedDevice{csr: []byte("csr")}
	// no signer
	c := New(dev, testCreds(false))
	res, err := c.Run(context.Background(), []byte("nonce"))
	if err == nil || !strings.Contains(err.Error(), "NOC signer") {
		t.Fatalf("a missing NOC signer must be refused: %v", err)
	}
	if len(res.Completed) == 0 {
		t.Fatal("steps before the signer is needed should still be reported")
	}

	// no trusted root
	creds := testCreds(true)
	creds.TrustedRoot = nil
	c = New(&scriptedDevice{csr: []byte("csr")}, creds)
	if _, err := c.Run(context.Background(), []byte("nonce")); err == nil ||
		!strings.Contains(err.Error(), "trusted root") {
		t.Fatalf("a missing trusted root must be refused: %v", err)
	}

	// no IPK
	creds = testCreds(true)
	creds.IPK = nil
	c = New(&scriptedDevice{csr: []byte("csr")}, creds)
	if _, err := c.Run(context.Background(), []byte("nonce")); err == nil ||
		!strings.Contains(err.Error(), "identity protection key") {
		t.Fatalf("a missing IPK must be refused: %v", err)
	}
}

// TestCSRFlowsToSigner proves the device's CSR is what gets signed and the
// resulting NOC is what gets installed.
func TestCSRFlowsToSigner(t *testing.T) {
	dev := &scriptedDevice{csr: []byte("device-csr-42")}
	var signedWith []byte
	creds := testCreds(true)
	creds.SignNOC = func(csr []byte) ([]byte, []byte, error) {
		signedWith = append([]byte(nil), csr...)
		return []byte("noc"), nil, nil
	}
	c := New(dev, creds)
	if _, err := c.Run(context.Background(), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	if string(signedWith) != "device-csr-42" {
		t.Fatalf("the signer must receive the device CSR, got %q", signedWith)
	}
}

// TestTransportFailurePropagates: a broken session is not a silent success.
func TestTransportFailurePropagates(t *testing.T) {
	dev := &scriptedDevice{invokeErr: errors.New("session closed")}
	c := New(dev, testCreds(true))
	if _, err := c.Run(context.Background(), []byte("nonce")); err == nil ||
		!strings.Contains(err.Error(), "session closed") {
		t.Fatalf("transport failure must propagate: %v", err)
	}
}

func TestEncodeReadAndInvoke(t *testing.T) {
	b, err := im.EncodeReadRequest([]im.AttributePath{{Endpoint: 1, Cluster: 0x0006, Attribute: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || b[0] != 0x15 { // anonymous struct
		t.Fatalf("read request must be a TLV struct: %x", b)
	}
	if _, err := im.EncodeReadRequest(nil); err == nil {
		t.Fatal("a read with no paths must be refused")
	}
	inv, err := im.EncodeInvokeRequest(im.CommandPath{Endpoint: 0, Cluster: 0x0030, Command: 0}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) == 0 {
		t.Fatal("invoke request must encode")
	}
}
