package dicom

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// budget_test.go exercises ParseBudget. Inputs are built byte by byte so that
// malformed lengths and nesting are represented exactly as they appear on the
// wire: encoding them through Write would "fix" them.

// explicitElem encodes an explicit VR little endian element with a defined
// length. long reports whether the VR uses the 12 byte (reserved length)
// header.
func explicitElem(t tag.Tag, vr string, vl uint32, value []byte, long bool) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, t.Group)
	_ = binary.Write(&b, binary.LittleEndian, t.Element)
	b.WriteString(vr)
	if long {
		_ = binary.Write(&b, binary.LittleEndian, uint16(0))
		_ = binary.Write(&b, binary.LittleEndian, vl)
	} else {
		_ = binary.Write(&b, binary.LittleEndian, uint16(vl))
	}
	b.Write(value)
	return b.Bytes()
}

// us, ul and cs encode the values used to build file meta information.
func us(v uint16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, v); return b }
func ul(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
func cs(s string) []byte { return []byte(s) }
func is(s string) []byte { return []byte(s) }

// fileMeta builds a valid group 2 header ending with the given transfer
// syntax, so tests can append arbitrary dataset bytes to it.
func fileMeta(transferSyntaxUID string) []byte {
	var meta bytes.Buffer
	// Group length is computed after the elements are known, so build the
	// elements first.
	var elems bytes.Buffer
	elems.Write(explicitElem(tag.MediaStorageSOPClassUID, "UI", 26, cs("1.2.840.10008.5.1.4.1.1.2\x00"), false))
	elems.Write(explicitElem(tag.MediaStorageSOPInstanceUID, "UI", 8, cs("1.2.3.4\x00"), false))
	elems.Write(explicitElem(tag.TransferSyntaxUID, "UI", uint32(len(transferSyntaxUID)), cs(transferSyntaxUID), false))
	elems.Write(explicitElem(tag.ImplementationClassUID, "UI", 6, cs("1.2.3\x00"), false))
	meta.Write(explicitElem(tag.FileMetaInformationGroupLength, "UL", 4, ul(uint32(elems.Len())), false))
	meta.Write(elems.Bytes())

	preamble := make([]byte, 128)
	out := append(preamble, []byte(magicWord)...)
	return append(out, meta.Bytes()...)
}

// dicomFile wraps dataset bytes in a valid file meta header using the given
// transfer syntax.
func dicomFile(transferSyntaxUID string, dataset []byte) []byte {
	return append(fileMeta(transferSyntaxUID), dataset...)
}

// explicitFile builds a file with the given dataset bytes in explicit VR
// little endian.
func explicitFile(dataset []byte) []byte {
	return dicomFile(uid.ExplicitVRLittleEndian, dataset)
}

// implicitElem encodes an element whose header is always implicit: the tag
// followed by a uint32 length, with no VR. Sequence items (fffe,e000) are
// always encoded this way, even in an explicit VR file.
func implicitElem(t tag.Tag, vl uint32, value []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, t.Group)
	_ = binary.Write(&b, binary.LittleEndian, t.Element)
	_ = binary.Write(&b, binary.LittleEndian, vl)
	b.Write(value)
	return b.Bytes()
}

// undefinedSequence builds a sequence element of undefined length whose items
// nest the given number of levels. Each level is a sequence containing one
// item; the innermost item has no contents.
func undefinedSequence(depth int) []byte {
	seq := explicitElem(tag.Tag{Group: 0x0008, Element: 0x1111}, "SQ", tag.VLUndefinedLength, nil, true)
	var contents bytes.Buffer
	if depth > 1 {
		contents.Write(undefinedSequence(depth - 1))
	}
	contents.Write(itemDelimitation())
	contents.Write(sequenceDelimitation())
	return append(seq, implicitElem(tag.Item, tag.VLUndefinedLength, contents.Bytes())...)
}

// itemDelimitation encodes the item delimitation item (fffe,e00d) and its
// zero length.
func itemDelimitation() []byte {
	return implicitElem(tag.ItemDelimitationItem, 0, nil)
}

// bulkItem encodes an item tag with the given declared length followed by up
// to hold bytes of contents.
func bulkItem(vl uint32, hold int) []byte {
	if hold > int(vl) {
		hold = int(vl)
	}
	return implicitElem(tag.Item, vl, make([]byte, hold))
}

// sequenceDelimitation encodes the sequence delimitation item (fffe,e0dd).
func sequenceDelimitation() []byte {
	return implicitElem(tag.SequenceDelimitationItem, 0, nil)
}

func TestParseBudgetDefaultAllowsTestdata(t *testing.T) {
	// The default budget must not make ordinary files fail.
	files, err := os.ReadDir("./testdata")
	if err != nil {
		t.Fatalf("unable to read testdata/: %v", err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".dcm") {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			if _, err := ParseFile("./testdata/"+f.Name(), nil); err != nil {
				t.Errorf("ParseFile(%s) with the default budget returned unexpected error: %v", f.Name(), err)
			}
		})
	}
}

// largeDeclaredLength is a value length small files routinely claim but cannot
// hold: it must be refused before it is allocated.
func TestParseBudgetRejectsHugeDeclaredLength(t *testing.T) {
	// An OB element declaring ~4 GiB in a 200 byte file, after a valid meta
	// header.
	huge := explicitElem(tag.Tag{Group: 0x0009, Element: 0x0001}, "OB", 0xFFFFFFF0, nil, true)
	data := explicitFile(huge)

	_, err := Parse(bytes.NewReader(data), int64(len(data)), nil)
	if !errors.Is(err, ErrorParseBudgetExceeded) {
		t.Fatalf("Parse of a %d byte file declaring a 4 GiB value: got error %v, want ErrorParseBudgetExceeded", len(data), err)
	}
	if errors.Is(err, dicomio.ErrorInsufficientBytesLeft) {
		t.Errorf("the budget must be enforced before the reader runs out of bytes; got %v", err)
	}
}

// A legitimately large image must parse once the caller raises the budget.
func TestParseBudgetRaisedAllowsLargeImage(t *testing.T) {
	const rows, cols = 96, 96
	pixels := make([]byte, rows*cols*2)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	image := append(explicitElem(tag.Rows, "US", 2, us(rows), false),
		explicitElem(tag.Columns, "US", 2, us(cols), false)...)
	image = append(image, explicitElem(tag.BitsAllocated, "US", 2, us(16), false)...)
	image = append(image, explicitElem(tag.SamplesPerPixel, "US", 2, us(1), false)...)
	image = append(image, explicitElem(tag.PixelData, "OW", uint32(len(pixels)), pixels, true)...)
	data := explicitFile(image)

	// With a budget that is too small for the PixelData, the parse fails.
	_, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithParseBudget(ParseBudget{MaxElementBytes: 1024}))
	if !errors.Is(err, ErrorParseBudgetExceeded) {
		t.Fatalf("Parse with a 1 KiB element budget: got error %v, want ErrorParseBudgetExceeded", err)
	}

	// Raising the budget lets the same, legal image parse.
	ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithParseBudget(ParseBudget{MaxElementBytes: 1 << 20}))
	if err != nil {
		t.Fatalf("Parse with a raised budget returned unexpected error: %v", err)
	}
	el, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		t.Fatalf("PixelData missing from the parsed dataset: %v", err)
	}
	pi := MustGetPixelDataInfo(el.Value)
	if got, want := len(pi.Frames), 1; got != want {
		t.Fatalf("unexpected frame count: got %d, want %d", got, want)
	}
	if diff := pi.Frames[0].NativeData.RawDataSlice(); diff == nil {
		t.Fatalf("native frame has no pixel data")
	}
}

func TestParseBudgetNestingDepth(t *testing.T) {
	const depth = 40 // more than DefaultMaxNestingDepth / 2, less than the limit
	data := explicitFile(undefinedSequence(depth))

	if _, err := Parse(bytes.NewReader(data), int64(len(data)), nil); err != nil {
		t.Fatalf("nested sequences within the default depth returned unexpected error: %v", err)
	}

	_, err := Parse(bytes.NewReader(data), int64(len(data)), nil, MaxNestingDepth(8))
	if !errors.Is(err, ErrorSequenceTooDeep) {
		t.Fatalf("Parse of %d nested sequences with a depth limit of 8: got error %v, want ErrorSequenceTooDeep", depth, err)
	}

	// The same file parses when the caller raises the depth.
	if _, err := Parse(bytes.NewReader(data), int64(len(data)), nil, MaxNestingDepth(depth*2+2)); err != nil {
		t.Fatalf("Parse with a raised nesting depth returned unexpected error: %v", err)
	}
}

// Very deep nesting must be refused before the recursion can exhaust the
// goroutine stack, which recover() could not catch.
func TestParseBudgetDeepNestingDoesNotOverflowStack(t *testing.T) {
	data := explicitFile(undefinedSequence(2000))
	_, err := Parse(bytes.NewReader(data), int64(len(data)), nil)
	if !errors.Is(err, ErrorSequenceTooDeep) {
		t.Fatalf("Parse of %d nested sequences: got error %v, want ErrorSequenceTooDeep", 2000, err)
	}
}

func TestParseBudgetSequenceScope(t *testing.T) {
	// A defined-length sequence claiming a large scope in a small file is
	// refused against the cumulative parse budget before PushLimit allocates
	// any limit state for a range the input cannot hold.
	seq := explicitElem(tag.Tag{Group: 0x0008, Element: 0x1111}, "SQ", 0x01000000, nil, true)
	data := explicitFile(seq)

	_, err := Parse(bytes.NewReader(data), int64(len(data)), nil, MaxParseBytes(4096))
	if !errors.Is(err, ErrorParseBudgetExceeded) {
		t.Fatalf("Parse of a sequence declaring a 16 MiB scope: got error %v, want ErrorParseBudgetExceeded", err)
	}
}

// Encapsulated (undefined length) PixelData items are bounded by the budget,
// and a truncated item returns an error instead of a partial frame list.
func TestParseBudgetEncapsulatedItems(t *testing.T) {
	// Build PixelData with undefined length: an empty basic offset table, then
	// one item declaring 16 MiB but holding only 64 bytes, then the
	// delimitation.
	var dataset bytes.Buffer
	dataset.Write(explicitElem(tag.Tag{Group: 0x0028, Element: 0x0010}, "US", 2, us(1), false))
	dataset.Write(explicitElem(tag.PixelData, "OB", tag.VLUndefinedLength, nil, true))
	dataset.Write(bulkItem(0, 0))
	dataset.Write(bulkItem(16<<20, 64))
	dataset.Write(sequenceDelimitation())
	data := explicitFile(dataset.Bytes())

	_, err := Parse(bytes.NewReader(data), int64(len(data)), nil, MaxElementBytes(1024))
	if !errors.Is(err, ErrorParseBudgetExceeded) {
		t.Fatalf("Parse of a 16 MiB encapsulated item in a %d byte file: got error %v, want ErrorParseBudgetExceeded", len(data), err)
	}

	// The default budget rejects the item before allocating 16 MiB too...
	_, err = Parse(bytes.NewReader(data), int64(len(data)), nil)
	if !errors.Is(err, ErrorParseBudgetExceeded) && !errors.Is(err, dicomio.ErrorInsufficientBytesLeft) {
		t.Fatalf("Parse of a truncated encapsulated item: got error %v, want a budget or insufficient bytes error", err)
	}
}

// Undefined length byte values (OB/OW) accumulate item by item; the total is
// bounded even when each item alone is small.
func TestParseBudgetUndefinedLengthByteValue(t *testing.T) {
	const itemCount = 64
	var dataset bytes.Buffer
	dataset.Write(explicitElem(tag.Tag{Group: 0x0009, Element: 0x0001}, "OB", tag.VLUndefinedLength, nil, true))
	for i := 0; i < itemCount; i++ {
		var item bytes.Buffer
		_ = binary.Write(&item, binary.LittleEndian, tag.Item.Group)
		_ = binary.Write(&item, binary.LittleEndian, tag.Item.Element)
		_ = binary.Write(&item, binary.LittleEndian, uint32(1024))
		item.Write(make([]byte, 1024))
		dataset.Write(item.Bytes())
	}
	var delim bytes.Buffer
	_ = binary.Write(&delim, binary.LittleEndian, tag.SequenceDelimitationItem.Group)
	_ = binary.Write(&delim, binary.LittleEndian, tag.SequenceDelimitationItem.Element)
	_ = binary.Write(&delim, binary.LittleEndian, uint32(0))
	dataset.Write(delim.Bytes())
	data := explicitFile(dataset.Bytes())

	_, err := Parse(bytes.NewReader(data), int64(len(data)), nil, MaxElementBytes(16<<10))
	if !errors.Is(err, ErrorParseBudgetExceeded) {
		t.Fatalf("Parse of %d KiB of undefined length bytes with a 16 KiB element budget: got error %v, want ErrorParseBudgetExceeded", itemCount, err)
	}

	if _, err := Parse(bytes.NewReader(data), int64(len(data)), nil); err != nil {
		t.Fatalf("Parse of %d KiB of undefined length bytes with the default budget returned unexpected error: %v", itemCount, err)
	}
}

// Dimensions that don't match the PixelData length must be reported as
// invalid, not wrapped around into a plausible value.
func TestNativeFramesRejectOverflowingDimensions(t *testing.T) {
	// Rows * Columns would overflow int64 on 64-bit platforms.
	const maxInt = int(^uint(0) >> 1)
	elem := func(tg tag.Tag, v int) *Element { return mustNewElement(tg, []int{v}) }

	cases := []struct {
		name string
		ds   Dataset
	}{
		{
			name: "overflowing pixels",
			ds: Dataset{Elements: []*Element{
				elem(tag.Rows, maxInt), elem(tag.Columns, maxInt),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				elem(tag.BitsAllocated, 16), elem(tag.SamplesPerPixel, 1),
			}},
		},
		{
			name: "overflowing frame count",
			ds: Dataset{Elements: []*Element{
				elem(tag.Rows, maxInt), elem(tag.Columns, 2),
				mustNewElement(tag.NumberOfFrames, []string{fmt.Sprint(maxInt)}),
				elem(tag.BitsAllocated, 16), elem(tag.SamplesPerPixel, 1),
			}},
		},
		{
			name: "zero rows",
			ds: Dataset{Elements: []*Element{
				elem(tag.Rows, 0), elem(tag.Columns, 2),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				elem(tag.BitsAllocated, 16), elem(tag.SamplesPerPixel, 1),
			}},
		},
		{
			name: "negative frame count",
			ds: Dataset{Elements: []*Element{
				elem(tag.Rows, 2), elem(tag.Columns, 2),
				mustNewElement(tag.NumberOfFrames, []string{"-4"}),
				elem(tag.BitsAllocated, 16), elem(tag.SamplesPerPixel, 1),
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, 32)
			r := &reader{
				rawReader: dicomio.NewReader(bufio.NewReader(bytes.NewReader(data)), binary.LittleEndian, int64(len(data))),
				budget:    newBudget(DefaultParseBudget()),
			}
			_, _, err := r.readNativeFrames(&tc.ds, nil, uint32(len(data)))
			if !errors.Is(err, ErrorInvalidPixelDimensions) {
				t.Fatalf("readNativeFrames(%+v): got error %v, want ErrorInvalidPixelDimensions", tc.ds, err)
			}
		})
	}
}

// A failed parse must not hand back a partially parsed dataset, and must
// close the frame channel so streaming consumers stop.
func TestParseBudgetFailureLeavesNoPartialData(t *testing.T) {
	// Valid meta header, then a valid element, then a huge declared value.
	good := explicitElem(tag.PatientName, "PN", 4, []byte("Bob\x00"), false)
	bad := explicitElem(tag.Tag{Group: 0x0009, Element: 0x0001}, "OB", 0xFFFFFFF0, nil, true)
	data := explicitFile(append(good, bad...))

	frameChan := make(chan *frame.Frame, 1)
	ds, err := Parse(bytes.NewReader(data), int64(len(data)), frameChan)
	if !errors.Is(err, ErrorParseBudgetExceeded) {
		t.Fatalf("Parse: got error %v, want ErrorParseBudgetExceeded", err)
	}
	if len(ds.Elements) != 0 {
		t.Errorf("Parse returned %d elements alongside an error; want none", len(ds.Elements))
	}
	if _, ok := <-frameChan; ok {
		t.Errorf("frame channel still produced a frame after a failed parse")
	}
}

// NewParser enforces the budget element by element as well, so a caller using
// the iterator API gets the same protection.
func TestParserNextBudget(t *testing.T) {
	big := explicitElem(tag.Tag{Group: 0x0009, Element: 0x0001}, "OB", uint32(1<<20), make([]byte, 1<<20), true)
	small := explicitElem(tag.PatientName, "PN", 4, []byte("Bob\x00"), false)
	data := explicitFile(append(small, big...))

	p, err := NewParser(bytes.NewReader(data), int64(len(data)), nil, MaxElementBytes(1024))
	if err != nil {
		t.Fatalf("NewParser returned unexpected error: %v", err)
	}
	// The first element fits and is returned.
	if _, err := p.Next(); err != nil {
		t.Fatalf("Next returned unexpected error for an element within budget: %v", err)
	}
	// The second declares a value over budget and is refused.
	if _, err := p.Next(); !errors.Is(err, ErrorParseBudgetExceeded) {
		t.Fatalf("Next: got error %v, want ErrorParseBudgetExceeded", err)
	}
}

func TestParseBudgetNegativeLimits(t *testing.T) {
	_, err := NewParser(bytes.NewReader(nil), 0, nil, MaxElementBytes(-1))
	if err == nil {
		t.Fatal("NewParser accepted a negative MaxElementBytes")
	}
}

func TestParseBudgetZeroDisablesLimits(t *testing.T) {
	// A zero budget disables every limit, including nesting.
	data := explicitFile(undefinedSequence(5))
	if _, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithParseBudget(ParseBudget{})); err != nil {
		t.Fatalf("Parse with a zero budget returned unexpected error: %v", err)
	}
}

// Streaming frames from an image that fails the budget must not leave a
// partially decoded frame on the channel.
func TestParseBudgetStreamingFrameFailure(t *testing.T) {
	const rows, cols = 32, 32
	pixels := make([]byte, rows*cols*2)
	image := append(explicitElem(tag.Rows, "US", 2, us(rows), false),
		explicitElem(tag.Columns, "US", 2, us(cols), false)...)
	image = append(image, explicitElem(tag.BitsAllocated, "US", 2, us(16), false)...)
	image = append(image, explicitElem(tag.SamplesPerPixel, "US", 2, us(1), false)...)
	image = append(image, explicitElem(tag.PixelData, "OW", uint32(len(pixels)), pixels, true)...)
	data := explicitFile(image)

	frameChan := make(chan *frame.Frame, 4)
	ds, err := Parse(bytes.NewReader(data), int64(len(data)), frameChan, MaxElementBytes(512))
	if !errors.Is(err, ErrorParseBudgetExceeded) {
		t.Fatalf("Parse: got error %v, want ErrorParseBudgetExceeded", err)
	}
	if len(ds.Elements) != 0 {
		t.Errorf("Parse returned %d elements alongside an error; want none", len(ds.Elements))
	}
	// The budget is checked before any frame is decoded, so the channel is
	// closed without a frame.
	for fr := range frameChan {
		t.Errorf("received frame %v after a budget failure", fr)
	}
}

// A one-bit image costs eight times its bytes in memory once decoded. A
// malformed file that declares many tiny one-bit frames must not turn a small
// input into a large allocation: the frames' bytes are read before each
// frame's buffer is allocated.
func TestParseUntilEOFOneBitDoesNotOverAllocate(t *testing.T) {
	// One pixel per frame at eight samples per pixel: one byte per frame.
	body := append(explicitElem(tag.Rows, "US", 2, us(1), false),
		explicitElem(tag.Columns, "US", 2, us(8), false)...)
	body = append(body, explicitElem(tag.BitsAllocated, "US", 2, us(1), false)...)
	body = append(body, explicitElem(tag.SamplesPerPixel, "US", 2, us(1), false)...)
	body = append(body, explicitElem(tag.NumberOfFrames, "IS", 16, []byte("2000000000      "), false)...)
	body = append(body, explicitElem(tag.PixelData, "OW", 1, []byte{0xAA}, true)...)
	data := explicitFile(body)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := ParseUntilEOF(bytes.NewReader(data), nil)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("ParseUntilEOF of a one-bit image missing its pixel data returned no error")
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("ParseUntilEOF of a %d byte input allocated %d bytes", len(data), allocated)
	}
}

// A parse that fails while setting up the parser must still close the frame
// channel, or a consumer ranging over it would wait forever.
func TestParseBudgetSetupFailureClosesFrameChannel(t *testing.T) {
	// A file that fails during NewParser (bad meta group length).
	data := []byte("not a dicom")
	frameChan := make(chan *frame.Frame)
	done := make(chan struct{})
	go func() {
		for range frameChan {
		}
		close(done)
	}()
	ds, err := Parse(bytes.NewReader(data), int64(len(data)), frameChan)
	if err == nil {
		t.Fatal("Parse of invalid input returned no error")
	}
	if len(ds.Elements) != 0 {
		t.Errorf("Parse returned %d elements alongside an error; want none", len(ds.Elements))
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("frame channel was not closed after a setup failure")
	}
}

// ParseUntilEOF has no exact input size, so a declared length cannot be
// compared with the bytes left. ReadBytes must then grow with the bytes that
// actually arrive, so a bogus declared length costs no more than the input.
func TestParseUntilEOFDoesNotAllocateFromDeclaredLength(t *testing.T) {
	// An OB element declaring 200 MiB in a 145 byte file.
	data := append(explicitFile(nil), explicitElem(tag.Tag{Group: 0x0009, Element: 0x0001}, "OB", 200<<20, nil, true)...)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ds, err := ParseUntilEOF(bytes.NewReader(data), nil)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatalf("ParseUntilEOF of a truncated 200 MiB value returned no error and %d elements", len(ds.Elements))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("ParseUntilEOF of a %d byte input allocated %d bytes from a declared length, want a small amount", len(data), allocated)
	}
}

// A non-item tag where an encapsulated frame item is expected is reported with
// its position instead of being treated as the end of the frames.
func TestReadRawItemNonItemIsAnError(t *testing.T) {
	var b bytes.Buffer
	b.Write(implicitElem(tag.Tag{Group: 0x0009, Element: 0x0001}, 4, []byte{1, 2, 3, 4}))
	r := &reader{
		rawReader: dicomio.NewReader(bufio.NewReader(&b), binary.LittleEndian, int64(b.Len())),
		budget:    newBudget(DefaultParseBudget()),
	}
	if _, _, err := r.readRawItem(false); err == nil {
		t.Fatal("readRawItem accepted a non-item tag without an error")
	}
}

// A defined-length sequence holding a non-item element must return an error
// rather than panic on the type assertion (found by FuzzDICOMParseWithBudget;
// see fuzz/testdata/fuzz/FuzzDICOMParseWithBudget/503cb541961fb967).
func TestReadSequenceNonItemDoesNotPanic(t *testing.T) {
	// Defined-length SQ whose contents are a plain string element, not an item.
	var body bytes.Buffer
	body.Write(explicitElem(tag.PatientName, "PN", 4, []byte("Bob\x00"), false))
	seq := explicitElem(tag.Tag{Group: 0x0008, Element: 0x1111}, "SQ", uint32(body.Len()), body.Bytes(), true)

	r := &reader{
		rawReader: dicomio.NewReader(bufio.NewReader(bytes.NewReader(seq)), binary.LittleEndian, int64(len(seq))),
		budget:    newBudget(DefaultParseBudget()),
	}
	if _, err := r.readSequence(tag.Tag{Group: 0x0008, Element: 0x1111}, "SQ", uint32(body.Len()), &Dataset{}); err == nil {
		t.Fatal("readSequence accepted a non-item element without an error")
	}
}
