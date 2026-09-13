package hfs

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidSignature  = errors.New("hfs: invalid volume signature")
	ErrUnsupportedFormat = errors.New("hfs: unsupported filesystem format")
	ErrUnsupportedHFS    = errors.New("hfs: classic HFS volume is not supported")
	ErrUnsupportedVer    = errors.New("hfs: unsupported volume version")
	ErrCorrupt           = errors.New("hfs: corrupt volume metadata")
	ErrShortRead         = errors.New("hfs: short read")
	ErrInvalidBTreeNode  = errors.New("hfs: invalid btree node")
	ErrInvalidBTreeKey   = errors.New("hfs: invalid btree key")
	ErrMissingExtent     = errors.New("hfs: missing extent data")
	ErrNotFound          = errors.New("hfs: not found")
	ErrNotFile           = errors.New("hfs: record is not a file")
	ErrNotDir            = errors.New("hfs: record is not a directory")
	ErrInvalidOffset     = errors.New("hfs: invalid read offset")
)

// ErrStopWalk ends a walk early.
//
// Every Walk method on [Volume] takes a callback, and returning this from one
// stops the traversal and makes the Walk method itself return nil. Any other
// error from a callback stops the walk too but is returned to the caller
// unchanged, so a genuine failure is never quietly reported as success:
//
//	var found CatalogRecord
//	err := vol.WalkCatalog(func(r CatalogRecord) error {
//		if r.Name == "secrets.txt" {
//			found = r
//			return hfs.ErrStopWalk
//		}
//		return nil
//	})
//
// It is matched with errors.Is, so a callback may wrap it to carry context of
// its own back out of the traversal.
//
// The library never returns this as a failure of its own, so a caller that
// never uses it will never see it.
var ErrStopWalk = errors.New("hfs: stop walk")

// endWalk maps a traversal's outcome onto what a Walk method returns: a
// deliberate stop becomes success, and everything else passes through.
//
// One named decision rather than the same three lines at each Walk method,
// because the repeated form had already drifted — one copy tested the sentinel
// with == where the rest used errors.Is, so a wrapped stop ended that walk with
// an error while ending every other one cleanly. That is the kind of
// inconsistency a caller discovers in production, not in a signature.
func endWalk(err error) error {
	if err == nil || errors.Is(err, ErrStopWalk) {
		return nil
	}
	return err
}

type ParseError struct {
	Op     string
	Offset int64
	Err    error
}

func (e *ParseError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("hfs: %s at offset %d: %v", e.Op, e.Offset, e.Err)
}

func (e *ParseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
