package hfs

import (
	"encoding/binary"
	"io"
)

func readAtExact(r io.ReaderAt, off int64, dst []byte) error {
	n, err := r.ReadAt(dst, off)
	if err != nil {
		if err == io.EOF {
			if n == len(dst) {
				return nil
			}
			return &ParseError{Op: "read", Offset: off, Err: ErrShortRead}
		}
		if n == len(dst) {
			return nil
		}
		return &ParseError{Op: "read", Offset: off, Err: err}
	}
	if n != len(dst) {
		return &ParseError{Op: "read", Offset: off, Err: ErrShortRead}
	}
	return nil
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func be64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
