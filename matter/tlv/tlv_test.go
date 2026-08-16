package tlv

import (
	"bytes"
	"testing"
)

// Golden vectors from Matter Core Specification, Appendix A.11 (worked
// examples). These pin the wire bytes; two implementations that agree here
// interoperate at the TLV layer.
func TestSpecVectors(t *testing.T) {
	cases := []struct {
		name  string
		build func(w *Writer)
		want  []byte
	}{
		{"anonymous u8 42", func(w *Writer) { w.PutUint(Anonymous, 42) }, []byte{0x04, 0x2A}},
		{"anonymous u16 420", func(w *Writer) { w.PutUint(Anonymous, 420) }, []byte{0x05, 0xA4, 0x01}},
		{"anonymous i8 -17", func(w *Writer) { w.PutInt(Anonymous, -17) }, []byte{0x00, 0xEF}},
		{"anonymous bool false", func(w *Writer) { w.PutBool(Anonymous, false) }, []byte{0x08}},
		{"anonymous bool true", func(w *Writer) { w.PutBool(Anonymous, true) }, []byte{0x09}},
		{"anonymous null", func(w *Writer) { w.PutNull(Anonymous) }, []byte{0x14}},
		{"context 1 u8 42", func(w *Writer) { w.PutUint(ContextTag(1), 42) }, []byte{0x24, 0x01, 0x2A}},
		{"anonymous 'Hello!'", func(w *Writer) { w.PutString(Anonymous, "Hello!") },
			[]byte{0x0C, 0x06, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x21}},
		{"anonymous bytes 00 01 02 03 04", func(w *Writer) { w.PutBytes(Anonymous, []byte{0, 1, 2, 3, 4}) },
			[]byte{0x10, 0x05, 0x00, 0x01, 0x02, 0x03, 0x04}},
		{"empty struct", func(w *Writer) {
			w.StartStruct(Anonymous)
			_ = w.EndContainer()
		}, []byte{0x15, 0x18}},
		{"struct {0:42, 1:-17}", func(w *Writer) {
			w.StartStruct(Anonymous)
			w.PutInt(ContextTag(0), 42)
			w.PutInt(ContextTag(1), -17)
			_ = w.EndContainer()
		}, []byte{0x15, 0x20, 0x00, 0x2A, 0x20, 0x01, 0xEF, 0x18}},
		{"array [0,1,2,3,4] as i8", func(w *Writer) {
			w.StartArray(Anonymous)
			for i := int64(0); i < 5; i++ {
				w.PutInt(Anonymous, i)
			}
			_ = w.EndContainer()
		}, []byte{0x16, 0x00, 0x00, 0x00, 0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04, 0x18}},
	}
	for _, c := range cases {
		w := NewWriter()
		c.build(w)
		got, err := w.Bytes()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s:\n got  %x\n want %x", c.name, got, c.want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	w := NewWriter()
	w.StartStruct(Anonymous)
	w.PutUint(ContextTag(0), 0xDEADBEEF)
	w.PutInt(ContextTag(1), -70000)
	w.PutString(ContextTag(2), "matter.js 灯，等着被配对")
	w.PutBytes(ContextTag(3), []byte{0xCA, 0xFE})
	w.PutBool(ContextTag(4), true)
	w.PutNull(ContextTag(5))
	w.PutFloat64(ContextTag(6), 17.9)
	w.StartArray(ContextTag(7))
	w.PutUint(Anonymous, 1)
	w.PutUint(Anonymous, 2)
	_ = w.EndContainer()
	_ = w.EndContainer()
	b, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	r := NewReader(b)
	seq := []struct {
		typ Type
		chk func(e *Element) bool
	}{
		{TypeStruct, nil},
		{TypeUint, func(e *Element) bool { return e.U == 0xDEADBEEF && e.Tag == ContextTag(0) }},
		{TypeInt, func(e *Element) bool { return e.I == -70000 }},
		{TypeString, func(e *Element) bool { return e.S == "matter.js 灯，等着被配对" }},
		{TypeBytes, func(e *Element) bool { return bytes.Equal(e.P, []byte{0xCA, 0xFE}) }},
		{TypeBool, func(e *Element) bool { return e.B }},
		{TypeNull, nil},
		{TypeFloat, func(e *Element) bool { return e.F > 17.89 && e.F < 17.91 }},
		{TypeArray, nil},
		{TypeUint, func(e *Element) bool { return e.U == 1 }},
		{TypeUint, func(e *Element) bool { return e.U == 2 }},
		{TypeEnd, nil},
		{TypeEnd, nil},
	}
	for i, s := range seq {
		e, err := r.Next()
		if err != nil || e == nil {
			t.Fatalf("elem %d: %v %v", i, e, err)
		}
		if e.Type != s.typ {
			t.Fatalf("elem %d: type %v want %v", i, e.Type, s.typ)
		}
		if s.chk != nil && !s.chk(e) {
			t.Fatalf("elem %d: value check failed: %+v", i, e)
		}
	}
	if e, err := r.Next(); e != nil || err != nil {
		t.Fatalf("expected clean EOF, got %v %v", e, err)
	}
}

func TestErrors(t *testing.T) {
	if _, err := NewReader([]byte{0x04}).Next(); err == nil {
		t.Fatal("truncated u8 must error")
	}
	if _, err := NewReader([]byte{0x18}).Next(); err == nil {
		t.Fatal("top-level end-of-container must error")
	}
	r := NewReader([]byte{0x15, 0x04, 0x2A}) // struct never closed
	_, _ = r.Next()
	_, _ = r.Next()
	if _, err := r.Next(); err == nil {
		t.Fatal("EOF inside container must error")
	}
	w := NewWriter()
	w.StartStruct(Anonymous)
	if _, err := w.Bytes(); err == nil {
		t.Fatal("unclosed writer container must error")
	}
	if err := NewWriter().EndContainer(); err == nil {
		t.Fatal("EndContainer at depth 0 must error")
	}
}

// FuzzReader: no input may panic or loop; decode errors are fine.
func FuzzReader(f *testing.F) {
	f.Add([]byte{0x15, 0x20, 0x00, 0x2A, 0x18})
	f.Add([]byte{0x0C, 0x06, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x21})
	f.Fuzz(func(t *testing.T, b []byte) {
		r := NewReader(b)
		for i := 0; i < 10000; i++ {
			e, err := r.Next()
			if err != nil || e == nil {
				return
			}
		}
	})
}
