// Package tlv implements Matter TLV (Matter Core Specification, Appendix A):
// the tag-length-value wire format every Matter interaction rides on. Pure
// Go, zero dependencies — the first brick of the native Matter fallback
// stack (K207 A5: 兜底强制).
//
// Scope (v0): anonymous + context tags (what the interaction model uses),
// common/implicit/fully-qualified tag DECODING, minimal-width integer
// encoding, containers (struct/array/list) with strict end-of-container
// matching. Certificate-form (fully-qualified) tag writing lands with the
// commissioning layer.
package tlv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Element types (control octet bits 4..0).
const (
	tInt8   = 0x00
	tInt16  = 0x01
	tInt32  = 0x02
	tInt64  = 0x03
	tUint8  = 0x04
	tUint16 = 0x05
	tUint32 = 0x06
	tUint64 = 0x07
	tFalse  = 0x08
	tTrue   = 0x09
	tFloat  = 0x0A
	tDouble = 0x0B
	tUTF8L1 = 0x0C
	tUTF8L2 = 0x0D
	tUTF8L4 = 0x0E
	tUTF8L8 = 0x0F
	tBytL1  = 0x10
	tBytL2  = 0x11
	tBytL4  = 0x12
	tBytL8  = 0x13
	tNull   = 0x14
	tStruct = 0x15
	tArray  = 0x16
	tList   = 0x17
	tEnd    = 0x18
)

// Tag controls (control octet bits 7..5).
const (
	tcAnonymous = 0x00
	tcContext   = 0x20
	tcCommon2   = 0x40
	tcCommon4   = 0x60
	tcImplicit2 = 0x80
	tcImplicit4 = 0xA0
	tcFull6     = 0xC0
	tcFull8     = 0xE0
)

// Type is the decoded element kind.
type Type int

const (
	TypeInt Type = iota
	TypeUint
	TypeBool
	TypeFloat
	TypeString
	TypeBytes
	TypeNull
	TypeStruct
	TypeArray
	TypeList
	TypeEnd
)

// Tag identifies an element. Anonymous when IsContext==false && !qualified.
type Tag struct {
	IsContext bool
	Context   uint8 // context tag number
	// Qualified (decoded only, v0): profile-qualified tag material.
	Qualified bool
	VendorID  uint16
	Profile   uint16
	Number    uint32
}

// Anonymous is the anonymous tag.
var Anonymous = Tag{}

// ContextTag returns a context-specific tag.
func ContextTag(n uint8) Tag { return Tag{IsContext: true, Context: n} }

var (
	ErrTruncated  = errors.New("tlv: truncated element")
	ErrBadElement = errors.New("tlv: invalid element")
	ErrContainer  = errors.New("tlv: container nesting mismatch")
)

// ---- Writer ----

// Writer builds a TLV byte string. Zero value is ready to use.
type Writer struct {
	b     []byte
	depth int
}

func NewWriter() *Writer { return &Writer{} }

// Bytes returns the encoding; it errors if containers are unbalanced.
func (w *Writer) Bytes() ([]byte, error) {
	if w.depth != 0 {
		return nil, fmt.Errorf("%w: %d unclosed container(s)", ErrContainer, w.depth)
	}
	return w.b, nil
}

func (w *Writer) control(tag Tag, elemType byte) {
	if tag.IsContext {
		w.b = append(w.b, tcContext|elemType, tag.Context)
		return
	}
	w.b = append(w.b, tcAnonymous|elemType)
}

// PutUint writes an unsigned integer with minimal width.
func (w *Writer) PutUint(tag Tag, v uint64) {
	switch {
	case v <= math.MaxUint8:
		w.control(tag, tUint8)
		w.b = append(w.b, byte(v))
	case v <= math.MaxUint16:
		w.control(tag, tUint16)
		w.b = binary.LittleEndian.AppendUint16(w.b, uint16(v))
	case v <= math.MaxUint32:
		w.control(tag, tUint32)
		w.b = binary.LittleEndian.AppendUint32(w.b, uint32(v))
	default:
		w.control(tag, tUint64)
		w.b = binary.LittleEndian.AppendUint64(w.b, v)
	}
}

// PutInt writes a signed integer with minimal width.
func (w *Writer) PutInt(tag Tag, v int64) {
	switch {
	case v >= math.MinInt8 && v <= math.MaxInt8:
		w.control(tag, tInt8)
		w.b = append(w.b, byte(int8(v)))
	case v >= math.MinInt16 && v <= math.MaxInt16:
		w.control(tag, tInt16)
		w.b = binary.LittleEndian.AppendUint16(w.b, uint16(int16(v)))
	case v >= math.MinInt32 && v <= math.MaxInt32:
		w.control(tag, tInt32)
		w.b = binary.LittleEndian.AppendUint32(w.b, uint32(int32(v)))
	default:
		w.control(tag, tInt64)
		w.b = binary.LittleEndian.AppendUint64(w.b, uint64(v))
	}
}

// PutBool writes a boolean.
func (w *Writer) PutBool(tag Tag, v bool) {
	if v {
		w.control(tag, tTrue)
	} else {
		w.control(tag, tFalse)
	}
}

// PutFloat64 writes a double.
func (w *Writer) PutFloat64(tag Tag, v float64) {
	w.control(tag, tDouble)
	w.b = binary.LittleEndian.AppendUint64(w.b, math.Float64bits(v))
}

// PutString writes a UTF-8 string.
func (w *Writer) PutString(tag Tag, s string) {
	w.putLen(tag, tUTF8L1, uint64(len(s)))
	w.b = append(w.b, s...)
}

// PutBytes writes an octet string.
func (w *Writer) PutBytes(tag Tag, p []byte) {
	w.putLen(tag, tBytL1, uint64(len(p)))
	w.b = append(w.b, p...)
}

func (w *Writer) putLen(tag Tag, base byte, n uint64) {
	switch {
	case n <= math.MaxUint8:
		w.control(tag, base)
		w.b = append(w.b, byte(n))
	case n <= math.MaxUint16:
		w.control(tag, base+1)
		w.b = binary.LittleEndian.AppendUint16(w.b, uint16(n))
	case n <= math.MaxUint32:
		w.control(tag, base+2)
		w.b = binary.LittleEndian.AppendUint32(w.b, uint32(n))
	default:
		w.control(tag, base+3)
		w.b = binary.LittleEndian.AppendUint64(w.b, n)
	}
}

// PutNull writes a null.
func (w *Writer) PutNull(tag Tag) { w.control(tag, tNull) }

// StartStruct / StartArray / StartList open containers; EndContainer closes
// the innermost one.
func (w *Writer) StartStruct(tag Tag) { w.control(tag, tStruct); w.depth++ }
func (w *Writer) StartArray(tag Tag)  { w.control(tag, tArray); w.depth++ }
func (w *Writer) StartList(tag Tag)   { w.control(tag, tList); w.depth++ }

func (w *Writer) EndContainer() error {
	if w.depth == 0 {
		return ErrContainer
	}
	w.b = append(w.b, tEnd)
	w.depth--
	return nil
}

// ---- Reader ----

// Element is one decoded element head plus its value.
type Element struct {
	Tag  Tag
	Type Type
	// Value fields — populated per Type.
	I int64
	U uint64
	F float64
	B bool
	S string
	P []byte
}

// Reader walks a TLV byte string element by element.
type Reader struct {
	b     []byte
	i     int
	depth int
}

func NewReader(b []byte) *Reader { return &Reader{b: b} }

// Next decodes the next element head+value. At an end-of-container it
// returns an Element with TypeEnd (depth bookkeeping included). io semantics:
// (nil, nil) means clean end of input at depth 0.
func (r *Reader) Next() (*Element, error) {
	if r.i >= len(r.b) {
		if r.depth != 0 {
			return nil, fmt.Errorf("%w: input ended inside a container", ErrContainer)
		}
		return nil, nil
	}
	ctl := r.b[r.i]
	r.i++
	elemType := ctl & 0x1F
	tagCtl := ctl & 0xE0

	var tag Tag
	switch tagCtl {
	case tcAnonymous:
	case tcContext:
		n, err := r.take(1)
		if err != nil {
			return nil, err
		}
		tag = ContextTag(n[0])
	case tcCommon2, tcImplicit2:
		p, err := r.take(2)
		if err != nil {
			return nil, err
		}
		tag = Tag{Qualified: true, Number: uint32(binary.LittleEndian.Uint16(p))}
	case tcCommon4, tcImplicit4:
		p, err := r.take(4)
		if err != nil {
			return nil, err
		}
		tag = Tag{Qualified: true, Number: binary.LittleEndian.Uint32(p)}
	case tcFull6:
		p, err := r.take(6)
		if err != nil {
			return nil, err
		}
		tag = Tag{Qualified: true, VendorID: binary.LittleEndian.Uint16(p), Profile: binary.LittleEndian.Uint16(p[2:]), Number: uint32(binary.LittleEndian.Uint16(p[4:]))}
	case tcFull8:
		p, err := r.take(8)
		if err != nil {
			return nil, err
		}
		tag = Tag{Qualified: true, VendorID: binary.LittleEndian.Uint16(p), Profile: binary.LittleEndian.Uint16(p[2:]), Number: binary.LittleEndian.Uint32(p[4:])}
	default:
		return nil, fmt.Errorf("%w: tag control 0x%02x", ErrBadElement, tagCtl)
	}

	e := &Element{Tag: tag}
	switch elemType {
	case tInt8, tInt16, tInt32, tInt64:
		width := 1 << (elemType - tInt8)
		p, err := r.take(width)
		if err != nil {
			return nil, err
		}
		e.Type = TypeInt
		e.I = readInt(p)
	case tUint8, tUint16, tUint32, tUint64:
		width := 1 << (elemType - tUint8)
		p, err := r.take(width)
		if err != nil {
			return nil, err
		}
		e.Type = TypeUint
		e.U = readUint(p)
	case tFalse, tTrue:
		e.Type = TypeBool
		e.B = elemType == tTrue
	case tFloat:
		p, err := r.take(4)
		if err != nil {
			return nil, err
		}
		e.Type = TypeFloat
		e.F = float64(math.Float32frombits(binary.LittleEndian.Uint32(p)))
	case tDouble:
		p, err := r.take(8)
		if err != nil {
			return nil, err
		}
		e.Type = TypeFloat
		e.F = math.Float64frombits(binary.LittleEndian.Uint64(p))
	case tUTF8L1, tUTF8L2, tUTF8L4, tUTF8L8:
		p, err := r.lenPayload(elemType - tUTF8L1)
		if err != nil {
			return nil, err
		}
		e.Type = TypeString
		e.S = string(p)
	case tBytL1, tBytL2, tBytL4, tBytL8:
		p, err := r.lenPayload(elemType - tBytL1)
		if err != nil {
			return nil, err
		}
		e.Type = TypeBytes
		e.P = p
	case tNull:
		e.Type = TypeNull
	case tStruct:
		e.Type = TypeStruct
		r.depth++
	case tArray:
		e.Type = TypeArray
		r.depth++
	case tList:
		e.Type = TypeList
		r.depth++
	case tEnd:
		if r.depth == 0 {
			return nil, fmt.Errorf("%w: end-of-container at top level", ErrContainer)
		}
		r.depth--
		e.Type = TypeEnd
	default:
		return nil, fmt.Errorf("%w: element type 0x%02x", ErrBadElement, elemType)
	}
	return e, nil
}

func (r *Reader) take(n int) ([]byte, error) {
	if r.i+n > len(r.b) {
		return nil, ErrTruncated
	}
	p := r.b[r.i : r.i+n]
	r.i += n
	return p, nil
}

func (r *Reader) lenPayload(widthExp byte) ([]byte, error) {
	lenBytes, err := r.take(1 << widthExp)
	if err != nil {
		return nil, err
	}
	n := readUint(lenBytes)
	if n > uint64(len(r.b)-r.i) {
		return nil, ErrTruncated
	}
	return r.take(int(n))
}

func readUint(p []byte) uint64 {
	var v uint64
	for i := len(p) - 1; i >= 0; i-- {
		v = v<<8 | uint64(p[i])
	}
	return v
}

func readInt(p []byte) int64 {
	u := readUint(p)
	shift := 64 - 8*len(p)
	return int64(u<<shift) >> shift
}

// SkipContainer consumes the children of the container element that Next()
// just returned, up to and including its matching end-of-container. Call it
// after Next() returns a Struct/Array/List you do not want to descend into —
// essential for forward compatibility: a decoder MUST skip unknown container
// fields rather than let their children leak into the parent scope.
func (r *Reader) SkipContainer() error {
	depth := 1
	for depth > 0 {
		e, err := r.Next()
		if err != nil {
			return err
		}
		if e == nil {
			return ErrContainer
		}
		switch e.Type {
		case TypeStruct, TypeArray, TypeList:
			depth++
		case TypeEnd:
			depth--
		}
	}
	return nil
}
