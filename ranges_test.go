package hfs

import (
	"bytes"
	"errors"
	"testing"
)

// TestDataForkRangesOnWrappedVolume is the acceptance test for byte-range
// addressing: a range returned for a file on a volume embedded in an HFS
// wrapper must be readable straight from the underlying image.
//
// Nothing else in the suite can catch a dropped base offset. Every other
// fixture puts the volume at offset zero, where an implementation that ignores
// the base offset entirely still returns the right bytes.
func TestDataForkRangesOnWrappedVolume(t *testing.T) {
	img := buildWrappedHFSPlusImage(t)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if got := vol.BaseOffset(); got != wrapperEmbeddedOffset {
		t.Fatalf("BaseOffset() = %d, want %d", got, wrapperEmbeddedOffset)
	}

	ranges, err := vol.DataForkRanges(wrapperFileCNID)
	if err != nil {
		t.Fatalf("DataForkRanges failed: %v", err)
	}
	if len(ranges) != 1 {
		t.Fatalf("got %d ranges, want 1: %#v", len(ranges), ranges)
	}
	r := ranges[0]

	wantOffset := wrapperEmbeddedOffset + int64(wrapperDataBlock)*int64(wrapperBlockSize)
	if r.DiskOffset != wantOffset {
		t.Fatalf("DiskOffset = %d, want %d", r.DiskOffset, wantOffset)
	}
	// A regression that drops the base offset lands exactly here, and would
	// otherwise be reported only as a mismatch of unrelated bytes.
	if r.DiskOffset == int64(wrapperDataBlock)*int64(wrapperBlockSize) {
		t.Fatal("DiskOffset omits the volume's base offset")
	}

	if r.StartBlock != wrapperDataBlock || r.BlockCount != 1 {
		t.Fatalf("unexpected block addressing: %#v", r)
	}
	if r.ForkOffset != 0 {
		t.Fatalf("ForkOffset = %d, want 0", r.ForkOffset)
	}
	if r.Length != int64(len(wrapperPayload)) {
		t.Fatalf("Length = %d, want %d", r.Length, len(wrapperPayload))
	}
	if want := int64(wrapperBlockSize) - int64(len(wrapperPayload)); r.Slack != want {
		t.Fatalf("Slack = %d, want %d", r.Slack, want)
	}
	if got := r.AllocatedLength(); got != int64(wrapperBlockSize) {
		t.Fatalf("AllocatedLength() = %d, want %d", got, wrapperBlockSize)
	}

	// The criterion itself: read the raw image directly, not through the
	// volume, at the offset the library reported.
	buf := make([]byte, r.Length)
	if _, err := bytes.NewReader(img).ReadAt(buf, r.DiskOffset); err != nil {
		t.Fatalf("reading the image at DiskOffset failed: %v", err)
	}
	if !bytes.Equal(buf, wrapperPayload) {
		t.Fatalf("bytes at DiskOffset = %q, want %q", buf, wrapperPayload)
	}

	// And the library's own reader must agree with its own addressing.
	f, err := vol.OpenFileByCNID(wrapperFileCNID)
	if err != nil {
		t.Fatalf("OpenFileByCNID failed: %v", err)
	}
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(got, wrapperPayload) {
		t.Fatalf("ReadAll = %q, want %q", got, wrapperPayload)
	}
}

func TestBaseOffsetZeroOnPlainVolume(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildValidCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if got := vol.BaseOffset(); got != 0 {
		t.Fatalf("BaseOffset() = %d, want 0 for a volume at the start of the reader", got)
	}
}

// TestBaseOffsetClassicHFS pins the classic-HFS meaning of the base offset:
// drAlBlSt*512, the start of the allocation-block area, because classic HFS
// numbers allocation block 0 from there rather than from the volume start.
func TestBaseOffsetClassicHFS(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("unexpected kind: %s", vol.Kind())
	}
	const wantBase = int64(4) * hfsSectorSize // the fixture's drAlBlSt is 4
	if got := vol.BaseOffset(); got != wantBase {
		t.Fatalf("BaseOffset() = %d, want %d", got, wantBase)
	}
	off, err := vol.BlockOffset(3)
	if err != nil {
		t.Fatalf("BlockOffset failed: %v", err)
	}
	if want := wantBase + 3*int64(vol.Header().BlockSize); off != want {
		t.Fatalf("BlockOffset(3) = %d, want %d", off, want)
	}
}

func TestExtentRangesSlackAndClipping(t *testing.T) {
	const blockSize = uint32(4096)
	vol := &Volume{header: VolumeHeader{BlockSize: blockSize}, maxAlloc: DefaultMaxAlloc}

	two := []ExtentDescriptor{{StartBlock: 10, BlockCount: 2}, {StartBlock: 30, BlockCount: 1}}

	tests := []struct {
		name        string
		exts        []ExtentDescriptor
		logicalSize int64
		want        []ByteRange
	}{
		{
			name:        "clips the final range and reports the remainder as slack",
			exts:        two,
			logicalSize: 8192 + 100,
			want: []ByteRange{
				{ForkOffset: 0, DiskOffset: 40960, Length: 8192, Slack: 0, StartBlock: 10, BlockCount: 2},
				{ForkOffset: 8192, DiskOffset: 122880, Length: 100, Slack: 3996, StartBlock: 30, BlockCount: 1},
			},
		},
		{
			name:        "an exact multiple of the block size leaves no slack",
			exts:        two,
			logicalSize: 12288,
			want: []ByteRange{
				{ForkOffset: 0, DiskOffset: 40960, Length: 8192, StartBlock: 10, BlockCount: 2},
				{ForkOffset: 8192, DiskOffset: 122880, Length: 4096, StartBlock: 30, BlockCount: 1},
			},
		},
		{
			name:        "a zero size makes every allocated byte slack",
			exts:        two,
			logicalSize: 0,
			want: []ByteRange{
				{ForkOffset: 0, DiskOffset: 40960, Length: 0, Slack: 8192, StartBlock: 10, BlockCount: 2},
				{ForkOffset: 8192, DiskOffset: 122880, Length: 0, Slack: 4096, StartBlock: 30, BlockCount: 1},
			},
		},
		{
			name:        "SizeUnknown treats everything as data",
			exts:        two,
			logicalSize: SizeUnknown,
			want: []ByteRange{
				{ForkOffset: 0, DiskOffset: 40960, Length: 8192, StartBlock: 10, BlockCount: 2},
				{ForkOffset: 8192, DiskOffset: 122880, Length: 4096, StartBlock: 30, BlockCount: 1},
			},
		},
		{
			name:        "a size larger than the extents hold is clamped, not rejected",
			exts:        two,
			logicalSize: 1 << 20,
			want: []ByteRange{
				{ForkOffset: 0, DiskOffset: 40960, Length: 8192, StartBlock: 10, BlockCount: 2},
				{ForkOffset: 8192, DiskOffset: 122880, Length: 4096, StartBlock: 30, BlockCount: 1},
			},
		},
		{
			name:        "an extent wholly past the size is still reported, as pure slack",
			exts:        two,
			logicalSize: 4096,
			want: []ByteRange{
				{ForkOffset: 0, DiskOffset: 40960, Length: 4096, Slack: 4096, StartBlock: 10, BlockCount: 2},
				{ForkOffset: 8192, DiskOffset: 122880, Length: 0, Slack: 4096, StartBlock: 30, BlockCount: 1},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vol.ExtentRanges(tc.exts, tc.logicalSize)
			if err != nil {
				t.Fatalf("ExtentRanges failed: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d ranges, want %d: %#v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("range %d = %#v, want %#v", i, got[i], tc.want[i])
				}
				if got[i].AllocatedLength() != int64(got[i].BlockCount)*int64(blockSize) {
					t.Fatalf("range %d: Length+Slack does not cover the allocated blocks: %#v", i, got[i])
				}
			}
		})
	}
}

func TestExtentRangesRejectsBadGeometry(t *testing.T) {
	exts := []ExtentDescriptor{{StartBlock: 1, BlockCount: 1}}

	t.Run("zero block size", func(t *testing.T) {
		vol := &Volume{header: VolumeHeader{BlockSize: 0}}
		if _, err := vol.ExtentRanges(exts, 10); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("expected ErrCorrupt, got %v", err)
		}
	})

	t.Run("block offset overflows int64", func(t *testing.T) {
		// A corrupt header can declare a 4 GiB block size, and a block number
		// near 2^32 of it does not fit in the offset type io.ReaderAt takes.
		vol := &Volume{header: VolumeHeader{BlockSize: 0xFFFFFFFF}}
		huge := []ExtentDescriptor{{StartBlock: 0xFFFFFFFF, BlockCount: 1}}
		if _, err := vol.ExtentRanges(huge, SizeUnknown); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("expected ErrCorrupt, got %v", err)
		}
	})

	t.Run("extent span overflows int64", func(t *testing.T) {
		vol := &Volume{header: VolumeHeader{BlockSize: 0xFFFFFFFF}}
		huge := []ExtentDescriptor{{StartBlock: 1, BlockCount: 0xFFFFFFFF}}
		if _, err := vol.ExtentRanges(huge, SizeUnknown); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("expected ErrCorrupt, got %v", err)
		}
	})

	t.Run("negative size that is not SizeUnknown", func(t *testing.T) {
		vol := &Volume{header: VolumeHeader{BlockSize: 4096}}
		if _, err := vol.ExtentRanges(exts, -99); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("expected ErrCorrupt, got %v", err)
		}
	})

	t.Run("an empty extent list is not an error", func(t *testing.T) {
		vol := &Volume{header: VolumeHeader{BlockSize: 4096}}
		got, err := vol.ExtentRanges(nil, 0)
		if err != nil || got != nil {
			t.Fatalf("ExtentRanges(nil) = %v, %v; want nil, nil", got, err)
		}
	})
}

// TestDataForkRangesFollowsOverflow checks that ranges cover a fork whose
// fragments continue into the extents-overflow tree, and that the marker bytes
// the fixture writes at the logical extent boundary are where the ranges say.
func TestDataForkRangesFollowsOverflow(t *testing.T) {
	img := buildExtentsOverflowTestImage(t, true)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	exts, err := vol.ResolveDataForkExtents(101)
	if err != nil {
		t.Fatalf("ResolveDataForkExtents failed: %v", err)
	}
	ranges, err := vol.DataForkRanges(101)
	if err != nil {
		t.Fatalf("DataForkRanges failed: %v", err)
	}
	if len(ranges) != len(exts) {
		t.Fatalf("got %d ranges for %d extents", len(ranges), len(exts))
	}
	if len(ranges) < 2 {
		t.Fatalf("fixture should span the overflow tree, got %d ranges", len(ranges))
	}

	for i, r := range ranges {
		if r.StartBlock != exts[i].StartBlock || r.BlockCount != exts[i].BlockCount {
			t.Fatalf("range %d does not match extent %d: %#v vs %#v", i, i, r, exts[i])
		}
		off, err := vol.BlockOffset(exts[i].StartBlock)
		if err != nil {
			t.Fatalf("BlockOffset failed: %v", err)
		}
		if r.DiskOffset != off {
			t.Fatalf("range %d DiskOffset = %d, want %d", i, r.DiskOffset, off)
		}
	}

	// ForkOffsets must be contiguous across the allocated spans.
	var expect int64
	for i, r := range ranges {
		if r.ForkOffset != expect {
			t.Fatalf("range %d ForkOffset = %d, want %d", i, r.ForkOffset, expect)
		}
		expect += r.AllocatedLength()
	}

	// The fixture writes "ABCD" at the end of the first extent and
	// "EFGHIJKL" at the start of the second.
	first := ranges[0]
	buf := make([]byte, 4)
	if _, err := bytes.NewReader(img).ReadAt(buf, first.DiskOffset+first.AllocatedLength()-4); err != nil {
		t.Fatalf("reading the first extent tail failed: %v", err)
	}
	if string(buf) != "ABCD" {
		t.Fatalf("first extent tail = %q, want %q", buf, "ABCD")
	}

	buf = make([]byte, 8)
	if _, err := bytes.NewReader(img).ReadAt(buf, ranges[1].DiskOffset); err != nil {
		t.Fatalf("reading the second extent head failed: %v", err)
	}
	if string(buf) != "EFGHIJKL" {
		t.Fatalf("second extent head = %q, want %q", buf, "EFGHIJKL")
	}
}

func TestXAttrRanges(t *testing.T) {
	vol := openXAttrFixture(t)

	t.Run("fork-backed attribute maps every extent", func(t *testing.T) {
		attrs, err := vol.ListXAttrs(101)
		if err != nil {
			t.Fatalf("ListXAttrs failed: %v", err)
		}
		var fork XAttr
		for _, a := range attrs {
			if a.Storage == XAttrFork {
				fork = a
				break
			}
		}
		if fork.Name == "" {
			t.Fatal("fixture has no fork-backed attribute on CNID 101")
		}

		ranges, err := vol.XAttrRanges(101, fork.Name)
		if err != nil {
			t.Fatalf("XAttrRanges failed: %v", err)
		}
		if len(ranges) != len(fork.Extents) {
			t.Fatalf("got %d ranges for %d extents", len(ranges), len(fork.Extents))
		}

		var expect, data int64
		for i, r := range ranges {
			if r.ForkOffset != expect {
				t.Fatalf("range %d ForkOffset = %d, want %d", i, r.ForkOffset, expect)
			}
			if r.StartBlock != fork.Extents[i].StartBlock {
				t.Fatalf("range %d StartBlock = %d, want %d", i, r.StartBlock, fork.Extents[i].StartBlock)
			}
			expect += r.AllocatedLength()
			data += r.Length
		}
		if data != int64(fork.Size) {
			t.Fatalf("ranges cover %d data bytes, want the attribute size %d", data, fork.Size)
		}
	})

	t.Run("inline attribute has no ranges and is not an error", func(t *testing.T) {
		attrs, err := vol.ListXAttrs(100)
		if err != nil {
			t.Fatalf("ListXAttrs failed: %v", err)
		}
		if len(attrs) == 0 || attrs[0].Storage != XAttrInline {
			t.Fatalf("fixture CNID 100 should carry an inline attribute, got %#v", attrs)
		}
		ranges, err := vol.XAttrRanges(100, attrs[0].Name)
		if err != nil {
			t.Fatalf("XAttrRanges on an inline attribute failed: %v", err)
		}
		if len(ranges) != 0 {
			t.Fatalf("inline attribute reported %d ranges, want none", len(ranges))
		}
	})

	t.Run("a missing attribute reports ErrNotFound", func(t *testing.T) {
		if _, err := vol.XAttrRanges(100, "com.example.absent"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}
