package hfs

import "errors"

// DefaultMaxAlloc bounds any single buffer sized from an on-disk field.
//
// Sizes read from a volume are attacker-controlled in the sense that matters
// here: a corrupt or hostile image can declare a fork of 2^60 bytes, and
// allocating that would take the process down with an OOM kill rather than an
// error. Being panic-free is not much use if the caller dies anyway.
const DefaultMaxAlloc = int64(1) << 30 // 1 GiB

// ErrSizeLimit reports that an on-disk size field exceeded the volume's
// allocation limit. See [Volume.SetMaxAlloc].
var ErrSizeLimit = errors.New("hfs: declared size exceeds allocation limit")

// SetMaxAlloc caps any single buffer this package will allocate from a size
// recorded on the volume. Reads that would exceed the cap return
// [ErrSizeLimit] instead of attempting the allocation.
//
// A value of 0 removes the limit. Negative values are ignored. The default is
// [DefaultMaxAlloc].
//
// This bounds whole-value reads such as [File.ReadAll] and [Volume.ReadXAttr].
// Streaming through [File.ReadAt] is unaffected, since the caller supplies the
// buffer — that is the way to read a legitimately huge fork.
//
// Safe for concurrent use.
func (v *Volume) SetMaxAlloc(n int64) {
	if v == nil || n < 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.maxAlloc = n
}

// MaxAlloc reports the current allocation cap. Zero means unlimited.
func (v *Volume) MaxAlloc() int64 {
	if v == nil {
		return DefaultMaxAlloc
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.maxAlloc
}

// checkAlloc reports whether a buffer of n bytes may be allocated.
func (v *Volume) checkAlloc(op string, n int64) error {
	if n < 0 {
		return &ParseError{Op: op, Offset: n, Err: ErrCorrupt}
	}
	limit := v.MaxAlloc()
	if limit > 0 && n > limit {
		return &ParseError{Op: op, Offset: n, Err: ErrSizeLimit}
	}
	return nil
}
