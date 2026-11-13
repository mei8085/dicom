package dicom

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/suyashkumar/dicom/pkg/debug"
	"github.com/suyashkumar/dicom/pkg/vrraw"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"golang.org/x/exp/constraints"
)

var (
	// ErrorOWRequiresEvenVL indicates that an element with VR=OW had a not even
	// value length which is not allowed.
	ErrorOWRequiresEvenVL = errors.New("vr of OW requires even value length")
	// ErrorUnsupportedVR indicates that this VR is not supported.
	ErrorUnsupportedVR = errors.New("unsupported VR")
	// ErrorUnsupportedBitsAllocated indicates that the BitsAllocated in the
	// NativeFrame PixelData is unsupported. In this situation, the rest of the
	// dataset returned is still valid.
	ErrorUnsupportedBitsAllocated     = errors.New("unsupported BitsAllocated")
	errorUnableToParseFloat           = errors.New("unable to parse float type")
	ErrorExpectedEvenLength           = errors.New("field length is not even, in violation of DICOM spec")
	ErrorExpectedDefinedLength        = errors.New("unable to read field of undefined length")
	ErrorExpectedSequenceDelimitation = errors.New("expected to find sequence delimitation item (fffe,e0dd)")
)

// nativeFrameOverheadBytes is a conservative estimate of the memory a parsed
// native frame costs beyond its pixel buffer: the frame.Frame header plus the
// NativeFrame struct and allocator rounding for small slices.
const nativeFrameOverheadBytes = 256

// elementOverheadBytes approximates the memory each parsed element costs beyond
// its value: the Element struct, its Value interface, and a slot in a
// Dataset.Elements slice. It is charged to the cumulative budget so that a
// long run of tiny elements cannot grow the dataset without bound.
const elementOverheadBytes = 64

// reader is responsible for mid-level dicom parsing capabilities, like
// reading tags, VRs, and elements from the low-level *dicomio.Reader dicom data.
// TODO(suyashkumar): consider revisiting naming of this struct "reader" as it
// interplays with the rawReader *dicomio.Reader. We could consider combining
// them, or embedding the *dicomio.Reader struct into reader.
type reader struct {
	rawReader *dicomio.Reader
	opts      parseOptSet
	// budget bounds the memory this parse may reserve and the nesting depth it
	// may reach. It is shared by the whole parse, so every nested read charges
	// the same cumulative limit.
	budget budget
}

// checkValue verifies, before allocating or reading, that a value of n bytes
// fits the budget, and charges it to the cumulative parse budget. context
// names the element or structure, and errors include the reader's byte offset
// so a malformed value can be located in the input.
func (r *reader) checkValue(n int64, context string) error {
	if n < 0 {
		return fmt.Errorf("%s: negative length %d: %w", context, n, ErrorParseBudgetExceeded)
	}
	// The declared length is checked against the budget first: a malformed
	// file may declare a length that is both over budget and more than the
	// input holds, and the budget error is the more informative one. Then the
	// bytes are charged, and the input's remaining size is verified.
	if err := r.budget.reserve(n, r.rawReader.BytesRead(), context); err != nil {
		return err
	}
	return r.checkReadable(n, context)
}

// checkMemory verifies, before allocating, that a value occupies at most the
// per-element memory limit.
func (r *reader) checkMemory(n int64, context string) error {
	return r.budget.checkElement(n, r.rawReader.BytesRead(), context)
}

// chargeRead charges bytes that are consumed without being retained (skipped
// values, or undefined length items that are discarded) against the cumulative
// budget.
func (r *reader) chargeRead(n int64, context string) error {
	return r.budget.chargeRead(n, r.rawReader.BytesRead(), context)
}

// checkScope verifies the declared length of a nested scope (a sequence or an
// item) against the cumulative budget without charging it; the scope's
// contents charge it as they are read.
func (r *reader) checkScope(n int64, context string) error {
	return r.budget.checkScope(n, r.rawReader.BytesRead(), context)
}

// checkReadable refuses a length larger than the input can still supply, so a
// truncated or bogus length is reported before it is allocated or read. The
// check only applies when the reader's limit is exact (see
// dicomio.Reader.ExactLimit); otherwise the read itself bounds it.
func (r *reader) checkReadable(n int64, context string) error {
	if n < 0 || !r.rawReader.ExactLimit() {
		return nil
	}
	if left := r.rawReader.BytesLeftUntilLimit(); n > left {
		return fmt.Errorf("%s at byte offset %d: declared length %d, %d bytes left: %w",
			context, r.rawReader.BytesRead(), n, left, dicomio.ErrorInsufficientBytesLeft)
	}
	return nil
}

// enterNesting opens a sequence or item scope, refusing to recurse past the
// configured nesting depth. The check happens before the recursive call so a
// malformed nest cannot exhaust the stack.
func (r *reader) enterNesting(context string) error {
	return r.budget.enterNesting(r.rawReader.BytesRead(), context)
}

// leaveNesting closes a scope opened by enterNesting.
func (r *reader) leaveNesting() { r.budget.leaveNesting() }

func (r *reader) readTag() (*tag.Tag, error) {
	group, gerr := r.rawReader.ReadUInt16()
	element, eerr := r.rawReader.ReadUInt16()

	if gerr == nil && eerr == nil {
		return &tag.Tag{Group: group, Element: element}, nil
	}

	return nil, fmt.Errorf("error reading tag: %w", errors.Join(gerr, eerr))
}

// TODO: Parsed VR should be an enum. Will require refactors of tag pkg.
func (r *reader) readVR(isImplicit bool, t tag.Tag) (string, error) {
	if isImplicit {
		if entry, err := tag.Find(t); err == nil {
			switch entry.Tag {
			case tag.PixelData, tag.OverlayData:
				// OW takes priority in these cases. See notes at:
				// 1. https://dicom.nema.org/medical/dicom/2024a/output/html/part05.html#sect_8.1.2
				// 2. https://dicom.nema.org/medical/dicom/2024a/output/html/part05.html#sect_8.2
				return "OW", nil
			default:
				return entry.VRs[0], nil
			}
		}
		return tag.UnknownVR, nil
	}

	// Explicit Transfer Syntax, read 2 byte VR:
	return r.rawReader.ReadString(2)

}

func (r *reader) readVL(isImplicit bool, t tag.Tag, vr string) (uint32, error) {
	if isImplicit {
		return r.rawReader.ReadUInt32()
	}

	// Explicit Transfer Syntax
	// More details here: https://dicom.nema.org/medical/dicom/current/output/html/part05.html#sect_7.1.2
	switch vr {
	// TODO: Parsed VR should be an enum. Will require refactors of tag pkg.
	case "NA", vrraw.OtherByte, vrraw.OtherDouble, vrraw.OtherFloat,
		vrraw.OtherLong, vrraw.OtherWord, vrraw.Sequence, vrraw.Unknown,
		vrraw.UnlimitedCharacters, vrraw.UniversalResourceIdentifier,
		vrraw.UnlimitedText:
		_ = r.rawReader.Skip(2) // ignore two reserved bytes (0000H)
		vl, err := r.rawReader.ReadUInt32()
		if err != nil {
			return 0, err
		}

		if vl == tag.VLUndefinedLength &&
			(vr == vrraw.UnlimitedCharacters ||
				vr == vrraw.UniversalResourceIdentifier ||
				vr == vrraw.UnlimitedText) {
			return 0, errors.New("UC, UR and UT may not have an Undefined Length, i.e.,a Value Length of FFFFFFFFH")
		}
		return vl, nil
	default:
		vl16, err := r.rawReader.ReadUInt16()
		if err != nil {
			return 0, err
		}
		vl := uint32(vl16)
		// Rectify Undefined Length VL
		if vl == 0xffff {
			vl = tag.VLUndefinedLength
		}
		return vl, nil
	}
}

func (r *reader) readValue(t tag.Tag, vr string, vl uint32, isImplicit bool, d *Dataset, fc chan<- *frame.Frame) (Value, error) {
	vrkind := tag.GetVRKind(t, vr)
	// TODO: if we keep consistent function signature, consider a static map of VR to func?
	switch vrkind {
	case tag.VRBytes:
		if vl == tag.VLUndefinedLength {
			return r.readUndefinedLengthByteValue()
		}
		return r.readBytes(t, vr, vl)
	case tag.VRString:
		return r.readString(t, vr, vl)
	case tag.VRDate:
		return r.readDate(t, vr, vl)
	case tag.VRUInt16List, tag.VRUInt32List, tag.VRInt16List, tag.VRInt32List, tag.VRTagList:
		return r.readInt(t, vr, vl)
	case tag.VRSequence:
		return r.readSequence(t, vr, vl, d)
	case tag.VRItem:
		return r.readSequenceItem(t, vr, vl, d)
	case tag.VRPixelData:
		return r.readPixelData(vl, d, fc)
	case tag.VRFloat32List, tag.VRFloat64List:
		return r.readFloat(t, vr, vl)
	// More details on how we treat Unknown VRs can be found at
	// https://github.com/suyashkumar/dicom/issues/220
	// and
	// https://github.com/suyashkumar/dicom/issues/231.
	// TODO(suyashkumar): consider replacing UN VRs with SQ earlier on if they
	// meet this criteria, so users of the Dataset can interact with it
	// correctly.
	case tag.VRUnknown:
		if vl == tag.VLUndefinedLength {
			return r.readSequence(t, vr, vl, d)
		}
		return r.readBytes(t, vr, vl)
	default:
		return r.readString(t, vr, vl)
	}
}

// readUndefinedLengthByteValue reads a value of type OB or OW of undefined length.
// see https://github.com/suyashkumar/dicom/issues/349 for details.
// pixel data should not use this and instead use readPixelData.
func (r *reader) readUndefinedLengthByteValue() (Value, error) {
	var allData []byte
	foundDelimeter := false

	for !r.rawReader.IsLimitExhausted() {
		data, terminated, err := r.readRawItem(false)
		if err != nil {
			return nil, fmt.Errorf("error reading undefined byte value: %w", err)
		}

		if terminated {
			foundDelimeter = true
			break
		}

		allData = append(allData, data...)
		// The value grows item by item, so it must be bounded as one value:
		// many small items could otherwise add up past the per-element limit.
		// Its bytes are already read, so only the per-element limit applies
		// here, not the bytes-left check.
		if err := r.checkMemory(int64(len(allData)), "readUndefinedLengthByteValue: accumulated value"); err != nil {
			return nil, err
		}
	}

	if !foundDelimeter {
		return nil, ErrorExpectedSequenceDelimitation
	}

	return &bytesValue{value: allData}, nil
}

// readHeader reads the DICOM magic header and group two metadata elements.
// This should only be called once per DICOM at the start of parsing.
func (r *reader) readHeader() ([]*Element, error) {
	// Check for Preamble (128 bytes) + magic word (4 bytes).
	data, err := r.rawReader.Peek(128 + 4)
	if err != nil {
		return nil, fmt.Errorf("error reading DICOM header preamble: %w", err)
	}
	if string(data[128:]) != magicWord {
		// If magic word is not at byte offset 128 this is a non-standard
		// non-compliant DICOM. We try to read this DICOM in a compatibility
		// mode, where we rewind to position 0 and blindly attempt to parse
		// a Dataset (and do not parse metadata in the usual way), which is
		// why we return nil error.
		return nil, nil
	}

	err = r.rawReader.Skip(128 + 4) // skip preamble + magic word
	if err != nil {
		return nil, err
	}

	// Must read metadata as LittleEndian explicit VR
	// Read the length of the metadata elements: (0002,0000) MetaElementGroupLength
	maybeMetaLen, err := r.readElement(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("error reading DICOM header element: %w", err)
	}

	metaElems := []*Element{maybeMetaLen} // TODO: maybe set capacity to a reasonable initial size
	metaElementGroupLengthDefined := true
	if maybeMetaLen.Tag != tag.FileMetaInformationGroupLength || maybeMetaLen.Value.ValueType() != Ints {
		// MetaInformationGroupLength is not present or of the wrong value type.
		if !r.opts.allowMissingMetaElementGroupLength {
			return nil, ErrorMetaElementGroupLength
		}
		metaElementGroupLengthDefined = false
	}

	if metaElementGroupLengthDefined {
		metaLens, ok := maybeMetaLen.Value.GetValue().([]int)
		if !ok || len(metaLens) == 0 {
			return nil, ErrorMetaElementGroupLength
		}
		metaLen := metaLens[0]
		if metaLen < 0 {
			return nil, fmt.Errorf("MetaElementGroupLength is %d: %w", metaLen, ErrorMetaElementGroupLength)
		}
		if err := r.checkScope(int64(metaLen), "readHeader: meta element group"); err != nil {
			return nil, err
		}

		// Read the metadata elements
		err = r.rawReader.PushLimit(int64(metaLen))
		if err != nil {
			return nil, err
		}
		defer r.rawReader.PopLimit()
		for !r.rawReader.IsLimitExhausted() {
			elem, err := r.readElement(nil, nil)
			if err != nil {
				// TODO: see if we can skip over malformed elements somehow
				return nil, fmt.Errorf("error reading DICOM header element: %w", err)
			}
			metaElems = append(metaElems, elem)
		}
	} else {
		// We cannot use the limit functionality
		debug.Log("Proceeding without metadata group length")
		for {
			// Lets peek into the tag field until we get to end-of-header
			groupBytes, err := r.rawReader.Peek(2)
			if err != nil {
				return nil, ErrorMetaElementGroupLength
			}
			var group uint16
			buff := bytes.NewBuffer(groupBytes)
			if err := binary.Read(buff, binary.LittleEndian, &group); err != nil {
				return nil, err
			}
			debug.Logf("header-group: %v", group)
			// Only read group 2 data
			if group != 0x0002 {
				break
			}
			elem, err := r.readElement(nil, nil)
			if err != nil {
				// TODO: see if we can skip over malformed elements somehow
				return nil, fmt.Errorf("error reading DICOM header element (with no meta element group length defined): %w", err)
			}
			metaElems = append(metaElems, elem)
		}
	}
	return metaElems, nil
}

func (r *reader) readPixelData(vl uint32, d *Dataset, fc chan<- *frame.Frame) (Value,
	error) {
	if vl == tag.VLUndefinedLength {
		var image PixelDataInfo
		image.IsEncapsulated = true
		// The first Item in PixelData is the basic offset table. Skip this for now.
		// TODO: use basic offset table
		_, _, err := r.readRawItem(true /*shouldSkip*/)
		if err != nil {
			return nil, fmt.Errorf("readPixelData: error skipping basic offset table: %w", err)
		}

		for !r.rawReader.IsLimitExhausted() {
			data, endOfItems, err := r.readRawItem(r.opts.skipPixelData /*shouldSkip*/)
			if err != nil {
				// Unlike before, an item error is returned instead of silently
				// ending the frames: a truncated or malformed encapsulated
				// PixelData must not look like a complete one, and callers must
				// not act on the frames collected so far.
				return nil, fmt.Errorf("readPixelData: error reading encapsulated frame item: %w", err)
			}

			if endOfItems {
				break
			}

			f := frame.Frame{
				Encapsulated: true,
				EncapsulatedData: frame.EncapsulatedFrame{
					Data: data,
				},
			}

			if fc != nil {
				fc <- &f
			}

			image.Frames = append(image.Frames, &f)
		}
		image.IntentionallySkipped = r.opts.skipPixelData
		return &pixelDataValue{PixelDataInfo: image}, nil
	}

	if r.opts.skipPixelData {
		// If we're here, it means the VL isn't undefined length, so we should
		// be able to safely skip the native PixelData. Nothing is allocated, so
		// only the cumulative bytes limit applies.
		if err := r.chargeRead(int64(vl), "readPixelData: skipped PixelData"); err != nil {
			return nil, err
		}
		if err := r.rawReader.Skip(int64(vl)); err != nil {
			return nil, fmt.Errorf("readPixelData: skipPixelData option was set, and had error skipping PixelData: %w", err)
		}
		return &pixelDataValue{PixelDataInfo{IntentionallySkipped: true}}, nil
	}

	if r.opts.skipProcessingPixelDataValue {
		if err := r.checkValue(int64(vl), "readPixelData: unprocessed PixelData"); err != nil {
			return nil, err
		}
		data, err := r.rawReader.ReadBytes(int64(vl))
		if err != nil {
			return nil, err
		}
		return &pixelDataValue{PixelDataInfo{IntentionallyUnprocessed: true, UnprocessedValueData: data}}, nil
	}

	// Assume we're reading NativeData data since we have a defined value length as per Part 5 Sec A.4 of DICOM spec.
	// We need Elements that have been already parsed (rows, cols, etc) to parse frames out of NativeData Pixel data
	if d == nil {
		return nil, errors.New("the Dataset context cannot be nil in order to read Native PixelData")
	}

	i, _, err := r.readNativeFrames(d, fc, vl)

	if err != nil {
		return nil, err
	}

	// TODO: avoid this copy
	return &pixelDataValue{PixelDataInfo: *i}, nil

}

func getNthBit(data byte, n int) int {
	debug.Logf("mask: %0b", 1<<n)
	if (1 << n & uint8(data)) > 0 {
		return 1
	}
	return 0
}

// fillBufferSingleBitAllocated expands one-bit samples from data into
// pixelData, eight samples per byte. data must already hold the frame's bytes;
// the caller reads them before allocating pixelData so a declared frame size
// the input cannot supply is never allocated.
func fillBufferSingleBitAllocated(pixelData []int, data []byte) error {
	debug.Logf("len of pixeldata: %d", len(pixelData))
	if len(pixelData)%8 > 0 {
		return errors.New("when bitsAllocated is 1, we can't read a number of samples that is not a multiple of 8")
	}
	if len(data)*8 < len(pixelData) {
		return fmt.Errorf("when bitsAllocated is 1, need %d bytes for %d samples, got %d: %w",
			len(pixelData)/8, len(pixelData), len(data), io.ErrUnexpectedEOF)
	}

	for i := 0; i < len(pixelData)/8; i++ {
		currentByte := data[i]
		debug.Logf("currentByte: %0b", currentByte)

		// Read in the 8 bits from the current byte.
		// Always treat the data as LittleEndian encoded.
		// This is what pydicom appears to do, and I can't get Go to properly
		// write out bytes literals in BigEndian, even using binary.Write
		// (in order to test what BigEndian might look like). We should consider
		// revisiting this more closely, and see if the most significant bit tag
		// should be used to determine the read order here.
		idx := 0
		for j := 7; j >= 0; j-- {
			pixelData[(8*i)+idx] = getNthBit(currentByte, j)
			debug.Logf("getbit #%d: %d", j, getNthBit(currentByte, j))
			idx++
		}

	}

	return nil
}

func makeErrorPixelData(reader *dicomio.Reader, vl uint32, fc chan<- *frame.Frame, parseErr error) (*PixelDataInfo, error) {
	data, err := reader.ReadBytes(int64(vl))
	if err != nil {
		return nil, fmt.Errorf("makeErrorPixelData: read pixelData: %w", err)
	}

	f := frame.Frame{
		EncapsulatedData: frame.EncapsulatedFrame{
			Data: data,
		},
	}

	if fc != nil {
		fc <- &f
	}
	image := PixelDataInfo{
		ParseErr: parseErr,
		Frames:   []*frame.Frame{&f},
	}
	return &image, nil
}

// readNativeFrames reads NativeData frames from a Decoder based on already parsed pixel information
// that should be available in parsedData (elements like NumberOfFrames, rows, columns, etc)
func (r *reader) readNativeFrames(parsedData *Dataset, fc chan<- *frame.Frame, vl uint32) (pixelData *PixelDataInfo,
	bytesToRead int, err error) {
	// Parse information from previously parsed attributes that are needed to parse NativeData Frames:
	rows, err := firstInt(parsedData, tag.Rows)
	if err != nil {
		return nil, 0, fmt.Errorf("readNativeFrames: error reading Rows: %w", err)
	}

	cols, err := firstInt(parsedData, tag.Columns)
	if err != nil {
		return nil, 0, fmt.Errorf("readNativeFrames: error reading Columns: %w", err)
	}

	nFrames := 1
	if nof, err := parsedData.FindElementByTag(tag.NumberOfFrames); err == nil {
		// odd that number of frames is encoded as a string...
		nFrames, err = numberOfFrames(nof)
		if err != nil {
			return nil, 0, fmt.Errorf("readNativeFrames: error reading NumberOfFrames: %w", err)
		}
	}

	bitsAllocated, err := firstInt(parsedData, tag.BitsAllocated)
	if err != nil {
		return nil, 0, fmt.Errorf("readNativeFrames: error reading BitsAllocated: %w", err)
	}

	samplesPerPixel, err := firstInt(parsedData, tag.SamplesPerPixel)
	if err != nil {
		return nil, 0, fmt.Errorf("readNativeFrames: error reading SamplesPerPixel: %w", err)
	}

	// Dimensions come straight from the file, so they must not wrap in int
	// arithmetic or produce a buffer larger than the input can support.
	if rows <= 0 || cols <= 0 || nFrames <= 0 || bitsAllocated <= 0 || samplesPerPixel <= 0 {
		return nil, 0, fmt.Errorf("readNativeFrames: Rows=%d Columns=%d NumberOfFrames=%d BitsAllocated=%d SamplesPerPixel=%d: %w",
			rows, cols, nFrames, bitsAllocated, samplesPerPixel, ErrorInvalidPixelDimensions)
	}
	switch bitsAllocated {
	case 1, 8, 16, 32:
	default:
		return nil, 0, fmt.Errorf("unsupported bitsAllocated, got: %v, %w", bitsAllocated, ErrorUnsupportedBitsAllocated)
	}

	pixelsPerFrame, ok := mulInts(rows, cols)
	if !ok {
		return nil, 0, fmt.Errorf("readNativeFrames: Rows=%d * Columns=%d overflows: %w", rows, cols, ErrorInvalidPixelDimensions)
	}

	debug.Logf("readNativeFrames:\nRows: %d\nCols:%d\nFrames::%d\nBitsAlloc:%d\nSamplesPerPixel:%d", rows, cols, nFrames, bitsAllocated, samplesPerPixel)

	bytesAllocated := bitsAllocated / 8
	if bitsAllocated == 1 {
		// One bit per sample. A sample count that is not a multiple of eight
		// is rejected by fillBufferSingleBitAllocated, as before.
		samples, ok := mulInts(pixelsPerFrame, samplesPerPixel)
		if !ok {
			return nil, 0, fmt.Errorf("readNativeFrames: pixel count overflows: %w", ErrorInvalidPixelDimensions)
		}
		bytesToRead, ok = mulInts(samples/8, nFrames)
		if !ok {
			return nil, 0, fmt.Errorf("readNativeFrames: 1-bit PixelData size overflows: %w", ErrorInvalidPixelDimensions)
		}
	} else {
		bytesToRead, ok = mulInts(bytesAllocated, samplesPerPixel, pixelsPerFrame, nFrames)
		if !ok {
			return nil, 0, fmt.Errorf("readNativeFrames: PixelData size overflows: %w", ErrorInvalidPixelDimensions)
		}
	}
	// A legal frame must fit the budget's per-value limit as well; a corrupt
	// declared frame count can otherwise multiply a small frame past it.
	if err := r.checkMemory(int64(bytesToRead), "readNativeFrames: computed PixelData size"); err != nil {
		return nil, 0, err
	}

	skipFinalPaddingByte := false
	if uint32(bytesToRead) != vl {
		switch {
		case uint32(bytesToRead) == vl-1 && vl%2 == 0:
			skipFinalPaddingByte = true
		case uint32(bytesToRead) == vl-1 && vl%2 != 0:
			return nil, 0, fmt.Errorf("error when reading Native PixelData, got vl=%d, expected even VL: %w", vl, ErrorExpectedEvenLength)
		default:
			// calculated bytesToRead and actual VL mismatch
			if !r.opts.allowMismatchPixelDataLength {
				return nil, 0, fmt.Errorf("error when reading Native PixelData: expected_vl=%d actual_vl=%d %w", bytesToRead, vl, ErrorMismatchPixelDataLength)
			}
			if err := r.checkValue(int64(vl), "readNativeFrames: mismatched PixelData"); err != nil {
				return nil, 0, err
			}
			image, err := makeErrorPixelData(r.rawReader, vl, fc, ErrorMismatchPixelDataLength)
			if err != nil {
				return nil, 0, fmt.Errorf("readNativeFrames: error making error pixel data: %w", err)
			}
			return image, int(vl), nil
		}
	}

	// Charge the memory the frames will occupy before any frame is allocated.
	// One-bit samples expand to one int each, and PixelDataInfo.Frames costs a
	// pointer per frame, so tiny frames can amplify the declared length far
	// beyond bytesToRead. Bound the estimate against both limits.
	if err := r.budget.checkNativeFrames(nFrames, bytesToRead, pixelsPerFrame, samplesPerPixel, bitsAllocated, r.rawReader.BytesRead()); err != nil {
		return nil, 0, err
	}
	if err := r.checkValue(int64(bytesToRead), "readNativeFrames: native PixelData"); err != nil {
		return nil, 0, err
	}
	if skipFinalPaddingByte {
		if err := r.checkValue(1, "readNativeFrames: native PixelData padding byte"); err != nil {
			return nil, 0, err
		}
	}

	// Parse the pixels:
	image := PixelDataInfo{
		IsEncapsulated: false,
	}
	// Under an exact limit the frame slice can be sized up front. Without one
	// (LimitReadUntilEOF or deflated input) it grows as frames are read, so a
	// declared count the input cannot supply costs no more than it holds.
	exact := r.rawReader.ExactLimit()
	if exact {
		image.Frames = make([]*frame.Frame, 0, nFrames)
	}
	bytesPerFrame := bytesToRead / nFrames
	bo := r.rawReader.ByteOrder()
	for frameIdx := 0; frameIdx < nFrames; frameIdx++ {
		// Read this frame's bytes first, then allocate its pixel buffer from
		// what actually arrived. A 1-bit frame expands eightfold into ints, so
		// allocating before the read would let a declared length the input
		// cannot supply become a large allocation.
		frameBytes, err := r.rawReader.ReadBytes(int64(bytesPerFrame))
		if err != nil {
			return nil, bytesToRead, fmt.Errorf("readNativeFrames: frame %d: %w", frameIdx, err)
		}

		currentFrame := frame.Frame{
			Encapsulated: false,
		}
		if bitsAllocated == 1 {
			nativeFrame := frame.NewNativeFrame[int](bitsAllocated, rows, cols, pixelsPerFrame, samplesPerPixel)
			if err := fillBufferSingleBitAllocated(nativeFrame.RawData, frameBytes); err != nil {
				return nil, bytesToRead, err
			}
			currentFrame.NativeData = nativeFrame
		} else {
			switch bitsAllocated {
			case 8:
				currentFrame, err = readNativeFrame[uint8](bitsAllocated, rows, cols, samplesPerPixel, pixelsPerFrame, frameBytes, bo)
			case 16:
				currentFrame, err = readNativeFrame[uint16](bitsAllocated, rows, cols, samplesPerPixel, pixelsPerFrame, frameBytes, bo)
			case 32:
				currentFrame, err = readNativeFrame[uint32](bitsAllocated, rows, cols, samplesPerPixel, pixelsPerFrame, frameBytes, bo)
			}
		}
		if err != nil {
			return nil, bytesToRead, err
		}
		image.Frames = append(image.Frames, &currentFrame)
		if fc != nil {
			fc <- &currentFrame // write the current frame to the frame channel
		}
	}
	if skipFinalPaddingByte {
		err := r.rawReader.Skip(1)
		if err != nil {
			return nil, bytesToRead, fmt.Errorf("error when reading Native PixelData: could not read padding byte, when one needed to be skipped: %w", err)
		}
		bytesToRead++
	}
	return &image, bytesToRead, nil
}

// firstInt returns the first integer value of the element with tag t. A
// missing element keeps the ErrorElementNotFound behaviour callers rely on; an
// element that is not an integer or holds no values is a malformed dimension.
func firstInt(d *Dataset, t tag.Tag) (int, error) {
	e, err := d.FindElementByTag(t)
	if err != nil {
		return 0, err
	}
	vals, ok := e.Value.GetValue().([]int)
	if !ok {
		return 0, fmt.Errorf("element %v has VR %s, not an integer: %w", t, e.RawValueRepresentation, ErrorInvalidPixelDimensions)
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("element %v has no value: %w", t, ErrorInvalidPixelDimensions)
	}
	return vals[0], nil
}

// numberOfFrames reads NumberOfFrames, an IS value that some files encode as
// an integer element instead.
func numberOfFrames(e *Element) (int, error) {
	switch v := e.Value.GetValue().(type) {
	case []string:
		if len(v) == 0 {
			return 0, fmt.Errorf("NumberOfFrames has no value: %w", ErrorInvalidPixelDimensions)
		}
		n, err := strconv.Atoi(strings.TrimSpace(v[0]))
		if err != nil {
			return 0, fmt.Errorf("error converting NumberOfFrames from string to int: %w", err)
		}
		return n, nil
	case []int:
		if len(v) == 0 {
			return 0, fmt.Errorf("NumberOfFrames has no value: %w", ErrorInvalidPixelDimensions)
		}
		return v[0], nil
	default:
		return 0, fmt.Errorf("NumberOfFrames is not a string: %w", ErrorInvalidPixelDimensions)
	}
}

// mulInts multiplies non-negative ints, reporting false if the product would
// overflow or if any factor is negative.
func mulInts(factors ...int) (int, bool) {
	result := 1
	for _, f := range factors {
		if f < 0 {
			return 0, false
		}
		if result != 0 && f > math.MaxInt/result {
			return 0, false
		}
		result *= f
	}
	return result, true
}

// readNativeFrame builds a single NativeFrame[I] from a frame's bytes, which
// the caller has already read. Doing the read first means a declared frame
// size larger than the input is reported before it is allocated.
// TODO(suyashkumar): refactor args to an options struct, or something more compact and readable.
func readNativeFrame[I constraints.Integer](bitsAllocated, rows, cols, samplesPerPixel, pixelsPerFrame int, data []byte, bo binary.ByteOrder) (frame.Frame, error) {
	// Check the bytes are present before allocating the pixel buffer.
	bytesPerSample := bitsAllocated / 8
	need := pixelsPerFrame * samplesPerPixel * bytesPerSample
	if len(data) < need {
		return frame.Frame{}, fmt.Errorf("could not read %d uint%d values from %d bytes: %w",
			pixelsPerFrame*samplesPerPixel, bitsAllocated, len(data), io.ErrUnexpectedEOF)
	}

	nativeFrame := frame.NewNativeFrame[I](bitsAllocated, rows, cols, pixelsPerFrame, samplesPerPixel)
	currentFrame := frame.Frame{
		Encapsulated: false,
		NativeData:   nativeFrame,
	}

	for pixel := 0; pixel < pixelsPerFrame; pixel++ {
		for value := 0; value < samplesPerPixel; value++ {
			i := ((pixel * samplesPerPixel) + value) * bytesPerSample
			switch bitsAllocated {
			case 8:
				v, ok := any(data[i]).(I)
				if !ok {
					return frame.Frame{}, fmt.Errorf("internal error - readNativeFrame unexpectedly unable to type cast pixel buffer data to the I type (%T), where bitsAllocated=%v", *new(I), bitsAllocated)
				}
				nativeFrame.RawData[(pixel*samplesPerPixel)+value] = v
			case 16:
				v, ok := any(bo.Uint16(data[i:])).(I)
				if !ok {
					return frame.Frame{}, fmt.Errorf("internal error - readNativeFrame unexpectedly unable to type cast pixel buffer data to the I type (%T), where bitsAllocated=%v", *new(I), bitsAllocated)
				}
				nativeFrame.RawData[(pixel*samplesPerPixel)+value] = v
			case 32:
				v, ok := any(bo.Uint32(data[i:])).(I)
				if !ok {
					return frame.Frame{}, fmt.Errorf("internal error - readNativeFrame unexpectedly unable to type cast pixel buffer data to the I type (%T), where bitsAllocated=%v", *new(I), bitsAllocated)
				}
				nativeFrame.RawData[(pixel*samplesPerPixel)+value] = v
			default:
				return frame.Frame{}, fmt.Errorf("readNativeFrame unsupported bitsAllocated=%d : %w", bitsAllocated, ErrorUnsupportedBitsAllocated)
			}
		}
	}
	return currentFrame, nil
}

// readSequence reads a sequence element (VR = SQ) that contains a subset of Items. Each item contains
// a set of Elements.
// See https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.5.2.html#table_7.5-1
func (r *reader) readSequence(t tag.Tag, vr string, vl uint32, d *Dataset) (Value, error) {
	// Only sequence elements count toward the nesting depth: an item's
	// contents are reached through the sequence that contains it, so bounding
	// sequences also bounds the recursion into items.
	if err := r.enterNesting(fmt.Sprintf("readSequence: sequence (%v)", t)); err != nil {
		return nil, err
	}
	defer r.leaveNesting()

	var sequences sequencesValue

	seqElements := &Dataset{}
	if vl == tag.VLUndefinedLength {
		for {
			subElement, err := r.readElement(seqElements, nil)
			if err != nil {
				// Stop reading due to error
				return nil, fmt.Errorf("readSequence: error reading subitem in a sequence: %w", err)
			}
			if subElement.Tag == tag.SequenceDelimitationItem {
				// Stop reading
				break
			}
			item, err := sequenceItemOf(subElement)
			if err != nil {
				return nil, err
			}

			// Append the Item element's dataset of elements to this Sequence's sequencesValue.
			sequences.value = append(sequences.value, item)
		}
	} else {
		// Sequence of elements for a total of VL bytes
		if err := r.checkScope(int64(vl), fmt.Sprintf("readSequence: sequence (%v) scope", t)); err != nil {
			return nil, err
		}
		err := r.rawReader.PushLimit(int64(vl))
		if err != nil {
			return nil, err
		}
		defer r.rawReader.PopLimit()
		for !r.rawReader.IsLimitExhausted() {
			subElement, err := r.readElement(seqElements, nil)
			if err != nil {
				// TODO: option to ignore errors parsing subelements?
				return nil, fmt.Errorf("readSequence: error reading subitem in a sequence: %w", err)
			}
			item, err := sequenceItemOf(subElement)
			if err != nil {
				return nil, err
			}

			// Append the Item element's dataset of elements to this Sequence's sequencesValue.
			sequences.value = append(sequences.value, item)
		}
	}

	return &sequences, nil
}

// sequenceItemOf returns the SequenceItem value of an element read inside a
// sequence, or an error if the element is not an item. A malformed sequence
// can hold any element here, so the type must be checked rather than asserted.
func sequenceItemOf(e *Element) (*SequenceItemValue, error) {
	item, ok := e.Value.(*SequenceItemValue)
	if e.Tag != tag.Item || !ok {
		return nil, fmt.Errorf("readSequence: non item element found in sequence: tag %v, VR %s, VL %d", e.Tag, e.RawValueRepresentation, e.ValueLength)
	}
	return item, nil
}

// readSequenceItem reads an item component of a sequence dicom element and returns an Element
// with a SequenceItem value.
func (r *reader) readSequenceItem(t tag.Tag, vr string, vl uint32, d *Dataset) (Value, error) {
	// Items do not add a nesting level of their own: they are always reached
	// through a sequence, which readSequence counts.
	var sequenceItem SequenceItemValue

	// seqElements holds items read so far.
	// TODO: deduplicate with sequenceItem above
	seqElements := Dataset{}

	if vl == tag.VLUndefinedLength {
		for {
			subElem, err := r.readElement(&seqElements, nil)
			if err != nil {
				return nil, fmt.Errorf("readSequenceItem: error reading subitem in a sequence item: %w", err)
			}
			if subElem.Tag == tag.ItemDelimitationItem {
				break
			}

			sequenceItem.elements = append(sequenceItem.elements, subElem)
			seqElements.Elements = append(seqElements.Elements, subElem)
		}
	} else {
		if err := r.checkScope(int64(vl), fmt.Sprintf("readSequenceItem: item (%v) scope", t)); err != nil {
			return nil, err
		}
		err := r.rawReader.PushLimit(int64(vl))
		if err != nil {
			return nil, err
		}
		defer r.rawReader.PopLimit()

		for !r.rawReader.IsLimitExhausted() {
			subElem, err := r.readElement(&seqElements, nil)
			if err != nil {
				return nil, fmt.Errorf("readSequenceItem: error reading subitem in a sequence item: %w", err)
			}

			sequenceItem.elements = append(sequenceItem.elements, subElem)
			seqElements.Elements = append(seqElements.Elements, subElem)
		}
	}

	return &sequenceItem, nil
}

func (r *reader) readBytes(t tag.Tag, vr string, vl uint32) (Value, error) {
	if vl == tag.VLUndefinedLength {
		return nil, ErrorExpectedDefinedLength
	}

	context := fmt.Sprintf("readBytes: element (%v)", t)
	if err := r.checkValue(int64(vl), context); err != nil {
		return nil, err
	}

	// TODO: add special handling of PixelData
	if vr == vrraw.OtherByte || vr == vrraw.Unknown {
		data, err := r.rawReader.ReadBytes(int64(vl))
		if err != nil {
			return nil, fmt.Errorf("error reading bytes element (%v) value: %w", t, err)
		}
		return &bytesValue{value: data}, nil
	} else if vr == vrraw.OtherWord {
		// OW -> stream of 16 bit words
		if vl%2 != 0 {
			return nil, fmt.Errorf("error reading bytes element (%v) value: %w", t, ErrorOWRequiresEvenVL)
		}

		data, err := r.rawReader.ReadBytes(int64(vl))
		if err != nil {
			return nil, fmt.Errorf("error reading bytes element (%v) value: %w", t, err)
		}
		// Words are kept little endian whatever the transfer syntax.
		// TODO: support bytes.BigEndian byte ordering
		if bo := r.rawReader.ByteOrder(); bo != binary.LittleEndian {
			for i := 0; i+1 < len(data); i += 2 {
				binary.LittleEndian.PutUint16(data[i:], bo.Uint16(data[i:]))
			}
		}
		return &bytesValue{value: data}, nil
	}

	return nil, fmt.Errorf("error reading bytes element (%v): %w", t, ErrorUnsupportedVR)
}

func (r *reader) readString(t tag.Tag, vr string, vl uint32) (Value, error) {
	if vl == tag.VLUndefinedLength {
		return nil, ErrorExpectedDefinedLength
	}
	if err := r.checkValue(int64(vl), fmt.Sprintf("readString: element (%v)", t)); err != nil {
		return nil, err
	}

	str, err := r.rawReader.ReadString(vl)
	if err != nil {
		return nil, fmt.Errorf("error reading string element (%v) value: %w", t, err)
	}
	onlySpaces := true
	for _, char := range str {
		if !unicode.IsSpace(char) {
			onlySpaces = false
		}
	}
	if !onlySpaces {
		// String may have '\0' suffix if its length is odd.
		str = strings.Trim(str, " \000")
	}

	// Split multiple strings
	strs := strings.Split(str, "\\")
	return &stringsValue{value: strs}, nil
}

func (r *reader) readFloat(t tag.Tag, vr string, vl uint32) (Value, error) {
	if err := r.checkValue(int64(vl), fmt.Sprintf("readFloat: element (%v)", t)); err != nil {
		return nil, err
	}
	err := r.rawReader.PushLimit(int64(vl))
	if err != nil {
		return nil, err
	}
	retVal := &floatsValue{value: make([]float64, 0, vl/2)}
	for !r.rawReader.IsLimitExhausted() {
		switch vr {
		case vrraw.FloatingPointSingle:
			val, err := r.rawReader.ReadFloat32()
			if err != nil {
				return nil, fmt.Errorf("error reading floating point element (%v) value: %w", t, err)
			}
			// TODO(suyashkumar): revisit this hack to prevent some internal representation issues upconverting from
			// float32 to float64. There is no loss of precision, but the value gets some additional significant digits
			// when using golang casting. This approach prevents those artifacts, but is less efficient.
			pval, err := strconv.ParseFloat(fmt.Sprint(val), 64)
			if err != nil {
				return nil, fmt.Errorf("error reading floating point element (%v) value during strconv.ParseFloat: %w", t, err)
			}
			retVal.value = append(retVal.value, pval)
			break
		case vrraw.FloatingPointDouble:
			val, err := r.rawReader.ReadFloat64()
			if err != nil {
				return nil, fmt.Errorf("error reading floating point element (%v) value: %w", t, err)
			}
			retVal.value = append(retVal.value, val)
			break
		default:
			return nil, fmt.Errorf("error reading floating point element(%v) value: unsupported VR: %w", t, errorUnableToParseFloat)
		}
	}
	r.rawReader.PopLimit()
	return retVal, nil
}

func (r *reader) readDate(t tag.Tag, vr string, vl uint32) (Value, error) {
	if err := r.checkValue(int64(vl), fmt.Sprintf("readDate: element (%v)", t)); err != nil {
		return nil, err
	}
	rawDate, err := r.rawReader.ReadString(vl)
	if err != nil {
		return nil, fmt.Errorf("error reading date element (%v) value: %w", t, err)
	}
	date := strings.Trim(rawDate, " \000")

	return &stringsValue{value: []string{date}}, nil

}

func (r *reader) readInt(t tag.Tag, vr string, vl uint32) (Value, error) {
	// TODO: add other integer types here
	if err := r.checkValue(int64(vl), fmt.Sprintf("readInt: element (%v)", t)); err != nil {
		return nil, err
	}
	err := r.rawReader.PushLimit(int64(vl))
	if err != nil {
		return nil, err
	}
	retVal := &intsValue{value: make([]int, 0, vl/2)}
	for !r.rawReader.IsLimitExhausted() {
		switch vr {
		case vrraw.UnsignedShort, vrraw.AttributeTag:
			val, err := r.rawReader.ReadUInt16()
			if err != nil {
				return nil, fmt.Errorf("error reading int element (%v) value (ReadUInt16): %w", t, err)
			}
			retVal.value = append(retVal.value, int(val))
			break
		case vrraw.UnsignedLong:
			val, err := r.rawReader.ReadUInt32()
			if err != nil {
				return nil, fmt.Errorf("error reading int element (%v) value (ReadUInt32): %w", t, err)
			}
			retVal.value = append(retVal.value, int(val))
			break
		case vrraw.SignedLong:
			val, err := r.rawReader.ReadInt32()
			if err != nil {
				return nil, fmt.Errorf("error reading int element (%v) value (ReadInt32): %w", t, err)
			}
			retVal.value = append(retVal.value, int(val))
			break
		case vrraw.SignedShort:
			val, err := r.rawReader.ReadInt16()
			if err != nil {
				return nil, fmt.Errorf("error reading int element (%v) value (ReadInt16): %w", t, err)
			}
			retVal.value = append(retVal.value, int(val))
			break
		default:
			return nil, fmt.Errorf("unable to parse integer type due to unknown VR %v", vr)
		}
	}
	r.rawReader.PopLimit()
	return retVal, err
}

// readElement reads the next element. If the next element is a sequence element,
// it may result in a collection of Elements. It takes a pointer to the Dataset of
// elements read so far, since previously read elements may be needed to parse
// certain Elements (like native PixelData). If the Dataset is nil, it is
// treated as an empty Dataset.
func (r *reader) readElement(d *Dataset, fc chan<- *frame.Frame) (*Element, error) {
	t, err := r.readTag()
	if err != nil {
		return nil, fmt.Errorf("readElement: error when reading element tag: %w", err)
	}
	debug.Logf("readElement: tag: %s", t.String())

	readImplicit := r.rawReader.IsImplicit()
	if *t == tag.Item {
		// Always read implicit for item elements
		readImplicit = true
	}

	vr, err := r.readVR(readImplicit, *t)
	if err != nil {
		return nil, fmt.Errorf("readElement: error when reading VR for element %v: %w", t, err)
	}
	debug.Logf("readElement: vr: %s", vr)

	vl, err := r.readVL(readImplicit, *t, vr)
	if err != nil {
		return nil, fmt.Errorf("readElement: error when reading VL for element %v: %w", t, err)
	}
	debug.Logf("readElement: vl: %d", vl)

	val, err := r.readValue(*t, vr, vl, readImplicit, d, fc)
	if err != nil {
		// Inside a sequence or item the error passes up unchanged; only the
		// top-level element wraps it, so a message quoting an element is not
		// re-copied at every level of the (now bounded) nesting.
		if r.budget.depth > 0 {
			return nil, err
		}
		return nil, fmt.Errorf("readElement: error when reading value for element %v: %w", t, err)
	}

	// Charge a small fixed overhead per element for the Element struct and its
	// slot in Dataset.Elements. A long run of zero-length elements is thus
	// bounded by the cumulative budget even though their values cost nothing.
	if err := r.chargeRead(elementOverheadBytes, fmt.Sprintf("readElement: element (%v) overhead", t)); err != nil {
		return nil, err
	}

	return &Element{Tag: *t, ValueRepresentation: tag.GetVRKind(*t, vr), RawValueRepresentation: vr, ValueLength: vl, Value: val}, nil
}

// Read an Item object as raw bytes, useful when parsing encapsulated PixelData.
// This returns the read raw item, an indication if this is the end of the set
// of items, and a possible error.
func (r *reader) readRawItem(shouldSkip bool) ([]byte, bool, error) {
	t, err := r.readTag()
	if err != nil {
		return nil, true, fmt.Errorf("readRawItem: error when reading item tag: %w", err)
	}
	// Item is always encoded implicit. PS3.6 7.5
	vr, err := r.readVR(true, *t)
	if err != nil {
		return nil, true, fmt.Errorf("readRawItem: error when reading VR for item %v: %w", t, err)
	}
	vl, err := r.readVL(true, *t, vr)
	if err != nil {
		return nil, true, fmt.Errorf("readRawItem: error when reading VL for item %v: %w", t, err)
	}

	if *t == tag.SequenceDelimitationItem {
		if vl != 0 {
			log.Printf("SequenceDelimitationItem's VL != 0: %d", vl)
		}
		return nil, true, nil
	}
	if *t != tag.Item {
		// Not an item: report where, and do not pretend the item list ended,
		// which would leave the caller looping without advancing.
		return nil, false, fmt.Errorf("readRawItem: expected item tag at byte offset %d, got %s", r.rawReader.BytesRead(), tag.DebugString(*t))
	}
	if vl == tag.VLUndefinedLength {
		return nil, false, fmt.Errorf("readRawItem: item %v at byte offset %d has undefined length, want a defined length", t, r.rawReader.BytesRead())
	}
	if vr != "NA" {
		return nil, true, fmt.Errorf("readRawItem: expected VR=NA, got VR=%s", vr)
	}

	if shouldSkip {
		if err := r.chargeRead(int64(vl), fmt.Sprintf("readRawItem: skipped item (%v)", t)); err != nil {
			return nil, false, err
		}
		if err := r.rawReader.Skip(int64(vl)); err != nil {
			return nil, false, fmt.Errorf("readRawItem: error when skipping item %v (vl=%d): %w", t, vl, err)
		}
	} else {
		if err := r.checkValue(int64(vl), fmt.Sprintf("readRawItem: item (%v) value", t)); err != nil {
			return nil, false, err
		}
		data, err := r.rawReader.ReadBytes(int64(vl))
		if err != nil {
			return nil, false, fmt.Errorf("readRawItem: error when reading item %v value: %w", t, err)
		}
		return data, false, nil
	}
	return nil, false, nil
}

// moreToRead returns true if there is more to read from the underlying dicom.
func (r *reader) moreToRead() bool {
	return !r.rawReader.IsLimitExhausted()
}
