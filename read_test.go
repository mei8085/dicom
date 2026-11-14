package dicom

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/suyashkumar/dicom/pkg/vrraw"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/frame"

	"github.com/google/go-cmp/cmp"

	"github.com/suyashkumar/dicom/pkg/tag"
)

func TestReadTag(t *testing.T) {
	cases := []struct {
		name    string
		data    []byte
		wantTag tag.Tag
		wantErr error
	}{
		{
			name:    "basic",
			data:    buildTagData(t, tag.Rows),
			wantTag: tag.Rows,
			wantErr: nil,
		},
		{
			name:    "custom",
			data:    buildTagData(t, tag.Tag{0x0011, 0x0010}),
			wantTag: tag.Tag{0x0011, 0x0010},
			wantErr: nil,
		},
		{
			name:    "expected EOF on group read",
			data:    []byte{0x1},
			wantErr: io.EOF,
		},
		{
			name:    "expected EOF on element read",
			data:    []byte{0x1, 0x2},
			wantErr: io.EOF,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.NewBuffer(tc.data)

			r := &reader{
				rawReader: dicomio.NewReader(bufio.NewReader(data), binary.LittleEndian, int64(data.Len())),
			}
			gotTag, err := r.readTag()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("TestReadTag: unexpected err. got: %v, want: %v", err, tc.wantErr)
			}

			if gotTag != nil && !gotTag.Equals(tc.wantTag) {
				t.Errorf("TestReadTag: unexpected output. got: %v, want: %v", gotTag, tc.wantTag)
			}
		})
	}

}

func TestReadFloat_float64(t *testing.T) {
	cases := []struct {
		name        string
		floats      []float64
		VR          string
		want        Value
		expectedErr error
	}{
		{
			name:        "float64",
			floats:      []float64{20.1, 32.22},
			VR:          vrraw.FloatingPointDouble,
			want:        &floatsValue{value: []float64{20.1, 32.22}},
			expectedErr: nil,
		},
		{
			name:        "float64 with wrong VR",
			floats:      []float64{20.1, 32.22},
			VR:          "XX",
			want:        nil,
			expectedErr: errorUnableToParseFloat,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Buffer{}
			for _, fl := range tc.floats {
				if err := binary.Write(&data, binary.LittleEndian, fl); err != nil {
					t.Errorf("TestReadFloat: Unable to setup test buffer")
				}
			}

			r := &reader{
				rawReader: dicomio.NewReader(bufio.NewReader(&data), binary.LittleEndian, int64(data.Len())),
			}
			got, err := r.readFloat(tag.Tag{}, tc.VR, uint32(data.Len()))
			if !errors.Is(err, tc.expectedErr) {
				t.Fatalf("readFloat(r, tg, %s, %d) got unexpected error: got: %v, want: %v", tc.VR, data.Len(), err, tc.expectedErr)
			}
			if diff := cmp.Diff(got, tc.want, cmp.AllowUnexported(floatsValue{})); diff != "" {
				t.Errorf("readFloat(r, tg, %s, %d) unexpected diff: %s", tc.VR, data.Len(), diff)
			}
		})
	}
}

func TestReadFloat_float32(t *testing.T) {
	cases := []struct {
		name        string
		floats      []float32
		VR          string
		want        Value
		expectedErr error
	}{
		{
			name:        "float32",
			floats:      []float32{20.1001, 32.22},
			VR:          vrraw.FloatingPointSingle,
			want:        &floatsValue{value: []float64{20.1001, 32.22}},
			expectedErr: nil,
		},
		{
			name:        "float32 with wrong VR",
			floats:      []float32{20.1001, 32.22},
			VR:          "XX",
			want:        nil,
			expectedErr: errorUnableToParseFloat,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Buffer{}
			for _, fl := range tc.floats {
				if err := binary.Write(&data, binary.LittleEndian, fl); err != nil {
					t.Errorf("TestReadFloat: Unable to setup test buffer")
				}
			}

			r := &reader{
				rawReader: dicomio.NewReader(bufio.NewReader(&data), binary.LittleEndian, int64(data.Len())),
			}
			got, err := r.readFloat(tag.Tag{}, tc.VR, uint32(data.Len()))
			if !errors.Is(err, tc.expectedErr) {
				t.Fatalf("readFloat(r, tg, %s, %d) got unexpected error: got: %v, want: %v", tc.VR, data.Len(), err, tc.expectedErr)
			}
			if diff := cmp.Diff(got, tc.want, cmp.AllowUnexported(floatsValue{})); diff != "" {
				t.Errorf("readFloat(r, tg, %s, %d) unexpected diff: %s", tc.VR, data.Len(), diff)
			}
		})
	}
}

func TestReadOWBytes(t *testing.T) {
	cases := []struct {
		name        string
		bytes       []byte
		VR          string
		want        Value
		expectedErr error
	}{
		{
			name:        "OW VR with even-number bytes",
			bytes:       []byte{0x1, 0x2, 0x3, 0x4},
			VR:          vrraw.OtherWord,
			want:        &bytesValue{value: []byte{0x1, 0x2, 0x3, 0x4}},
			expectedErr: nil,
		},
		{
			name:        "UN VR even-number bytes",
			bytes:       []byte{0x1, 0x2, 0x3, 0x4},
			VR:          vrraw.Unknown,
			want:        &bytesValue{value: []byte{0x1, 0x2, 0x3, 0x4}},
			expectedErr: nil,
		},
		{
			name:        "error on odd-number bytes",
			bytes:       []byte{0x1, 0x2, 0x3},
			VR:          vrraw.OtherWord,
			want:        nil,
			expectedErr: ErrorOWRequiresEvenVL,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Buffer{}
			if err := binary.Write(&data, binary.LittleEndian, tc.bytes); err != nil {
				t.Errorf("TestReadOWBytes: Unable to setup test buffer")
			}

			r := &reader{rawReader: dicomio.NewReader(bufio.NewReader(&data), binary.LittleEndian, int64(data.Len()))}

			got, err := r.readBytes(tag.Tag{}, tc.VR, uint32(data.Len()))
			if !errors.Is(err, tc.expectedErr) {
				t.Fatalf("readBytes(r, tg, %s, %d) got unexpected error: got: %v, want: %v", tc.VR, data.Len(), err, tc.expectedErr)
			}
			if diff := cmp.Diff(got, tc.want, cmp.AllowUnexported(bytesValue{})); diff != "" {
				t.Errorf("readBytes(r, tg, %s, %d) unexpected diff: %s", tc.VR, data.Len(), diff)
			}
		})
	}
}

func TestReadUndefinedLengthByteValue(t *testing.T) {
	cases := []struct {
		name        string
		bytes       []byte
		want        Value
		expectedErr error
	}{
		{
			name: "happy path",
			bytes: []byte{
				0xfe, 0xff, 0x00, 0xe0, // item tag (fffe, e000)
				0x04, 0x00, 0x00, 0x00, // length 4
				0x01, 0x02, 0x03, 0x04, // data (4 bytes)
				0xfe, 0xff, 0xdd, 0xe0, // sequence delimitation item tag (fffe, e0dd)
				0x00, 0x00, 0x00, 0x00, // length 0 (required)
			},
			want:        &bytesValue{value: []byte{0x01, 0x02, 0x03, 0x04}},
			expectedErr: nil,
		},
		{
			name: "not formatted as sequence",
			bytes: []byte{
				0x01, 0x02, 0x03, 0x04, // data (4 bytes)
			},
			want:        &bytesValue{value: []byte{}},
			expectedErr: io.EOF,
		},
		{
			name: "missing delimitation item",
			bytes: []byte{
				0xfe, 0xff, 0x00, 0xe0, // item tag (fffe, e000)
				0x04, 0x00, 0x00, 0x00, // length 4
				0x01, 0x02, 0x03, 0x04, // data (4 bytes)
			},
			want:        &bytesValue{value: []byte{}},
			expectedErr: ErrorExpectedSequenceDelimitation,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Buffer{}
			if err := binary.Write(&data, binary.LittleEndian, tc.bytes); err != nil {
				t.Errorf("TestReadOWBytes: Unable to setup test buffer")
			}

			r := &reader{rawReader: dicomio.NewReader(bufio.NewReader(&data), binary.LittleEndian, int64(data.Len()))}

			got, err := r.readUndefinedLengthByteValue()
			if tc.expectedErr != nil {
				if !errors.Is(err, tc.expectedErr) {
					t.Fatalf("readUndefinedLengthByteValue got unexpected error: got: %v, want: %v", err, tc.expectedErr)
				}
			} else if diff := cmp.Diff(got, tc.want, cmp.AllowUnexported(bytesValue{})); diff != "" {
				t.Errorf("readUndefinedLengthByteValue unexpected diff: %s", diff)
			}
		})
	}
}

func TestReadNativeFrames(t *testing.T) {
	cases := []struct {
		Name              string
		existingData      Dataset
		uint16Data        []uint16
		dataBytes         []byte
		uint32Data        []uint32
		expectedPixelData *PixelDataInfo
		expectedError     error
		pixelVLOverride   uint32
		parseOptSet       parseOptSet
	}{
		{
			Name: "5x5, 1 frame, 1 samples/pixel, bitsAllocated=16",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{5}),
				mustNewElement(tag.Columns, []int{5}),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				mustNewElement(tag.BitsAllocated, []int{16}),
				mustNewElement(tag.SamplesPerPixel, []int{1}),
			}},
			uint16Data: []uint16{1, 2, 3, 4, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			expectedPixelData: &PixelDataInfo{
				IsEncapsulated: false,
				Frames: []*frame.Frame{
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint16]{
							InternalBitsPerSample:   16,
							InternalRows:            5,
							InternalCols:            5,
							InternalSamplesPerPixel: 1,
							RawData:                 []uint16{1, 2, 3, 4, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
						},
					},
				},
			},
			expectedError: nil,
		},
		{
			Name: "2x2, 3 frames, 1 samples/pixel, bitsAllocated=16",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{2}),
				mustNewElement(tag.Columns, []int{2}),
				mustNewElement(tag.NumberOfFrames, []string{"3"}),
				mustNewElement(tag.BitsAllocated, []int{16}),
				mustNewElement(tag.SamplesPerPixel, []int{1}),
			}},
			uint16Data: []uint16{1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3, 0},
			expectedPixelData: &PixelDataInfo{
				IsEncapsulated: false,
				Frames: []*frame.Frame{
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint16]{
							InternalBitsPerSample:   16,
							InternalRows:            2,
							InternalCols:            2,
							InternalSamplesPerPixel: 1,
							RawData:                 []uint16{1, 2, 3, 2},
						},
					},
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint16]{
							InternalBitsPerSample:   16,
							InternalRows:            2,
							InternalCols:            2,
							InternalSamplesPerPixel: 1,
							RawData:                 []uint16{1, 2, 3, 2},
						},
					},
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint16]{
							InternalBitsPerSample:   16,
							InternalRows:            2,
							InternalCols:            2,
							InternalSamplesPerPixel: 1,
							RawData:                 []uint16{1, 2, 3, 0},
						},
					},
				},
			},
			expectedError: nil,
		},
		{
			Name: "2x2, 2 frames, 2 samples/pixel, bitsAllocated=16",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{2}),
				mustNewElement(tag.Columns, []int{2}),
				mustNewElement(tag.NumberOfFrames, []string{"2"}),
				mustNewElement(tag.BitsAllocated, []int{16}),
				mustNewElement(tag.SamplesPerPixel, []int{2}),
			}},
			uint16Data: []uint16{1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3, 5},
			expectedPixelData: &PixelDataInfo{
				IsEncapsulated: false,
				Frames: []*frame.Frame{
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint16]{
							InternalBitsPerSample:   16,
							InternalRows:            2,
							InternalCols:            2,
							InternalSamplesPerPixel: 2,
							RawData:                 []uint16{1, 2, 3, 2, 1, 2, 3, 2},
						},
					},
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint16]{
							InternalBitsPerSample:   16,
							InternalRows:            2,
							InternalCols:            2,
							InternalSamplesPerPixel: 2,
							RawData:                 []uint16{1, 2, 3, 2, 1, 2, 3, 5},
						},
					},
				},
			},
			expectedError: nil,
		},
		{
			Name: "bitsAllocated=32",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{5}),
				mustNewElement(tag.Columns, []int{5}),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				mustNewElement(tag.BitsAllocated, []int{32}),
				mustNewElement(tag.SamplesPerPixel, []int{1}),
			}},
			uint32Data: []uint32{1, 2, 3, 4, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			expectedPixelData: &PixelDataInfo{
				IsEncapsulated: false,
				Frames: []*frame.Frame{
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint32]{
							InternalBitsPerSample:   32,
							InternalRows:            5,
							InternalCols:            5,
							InternalSamplesPerPixel: 1,
							RawData:                 []uint32{1, 2, 3, 4, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
						},
					},
				},
			},
			expectedError: nil,
		},
		{
			Name: "insufficient bytes, uint32",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{2}),
				mustNewElement(tag.Columns, []int{2}),
				mustNewElement(tag.NumberOfFrames, []string{"2"}),
				mustNewElement(tag.BitsAllocated, []int{32}),
				mustNewElement(tag.SamplesPerPixel, []int{2}),
			}},
			uint16Data:        []uint16{1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3},
			expectedPixelData: nil,
			expectedError:     ErrorMismatchPixelDataLength,
		},
		{
			Name: "redundant bytes, uint32",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{2}),
				mustNewElement(tag.Columns, []int{2}),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				mustNewElement(tag.BitsAllocated, []int{32}),
				mustNewElement(tag.SamplesPerPixel, []int{2}),
			}},
			uint16Data:        []uint16{1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3, 2, 2},
			expectedPixelData: nil,
			expectedError:     ErrorMismatchPixelDataLength,
		},
		{
			Name: "redundant bytes, uint32 with allowing mismatch length",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{2}),
				mustNewElement(tag.Columns, []int{2}),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				mustNewElement(tag.BitsAllocated, []int{32}),
				mustNewElement(tag.SamplesPerPixel, []int{2}),
			}},
			uint16Data: []uint16{1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3, 2, 1, 2, 3, 2, 2},
			expectedPixelData: &PixelDataInfo{
				ParseErr: ErrorMismatchPixelDataLength,
				Frames: []*frame.Frame{
					{
						EncapsulatedData: frame.EncapsulatedFrame{
							Data: []byte{1, 0, 2, 0, 3, 0, 2, 0, 1, 0, 2, 0, 3, 0, 2, 0, 1, 0, 2, 0, 3, 0, 2, 0, 1, 0, 2, 0, 3, 0, 2, 0, 2, 0},
						},
					},
				},
			},
			parseOptSet:   parseOptSet{allowMismatchPixelDataLength: true},
			expectedError: nil,
		},
		{
			Name: "missing Columns",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{5}),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				mustNewElement(tag.BitsAllocated, []int{16}),
				mustNewElement(tag.SamplesPerPixel, []int{1}),
			}},
			uint16Data:        []uint16{1, 2, 3, 4, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			expectedPixelData: nil,
			expectedError:     ErrorElementNotFound,
		},
		{
			Name: "unsupported BitsAllocated",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{5}),
				mustNewElement(tag.Columns, []int{2}),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				mustNewElement(tag.BitsAllocated, []int{24}),
				mustNewElement(tag.SamplesPerPixel, []int{1}),
			}},
			uint16Data:        []uint16{1, 2, 3, 4, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			expectedPixelData: nil,
			expectedError:     ErrorUnsupportedBitsAllocated,
		},
		{
			Name: "3x3, 3 frames, 1 samples/pixel, bytes data (uint8) with padded 0",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{3}),
				mustNewElement(tag.Columns, []int{3}),
				mustNewElement(tag.NumberOfFrames, []string{"3"}),
				mustNewElement(tag.BitsAllocated, []int{8}),
				mustNewElement(tag.SamplesPerPixel, []int{1}),
			}},
			dataBytes: []byte{11, 12, 13, 21, 22, 23, 31, 32, 33, 11, 12, 13, 21, 22, 23, 31, 32, 33, 11, 12, 13, 21, 22, 23, 31, 32, 33, 0}, // there is a 28th byte to make total value length even, as required by DICOM spec
			expectedPixelData: &PixelDataInfo{
				IsEncapsulated: false,
				Frames: []*frame.Frame{
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint8]{
							InternalBitsPerSample:   8,
							InternalRows:            3,
							InternalCols:            3,
							InternalSamplesPerPixel: 1,
							RawData:                 []uint8{11, 12, 13, 21, 22, 23, 31, 32, 33},
						},
					},

					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint8]{
							InternalBitsPerSample:   8,
							InternalRows:            3,
							InternalCols:            3,
							InternalSamplesPerPixel: 1,
							RawData:                 []uint8{11, 12, 13, 21, 22, 23, 31, 32, 33},
						},
					},
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint8]{
							InternalBitsPerSample:   8,
							InternalRows:            3,
							InternalCols:            3,
							InternalSamplesPerPixel: 1,
							RawData:                 []uint8{11, 12, 13, 21, 22, 23, 31, 32, 33},
						},
					},
				},
			},
			expectedError: nil,
		},
		{
			Name: "1x1, 3 frames, 3 samples/pixel, bytes data (uint8) with padded 0",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{1}),
				mustNewElement(tag.Columns, []int{1}),
				mustNewElement(tag.NumberOfFrames, []string{"3"}),
				mustNewElement(tag.BitsAllocated, []int{8}),
				mustNewElement(tag.SamplesPerPixel, []int{3}),
			}},
			dataBytes: []byte{1, 2, 3, 1, 2, 3, 1, 2, 3, 0}, // 10th byte to make total value length even
			expectedPixelData: &PixelDataInfo{
				IsEncapsulated: false,
				Frames: []*frame.Frame{
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint8]{
							InternalBitsPerSample:   8,
							InternalRows:            1,
							InternalCols:            1,
							InternalSamplesPerPixel: 3,
							RawData:                 []uint8{1, 2, 3},
						},
					},
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint8]{
							InternalBitsPerSample:   8,
							InternalRows:            1,
							InternalCols:            1,
							InternalSamplesPerPixel: 3,
							RawData:                 []uint8{1, 2, 3},
						},
					},
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[uint8]{
							InternalBitsPerSample:   8,
							InternalRows:            1,
							InternalCols:            1,
							InternalSamplesPerPixel: 3,
							RawData:                 []uint8{1, 2, 3},
						},
					},
				},
			},
			expectedError: nil,
		},
		{
			Name: "1x1, 2 frames, 3 samples/pixel, bad pixel length",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{1}),
				mustNewElement(tag.Columns, []int{1}),
				mustNewElement(tag.NumberOfFrames, []string{"2"}),
				mustNewElement(tag.BitsAllocated, []int{8}),
				mustNewElement(tag.SamplesPerPixel, []int{3}),
			}},
			dataBytes:         []byte{1, 2, 3, 1, 2, 3},
			expectedPixelData: nil,
			pixelVLOverride:   7,
			expectedError:     ErrorExpectedEvenLength,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			dcmdata := bytes.Buffer{}
			var expectedBytes int

			if len(tc.dataBytes) != 0 {
				// writing byte-by-byte
				expectedBytes = len(tc.dataBytes)
				for _, item := range tc.dataBytes {
					if err := binary.Write(&dcmdata, binary.LittleEndian, item); err != nil {
						t.Errorf("TestReadNativeFrames: Unable to setup test buffer")
					}
				}
			} else if len(tc.uint16Data) != 0 {
				// writing 2 bytes (uint16) at a time
				expectedBytes = len(tc.uint16Data) * 2
				for _, item := range tc.uint16Data {
					if err := binary.Write(&dcmdata, binary.LittleEndian, item); err != nil {
						t.Errorf("TestReadNativeFrames: Unable to setup test buffer")
					}
				}
			} else if len(tc.uint32Data) != 0 {
				expectedBytes = len(tc.uint32Data) * 4
				for _, item := range tc.uint32Data {
					if err := binary.Write(&dcmdata, binary.LittleEndian, item); err != nil {
						t.Errorf("TestReadNativeFrames: Unable to setup test buffer")
					}
				}
			}

			var vl uint32
			if tc.pixelVLOverride > 0 {
				vl = tc.pixelVLOverride
			} else {
				vl = uint32(dcmdata.Len())
			}

			r := &reader{
				rawReader: dicomio.NewReader(bufio.NewReader(&dcmdata), binary.LittleEndian, int64(dcmdata.Len())),
				opts:      tc.parseOptSet,
			}

			pixelData, bytesRead, err := r.readNativeFrames(&tc.existingData, nil, vl)
			if !errors.Is(err, tc.expectedError) {
				t.Errorf("TestReadNativeFrames(%+v): did not get expected error. got: %v, want: %v", tc, err, tc.expectedError)
			}
			if err == nil && bytesRead != expectedBytes {
				t.Errorf("TestReadNativeFrames(%+v): did not read expected number of bytes. got: %d, want: %d", tc, bytesRead, expectedBytes)
			}

			if diff := cmp.Diff(tc.expectedPixelData, pixelData, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("TestReadNativeFrames(%+v): unexpected diff: %v", tc, diff)
			}
		})
	}
}

func TestReadPixelData_SkipPixelData(t *testing.T) {
	cases := []struct {
		name string
		vl   uint32
		data []byte
	}{
		{
			name: "NativePixelData",
			vl:   6,
			data: []byte{1, 2, 3, 4, 5, 6},
		},
		{
			name: "EncapsulatedPixelData",
			vl:   tag.VLUndefinedLength,
			data: makeEncapsulatedSequence(t),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			opts := parseOptSet{skipPixelData: true}
			dcmdata := bytes.NewBuffer(tc.data)

			r := &reader{
				rawReader: dicomio.NewReader(bufio.NewReader(dcmdata), binary.LittleEndian, int64(dcmdata.Len())),
				opts:      opts,
			}
			val, err := r.readPixelData(tc.vl, &Dataset{}, nil)
			if err != nil {
				t.Errorf("unexpected error in readPixelData: %v", err)
			}
			pixelVal, ok := val.GetValue().(PixelDataInfo)
			if !ok {
				t.Errorf("Expected value to be of type PixelDataInfo")
			}
			if !pixelVal.IntentionallySkipped {
				t.Errorf("Expected PixelDataInfo to have IntentionallySkipped=true")
			}
		})
	}
}

// Used to encode the data from the generated headers.
type headerData struct {
	// The byte encoded header data.
	HeaderBytes *bytes.Buffer
	// The decoded elements conforming the header.
	Elements []*Element
}

// Write a collection of elements and return them as an encoded buffer of bytes.
func writeElements(elements []*Element) ([]byte, error) {
	buff := bytes.Buffer{}
	dcmWriter, err := NewWriter(&buff)
	if err != nil {
		return []byte{}, err
	}
	dcmWriter.SetTransferSyntax(binary.LittleEndian, true)

	for _, e := range elements {
		err := dcmWriter.WriteElement(e)
		if err != nil {
			return nil, err
		}
	}
	data := buff.Bytes()
	return data, nil
}

// Returns a fake DICOM group 2 header with the FileMetaInformationGroupLength tag missing (0x0002,0x0000).
func headerWithNoFileMetaInformationGroupLength() (*headerData, error) {
	headerData := new(headerData)

	elements := []*Element{
		mustNewElement(tag.MediaStorageSOPClassUID, []string{"SecondaryCapture"}),
		mustNewElement(tag.MediaStorageSOPInstanceUID, []string{"1.3.6.1.4.1.35190.4.1.20210608.607733549593"}),
		mustNewElement(tag.TransferSyntaxUID, []string{"=RLELossless"}),
		mustNewElement(tag.ImplementationClassUID, []string{"1.6.6.1.4.1.9590.100.1.0.100.4.0"}),
		mustNewElement(tag.SOPInstanceUID, []string{"1.3.6.1.4.1.35190.4.1.20210608.607733549593"}),
	}
	data, err := writeElements(elements)
	if err != nil {
		return nil, err
	}

	// Construct valid DICOM header preamble.
	magicWord := []byte("DICM")
	preamble := make([]byte, 128)
	preamble = append(preamble, magicWord...)
	headerBytes := append(preamble, data...)
	headerData.HeaderBytes = bytes.NewBuffer(headerBytes)
	headerData.Elements = elements[0 : len(elements)-1]
	return headerData, nil
}

// Returns a fake DICOM group 2 header with a FileMetaInformationGroupLength tag (0x0002,0x0000).
func headerWithFileMetaInformationGroupLength() (*headerData, error) {
	headerData := new(headerData)

	sopInstanceUidElement := mustNewElement(tag.SOPInstanceUID, []string{"1.3.6.1.4.1.35190.4.1.20210608.607733549593"})
	elements := []*Element{
		mustNewElement(tag.FileMetaInformationVersion, []byte{0x00, 0x01}),
		mustNewElement(tag.MediaStorageSOPClassUID, []string{"1.2.276.0.7230010.3.1.0.1"}),
		mustNewElement(tag.MediaStorageSOPInstanceUID, []string{"1.3.6.1.4.1.35190.4.1.20210608.607733549593"}),
		mustNewElement(tag.TransferSyntaxUID, []string{"=RLELossless"}),
		mustNewElement(tag.ImplementationClassUID, []string{"1.2.276.0.7230010.3.0.3.6.7"}),
		mustNewElement(tag.ImplementationVersionName, []string{"OFFIS_DCMTK_367"}),
	}
	dataHeader, err := writeElements(elements)
	if err != nil {
		return nil, err
	}
	fileMetaInfoElement := mustNewElement(tag.FileMetaInformationGroupLength, []int{len(dataHeader)})
	dataFileMetaInfo, err := writeElements([]*Element{fileMetaInfoElement})
	if err != nil {
		return nil, err
	}
	dataSopInstanceUid, err := writeElements([]*Element{sopInstanceUidElement})
	if err != nil {
		return nil, err
	}
	data := append(dataFileMetaInfo, dataHeader...)
	data = append(data, dataSopInstanceUid...)

	// Construct valid DICOM header preamble.
	magicWord := []byte("DICM")
	preamble := make([]byte, 128)
	preamble = append(preamble, magicWord...)
	headerBytes := append(preamble, data...)
	headerData.HeaderBytes = bytes.NewBuffer(headerBytes)
	headerData.Elements = append([]*Element{fileMetaInfoElement}, elements...)
	return headerData, nil
}

func TestReadHeader_TryAllowErrorMetaElementGroupLength(t *testing.T) {
	opts := parseOptSet{allowMissingMetaElementGroupLength: true}

	t.Run("NoFileMetaInformationGroupLength", func(t *testing.T) {
		dcmheaderNoInfoGrpLen, err := headerWithNoFileMetaInformationGroupLength()
		if err != nil {
			t.Fatalf("unsuccessful generation of fake header data")
		} else {
			r := &reader{
				rawReader: dicomio.NewReader(bufio.NewReader(dcmheaderNoInfoGrpLen.HeaderBytes), binary.LittleEndian, int64(dcmheaderNoInfoGrpLen.HeaderBytes.Len())),
				opts:      opts,
			}
			r.rawReader.SetTransferSyntax(binary.LittleEndian, true)
			wantElements, err := r.readHeader()
			if err != nil {
				t.Errorf("unsuccessful readHeader when parse option %v is turned on and header has no MetaElementGroupLength tag", opts.allowMissingMetaElementGroupLength)
			}
			// Ensure dataset read from readHeader and the test header are the same except for the ValueLength field.
			if diff := cmp.Diff(wantElements, dcmheaderNoInfoGrpLen.Elements, cmp.AllowUnexported(allValues...), cmpopts.IgnoreFields(Element{}, "ValueLength")); diff != "" {
				t.Errorf("Elements parsed from test header do not match: %v", diff)
			}
		}
	})

	t.Run("WithFileMetaInformationGroupLength", func(t *testing.T) {
		dcmHeaderInfoGrpLen, err := headerWithFileMetaInformationGroupLength()
		if err != nil {
			t.Fatalf("unsuccessful generation of fake header data with FileMetaInformationGroupLength")
		} else {
			r := &reader{
				rawReader: dicomio.NewReader(bufio.NewReader(dcmHeaderInfoGrpLen.HeaderBytes), binary.LittleEndian, int64(dcmHeaderInfoGrpLen.HeaderBytes.Len())),
				opts:      opts,
			}
			r.rawReader.SetTransferSyntax(binary.LittleEndian, true)
			wantElements, err := r.readHeader()
			if err != nil {
				t.Errorf("unsuccessful readHeader when parse option %v is turned on and header has no MetaElementGroupLength tag", opts.allowMissingMetaElementGroupLength)
			}
			// Ensure dataset read from readHeader and the test header are the same except for the ValueLength field.
			if diff := cmp.Diff(wantElements, dcmHeaderInfoGrpLen.Elements, cmp.AllowUnexported(allValues...), cmpopts.IgnoreFields(Element{}, "ValueLength")); diff != "" {
				t.Errorf("Elements parsed from test header do not match: %v", diff)
			}
		}
	})
}

func TestReadPixelData_TrySkipProcessingPixelDataValue(t *testing.T) {
	opts := parseOptSet{skipProcessingPixelDataValue: true}
	valueBytes := []byte{1, 2, 3, 4, 5, 6}
	dcmdata := bytes.NewBuffer(valueBytes)

	r := &reader{
		rawReader: dicomio.NewReader(bufio.NewReader(dcmdata), binary.LittleEndian, int64(dcmdata.Len())),
		opts:      opts,
	}
	val, err := r.readPixelData(6, &Dataset{}, nil)
	if err != nil {
		t.Errorf("unexpected error in readPixelData: %v", err)
	}
	pixelVal, ok := val.GetValue().(PixelDataInfo)
	if !ok {
		t.Errorf("Expected value to be of type PixelDataInfo")
	}
	if !pixelVal.IntentionallyUnprocessed {
		t.Errorf("Expected PixelDataInfo to have IntentionallyUnprocessed=true")
	}
	if !cmp.Equal(pixelVal.UnprocessedValueData, valueBytes) {
		t.Errorf("expected UnprocessedValueData to match valueBytes. got: %v, want: %v", pixelVal.UnprocessedValueData, valueBytes)
	}

}

func makeEncapsulatedSequence(t *testing.T) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	w := dicomio.NewWriter(buf, binary.LittleEndian, true)

	writePixelData(w, tag.PixelData, &pixelDataValue{PixelDataInfo{IsEncapsulated: true, Frames: []*frame.Frame{
		{
			Encapsulated: true,
			EncapsulatedData: frame.EncapsulatedFrame{
				Data: []byte{1, 2, 3, 4},
			},
		},
	}}}, "", tag.VLUndefinedLength)

	return buf.Bytes()

}

func TestReadNativeFrames_OneBitAllocated(t *testing.T) {
	cases := []struct {
		Name              string
		existingData      Dataset
		data              []byte
		expectedPixelData *PixelDataInfo
		expectedError     error
		byteOrder         binary.ByteOrder
	}{
		{
			Name: "LittleEndian, 4x4, 1 frames, 1 samples/pixel",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{4}),
				mustNewElement(tag.Columns, []int{4}),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				mustNewElement(tag.BitsAllocated, []int{1}),
				mustNewElement(tag.SamplesPerPixel, []int{1}),
			}},
			data: []byte{0b00010111, 0b10010111},
			expectedPixelData: &PixelDataInfo{
				IsEncapsulated: false,
				Frames: []*frame.Frame{
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[int]{
							InternalBitsPerSample:   1,
							InternalRows:            4,
							InternalCols:            4,
							InternalSamplesPerPixel: 1,
							RawData:                 []int{0, 0, 0, 1, 0, 1, 1, 1, 1, 0, 0, 1, 0, 1, 1, 1},
						},
					},
				},
			},
			expectedError: nil,
			byteOrder:     binary.LittleEndian,
		},
		{
			Name: "\"BigEndian\" (maybe), 4x4, 1 frames, 1 samples/pixel",
			existingData: Dataset{Elements: []*Element{
				mustNewElement(tag.Rows, []int{4}),
				mustNewElement(tag.Columns, []int{4}),
				mustNewElement(tag.NumberOfFrames, []string{"1"}),
				mustNewElement(tag.BitsAllocated, []int{1}),
				mustNewElement(tag.SamplesPerPixel, []int{1}),
			}},
			data: []byte{0b00010111, 0b10010111},
			expectedPixelData: &PixelDataInfo{
				IsEncapsulated: false,
				Frames: []*frame.Frame{
					{
						Encapsulated: false,
						NativeData: &frame.NativeFrame[int]{
							InternalBitsPerSample:   1,
							InternalRows:            4,
							InternalCols:            4,
							InternalSamplesPerPixel: 1,
							RawData:                 []int{0, 0, 0, 1, 0, 1, 1, 1, 1, 0, 0, 1, 0, 1, 1, 1},
						},
					},
				},
			},
			expectedError: nil,
			// For some reason, this doesn't make a difference, even though we
			// don't change the order in which we read the bits in the
			// implementation. For some reason, binary.Write isn't changing the
			// order of the bits (or it is, and we somehow always read it back
			// in the correct order).
			byteOrder: binary.BigEndian,
		},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			dcmdata := bytes.Buffer{}
			for _, item := range tc.data {
				if err := binary.Write(&dcmdata, tc.byteOrder, item); err != nil {
					t.Errorf("TestReadNativeFrames: Unable to setup test buffer")
				}
			}

			r := &reader{rawReader: dicomio.NewReader(bufio.NewReader(&dcmdata), tc.byteOrder, int64(dcmdata.Len()))}

			pixelData, _, err := r.readNativeFrames(&tc.existingData, nil, uint32(dcmdata.Len()))
			if !errors.Is(err, tc.expectedError) {
				t.Errorf("TestReadNativeFrames(%v): did not get expected error. got: %v, want: %v", tc.data, err, tc.expectedError)
			}

			if diff := cmp.Diff(tc.expectedPixelData, pixelData); diff != "" {
				t.Errorf("TestReadNativeFrames(%v): unexpected diff: %v\ndata:%v", tc.data, diff, pixelData)
			}
		})
	}
}

func BenchmarkReadNativeFrames(b *testing.B) {
	cases := []struct {
		Name            string
		Rows            int
		Cols            int
		NumFrames       int
		SamplesPerPixel int
	}{
		{
			Name:            "10x10, 10 frames, 1 sample/pixel",
			Rows:            10,
			Cols:            10,
			NumFrames:       10,
			SamplesPerPixel: 1,
		},
		{
			Name:            "100x100, 10 frames, 1 sample/pixel",
			Rows:            100,
			Cols:            100,
			NumFrames:       10,
			SamplesPerPixel: 1,
		},
		{
			Name:            "512x512, 10 frames, 1 sample/pixel",
			Rows:            512,
			Cols:            512,
			NumFrames:       10,
			SamplesPerPixel: 1,
		},
		{
			Name:            "512x512, 10 frames, 5 sample/pixel",
			Rows:            512,
			Cols:            512,
			NumFrames:       10,
			SamplesPerPixel: 5,
		},
	}
	for _, c := range cases {
		b.Run(c.Name, func(b *testing.B) {
			dataset, dataBytes := buildReadNativeFramesInput(c.Rows, c.Cols, c.NumFrames, c.SamplesPerPixel, b)
			bytesReader := bytes.NewReader(dataBytes)
			bufioReader := bufio.NewReader(bytesReader)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bytesReader.Reset(dataBytes)
				bufioReader.Reset(bytesReader)
				rawReader := dicomio.NewReader(bufioReader, binary.LittleEndian, int64(len(dataBytes)))
				r := &reader{rawReader: rawReader}
				_, _, err := r.readNativeFrames(dataset, nil, uint32(len(dataBytes)))
				if err != nil {
					b.Fatalf("benchmark failed: %v", err)
				}
			}
		})
	}
}

func buildReadNativeFramesInput(rows, cols, numFrames, samplesPerPixel int, b *testing.B) (*Dataset, []byte) {
	b.Helper()
	dataset := Dataset{
		Elements: []*Element{
			mustNewElement(tag.Rows, []int{rows}),
			mustNewElement(tag.Columns, []int{cols}),
			mustNewElement(tag.NumberOfFrames, []string{strconv.Itoa(numFrames)}),
			mustNewElement(tag.BitsAllocated, []int{16}),
			mustNewElement(tag.SamplesPerPixel, []int{samplesPerPixel}),
		},
	}
	dcmdata := bytes.Buffer{}
	dcmdata.Grow(2 * numFrames * rows * cols * samplesPerPixel)
	for fr := 0; fr < numFrames; fr++ {
		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				for pxs := 0; pxs < samplesPerPixel; pxs++ {
					if err := binary.Write(&dcmdata, binary.LittleEndian, uint16(rand.Intn(100))); err != nil {
						b.Fatalf("TestReadNativeFrames: Unable to setup test buffer")
					}
				}
			}
		}
	}

	return &dataset, dcmdata.Bytes()
}

func buildTagData(t *testing.T, tg tag.Tag) []byte {
	t.Helper()
	data := bytes.Buffer{}
	if err := binary.Write(&data, binary.LittleEndian, tg.Group); err != nil {
		t.Errorf("buildTagData: Unable to setup test buffer: %v", err)
	}
	if err := binary.Write(&data, binary.LittleEndian, tg.Element); err != nil {
		t.Errorf("buildTagData: Unable to setup test buffer: %v", err)
	}

	return data.Bytes()
}

// newNativeFramesReader builds an in-package reader over dataBytes with a hard
// limit matching the provided Value Length.
func newNativeFramesReader(dataBytes []byte, vl uint32) *reader {
	return &reader{
		rawReader: dicomio.NewReader(bufio.NewReader(bytes.NewReader(dataBytes)), binary.LittleEndian, int64(vl)),
	}
}

// pixelDimensionDataset builds a Dataset holding the attributes needed to parse
// native PixelData. Individual attributes can be overridden (including with
// wrong-typed values) via the overrides map.
func pixelDimensionDataset(rows, cols, nFrames, bitsAllocated, samplesPerPixel int, overrides map[tag.Tag]*Element) Dataset {
	elems := []*Element{
		mustNewElement(tag.Rows, []int{rows}),
		mustNewElement(tag.Columns, []int{cols}),
		mustNewElement(tag.NumberOfFrames, []string{strconv.Itoa(nFrames)}),
		mustNewElement(tag.BitsAllocated, []int{bitsAllocated}),
		mustNewElement(tag.SamplesPerPixel, []int{samplesPerPixel}),
	}
	if overrides != nil {
		for i, e := range elems {
			if replacement, ok := overrides[e.Tag]; ok {
				elems[i] = replacement
			}
		}
	}
	return Dataset{Elements: elems}
}

// TestReadNativeFrames_MalformedDimensions ensures malicious or malformed
// dimension attributes produce clear errors instead of panics, oversized
// allocations, truncation-driven mismatches, or out-of-bounds slices.
func TestReadNativeFrames_MalformedDimensions(t *testing.T) {
	// uint32 truncation attack: 2*2*1*(2^30+2) bytes = 2^32 + 8, whose low 32
	// bits equal 8. The old uint32() cast treated this as matching a VL of 8 and
	// attempted a multi-gigabyte allocation.
	truncationFrames := (1 << 30) + 2

	cases := []struct {
		name        string
		dataset     Dataset
		vl          uint32
		dataBytes   []byte
		wantErr     error
		skip32Bit   bool // test relies on 64-bit int arithmetic
		wantNoPanic bool
	}{
		{
			name:      "uint32 truncation of oversized frame declaration",
			skip32Bit: true,
			dataset:   pixelDimensionDataset(2, 2, truncationFrames, 8, 1, nil),
			vl:        8,
			dataBytes: []byte{1, 2, 3, 4, 5, 6, 7, 8},
			wantErr:   ErrorMismatchPixelDataLength,
		},
		{
			name:      "checked multiplication overflow",
			skip32Bit: true,
			// 3000000*3000000 alone is 9e12; multiplied by SamplesPerPixel(3)
			// and NumberOfFrames(1000000000) it exceeds math.MaxInt64, while
			// every individual attribute still fits in int32.
			dataset:   pixelDimensionDataset(3000000, 3000000, 1000000000, 8, 3, nil),
			vl:        8,
			dataBytes: []byte{1, 2, 3, 4, 5, 6, 7, 8},
			wantErr:   errInvalidPixelDataAttribute,
		},
		{
			name:      "zero rows",
			dataset:   pixelDimensionDataset(0, 2, 1, 8, 1, nil),
			vl:        4,
			dataBytes: []byte{1, 2, 3, 4},
			wantErr:   errInvalidPixelDataAttribute,
		},
		{
			name:      "negative columns",
			dataset:   pixelDimensionDataset(2, -2, 1, 8, 1, nil),
			vl:        4,
			dataBytes: []byte{1, 2, 3, 4},
			wantErr:   errInvalidPixelDataAttribute,
		},
		{
			name:      "zero samples per pixel",
			dataset:   pixelDimensionDataset(2, 2, 1, 8, 0, nil),
			vl:        4,
			dataBytes: []byte{1, 2, 3, 4},
			wantErr:   errInvalidPixelDataAttribute,
		},
		{
			name:      "unsupported odd bitsAllocated",
			dataset:   pixelDimensionDataset(2, 2, 1, 4, 1, nil),
			vl:        2,
			dataBytes: []byte{1, 2},
			wantErr:   ErrorUnsupportedBitsAllocated,
		},
		{
			name:      "unsupported 24-bit bitsAllocated",
			dataset:   pixelDimensionDataset(2, 2, 1, 24, 1, nil),
			vl:        12,
			dataBytes: make([]byte, 12),
			wantErr:   ErrorUnsupportedBitsAllocated,
		},
		{
			name: "BitsAllocated has wrong value type",
			dataset: pixelDimensionDataset(2, 2, 1, 8, 1, map[tag.Tag]*Element{
				tag.BitsAllocated: {
					Tag:                 tag.BitsAllocated,
					ValueRepresentation: tag.VRString,
					Value:               &stringsValue{value: []string{"not-an-int"}},
				},
			}),
			vl:          4,
			dataBytes:   []byte{1, 2, 3, 4},
			wantErr:     errInvalidPixelDataAttribute,
			wantNoPanic: true,
		},
		{
			name: "NumberOfFrames has wrong value type",
			dataset: pixelDimensionDataset(2, 2, 1, 8, 1, map[tag.Tag]*Element{
				tag.NumberOfFrames: {
					Tag:                 tag.NumberOfFrames,
					ValueRepresentation: tag.VRBytes,
					Value:               &bytesValue{value: []byte{1, 2, 3, 4}},
				},
			}),
			vl:          4,
			dataBytes:   []byte{1, 2, 3, 4},
			wantErr:     errInvalidPixelDataAttribute,
			wantNoPanic: true,
		},
		{
			name: "Rows has no values",
			dataset: pixelDimensionDataset(2, 2, 1, 8, 1, map[tag.Tag]*Element{
				tag.Rows: {
					Tag:                 tag.Rows,
					ValueRepresentation: tag.VRUInt16List,
					Value:               &intsValue{value: []int{}},
				},
			}),
			vl:          4,
			dataBytes:   []byte{1, 2, 3, 4},
			wantErr:     errInvalidPixelDataAttribute,
			wantNoPanic: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip32Bit && strconv.IntSize < 64 {
				t.Skip("this case requires 64-bit int arithmetic")
			}
			r := newNativeFramesReader(tc.dataBytes, tc.vl)

			var (
				pixelData *PixelDataInfo
				err       error
				panicked  any
			)
			func() {
				defer func() {
					panicked = recover()
				}()
				pixelData, _, err = r.readNativeFrames(&tc.dataset, nil, tc.vl)
			}()

			if panicked != nil {
				t.Fatalf("readNativeFrames panicked on malformed input: %v", panicked)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("readNativeFrames unexpected error. got: %v, want: %v", err, tc.wantErr)
			}
			if pixelData != nil {
				t.Fatalf("expected nil PixelDataInfo on error, got: %+v", pixelData)
			}
		})
	}
}

// TestReadNativeFrames_TruncatedData verifies that when the dimension-derived
// length matches the declared VL but the underlying stream is short, the read
// fails cleanly (bounded by the pushed VL limit) without panicking or reading
// past the element boundary.
func TestReadNativeFrames_TruncatedData(t *testing.T) {
	// 2x2, 1 frame, 1 sample/pixel, 16-bit => 8 expected bytes, supply only 6.
	dataset := pixelDimensionDataset(2, 2, 1, 16, 1, nil)
	dataBytes := []byte{1, 0, 2, 0, 3, 0} // 6 bytes instead of 8
	vl := uint32(8)
	r := newNativeFramesReader(dataBytes, vl)

	var panicked any
	_, _, err := func() (pd *PixelDataInfo, n int, e error) {
		defer func() { panicked = recover() }()
		return r.readNativeFrames(&dataset, nil, vl)
	}()

	if panicked != nil {
		t.Fatalf("readNativeFrames panicked on truncated input: %v", panicked)
	}
	if err == nil {
		t.Fatal("expected an error for truncated pixel data, got nil")
	}
	// The hard VL limit must have prevented consuming beyond the element.
	if left := r.rawReader.BytesLeftUntilLimit(); left < 0 {
		t.Fatalf("reader advanced past the pixel data VL limit, bytes left: %d", left)
	}
}

// TestReadNativeFrames_FrameBoundaryAndPadding confirms multi-frame decoding
// keeps frame boundaries distinct and that a single trailing even-VL padding
// byte is not delivered as pixel data.
func TestReadNativeFrames_FrameBoundaryAndPadding(t *testing.T) {
	// 2 frames of 1x1 8-bit with 1 sample each = 1 byte per frame; across both
	// frames that is 2 pixel bytes, which is even and needs no padding. To
	// exercise the padding path use 3 pixel bytes total (odd) with one trailing
	// padding byte to reach an even VL of 4: frame 0 gets bytes {1,2,3} via a
	// 1x3 frame, and we use a single frame so the padding is unambiguous.
	dataset := pixelDimensionDataset(1, 3, 1, 8, 1, nil) // 3 pixels/frame, 1 frame
	dataBytes := []byte{1, 2, 3, 0}                      // 3 pixel bytes + padding 0
	vl := uint32(4)                                      // even VL; computed pixel size is 3
	r := newNativeFramesReader(dataBytes, vl)

	pixelData, bytesRead, err := r.readNativeFrames(&dataset, nil, vl)
	if err != nil {
		t.Fatalf("readNativeFrames unexpected error: %v", err)
	}
	if bytesRead != 4 {
		t.Errorf("bytesRead = %d, want 4 (including padding byte)", bytesRead)
	}
	if len(pixelData.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(pixelData.Frames))
	}
	frame0 := pixelData.Frames[0].NativeData.RawDataSlice().([]uint8)
	if diff := cmp.Diff([]uint8{1, 2, 3}, frame0); diff != "" {
		t.Errorf("frame mismatch (-want +got):\n%s", diff)
	}
	if r.rawReader.BytesLeftUntilLimit() != 0 {
		t.Errorf("expected reader to be exactly at VL limit, bytes left: %d", r.rawReader.BytesLeftUntilLimit())
	}
}

// TestReadNativeFrames_MultiFrameBoundary decodes two frames without padding and
// verifies each frame receives only its own bytes.
func TestReadNativeFrames_MultiFrameBoundary(t *testing.T) {
	// 2 frames of 1x2 8-bit 1 sample = 2 bytes per frame, 4 total, no padding.
	dataset := pixelDimensionDataset(1, 2, 2, 8, 1, nil)
	dataBytes := []byte{1, 2, 3, 4}
	vl := uint32(4)
	r := newNativeFramesReader(dataBytes, vl)

	pixelData, bytesRead, err := r.readNativeFrames(&dataset, nil, vl)
	if err != nil {
		t.Fatalf("readNativeFrames unexpected error: %v", err)
	}
	if bytesRead != 4 {
		t.Errorf("bytesRead = %d, want 4", bytesRead)
	}
	if len(pixelData.Frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(pixelData.Frames))
	}
	first := pixelData.Frames[0].NativeData.RawDataSlice().([]uint8)
	second := pixelData.Frames[1].NativeData.RawDataSlice().([]uint8)
	if diff := cmp.Diff([]uint8{1, 2}, first); diff != "" {
		t.Errorf("first frame mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]uint8{3, 4}, second); diff != "" {
		t.Errorf("second frame mismatch (-want +got):\n%s", diff)
	}
	if r.rawReader.BytesLeftUntilLimit() != 0 {
		t.Errorf("expected reader to be exactly at VL limit, bytes left: %d", r.rawReader.BytesLeftUntilLimit())
	}
}
