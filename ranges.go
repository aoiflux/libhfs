package libhfs

import "math"

// SizeUnknown tells [Volume.ExtentRanges] that no logical size is available, so
// every byte of every extent is treated as data and no range reports slack.
//
// It is a named sentinel rather than a bare negative number because giving up a
// distinction the rest of this API depends on should read as a deliberate act at
// the call site.
const SizeUnknown = int64(-1)

// ByteRange is one contiguous run of a fork's bytes at its place in the image.
//
// It is the physical counterpart of [ExtentDescriptor]: the same fragment
// expressed in image byte offsets a caller can hand straight to the io.ReaderAt
// the volume was opened with, rather than in allocation blocks that only mean
// something once the volume's base offset and block size are both known.
// Getting that conversion wrong is silent — it reads the wrong part of the image
// and returns plausible bytes — which is why this package exposes the result
// rather than the recipe.
type ByteRange struct {
	// ForkOffset is where this range begins within the fork's logical address
	// space. The first range starts at zero and each subsequent one continues
	// from the end of the allocated span before it, so a caller that notices a
	// changed block can map it back to an offset in the file.
	ForkOffset int64

	// DiskOffset is where this range begins in the image, measured from the
	// start of the io.ReaderAt passed to [Open]. It already accounts for
	// [Volume.BaseOffset].
	DiskOffset int64

	// Length is how many of this range's bytes belong to the fork. Reading
	// Length bytes at DiskOffset yields fork content and nothing else.
	//
	// It is the full allocated span except in the last range holding data,
	// where the fork's logical size ends part-way through an allocation block.
	// A range lying entirely beyond the logical size has Length zero and is
	// still returned, because an over-allocated fork is a fact about the volume
	// and dropping the range would hide it.
	Length int64

	// Slack is how many allocated bytes follow Length in this range.
	//
	// Those bytes are inside blocks the fork owns but outside the file, so they
	// usually hold what the previous occupant of the block left behind. They are
	// reported separately rather than folded into Length so that reading a file
	// cannot accidentally include them and examining slack cannot accidentally
	// be skipped: DiskOffset+Length is where slack begins and Slack is how much
	// there is.
	Slack int64

	// StartBlock is the allocation block DiskOffset falls at the start of, so a
	// range can be cross-referenced against [Volume.BlockAllocated] and the
	// [ExtentDescriptor] it came from.
	StartBlock uint32

	// BlockCount is how many allocation blocks the range spans, counting the
	// partly used final block. Length+Slack always equals
	// BlockCount*Header().BlockSize.
	BlockCount uint32
}

// AllocatedLength returns the range's full span in bytes, data and slack
// together.
func (r ByteRange) AllocatedLength() int64 { return r.Length + r.Slack }

// ExtentRanges converts an extent list into image byte ranges.
//
// This is the primitive the fork-, attribute- and recovery-level range methods
// are built from, and it is exported because the package hands out extent lists
// that none of those cover: [XAttr.Extents], the fork data inside a
// [DeletedRecord], and the special-file forks on [VolumeHeader]. Pairing an
// extent list with the wrong block size, or forgetting the base offset entirely,
// produces offsets that are wrong rather than invalid, so the conversion belongs
// on the volume the extents came from.
//
// logicalSize is the fork's size in bytes and decides where data ends and slack
// begins; pass [SizeUnknown] when it is not recorded. A logicalSize larger than
// the extents can hold is clamped to what they hold rather than rejected,
// because a recovered or damaged record routinely declares more than its extents
// cover — see [Volume.OpenDeleted], which takes the same view.
//
// Extents are not re-ordered, merged, or validated against the allocation
// bitmap. The result has one range per extent, in the order given.
func (v *Volume) ExtentRanges(exts []ExtentDescriptor, logicalSize int64) ([]ByteRange, error) {
	if v == nil {
		return nil, &ParseError{Op: "extent_ranges", Offset: 0, Err: ErrCorrupt}
	}
	if logicalSize < 0 && logicalSize != SizeUnknown {
		return nil, &ParseError{Op: "extent_ranges", Offset: logicalSize, Err: ErrCorrupt}
	}
	if len(exts) == 0 {
		return nil, nil
	}

	blockSize := int64(v.header.BlockSize)
	if blockSize <= 0 {
		return nil, &ParseError{Op: "extent_ranges", Offset: 0, Err: ErrCorrupt}
	}

	out := make([]ByteRange, 0, len(exts))
	forkOffset := int64(0)
	for _, e := range exts {
		diskOffset, err := v.BlockOffset(e.StartBlock)
		if err != nil {
			return nil, err
		}

		span := uint64(e.BlockCount) * uint64(blockSize)
		if span > uint64(math.MaxInt64)-uint64(forkOffset) {
			return nil, &ParseError{Op: "extent_ranges", Offset: int64(e.StartBlock), Err: ErrCorrupt}
		}
		// The range's end must be addressable too, not just its start.
		// BlockOffset accepts a start close to MaxInt64, and this API promises
		// that slack begins at DiskOffset+Length; without this check that sum
		// wraps negative and a caller reads at a negative offset instead of
		// being told the geometry is corrupt.
		if span > uint64(math.MaxInt64)-uint64(diskOffset) {
			return nil, &ParseError{Op: "extent_ranges", Offset: int64(e.StartBlock), Err: ErrCorrupt}
		}

		data := int64(span)
		if logicalSize != SizeUnknown {
			remaining := logicalSize - forkOffset
			if remaining < 0 {
				remaining = 0
			}
			if remaining < data {
				data = remaining
			}
		}

		out = append(out, ByteRange{
			ForkOffset: forkOffset,
			DiskOffset: diskOffset,
			Length:     data,
			Slack:      int64(span) - data,
			StartBlock: e.StartBlock,
			BlockCount: e.BlockCount,
		})
		forkOffset += int64(span)
	}
	return out, nil
}

// DataForkRanges returns the image byte ranges a file's data fork occupies.
//
// It is the byte-addressed form of [Volume.ResolveDataForkExtents] and follows
// the extents-overflow tree in exactly the same way, so a fork with more than
// eight fragments is reported completely. Like that method it resolves a hard
// link to its target inode before reading the fork, so the ranges belong to the
// content rather than to the link.
//
// Ranges describe the fork as it is stored, never as it is served. A file
// compressed with decmpfs has its payload in an extended attribute or in the
// resource fork, and its data fork is genuinely empty on disk, so this returns
// no ranges while [Volume.OpenFileByCNID] returns the decompressed contents —
// check [CatalogRecord.Compressed] before reading anything into that.
func (v *Volume) DataForkRanges(cnid uint32) ([]ByteRange, error) {
	return v.forkRanges(cnid, false)
}

// ResourceForkRanges returns the image byte ranges a file's resource fork
// occupies.
//
// See [Volume.DataForkRanges]; the same rules apply, except that a compressed
// file's payload is often here, which is why the resource fork is never
// decompressed on the way out.
func (v *Volume) ResourceForkRanges(cnid uint32) ([]ByteRange, error) {
	return v.forkRanges(cnid, true)
}

// forkRanges mirrors resolveForkExtents so that resolving the extents and
// learning the fork's logical size cost one catalog lookup rather than two.
func (v *Volume) forkRanges(cnid uint32, resource bool) ([]ByteRange, error) {
	rec, err := v.OpenCNID(cnid)
	if err != nil {
		return nil, err
	}

	fork := rec.DataFork
	forkType := extentKeyTypeData
	if resource {
		fork = rec.RsrcFork
		forkType = extentKeyTypeRsrc
	}

	exts, err := v.resolveForkExtentsFromFork(cnid, fork, forkType)
	if err != nil {
		return nil, err
	}
	if fork.LogicalSize > 1<<62 {
		return nil, &ParseError{Op: "fork_ranges", Offset: int64(cnid), Err: ErrCorrupt}
	}
	return v.ExtentRanges(exts, int64(fork.LogicalSize))
}

// XAttrRanges returns the image byte ranges holding an extended attribute's
// value.
//
// It is empty, with a nil error, for an inline attribute: those live in the
// attributes B-tree record rather than in allocation blocks, so there is nothing
// to address. Having none is not a failure, which is the same view
// [Volume.ListXAttrs] takes. Check [XAttr.Storage] to tell the two cases apart.
//
// Attribute extents come from the attributes tree alone. Unlike a fork, whose
// ninth and later fragments live in the extents-overflow tree, an attribute's
// extra fragments are carried in extension records beside the fork-data record
// itself, and this package stitches them on when the attribute is listed. It
// also does not trim them against a block total, because an attribute record
// records no such total: an attribute whose extents describe more space than its
// size needs reports a final range with a Length of zero and the whole span as
// Slack, which is the honest reading of what is on disk.
//
// For an [XAttr] already in hand, this is
// v.ExtentRanges(a.Extents, int64(a.Size)).
func (v *Volume) XAttrRanges(cnid uint32, name string) ([]ByteRange, error) {
	attr, err := v.GetXAttr(cnid, name)
	if err != nil {
		return nil, err
	}
	if attr.Storage != XAttrFork || len(attr.Extents) == 0 {
		return nil, nil
	}
	if attr.Size > 1<<62 {
		return nil, &ParseError{Op: "xattr_ranges", Offset: int64(cnid), Err: ErrCorrupt}
	}
	return v.ExtentRanges(attr.Extents, int64(attr.Size))
}
