package dicom

import (
	"errors"
	"fmt"
	"math"
	"unsafe"
)

var (
	// ErrorParseBudgetExceeded indicates that reading or reserving a value
	// would exceed the ParseBudget configured for this parse. It is returned
	// before the offending allocation or read is attempted. Use errors.Is to
	// test for it; the error message also names the element or structure, its
	// byte offset, and the limit that was crossed.
	ErrorParseBudgetExceeded = errors.New("dicom parse budget exceeded")

	// ErrorSequenceTooDeep indicates that sequences were nested more deeply
	// than ParseBudget.MaxNestingDepth allows. It is checked before recursing
	// into the nested structure, so malformed input cannot exhaust the
	// goroutine stack.
	ErrorSequenceTooDeep = errors.New("sequences nested deeper than the parse budget allows")

	// ErrorInvalidPixelDimensions indicates that the elements describing
	// native PixelData (Rows, Columns, NumberOfFrames, BitsAllocated,
	// SamplesPerPixel) cannot describe a pixel buffer: a value is negative,
	// has the wrong type, or the computed sizes overflow. Missing Rows,
	// Columns, BitsAllocated or SamplesPerPixel return ErrorElementNotFound
	// instead, as they did before budgets existed.
	ErrorInvalidPixelDimensions = errors.New("invalid native PixelData dimensions")
)

const (
	// DefaultMaxElementBytes is the default limit on the memory a single
	// element value, sequence scope, sequence item, or native PixelData frame
	// may reserve. The default keeps ordinary studies working while stopping a
	// declared length in a small, malformed file from becoming a multi-gigabyte
	// allocation. Raise it with ParseBudget.MaxElementBytes to read
	// legitimately larger images.
	DefaultMaxElementBytes int64 = 256 << 20 // 256 MiB

	// DefaultMaxParseBytes is the default limit on the total number of bytes
	// a parse may read or reserve across all elements. It bounds undefined
	// length structures, which have no declared length to check, and datasets
	// that consist of many moderately sized elements.
	DefaultMaxParseBytes int64 = 1 << 30 // 1 GiB

	// DefaultMaxNestingDepth is the default limit on nested sequence scopes.
	// Structured report content trees, the deepest found in practice, stay in
	// the tens.
	DefaultMaxNestingDepth = 64
)

// ParseBudget bounds the resources a Parser may consume while reading
// untrusted DICOM input. Every field is independent: a zero value disables
// that field's limit, so the zero ParseBudget disables budgeting entirely.
//
// When the WithParseBudget option is not given, the parser uses
// DefaultParseBudget. That default is deliberately finite: a legal but large
// image may need a larger budget, which the caller can grant explicitly:
//
//	budget := dicom.DefaultParseBudget()
//	budget.MaxElementBytes = 2 << 30 // allow 2 GiB values
//	dataset, err := dicom.ParseFile("large.dcm", nil, dicom.WithParseBudget(budget))
//
// The budget is enforced before a value is allocated or read, so malformed
// input is rejected with a locatable error rather than with an allocation
// that the input cannot support.
type ParseBudget struct {
	// MaxElementBytes bounds the memory a single element value, sequence
	// scope, sequence item, encapsulated frame, or native PixelData buffer
	// may reserve. Zero disables the limit.
	MaxElementBytes int64

	// MaxParseBytes bounds the cumulative number of bytes read or reserved
	// over the whole parse, including bytes discarded while skipping. Zero
	// disables the limit.
	MaxParseBytes int64

	// MaxNestingDepth bounds how many sequences may be open at once. It is
	// checked before recursing, so it also bounds the parser's stack usage.
	// Sequence items are reached only through a sequence, so they do not add
	// to the depth beyond the sequences that contain them.
	MaxNestingDepth int
}

// DefaultParseBudget returns the budget used when the caller does not supply
// one with WithParseBudget.
func DefaultParseBudget() ParseBudget {
	return ParseBudget{
		MaxElementBytes: DefaultMaxElementBytes,
		MaxParseBytes:   DefaultMaxParseBytes,
		MaxNestingDepth: DefaultMaxNestingDepth,
	}
}

// WithParseBudget sets the resource budget used by the parser. A zero field
// disables that limit, and the zero ParseBudget disables all limits. Negative
// values are rejected by NewParser.
func WithParseBudget(budget ParseBudget) ParseOption {
	return func(set *parseOptSet) {
		set.budget = budget
	}
}

// MaxElementBytes returns a ParseOption that sets the maximum memory a single
// value may reserve, leaving the other limits at their defaults. Zero disables
// this limit.
func MaxElementBytes(n int64) ParseOption {
	return func(set *parseOptSet) {
		set.budget.MaxElementBytes = n
	}
}

// MaxParseBytes returns a ParseOption that sets the cumulative byte limit for
// one parse, leaving the other limits at their defaults. Zero disables this
// limit.
func MaxParseBytes(n int64) ParseOption {
	return func(set *parseOptSet) {
		set.budget.MaxParseBytes = n
	}
}

// MaxNestingDepth returns a ParseOption that sets the maximum nesting depth of
// sequences, leaving the other limits at their defaults. Zero disables this
// limit.
func MaxNestingDepth(n int) ParseOption {
	return func(set *parseOptSet) {
		set.budget.MaxNestingDepth = n
	}
}

// validate rejects negative limits, which have no meaning: zero already
// disables a limit.
func (b ParseBudget) validate() error {
	switch {
	case b.MaxElementBytes < 0:
		return fmt.Errorf("ParseBudget.MaxElementBytes must not be negative, got %d", b.MaxElementBytes)
	case b.MaxParseBytes < 0:
		return fmt.Errorf("ParseBudget.MaxParseBytes must not be negative, got %d", b.MaxParseBytes)
	case b.MaxNestingDepth < 0:
		return fmt.Errorf("ParseBudget.MaxNestingDepth must not be negative, got %d", b.MaxNestingDepth)
	}
	return nil
}

// budget tracks the resource budget across the elements of one parse.
type budget struct {
	limits ParseBudget
	// used is the cumulative bytes read or reserved so far.
	used int64
	// depth is the number of sequence scopes currently open.
	depth int
}

func newBudget(limits ParseBudget) budget {
	return budget{limits: limits}
}

// enabled reports whether any limit is configured.
func (b *budget) enabled() bool {
	return b != nil && (b.limits.MaxElementBytes > 0 || b.limits.MaxParseBytes > 0 || b.limits.MaxNestingDepth > 0)
}

// checkDeclared validates the declared length (in bytes) of a value before it
// is allocated or read. It does not charge the cumulative budget: the caller
// charges it with reserve once the value is actually read.
func (b *budget) checkDeclared(n int64, offset int64, context string) error {
	if !b.enabled() {
		return nil
	}
	if n < 0 {
		return fmt.Errorf("%s at byte offset %d: negative length %d: %w", context, offset, n, ErrorParseBudgetExceeded)
	}
	if b.limits.MaxElementBytes > 0 && n > b.limits.MaxElementBytes {
		return fmt.Errorf("%s at byte offset %d: declared length %d exceeds max element bytes %d: %w",
			context, offset, n, b.limits.MaxElementBytes, ErrorParseBudgetExceeded)
	}
	if b.limits.MaxParseBytes > 0 {
		if left := b.limits.MaxParseBytes - b.used; n > left {
			return fmt.Errorf("%s at byte offset %d: declared length %d exceeds remaining parse budget %d: %w",
				context, offset, n, left, ErrorParseBudgetExceeded)
		}
	}
	return nil
}

// checkElement validates an amount of memory a value will occupy against the
// per-element limit. Unlike checkDeclared it does not consult the cumulative
// limit, which is for bytes not yet read.
func (b *budget) checkElement(n int64, offset int64, context string) error {
	if !b.enabled() {
		return nil
	}
	if n < 0 {
		return fmt.Errorf("%s at byte offset %d: negative length %d: %w", context, offset, n, ErrorParseBudgetExceeded)
	}
	if b.limits.MaxElementBytes > 0 && n > b.limits.MaxElementBytes {
		return fmt.Errorf("%s at byte offset %d: length %d exceeds max element bytes %d: %w",
			context, offset, n, b.limits.MaxElementBytes, ErrorParseBudgetExceeded)
	}
	return nil
}

// checkScope validates the declared length of a nested scope (a sequence, an
// item, or a skipped range) against the cumulative budget. The per-element
// limit applies to values actually read, not to a range that merely contains
// them, and the scope's contents charge the cumulative budget as they are read.
func (b *budget) checkScope(n int64, offset int64, context string) error {
	if !b.enabled() || b.limits.MaxParseBytes <= 0 {
		return nil
	}
	if n < 0 {
		return fmt.Errorf("%s at byte offset %d: negative length %d: %w", context, offset, n, ErrorParseBudgetExceeded)
	}
	if left := b.limits.MaxParseBytes - b.used; n > left {
		return fmt.Errorf("%s at byte offset %d: declared length %d exceeds remaining parse budget %d: %w",
			context, offset, n, left, ErrorParseBudgetExceeded)
	}
	return nil
}

// reserve charges cost bytes against the cumulative budget after checking it
// against the per-element and cumulative limits. Use it for a value that will
// both be allocated and read.
func (b *budget) reserve(cost int64, offset int64, context string) error {
	if err := b.checkDeclared(cost, offset, context); err != nil {
		return err
	}
	if b.limits.MaxParseBytes > 0 {
		b.used += cost
	}
	return nil
}

// chargeRead charges bytes that are consumed without being retained (skipped
// values and scopes) against the cumulative budget only. The per-element limit
// bounds memory, and nothing is allocated here, so it does not apply.
func (b *budget) chargeRead(n int64, offset int64, context string) error {
	if !b.enabled() || b.limits.MaxParseBytes <= 0 {
		return nil
	}
	if n < 0 {
		return fmt.Errorf("%s at byte offset %d: negative length %d: %w", context, offset, n, ErrorParseBudgetExceeded)
	}
	if left := b.limits.MaxParseBytes - b.used; n > left {
		return fmt.Errorf("%s at byte offset %d: skipped %d bytes exceeds remaining parse budget %d: %w",
			context, offset, n, left, ErrorParseBudgetExceeded)
	}
	b.used += n
	return nil
}

// checkNativeFrames bounds the memory a native PixelData element will occupy
// once decoded. Each frame stores one value per sample (pixelsPerFrame x
// samplesPerPixel), which for one-bit samples is an int rather than a bit,
// plus a small per-frame overhead. Many tiny frames can thus cost far more
// than the declared PixelData length, which is what a malformed file would
// exploit. The per-element limit, not the cumulative one, is what matters
// here: the bytes have not been read yet, so the cumulative bytes check in
// checkDeclared would double-count them.
func (b *budget) checkNativeFrames(nFrames, bytesToRead, pixelsPerFrame, samplesPerPixel, bitsAllocated int, offset int64) error {
	if !b.enabled() {
		return nil
	}
	samples := saturatingMul(int64(pixelsPerFrame), int64(samplesPerPixel))
	perSample := int64(1)
	if bitsAllocated == 1 {
		perSample = int64(unsafe.Sizeof(int(0)))
	}
	cost := saturatingMul(int64(nFrames), saturatingAdd(saturatingMul(samples, perSample), nativeFrameOverheadBytes))
	if cost < int64(bytesToRead) {
		cost = int64(bytesToRead)
	}
	return b.checkElement(cost, offset, "readNativeFrames: native frame memory")
}

// saturatingAdd adds non-negative int64s, returning math.MaxInt64 rather than
// overflowing.
func saturatingAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// saturatingMul multiplies non-negative int64s, returning math.MaxInt64 rather
// than overflowing.
func saturatingMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// enterNesting checks and opens one nesting scope (a sequence).
func (b *budget) enterNesting(offset int64, context string) error {
	if !b.enabled() || b.limits.MaxNestingDepth <= 0 {
		return nil
	}
	if b.depth >= b.limits.MaxNestingDepth {
		return fmt.Errorf("%s at byte offset %d: nested deeper than max nesting depth %d: %w",
			context, offset, b.limits.MaxNestingDepth, ErrorSequenceTooDeep)
	}
	b.depth++
	return nil
}

// leaveNesting closes a scope opened by enterNesting.
func (b *budget) leaveNesting() {
	if b != nil && b.depth > 0 {
		b.depth--
	}
}
