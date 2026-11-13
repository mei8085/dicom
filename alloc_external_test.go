package dicom_test

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// explicitElemBytes encodes an explicit VR little endian element with a
// defined or undefined length; VRs with a 12 byte header are detected from the
// VR string.
func explicitElemBytes(t tag.Tag, vr string, vl uint32, value []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, t.Group)
	_ = binary.Write(&b, binary.LittleEndian, t.Element)
	b.WriteString(vr)
	switch vr {
	case "OB", "OW", "SQ", "UN", "UT", "UC", "UR", "OF", "OD", "OL":
		_ = binary.Write(&b, binary.LittleEndian, uint16(0))
		_ = binary.Write(&b, binary.LittleEndian, vl)
	default:
		_ = binary.Write(&b, binary.LittleEndian, uint16(vl))
	}
	b.Write(value)
	return b.Bytes()
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

// TestFuzzedInputAllocIsBounded asserts that parsing a small malformed input
// cannot allocate memory far beyond its size. Each case encodes a declared
// length that a tiny file cannot hold; before budgets, these were allocation
// primitives (#349).
func TestFuzzedInputAllocIsBounded(t *testing.T) {
	header := func() []byte {
		p := make([]byte, 128)
		p = append(p, []byte("DICM")...)
		// (0002,0000) UL group length 0, then a transfer syntax element is not
		// needed: the parser infer the transfer syntax from the next element.
		p = append(p, explicitElemBytes(tag.FileMetaInformationGroupLength, "UL", 4, []byte{0, 0, 0, 0})...)
		return p
	}

	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "OB declaring 4GiB",
			body: explicitElemBytes(tag.Tag{Group: 0x0009, Element: 0x0001}, "OB", 0xFFFFFFF0, nil),
		},
		{
			name: "OW declaring 4GiB",
			body: explicitElemBytes(tag.Tag{Group: 0x0009, Element: 0x0002}, "OW", 0xFFFFFFF0, nil),
		},
		{
			name: "LT declaring 4GiB",
			body: explicitElemBytes(tag.Tag{Group: 0x0009, Element: 0x0003}, "LT", 0xFFFFFFF0, nil),
		},
		{
			name: "SQ declaring 4GiB",
			body: explicitElemBytes(tag.Tag{Group: 0x0008, Element: 0x1111}, "SQ", 0xFFFFFFF0, nil),
		},
		{
			name: "UN undefined length with no items",
			body: explicitElemBytes(tag.Tag{Group: 0x0009, Element: 0x0004}, "UN", tag.VLUndefinedLength, nil),
		},
		{
			name: "native PixelData with 4GiB length",
			body: func() []byte {
				var b bytes.Buffer
				b.Write(explicitElemBytes(tag.Rows, "US", 2, u16(1)))
				b.Write(explicitElemBytes(tag.Columns, "US", 2, u16(1)))
				b.Write(explicitElemBytes(tag.BitsAllocated, "US", 2, u16(16)))
				b.Write(explicitElemBytes(tag.SamplesPerPixel, "US", 2, u16(1)))
				b.Write(explicitElemBytes(tag.PixelData, "OW", 0xFFFFFFF0, nil))
				return b.Bytes()
			}(),
		},
		{
			name: "native PixelData with huge frame count",
			body: func() []byte {
				var b bytes.Buffer
				b.Write(explicitElemBytes(tag.Rows, "US", 2, u16(1)))
				b.Write(explicitElemBytes(tag.Columns, "US", 2, u16(1)))
				b.Write(explicitElemBytes(tag.NumberOfFrames, "IS", 16, []byte("1073741824\x00\x00")))
				b.Write(explicitElemBytes(tag.BitsAllocated, "US", 2, u16(8)))
				b.Write(explicitElemBytes(tag.SamplesPerPixel, "US", 2, u16(1)))
				b.Write(explicitElemBytes(tag.PixelData, "OW", 8, make([]byte, 8)))
				return b.Bytes()
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := append(header(), tc.body...)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := dicom.Parse(bytes.NewReader(data), int64(len(data)), nil)
			runtime.ReadMemStats(&after)

			allocated := after.TotalAlloc - before.TotalAlloc
			// A parse of an n byte input may allocate modestly more than n
			// (buffers, slices, values), but not gigabytes.
			limit := uint64(64<<20) + 64*uint64(len(data))
			if allocated > limit {
				t.Fatalf("Parse of a %d byte malformed input allocated %d bytes (err=%v), want at most %d",
					len(data), allocated, err, limit)
			}
		})
	}
}
