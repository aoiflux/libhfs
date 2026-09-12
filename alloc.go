package hfs

// The allocation file is a bitmap with one bit per allocation block, most
// significant bit first: block N is bit (7 - N%8) of byte N/8. A set bit means
// the block is in use.
//
// Classic HFS keeps the same layout in its volume bitmap, which lives at a
// fixed offset rather than in a fork.

// BlockAllocated reports whether an allocation block is currently in use.
//
// For a recovered or deleted file this is the question that matters: its extent
// list still points at blocks, but the contents are only meaningful while those
// blocks remain unallocated. A block that has been handed to another file holds
// that file's data, not the deleted one's.
func (v *Volume) BlockAllocated(block uint32) (bool, error) {
	if v == nil || v.reader == nil {
		return false, &ParseError{Op: "block_allocated", Offset: int64(block), Err: ErrCorrupt}
	}
	if block >= v.header.TotalBlocks {
		return false, &ParseError{Op: "block_allocated", Offset: int64(block), Err: ErrInvalidOffset}
	}

	byteOff := int64(block / bitsPerByte)
	mask := byte(1) << (bitmapMSBFirst - block%bitsPerByte)

	var b [1]byte
	if err := v.readBitmapAt(byteOff, b[:]); err != nil {
		return false, err
	}
	return b[0]&mask != 0, nil
}

// WalkUnallocated invokes cb for each maximal run of consecutive free blocks,
// in ascending block order. Returning a non-nil error from cb stops the walk
// and returns that error.
//
// This is the input to carving: unallocated space is where deleted content
// survives.
func (v *Volume) WalkUnallocated(cb func(start, count uint32) error) error {
	if cb == nil {
		return nil
	}
	if v == nil || v.reader == nil {
		return &ParseError{Op: "walk_unallocated", Offset: 0, Err: ErrCorrupt}
	}

	total := v.header.TotalBlocks
	if total == 0 {
		return nil
	}

	buf := make([]byte, bitmapChunkBytes)

	var runStart uint32
	var runLen uint32

	flush := func() error {
		if runLen == 0 {
			return nil
		}
		err := cb(runStart, runLen)
		runLen = 0
		return err
	}

	// Widen before rounding up. total is a uint32, so a volume declaring close
	// to 2^32 blocks wraps total+7 round to a small number: the loop below then
	// reads nothing and reports the whole volume as allocated, which is exactly
	// backwards for a carver looking for free space.
	bitmapBytes := (int64(total) + bitsPerByte - 1) / bitsPerByte
	for off := int64(0); off < bitmapBytes; off += bitmapChunkBytes {
		n := int64(bitmapChunkBytes)
		if off+n > bitmapBytes {
			n = bitmapBytes - off
		}
		chunk := buf[:n]
		if err := v.readBitmapAt(off, chunk); err != nil {
			return err
		}

		for i, bt := range chunk {
			base := uint32(off+int64(i)) * bitsPerByte
			// A fully allocated byte is the common case on a used volume, so
			// short-circuit rather than testing eight bits.
			if bt == 0xFF {
				if err := flush(); err != nil {
					return err
				}
				continue
			}
			for bit := range bitsPerByte {
				block := base + uint32(bit)
				if block >= total {
					break
				}
				if bt&(1<<(bitmapMSBFirst-bit)) != 0 {
					if err := flush(); err != nil {
						return err
					}
					continue
				}
				if runLen == 0 {
					runStart = block
				}
				runLen++
			}
		}
	}
	return flush()
}

// FreeBlockCount counts unallocated blocks by reading the bitmap.
//
// The volume header also records a free-block count, but that is the writer's
// claim rather than an observation. A mismatch means the volume was not
// unmounted cleanly, or that its metadata is inconsistent — either way it is
// worth knowing, so this counts the bits instead of trusting the header.
func (v *Volume) FreeBlockCount() (uint32, error) {
	var free uint32
	err := v.WalkUnallocated(func(_, count uint32) error {
		free += count
		return nil
	})
	if err != nil {
		return 0, err
	}
	return free, nil
}

// readBitmapAt reads len(dst) bytes from the allocation bitmap starting at
// byte offset off within it.
func (v *Volume) readBitmapAt(off int64, dst []byte) error {
	if v.kind == KindHFS {
		// Classic HFS stores the volume bitmap at drVBMSt, a 512-byte-sector
		// offset from the start of the volume, rather than in a fork.
		//
		// baseOffset is deliberately not added. On classic HFS it is
		// drAlBlSt*512, the start of the allocation-block area, and drVBMSt is
		// measured from the same origin — the start of the volume, which for a
		// volume read as classic HFS is always offset zero in the reader. The
		// bitmap sits before the allocation-block area, between it and the MDB,
		// so adding the two would skip past the bitmap into file data and
		// silently report another file's bytes as allocation state.
		return readAtExact(v.reader, v.hfsBitmapOffset()+off, dst)
	}

	exts, err := v.forkExtents(allocationFileCNID, v.header.AllocationFile)
	if err != nil {
		return err
	}
	if len(exts) == 0 {
		return &ParseError{Op: "read_bitmap", Offset: off, Err: ErrMissingExtent}
	}
	return readFromExtents(v.reader, exts, v.header.BlockSize, v.baseOffset, off, dst)
}

// hfsBitmapOffset returns the byte offset of the classic HFS volume bitmap,
// relative to the start of the volume.
//
// It is not relative to the allocation-block area: drVBMSt counts 512-byte
// sectors from the volume start, and the bitmap sits between the MDB and the
// first allocation block.
func (v *Volume) hfsBitmapOffset() int64 {
	return int64(v.hfsVBMStart) * hfsSectorSize
}
