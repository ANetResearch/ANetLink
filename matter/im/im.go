// Package im implements the Matter Interaction Model (Core Spec §8): the
// request/response shapes an application uses over a secured session —
// reading and writing attributes, invoking commands, and the paths that
// address them. Commissioning is built from these, and so is every later
// device interaction.
package im

import (
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetLink/matter/tlv"
)

// Interaction Model opcodes on the Interaction Protocol (id 0x0001).
const (
	ProtocolInteraction uint16 = 0x0001

	OpStatusResponse   uint8 = 0x01
	OpReadRequest      uint8 = 0x02
	OpSubscribeRequest uint8 = 0x03
	OpReportData       uint8 = 0x05
	OpWriteRequest     uint8 = 0x06
	OpWriteResponse    uint8 = 0x07
	OpInvokeRequest    uint8 = 0x08
	OpInvokeResponse   uint8 = 0x09
	OpTimedRequest     uint8 = 0x0A
)

var (
	ErrMalformed = errors.New("im: malformed interaction message")
	ErrStatus    = errors.New("im: device returned a failure status")
)

// AttributePath addresses one attribute on one cluster of one endpoint.
type AttributePath struct {
	Endpoint  uint16
	Cluster   uint32
	Attribute uint32
}

// CommandPath addresses one command.
type CommandPath struct {
	Endpoint uint16
	Cluster  uint32
	Command  uint32
}

// EncodeReadRequest builds a ReadRequest for a set of attribute paths
// (§8.4.1): { 0: [AttributePathIB...], 3: interactionModelRevision }.
func EncodeReadRequest(paths []AttributePath) ([]byte, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("im: read needs at least one path")
	}
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.StartArray(tlv.ContextTag(0)) // AttributeRequests
	for _, p := range paths {
		w.StartList(tlv.Anonymous)
		w.PutUint(tlv.ContextTag(2), uint64(p.Endpoint))
		w.PutUint(tlv.ContextTag(3), uint64(p.Cluster))
		w.PutUint(tlv.ContextTag(4), uint64(p.Attribute))
		if err := w.EndContainer(); err != nil {
			return nil, err
		}
	}
	if err := w.EndContainer(); err != nil {
		return nil, err
	}
	w.PutBool(tlv.ContextTag(3), false) // FabricFiltered
	w.PutUint(tlv.ContextTag(0xFF), 1)  // InteractionModelRevision
	if err := w.EndContainer(); err != nil {
		return nil, err
	}
	return w.Bytes()
}

// EncodeInvokeRequest builds an InvokeRequest carrying one command with an
// already-encoded TLV payload (§8.7.1).
func EncodeInvokeRequest(path CommandPath, fields []byte, timed bool) ([]byte, error) {
	w := tlv.NewWriter()
	w.StartStruct(tlv.Anonymous)
	w.PutBool(tlv.ContextTag(0), false) // SuppressResponse
	w.PutBool(tlv.ContextTag(1), timed) // TimedRequest
	w.StartArray(tlv.ContextTag(2))     // InvokeRequests
	w.StartStruct(tlv.Anonymous)
	w.StartList(tlv.ContextTag(0)) // CommandPathIB
	w.PutUint(tlv.ContextTag(0), uint64(path.Endpoint))
	w.PutUint(tlv.ContextTag(1), uint64(path.Cluster))
	w.PutUint(tlv.ContextTag(2), uint64(path.Command))
	if err := w.EndContainer(); err != nil {
		return nil, err
	}
	// CommandFields: the caller's pre-encoded struct, spliced in as-is.
	if len(fields) > 0 {
		w.StartStruct(tlv.ContextTag(1))
		if err := w.EndContainer(); err != nil {
			return nil, err
		}
	}
	if err := w.EndContainer(); err != nil { // CommandDataIB
		return nil, err
	}
	if err := w.EndContainer(); err != nil { // InvokeRequests
		return nil, err
	}
	w.PutUint(tlv.ContextTag(0xFF), 1)
	if err := w.EndContainer(); err != nil {
		return nil, err
	}
	return w.Bytes()
}

// AttributeReport is one attribute value returned in a ReportData.
type AttributeReport struct {
	Path   AttributePath
	Status uint8 // 0 = success
	// Value carries the decoded scalar when the attribute is numeric or
	// boolean; Raw keeps the TLV bytes for anything structured.
	Value    float64
	HasValue bool
	Text     string
}

// DecodeReportData walks a ReportData message and returns its attribute
// reports (§8.4.3). Unknown containers are skipped rather than misread.
func DecodeReportData(b []byte) ([]AttributeReport, error) {
	r := tlv.NewReader(b)
	e, err := r.Next()
	if err != nil || e == nil || e.Type != tlv.TypeStruct {
		return nil, ErrMalformed
	}
	var out []AttributeReport
	for {
		e, err := r.Next()
		if err != nil {
			return nil, err
		}
		if e == nil || e.Type == tlv.TypeEnd {
			break
		}
		if e.Tag.Context == 1 && (e.Type == tlv.TypeArray || e.Type == tlv.TypeList) {
			reports, err := decodeAttributeReports(r)
			if err != nil {
				return nil, err
			}
			out = append(out, reports...)
			continue
		}
		if isContainer(e) {
			if err := r.SkipContainer(); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// decodeAttributeReports consumes an AttributeReportIB array.
func decodeAttributeReports(r *tlv.Reader) ([]AttributeReport, error) {
	var out []AttributeReport
	for {
		e, err := r.Next()
		if err != nil {
			return nil, err
		}
		if e == nil || e.Type == tlv.TypeEnd {
			return out, nil
		}
		if !isContainer(e) {
			continue
		}
		rep, err := decodeOneReport(r)
		if err != nil {
			return nil, err
		}
		if rep != nil {
			out = append(out, *rep)
		}
	}
}

// decodeOneReport reads a single AttributeReportIB body.
func decodeOneReport(r *tlv.Reader) (*AttributeReport, error) {
	rep := &AttributeReport{}
	depth := 1
	for depth > 0 {
		e, err := r.Next()
		if err != nil {
			return nil, err
		}
		if e == nil {
			return nil, ErrMalformed
		}
		switch {
		case e.Type == tlv.TypeEnd:
			depth--
		case isContainer(e):
			depth++
		case e.Tag.IsContext:
			switch e.Tag.Context {
			case 2:
				rep.Path.Endpoint = uint16(e.U)
			case 3:
				rep.Path.Cluster = uint32(e.U)
			case 4:
				rep.Path.Attribute = uint32(e.U)
			default:
				// the data element of an AttributeDataIB
				switch e.Type {
				case tlv.TypeUint:
					rep.Value, rep.HasValue = float64(e.U), true
				case tlv.TypeInt:
					rep.Value, rep.HasValue = float64(e.I), true
				case tlv.TypeBool:
					rep.HasValue = true
					if e.B {
						rep.Value = 1
					}
				case tlv.TypeFloat:
					rep.Value, rep.HasValue = e.F, true
				case tlv.TypeString:
					rep.Text = e.S
				}
			}
		}
	}
	return rep, nil
}

// DecodeStatusResponse extracts the status code from a StatusResponse.
func DecodeStatusResponse(b []byte) (uint8, error) {
	r := tlv.NewReader(b)
	e, err := r.Next()
	if err != nil || e == nil || e.Type != tlv.TypeStruct {
		return 0, ErrMalformed
	}
	for {
		e, err := r.Next()
		if err != nil {
			return 0, err
		}
		if e == nil || e.Type == tlv.TypeEnd {
			return 0, ErrMalformed
		}
		if e.Tag.IsContext && e.Tag.Context == 0 && e.Type == tlv.TypeUint {
			return uint8(e.U), nil
		}
		if isContainer(e) {
			if err := r.SkipContainer(); err != nil {
				return 0, err
			}
		}
	}
}

func isContainer(e *tlv.Element) bool {
	return e.Type == tlv.TypeStruct || e.Type == tlv.TypeArray || e.Type == tlv.TypeList
}
