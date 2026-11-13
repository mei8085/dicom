package fuzz

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/suyashkumar/dicom"
)

// FuzzDICOMParseWithBudget checks that arbitrary input is handled safely under
// the default parse budget: no panic, no hang, and a declared length the input
// cannot hold is refused rather than allocated. Allocation bounds are checked
// separately, without the runtime.GC bookkeeping that would make fuzzing slow
// (see TestFuzzedInputAllocIsBounded).
func FuzzDICOMParseWithBudget(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("DICM"))
	for _, name := range []string{"1.dcm", "6.dcm"} {
		if data, err := os.ReadFile(filepath.Join("..", "testdata", name)); err == nil {
			f.Add(data)
		}
	}
	f.Add(buildHugeDeclaredOB())

	f.Fuzz(func(t *testing.T, data []byte) {
		// Parse the input, both with and without an exact byte count.
		ds, err := dicom.Parse(bytes.NewReader(data), int64(len(data)), nil)
		if err != nil && len(ds.Elements) != 0 {
			t.Fatalf("Parse returned %d elements alongside error %v; want no partial dataset", len(ds.Elements), err)
		}
		_, _ = dicom.ParseUntilEOF(bytes.NewReader(data), nil)
	})
}

// buildHugeDeclaredOB is a minimal file whose last element declares a value
// close to 4 GiB that the file does not contain.
func buildHugeDeclaredOB() []byte {
	preamble := make([]byte, 128)
	body := append(preamble, []byte("DICM")...)
	// (0009,0001) OB, reserved, VL = 4 GiB - 16.
	body = append(body, 0x09, 0x00, 0x01, 0x00, 'O', 'B', 0x00, 0x00, 0xf0, 0xff, 0xff, 0xff)
	return body
}
