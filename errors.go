package hfs

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidSignature  = errors.New("hfs: invalid volume signature")
	ErrUnsupportedFormat = errors.New("hfs: unsupported filesystem format")
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
