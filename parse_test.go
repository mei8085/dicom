package dicom_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"

	"github.com/suyashkumar/dicom/pkg/frame"

	"github.com/suyashkumar/dicom"
)

// TestParse is an end-to-end sanity check over DICOMs in testdata/. Currently,
// it only checks that no error is returned when parsing the files.
func TestParse(t *testing.T) {
	files, err := os.ReadDir("./testdata")
	if err != nil {
		t.Fatalf("unable to read testdata/: %v", err)
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".dcm") {
			t.Run(f.Name(), func(t *testing.T) {
				dcm, err := os.Open("./testdata/" + f.Name())
				if err != nil {
					t.Errorf("Unable to open %s. Error: %v", f.Name(), err)
				}
				defer dcm.Close()
				info, err := dcm.Stat()
				if err != nil {
					t.Errorf("Unable to stat %s. Error: %v", f.Name(), err)
				}
				_, err = dicom.Parse(dcm, info.Size(), nil)
				if err != nil {
					t.Errorf("dicom.Parse(%s) unexpected error: %v", f.Name(), err)
				}
			})
		}
	}
}

func TestParseUntilEOF(t *testing.T) {
	files, err := os.ReadDir("./testdata")
	if err != nil {
		t.Fatalf("unable to read testdata/: %v", err)
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".dcm") {
			t.Run(f.Name(), func(t *testing.T) {
				dcm, err := os.Open("./testdata/" + f.Name())
				if err != nil {
					t.Errorf("Unable to open %s. Error: %v", f.Name(), err)
				}
				defer dcm.Close()

				if err != nil {
					t.Errorf("Unable to stat %s. Error: %v", f.Name(), err)
				}
				_, err = dicom.ParseUntilEOF(dcm, nil)
				if err != nil {
					t.Errorf("dicom.Parse(%s) unexpected error: %v", f.Name(), err)
				}
			})
		}
	}
}

// TestNewParserSkipMetadataReadOnNewParserInit tests that NewParser with the SkipMetadataReadOnNewParserInit option
// parses the specified dataset but not its header metadata.
func TestNewParserSkipMetadataReadOnNewParserInit(t *testing.T) {
	fStat, err := os.Stat("./testdata/1.dcm")
	if err != nil {
		t.Fatalf("Unable to stat %s. Error: %v", fStat.Name(), err)
	}

	f, err := os.Open("./testdata/1.dcm")
	if err != nil {
		t.Fatalf("Unable to open %s. Error: %v", f.Name(), err)
	}

	p, err := dicom.NewParser(f, fStat.Size(), nil, dicom.SkipMetadataReadOnNewParserInit())
	if err != nil {
		t.Fatalf("dicom.Parse(%s) unexpected error: %v", f.Name(), err)
	}

	metadata := p.GetMetadata()
	if len(metadata.Elements) > 0 {
		t.Fatalf("Found %d metadata elements despite SkipMetadataReadOnNewParserInit()", len(metadata.Elements))
	}
}

func TestParseFile_SkipPixelData(t *testing.T) {
	t.Run("WithSkipPixelData", func(t *testing.T) {
		runForEveryTestFile(t, func(t *testing.T, filename string) {
			dataset, err := dicom.ParseFile(filename, nil, dicom.SkipPixelData())
			if err != nil {
				t.Errorf("Unexpected error parsing dataset: %v, dataset: %v", err, dataset)
			}
			// If PixelData present in this DICOM, check if it's populated
			// correctly. The current test assumption is that if PixelData is
			// missing, it was not originally in the dicom (which we should
			// consider revisiting).
			el, err := dataset.FindElementByTag(tag.PixelData)
			if err == nil {
				pixelData := dicom.MustGetPixelDataInfo(el.Value)
				if !pixelData.IntentionallySkipped {
					t.Errorf("Expected pixelData.IntentionallySkipped=true, got false")
				}
				if got := len(pixelData.Frames); got != 0 {
					t.Errorf("unexpected frames length. got: %v, want: %v", got, 0)
				}
			}
		})
	})
	t.Run("WithNOSkipPixelData", func(t *testing.T) {
		runForEveryTestFile(t, func(t *testing.T, filename string) {
			dataset, err := dicom.ParseFile(filename, nil)
			if err != nil {
				t.Errorf("Unexpected error parsing dataset: %v, dataset: %v", err, dataset)
			}
			// If PixelData present in this DICOM, check if it's populated
			// correctly. The current test assumption is that if PixelData is
			// missing, it was not originally in the dicom (which we should
			// consider revisiting).
			el, err := dataset.FindElementByTag(tag.PixelData)
			if err == nil {
				pixelData := dicom.MustGetPixelDataInfo(el.Value)
				if pixelData.IntentionallySkipped {
					t.Errorf("Expected pixelData.IntentionallySkipped=false when SkipPixelData option not present, got true")
				}
				if len(pixelData.Frames) == 0 {
					t.Errorf("unexpected frames length when SkipPixelData=false. got: %v, want: >0", len(pixelData.Frames))
				}
			}
		})
	})
}

func TestParseFile_SkipProcessingPixelDataValue(t *testing.T) {
	t.Run("WithSkipProcessingPixelDataValue", func(t *testing.T) {
		runForEveryTestFile(t, func(t *testing.T, filename string) {
			dataset, err := dicom.ParseFile(filename, nil, dicom.SkipProcessingPixelDataValue())
			if err != nil {
				t.Errorf("Unexpected error parsing dataset: %v", dataset)
			}
			el, err := dataset.FindElementByTag(tag.PixelData)
			if err != nil {
				t.Errorf("Unexpected error when finding PixelData in Dataset: %v", err)
			}
			pixelData := dicom.MustGetPixelDataInfo(el.Value)
			if !pixelData.IntentionallyUnprocessed {
				t.Errorf("Expected pixelData.IntentionallyUnprocessed=true, got false")
			}
			if got := len(pixelData.Frames); got != 0 {
				t.Errorf("unexpected frames length. got: %v, want: %v", got, 0)
			}
		})
	})
	t.Run("WithNOSkipProcessingPixelDataValue", func(t *testing.T) {
		runForEveryTestFile(t, func(t *testing.T, filename string) {
			dataset, err := dicom.ParseFile(filename, nil)
			if err != nil {
				t.Errorf("Unexpected error parsing dataset: %v", dataset)
			}
			el, err := dataset.FindElementByTag(tag.PixelData)
			if err != nil {
				t.Errorf("Unexpected error when finding PixelData in Dataset: %v", err)
			}
			pixelData := dicom.MustGetPixelDataInfo(el.Value)
			if pixelData.IntentionallyUnprocessed {
				t.Errorf("Expected pixelData.IntentionallyUnprocessed=false when TestParseFile_SkipProcessingPixelDataValue option not present, got true")
			}
			if len(pixelData.Frames) == 0 {
				t.Errorf("unexpected frames length when TestParseFile_SkipProcessingPixelDataValue=false. got: %v, want: >0", len(pixelData.Frames))
			}
		})
	})
	t.Run("WithAllowErrorMetaElementGroupLength", func(t *testing.T) {
		runForEveryTestFile(t, func(t *testing.T, filename string) {
			dataset, err := dicom.ParseFile(filename, nil, dicom.AllowMissingMetaElementGroupLength())
			if err != nil {
				t.Errorf("Unexpected error parsing dataset: %v", dataset)
			}
		})
	})
}

func TestParseFile_AllowUnknownSpecificCharacterSet(t *testing.T) {
	file, err := os.CreateTemp("", "unknown_specific_character_set.dcm")
	if err != nil {
		t.Fatalf("Unexpected error when creating tempfile: %v", err)
	}

	transferSyntaxUIDElement, err := dicom.NewElement(tag.TransferSyntaxUID, []string{uid.ImplicitVRLittleEndian})
	if err != nil {
		t.Fatalf("Unexpected error when creating transfer syntax uid element: %v", err)
	}
	specificCharacterSetElement, err := dicom.NewElement(tag.SpecificCharacterSet, []string{"UNKNOWN"})
	if err != nil {
		t.Fatalf("Unexpected error when creating specific character set element: %v", err)
	}
	patientNameElement, err := dicom.NewElement(tag.PatientName, []string{"Bob", "Jones"})
	if err != nil {
		t.Fatalf("Unexpected error when creating patient name element: %v", err)
	}
	unknownCharacterSetDataset := dicom.Dataset{Elements: []*dicom.Element{
		transferSyntaxUIDElement,
		specificCharacterSetElement,
		patientNameElement,
	}}
	err = dicom.Write(file, unknownCharacterSetDataset)
	if err != nil {
		t.Errorf("Unexpected error writing dataset: %v", unknownCharacterSetDataset)
	}
	file.Close()

	t.Run("WithoutAllowUnknownSpecificCharacterSet", func(t *testing.T) {
		dataset, err := dicom.ParseFile(file.Name(), nil)
		if err == nil {
			t.Errorf("Expected error parsing dataset: %v", dataset)
		}
	})
	t.Run("WithAllowUnknownSpecificCharacterSet", func(t *testing.T) {
		dataset, err := dicom.ParseFile(file.Name(), nil, dicom.AllowUnknownSpecificCharacterSet())
		if err != nil {
			t.Errorf("Unexpected error parsing dataset: %v", dataset)
		}
	})
}

// BenchmarkParse runs sanity benchmarks over the sample files in testdata.
func BenchmarkParse(b *testing.B) {
	cases := []struct {
		name string
		opts []dicom.ParseOption
	}{
		{
			name: "NoOptions",
		},
		{
			name: "SkipPixelData",
			opts: []dicom.ParseOption{dicom.SkipPixelData()},
		},
		{
			name: "SkipProcessingPixelDataValue",
			opts: []dicom.ParseOption{dicom.SkipProcessingPixelDataValue()},
		},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			files, err := os.ReadDir("./testdata")
			if err != nil {
				b.Fatalf("unable to read testdata/: %v", err)
			}
			for _, f := range files {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".dcm") {
					b.Run(f.Name(), func(b *testing.B) {
						dcm, err := os.Open("./testdata/" + f.Name())
						if err != nil {
							b.Errorf("Unable to open %s. Error: %v", f.Name(), err)
						}
						defer dcm.Close()

						data, err := io.ReadAll(dcm)
						if err != nil {
							b.Errorf("Unable to read file into memory for benchmark: %v", err)
						}

						b.ResetTimer()

						for i := 0; i < b.N; i++ {
							_, _ = dicom.Parse(bytes.NewBuffer(data), int64(len(data)), nil, tc.opts...)
						}
					})
				}
			}
		})
	}
}

func BenchmarkParser_NextAPI(b *testing.B) {
	files, err := os.ReadDir("./testdata")
	if err != nil {
		b.Fatalf("unable to read testdata/: %v", err)
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".dcm") {
			b.Run(f.Name(), func(b *testing.B) {
				dcm, err := os.Open("./testdata/" + f.Name())
				if err != nil {
					b.Errorf("Unable to open %s. Error: %v", f.Name(), err)
				}
				defer dcm.Close()

				data, err := io.ReadAll(dcm)
				if err != nil {
					b.Errorf("Unable to read file into memory for benchmark: %v", err)
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r := bytes.NewReader(data)
					p, _ := dicom.NewParser(r, int64(len(data)), nil)
					var err error
					for err == nil {
						_, err = p.Next()
					}
				}
			})
		}
	}
}

func Example_readFile() {
	// See also: dicom.Parse, which uses a more generic io.Reader API.
	dataset, _ := dicom.ParseFile("testdata/1.dcm", nil)

	// Dataset will nicely print the DICOM dataset data out of the box.
	fmt.Println(dataset)

	// Dataset is also JSON serializable out of the box.
	j, _ := json.Marshal(dataset)
	fmt.Println(j)
}

func Example_streamingFrames() {
	frameChan := make(chan *frame.Frame)

	// Go routine to handle streaming frames as they are parsed. This may be
	// useful when parsing a large DICOM with many frames from a network source,
	// where image frames can start to be processed before the entire DICOM
	// is parsed (or even read from storage).
	go func() {
		for fr := range frameChan {
			fmt.Println(fr)
		}
	}()

	dataset, _ := dicom.ParseFile("testdata/1.dcm", frameChan)
	fmt.Println(dataset) // The full dataset
}

func Example_getImageFrames() {
	dataset, _ := dicom.ParseFile("testdata/1.dcm", nil)
	pixelDataElement, _ := dataset.FindElementByTag(tag.PixelData)
	pixelDataInfo := dicom.MustGetPixelDataInfo(pixelDataElement.Value)
	for i, fr := range pixelDataInfo.Frames {
		img, _ := fr.GetImage() // The Go image.Image for this frame
		f, _ := os.Create(fmt.Sprintf("image_%d.jpg", i))
		_ = jpeg.Encode(f, img, &jpeg.Options{Quality: 100})
		_ = f.Close()
	}
}

func runForEveryTestFile(t *testing.T, testFunc func(t *testing.T, filename string)) {
	files, err := os.ReadDir("./testdata")
	if err != nil {
		t.Fatalf("unable to read testdata/: %v", err)
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".dcm") {
			fullName := "./testdata/" + f.Name()
			t.Run(fullName, func(t *testing.T) {
				testFunc(t, fullName)
			})
		}
	}
}

// writeImplicitTag appends a little-endian tag and 4-byte VL (implicit VR
// element header) to buf.
func writeImplicitTag(buf *bytes.Buffer, tg tag.Tag, vl uint32) {
	_ = binary.Write(buf, binary.LittleEndian, tg.Group)
	_ = binary.Write(buf, binary.LittleEndian, tg.Element)
	_ = binary.Write(buf, binary.LittleEndian, vl)
}

// buildImplicitLEHeader builds a DICOM preamble and group 2 metadata
// declaring the Implicit VR Little Endian transfer syntax.
func buildImplicitLEHeader() []byte {
	meta := &bytes.Buffer{}
	// (0002,0010) TransferSyntaxUID = Implicit VR Little Endian.
	ts := "1.2.840.10008.1.2"
	if len(ts)%2 != 0 {
		ts += "\x00"
	}
	_ = binary.Write(meta, binary.LittleEndian, uint16(0x0002))
	_ = binary.Write(meta, binary.LittleEndian, uint16(0x0010))
	meta.WriteString("UI")
	_ = binary.Write(meta, binary.LittleEndian, uint16(len(ts)))
	meta.WriteString(ts)

	buf := &bytes.Buffer{}
	buf.Write(make([]byte, 128)) // preamble
	buf.WriteString("DICM")
	// (0002,0000) FileMetaInformationGroupLength.
	_ = binary.Write(buf, binary.LittleEndian, uint16(0x0002))
	_ = binary.Write(buf, binary.LittleEndian, uint16(0x0000))
	buf.WriteString("UL")
	_ = binary.Write(buf, binary.LittleEndian, uint16(4))
	_ = binary.Write(buf, binary.LittleEndian, uint32(meta.Len()))
	buf.Write(meta.Bytes())
	return buf.Bytes()
}

// buildMixedLengthNesting builds an implicit VR little endian DICOM body with
// sequences and items nested with interleaved defined and undefined lengths:
//
//	ReferencedImageSequence (undefined length)
//	  Item (defined length)
//	    ReferencedStudySequence (defined length)
//	      Item (undefined length)
//	        PatientName = "Bob "
//	        Item Delimitation Item
//	    PixelData (undefined length, encapsulated, one frame {1,2,3,4})
//	  Sequence Delimitation Item
func buildMixedLengthNesting() []byte {
	// Innermost item: PatientName, closed by an item delimitation item.
	innerItem := &bytes.Buffer{}
	writeImplicitTag(innerItem, tag.PatientName, 4)
	innerItem.WriteString("Bob ")
	writeImplicitTag(innerItem, tag.ItemDelimitationItem, 0)

	// ReferencedStudySequence with a defined length.
	studySeq := &bytes.Buffer{}
	writeImplicitTag(studySeq, tag.Item, tag.VLUndefinedLength)
	studySeq.Write(innerItem.Bytes())

	// Encapsulated PixelData of undefined length with one frame.
	pixelData := &bytes.Buffer{}
	writeImplicitTag(pixelData, tag.Item, 0) // basic offset table
	writeImplicitTag(pixelData, tag.Item, 4)
	pixelData.Write([]byte{1, 2, 3, 4})
	writeImplicitTag(pixelData, tag.SequenceDelimitationItem, 0)

	// Item of the outer sequence with a defined length.
	outerItemContents := &bytes.Buffer{}
	writeImplicitTag(outerItemContents, tag.ReferencedStudySequence, uint32(studySeq.Len()))
	outerItemContents.Write(studySeq.Bytes())
	writeImplicitTag(outerItemContents, tag.PixelData, tag.VLUndefinedLength)
	outerItemContents.Write(pixelData.Bytes())

	// Outermost sequence with an undefined length.
	out := &bytes.Buffer{}
	writeImplicitTag(out, tag.ReferencedImageSequence, tag.VLUndefinedLength)
	writeImplicitTag(out, tag.Item, uint32(outerItemContents.Len()))
	out.Write(outerItemContents.Bytes())
	writeImplicitTag(out, tag.SequenceDelimitationItem, 0)

	return append(buildImplicitLEHeader(), out.Bytes()...)
}

// buildDeepImplicitNesting builds depth levels of nested undefined-length
// implicit VR sequences and items, with a PatientName at the innermost level.
func buildDeepImplicitNesting(depth int) []byte {
	buf := &bytes.Buffer{}
	buf.Write(buildImplicitLEHeader())
	for i := 0; i < depth; i++ {
		writeImplicitTag(buf, tag.ReferencedImageSequence, tag.VLUndefinedLength)
		writeImplicitTag(buf, tag.Item, tag.VLUndefinedLength)
	}
	writeImplicitTag(buf, tag.PatientName, 4)
	buf.WriteString("Bob ")
	for i := 0; i < depth; i++ {
		writeImplicitTag(buf, tag.ItemDelimitationItem, 0)
		writeImplicitTag(buf, tag.SequenceDelimitationItem, 0)
	}
	return buf.Bytes()
}

// verifyMixedLengthNesting checks that the dataset produced by parsing
// buildMixedLengthNesting fully reconstructs the nested items and pixel data.
func verifyMixedLengthNesting(t *testing.T, ds dicom.Dataset) {
	t.Helper()

	seqElem, err := ds.FindElementByTag(tag.ReferencedImageSequence)
	if err != nil {
		t.Fatalf("unable to find ReferencedImageSequence: %v", err)
	}
	items := seqElem.Value.GetValue().([]*dicom.SequenceItemValue)
	if len(items) != 1 {
		t.Fatalf("expected 1 item in ReferencedImageSequence, got %d", len(items))
	}
	outerElems := items[0].GetValue().([]*dicom.Element)
	if len(outerElems) != 2 {
		t.Fatalf("expected 2 elements in outer item, got %d", len(outerElems))
	}

	// The nested defined-length sequence and its undefined-length item.
	studySeq := outerElems[0]
	if studySeq.Tag != tag.ReferencedStudySequence {
		t.Fatalf("expected ReferencedStudySequence, got %v", studySeq.Tag)
	}
	studyItems := studySeq.Value.GetValue().([]*dicom.SequenceItemValue)
	if len(studyItems) != 1 {
		t.Fatalf("expected 1 item in ReferencedStudySequence, got %d", len(studyItems))
	}
	studyElems := studyItems[0].GetValue().([]*dicom.Element)
	if len(studyElems) != 1 || studyElems[0].Tag != tag.PatientName {
		t.Fatalf("expected PatientName in nested item, got %v", studyElems)
	}
	if got := dicom.MustGetStrings(studyElems[0].Value)[0]; got != "Bob" {
		t.Errorf("unexpected PatientName: got %q, want %q", got, "Bob")
	}

	// The encapsulated pixel data nested in the same item.
	pixelElem := outerElems[1]
	if pixelElem.Tag != tag.PixelData {
		t.Fatalf("expected PixelData, got %v", pixelElem.Tag)
	}
	pixelData := dicom.MustGetPixelDataInfo(pixelElem.Value)
	if !pixelData.IsEncapsulated {
		t.Errorf("expected encapsulated pixel data")
	}
	if len(pixelData.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(pixelData.Frames))
	}
	if got := pixelData.Frames[0].EncapsulatedData.Data; !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Errorf("unexpected frame data: got %v, want [1 2 3 4]", got)
	}
}

// TestParse_MixedLengthNestingMaxDepth verifies that legal nesting with
// interleaved defined and undefined lengths is fully reconstructed within a
// depth budget, that exceeding the budget yields a recognizable error, and
// that a failed parse does not affect parsing subsequent independent inputs.
func TestParse_MixedLengthNestingMaxDepth(t *testing.T) {
	data := buildMixedLengthNesting()

	t.Run("within budget parses fully", func(t *testing.T) {
		ds, err := dicom.ParseUntilEOF(bytes.NewReader(data), nil,
			dicom.MaxNestedElementDepth(4))
		if err != nil {
			t.Fatalf("ParseUntilEOF returned unexpected error: %v", err)
		}
		verifyMixedLengthNesting(t, ds)
	})

	t.Run("default has no depth limit", func(t *testing.T) {
		ds, err := dicom.ParseUntilEOF(bytes.NewReader(data), nil)
		if err != nil {
			t.Fatalf("ParseUntilEOF returned unexpected error: %v", err)
		}
		verifyMixedLengthNesting(t, ds)
	})

	t.Run("budget exceeded gives recognizable error", func(t *testing.T) {
		_, err := dicom.ParseUntilEOF(bytes.NewReader(data), nil,
			dicom.MaxNestedElementDepth(3))
		if !errors.Is(err, dicom.ErrorExceedsMaxNestedDepth) {
			t.Fatalf("expected ErrorExceedsMaxNestedDepth, got: %v", err)
		}
	})

	t.Run("failed parse does not affect subsequent parses", func(t *testing.T) {
		deep := buildDeepImplicitNesting(1000)
		_, err := dicom.ParseUntilEOF(bytes.NewReader(deep), nil,
			dicom.MaxNestedElementDepth(8))
		if !errors.Is(err, dicom.ErrorExceedsMaxNestedDepth) {
			t.Fatalf("expected ErrorExceedsMaxNestedDepth, got: %v", err)
		}

		ds, err := dicom.ParseUntilEOF(bytes.NewReader(data), nil,
			dicom.MaxNestedElementDepth(8))
		if err != nil {
			t.Fatalf("ParseUntilEOF after a failed parse returned unexpected error: %v", err)
		}
		verifyMixedLengthNesting(t, ds)
	})
}
