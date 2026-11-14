package dicom

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// writeTagBytes appends the little-endian encoded tag to buf.
func writeTagBytes(buf *bytes.Buffer, tg tag.Tag) {
	_ = binary.Write(buf, binary.LittleEndian, tg.Group)
	_ = binary.Write(buf, binary.LittleEndian, tg.Element)
}

// writeExplicitSequenceHeader appends an explicit VR sequence element header
// (tag, VR=SQ, reserved bytes, VL) to buf.
func writeExplicitSequenceHeader(buf *bytes.Buffer, tg tag.Tag, vl uint32) {
	writeTagBytes(buf, tg)
	buf.WriteString("SQ")
	buf.Write([]byte{0x00, 0x00}) // reserved bytes
	_ = binary.Write(buf, binary.LittleEndian, vl)
}

// writeItemHeader appends an item header (always implicit) to buf.
func writeItemHeader(buf *bytes.Buffer, vl uint32) {
	writeTagBytes(buf, tag.Item)
	_ = binary.Write(buf, binary.LittleEndian, vl)
}

// writeDelimitation appends an item or sequence delimitation tag with VL=0.
func writeDelimitation(buf *bytes.Buffer, tg tag.Tag) {
	writeTagBytes(buf, tg)
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
}

// writeExplicitStringElement appends an explicit VR string element with a
// 2-byte VL to buf.
func writeExplicitStringElement(buf *bytes.Buffer, tg tag.Tag, vr, value string) {
	writeTagBytes(buf, tg)
	buf.WriteString(vr)
	_ = binary.Write(buf, binary.LittleEndian, uint16(len(value)))
	buf.WriteString(value)
}

// buildNestedExplicitSequences returns bytes for depth levels of nested
// undefined-length sequences, each containing a single undefined-length item,
// with a PatientName element at the innermost level. If closeDelimiters is
// true, all item and sequence delimitation items are written; otherwise the
// bytes are truncated before any delimitation item.
func buildNestedExplicitSequences(depth int, closeDelimiters bool) []byte {
	buf := &bytes.Buffer{}
	for i := 0; i < depth; i++ {
		writeExplicitSequenceHeader(buf, tag.ReferencedImageSequence, tag.VLUndefinedLength)
		writeItemHeader(buf, tag.VLUndefinedLength)
	}
	writeExplicitStringElement(buf, tag.PatientName, "PN", "Bob ")
	if closeDelimiters {
		for i := 0; i < depth; i++ {
			writeDelimitation(buf, tag.ItemDelimitationItem)
			writeDelimitation(buf, tag.SequenceDelimitationItem)
		}
	}
	return buf.Bytes()
}

func newTestReader(data []byte, opts parseOptSet) *reader {
	return &reader{
		rawReader: dicomio.NewReader(bufio.NewReader(bytes.NewBuffer(data)), binary.LittleEndian, int64(len(data))),
		opts:      opts,
	}
}

// findPatientName walks a nested sequence element down to the innermost
// PatientName string value.
func findPatientName(t *testing.T, elem *Element, depth int) string {
	t.Helper()
	current := elem
	for i := 0; i < depth; i++ {
		if current.Value.ValueType() != Sequences {
			t.Fatalf("level %d: expected Sequences value, got %v", i, current.Value.ValueType())
		}
		items := current.Value.GetValue().([]*SequenceItemValue)
		if len(items) != 1 {
			t.Fatalf("level %d: expected 1 sequence item, got %d", i, len(items))
		}
		elems := items[0].GetValue().([]*Element)
		if len(elems) != 1 {
			t.Fatalf("level %d: expected 1 element in item, got %d", i, len(elems))
		}
		current = elems[0]
	}
	if current.Tag != tag.PatientName {
		t.Fatalf("expected innermost element to be PatientName, got %v", current.Tag)
	}
	return MustGetStrings(current.Value)[0]
}

func TestReadSequence_MaxNestedDepth(t *testing.T) {
	t.Run("within budget parses fully", func(t *testing.T) {
		depth := 8
		data := buildNestedExplicitSequences(depth, true)
		// Each level contributes two nesting levels (sequence + item).
		r := newTestReader(data, parseOptSet{maxNestedDepth: 2 * depth})
		elem, err := r.readElement(nil, nil)
		if err != nil {
			t.Fatalf("readElement returned unexpected error: %v", err)
		}
		if got := findPatientName(t, elem, depth); got != "Bob" {
			t.Errorf("unexpected innermost PatientName: got %q, want %q", got, "Bob")
		}
		if r.nestedDepth != 0 {
			t.Errorf("expected nestedDepth to return to 0 after parsing, got %d", r.nestedDepth)
		}
	})

	t.Run("budget exceeded", func(t *testing.T) {
		depth := 8
		data := buildNestedExplicitSequences(depth, true)
		r := newTestReader(data, parseOptSet{maxNestedDepth: 4})
		_, err := r.readElement(nil, nil)
		if !errors.Is(err, ErrorExceedsMaxNestedDepth) {
			t.Fatalf("expected ErrorExceedsMaxNestedDepth, got: %v", err)
		}
	})

	t.Run("very deep input fails with bounded resources", func(t *testing.T) {
		// 10000 levels of nesting would recurse 20000 frames deep without a
		// budget; with a budget the parser must fail quickly instead.
		data := buildNestedExplicitSequences(10000, true)
		r := newTestReader(data, parseOptSet{maxNestedDepth: 16})
		_, err := r.readElement(nil, nil)
		if !errors.Is(err, ErrorExceedsMaxNestedDepth) {
			t.Fatalf("expected ErrorExceedsMaxNestedDepth, got: %v", err)
		}
	})

	t.Run("no budget by default", func(t *testing.T) {
		depth := 64
		data := buildNestedExplicitSequences(depth, true)
		r := newTestReader(data, parseOptSet{})
		elem, err := r.readElement(nil, nil)
		if err != nil {
			t.Fatalf("readElement returned unexpected error: %v", err)
		}
		if got := findPatientName(t, elem, depth); got != "Bob" {
			t.Errorf("unexpected innermost PatientName: got %q, want %q", got, "Bob")
		}
	})
}

func TestReadSequence_MissingDelimitation(t *testing.T) {
	cases := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{
			name: "sequence delimitation missing entirely",
			data: func() []byte {
				buf := &bytes.Buffer{}
				writeExplicitSequenceHeader(buf, tag.ReferencedImageSequence, tag.VLUndefinedLength)
				return buf.Bytes()
			}(),
			wantErr: ErrorExpectedSequenceDelimitation,
		},
		{
			name: "item delimitation missing",
			data: func() []byte {
				buf := &bytes.Buffer{}
				writeExplicitSequenceHeader(buf, tag.ReferencedImageSequence, tag.VLUndefinedLength)
				writeItemHeader(buf, tag.VLUndefinedLength)
				writeExplicitStringElement(buf, tag.PatientName, "PN", "Bob ")
				return buf.Bytes()
			}(),
			wantErr: ErrorExpectedItemDelimitation,
		},
		{
			name: "sequence delimitation missing after item closes",
			data: func() []byte {
				buf := &bytes.Buffer{}
				writeExplicitSequenceHeader(buf, tag.ReferencedImageSequence, tag.VLUndefinedLength)
				writeItemHeader(buf, tag.VLUndefinedLength)
				writeExplicitStringElement(buf, tag.PatientName, "PN", "Bob ")
				writeDelimitation(buf, tag.ItemDelimitationItem)
				return buf.Bytes()
			}(),
			wantErr: ErrorExpectedSequenceDelimitation,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestReader(tc.data, parseOptSet{})
			_, err := r.readElement(nil, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected error %v, got: %v", tc.wantErr, err)
			}
		})
	}
}

// TestReadSequence_PopsLimitOnError ensures that when parsing a defined-length
// sequence fails, the length limits pushed on the underlying reader are
// released, so parsing can continue at the correct position afterwards.
func TestReadSequence_PopsLimitOnError(t *testing.T) {
	// An item of defined length 8 containing only an element header whose
	// declared VL (100) extends past the item's boundary.
	malformedItem := &bytes.Buffer{}
	writeItemHeader(malformedItem, 8)
	writeTagBytes(malformedItem, tag.PatientName)
	malformedItem.WriteString("PN")
	_ = binary.Write(malformedItem, binary.LittleEndian, uint16(100))

	data := &bytes.Buffer{}
	writeExplicitSequenceHeader(data, tag.ReferencedImageSequence, uint32(malformedItem.Len()))
	data.Write(malformedItem.Bytes())
	// A valid element directly after the sequence's defined length.
	writeExplicitStringElement(data, tag.PatientName, "PN", "Bob ")

	r := newTestReader(data.Bytes(), parseOptSet{})
	if _, err := r.readElement(nil, nil); err == nil {
		t.Fatalf("expected error reading malformed sequence, got nil")
	}

	// The limits pushed for the sequence and item must have been released,
	// so the trailing element is readable at the expected position.
	elem, err := r.readElement(nil, nil)
	if err != nil {
		t.Fatalf("readElement after failed sequence returned unexpected error: %v", err)
	}
	if elem.Tag != tag.PatientName {
		t.Fatalf("expected to read trailing PatientName element, got %v", elem.Tag)
	}
	if got := MustGetStrings(elem.Value)[0]; got != "Bob" {
		t.Errorf("unexpected trailing element value: got %q, want %q", got, "Bob")
	}
}
