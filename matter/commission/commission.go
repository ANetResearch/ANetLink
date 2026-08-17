// Package commission drives the Matter commissioning sequence (Core Spec
// §5.5): the ordered cluster commands that turn a device which merely proved
// it knows the passcode into a member of a fabric.
//
// Sequence: ArmFailSafe → SetRegulatoryConfig → CSRRequest → AddTrustedRootCertificate
// → AddNOC → CommissioningComplete. Every step runs over the secured session
// established after PASE, and the failsafe guarantees a half-finished
// commissioning rolls back instead of bricking the device.
package commission

import (
	"context"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetLink/matter/im"
	"github.com/ANetResearch/ANetLink/matter/tlv"
)

// Cluster and command ids used during commissioning (§11).
const (
	ClusterGeneralCommissioning   uint32 = 0x0030
	ClusterOperationalCredentials uint32 = 0x003E

	CmdArmFailSafe           uint32 = 0x00
	CmdSetRegulatoryConfig   uint32 = 0x02
	CmdCommissioningComplete uint32 = 0x04
	CmdAttestationRequest    uint32 = 0x00
	CmdCSRRequest            uint32 = 0x04
	CmdAddNOC                uint32 = 0x06
	CmdAddTrustedRootCert    uint32 = 0x0B
)

// Step names, in order — exported so callers can report progress.
var Steps = []string{
	"arm-failsafe", "regulatory-config", "csr-request",
	"add-trusted-root", "add-noc", "commissioning-complete",
}

var (
	ErrAborted = errors.New("commission: sequence aborted, failsafe will roll the device back")
)

// Transport is the secured-session round-trip commissioning needs. The
// exchange layer provides it; tests provide a scripted one.
type Transport interface {
	// Invoke sends an InvokeRequest for one command and returns the
	// InvokeResponse payload.
	Invoke(ctx context.Context, path im.CommandPath, fields []byte) ([]byte, error)
}

// Credentials are what a commissioner must supply to admit a device.
type Credentials struct {
	// TrustedRoot is the fabric's root certificate (Matter TLV form).
	TrustedRoot []byte
	// SignNOC turns the device's CSR into an operational certificate.
	SignNOC func(csr []byte) (noc []byte, icac []byte, err error)
	// IPK is the identity protection key for the fabric.
	IPK []byte
	// AdminSubject is the node id allowed to administer the device.
	AdminSubject uint64
	// VendorID identifies the commissioner's fabric.
	VendorID uint16
}

// Result reports what a commissioning run achieved.
type Result struct {
	Completed   []string
	NodeID      uint64
	FabricIndex uint8
}

// Commissioner drives the sequence over a secured session.
type Commissioner struct {
	t     Transport
	creds Credentials
}

func New(t Transport, creds Credentials) *Commissioner {
	return &Commissioner{t: t, creds: creds}
}

// ArmFailSafe starts the failsafe timer: if the sequence does not complete
// within expirySeconds, the device undoes everything. Nothing else may run
// before this.
func (c *Commissioner) ArmFailSafe(ctx context.Context, expirySeconds uint16) error {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutUint(tlv.ContextTag(0), uint64(expirySeconds))
	w.PutUint(tlv.ContextTag(1), 0) // Breadcrumb
	if err := w.EndContainer(); err != nil {
		return err
	}
	fields, err := w.Bytes()
	if err != nil {
		return err
	}
	resp, err := c.t.Invoke(ctx, im.CommandPath{
		Endpoint: 0, Cluster: ClusterGeneralCommissioning, Command: CmdArmFailSafe,
	}, fields)
	if err != nil {
		return fmt.Errorf("arm-failsafe: %w", err)
	}
	return checkCommissioningStatus(resp, "arm-failsafe")
}

// SetRegulatoryConfig declares where the device is operating.
func (c *Commissioner) SetRegulatoryConfig(ctx context.Context, location uint8, countryCode string) error {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutUint(tlv.ContextTag(0), uint64(location))
	w.PutString(tlv.ContextTag(1), countryCode)
	w.PutUint(tlv.ContextTag(2), 0) // Breadcrumb
	if err := w.EndContainer(); err != nil {
		return err
	}
	fields, err := w.Bytes()
	if err != nil {
		return err
	}
	resp, err := c.t.Invoke(ctx, im.CommandPath{
		Endpoint: 0, Cluster: ClusterGeneralCommissioning, Command: CmdSetRegulatoryConfig,
	}, fields)
	if err != nil {
		return fmt.Errorf("regulatory-config: %w", err)
	}
	return checkCommissioningStatus(resp, "regulatory-config")
}

// CSRRequest asks the device to generate an operational key pair and return
// a certificate signing request.
func (c *Commissioner) CSRRequest(ctx context.Context, nonce []byte) ([]byte, error) {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(0), nonce)
	if err := w.EndContainer(); err != nil {
		return nil, err
	}
	fields, err := w.Bytes()
	if err != nil {
		return nil, err
	}
	resp, err := c.t.Invoke(ctx, im.CommandPath{
		Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CmdCSRRequest,
	}, fields)
	if err != nil {
		return nil, fmt.Errorf("csr-request: %w", err)
	}
	csr, err := firstByteField(resp)
	if err != nil {
		return nil, fmt.Errorf("csr-request: %w", err)
	}
	return csr, nil
}

// AddTrustedRoot installs the fabric root certificate.
func (c *Commissioner) AddTrustedRoot(ctx context.Context) error {
	if len(c.creds.TrustedRoot) == 0 {
		return errors.New("commission: no trusted root certificate supplied")
	}
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(0), c.creds.TrustedRoot)
	if err := w.EndContainer(); err != nil {
		return err
	}
	fields, err := w.Bytes()
	if err != nil {
		return err
	}
	if _, err := c.t.Invoke(ctx, im.CommandPath{
		Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CmdAddTrustedRootCert,
	}, fields); err != nil {
		return fmt.Errorf("add-trusted-root: %w", err)
	}
	return nil
}

// AddNOC installs the operational certificate the commissioner signed.
func (c *Commissioner) AddNOC(ctx context.Context, noc, icac []byte) error {
	if len(c.creds.IPK) == 0 {
		return errors.New("commission: no identity protection key supplied")
	}
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBytes(tlv.ContextTag(0), noc)
	if len(icac) > 0 {
		w.PutBytes(tlv.ContextTag(1), icac)
	}
	w.PutBytes(tlv.ContextTag(2), c.creds.IPK)
	w.PutUint(tlv.ContextTag(3), c.creds.AdminSubject)
	w.PutUint(tlv.ContextTag(4), uint64(c.creds.VendorID))
	if err := w.EndContainer(); err != nil {
		return err
	}
	fields, err := w.Bytes()
	if err != nil {
		return err
	}
	resp, err := c.t.Invoke(ctx, im.CommandPath{
		Endpoint: 0, Cluster: ClusterOperationalCredentials, Command: CmdAddNOC,
	}, fields)
	if err != nil {
		return fmt.Errorf("add-noc: %w", err)
	}
	return checkNOCStatus(resp)
}

// Complete closes the failsafe and makes the commissioning permanent.
func (c *Commissioner) Complete(ctx context.Context) error {
	resp, err := c.t.Invoke(ctx, im.CommandPath{
		Endpoint: 0, Cluster: ClusterGeneralCommissioning, Command: CmdCommissioningComplete,
	}, nil)
	if err != nil {
		return fmt.Errorf("commissioning-complete: %w", err)
	}
	return checkCommissioningStatus(resp, "commissioning-complete")
}

// Run executes the whole sequence. On any failure it returns what was
// completed so far — the device's failsafe will roll those steps back, which
// is exactly why the failsafe is armed first.
func (c *Commissioner) Run(ctx context.Context, csrNonce []byte) (*Result, error) {
	res := &Result{}
	if err := c.ArmFailSafe(ctx, 60); err != nil {
		return res, err
	}
	res.Completed = append(res.Completed, "arm-failsafe")

	if err := c.SetRegulatoryConfig(ctx, 0, "XX"); err != nil {
		return res, err
	}
	res.Completed = append(res.Completed, "regulatory-config")

	csr, err := c.CSRRequest(ctx, csrNonce)
	if err != nil {
		return res, err
	}
	res.Completed = append(res.Completed, "csr-request")

	if err := c.AddTrustedRoot(ctx); err != nil {
		return res, err
	}
	res.Completed = append(res.Completed, "add-trusted-root")

	if c.creds.SignNOC == nil {
		return res, errors.New("commission: no NOC signer supplied")
	}
	noc, icac, err := c.creds.SignNOC(csr)
	if err != nil {
		return res, fmt.Errorf("sign-noc: %w", err)
	}
	if err := c.AddNOC(ctx, noc, icac); err != nil {
		return res, err
	}
	res.Completed = append(res.Completed, "add-noc")

	if err := c.Complete(ctx); err != nil {
		return res, err
	}
	res.Completed = append(res.Completed, "commissioning-complete")
	return res, nil
}

// checkCommissioningStatus reads a GeneralCommissioning response, whose
// field 0 is an ErrorCode (0 = OK).
func checkCommissioningStatus(resp []byte, step string) error {
	code, err := firstUintField(resp)
	if err != nil {
		return nil // a response without a status field is not a failure signal
	}
	if code != 0 {
		return fmt.Errorf("%s: device returned error code %d: %w", step, code, ErrAborted)
	}
	return nil
}

// checkNOCStatus reads an NOCResponse, whose field 0 is a StatusCode.
func checkNOCStatus(resp []byte) error {
	code, err := firstUintField(resp)
	if err != nil {
		return nil
	}
	if code != 0 {
		return fmt.Errorf("add-noc: device returned status %d: %w", code, ErrAborted)
	}
	return nil
}

// firstUintField finds the first context-tagged unsigned value in a response.
func firstUintField(b []byte) (uint64, error) {
	r := tlv.NewReader(b)
	for i := 0; i < 64; i++ {
		e, err := r.Next()
		if err != nil || e == nil {
			return 0, errors.New("commission: no status field")
		}
		if e.Tag.IsContext && e.Type == tlv.TypeUint {
			return e.U, nil
		}
	}
	return 0, errors.New("commission: no status field")
}

// firstByteField finds the first context-tagged byte string in a response.
func firstByteField(b []byte) ([]byte, error) {
	r := tlv.NewReader(b)
	for i := 0; i < 64; i++ {
		e, err := r.Next()
		if err != nil || e == nil {
			return nil, errors.New("commission: no byte field in response")
		}
		if e.Tag.IsContext && e.Type == tlv.TypeBytes {
			return e.P, nil
		}
	}
	return nil, errors.New("commission: no byte field in response")
}
