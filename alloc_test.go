package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
	"testing"
)

// The bitmap and the volume header are two independent records of the same
// fact. Counting the bits and comparing against the header's claim validates
// both the bitmap addressing and the header parse.
func TestCorpusAllocationBitmapMatchesHeader(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	counted, err := vol.FreeBlockCount()
	if err != nil {
		t.Fatalf("FreeBlockCount: %v", err)
	}
	claimed := vol.Header().FreeBlocks

	t.Logf("bitmap counts %d free blocks; header claims %d (of %d total)",
		counted, claimed, vol.Header().TotalBlocks)

	if counted != claimed {
		t.Errorf("free block count mismatch: bitmap %d, header %d (delta %+d)",
			counted, claimed, int64(counted)-int64(claimed))
	}
}

// Blocks holding live file data must read as allocated. This is the property
// deleted-file recovery depends on to tell surviving content from reused space.
func TestCorpusLiveFileBlocksAreAllocated(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	var checked, allocated int
	err := vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Type != CatalogRecordFile || r.DataFork.TotalBlocks == 0 || checked >= 200 {
			return nil
		}
		exts, err := vol.ResolveDataForkExtents(r.CNID)
		if err != nil {
			return nil
		}
		for _, e := range exts {
			for b := e.StartBlock; b < e.StartBlock+e.BlockCount && b < e.StartBlock+4; b++ {
				checked++
				inUse, err := vol.BlockAllocated(b)
				if err != nil {
					t.Errorf("BlockAllocated(%d) for cnid %d: %v", b, r.CNID, err)
					continue
				}
				if inUse {
					allocated++
				} else {
					t.Errorf("block %d holds live data for cnid %d (%q) but reads as free",
						b, r.CNID, r.Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}
	t.Logf("checked %d blocks belonging to live files; %d allocated", checked, allocated)
	if checked == 0 {
		skipEmptyCorpus(t, vol, "live file blocks")
		t.Fatal("no blocks checked; the test proved nothing")
	}
}

func TestCorpusWalkUnallocatedRuns(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	var runs int
	var blocks uint32
	var lastEnd uint32
	total := vol.Header().TotalBlocks

	err := vol.WalkUnallocated(func(start, count uint32) error {
		if count == 0 {
			t.Errorf("run at %d has zero length", start)
		}
		if start < lastEnd {
			t.Errorf("run at %d overlaps or precedes the previous run ending at %d", start, lastEnd)
		}
		if start >= lastEnd && lastEnd != 0 && start == lastEnd {
			t.Errorf("run at %d is adjacent to the previous one; runs must be maximal", start)
		}
		if start+count > total {
			t.Errorf("run %d+%d exceeds total blocks %d", start, count, total)
		}
		lastEnd = start + count
		runs++
		blocks += count
		return nil
	})
	if err != nil {
		t.Fatalf("WalkUnallocated: %v", err)
	}
	t.Logf("%d free runs covering %d blocks", runs, blocks)

	if got, err := vol.FreeBlockCount(); err != nil || got != blocks {
		t.Errorf("FreeBlockCount = %d, %v; walk summed to %d", got, err, blocks)
	}
}

// A callback's own error stops the walk and comes back unchanged. The sentinel
// here is deliberately not [ErrStopWalk]: that one means "I am done", and
// reporting it as a failure would be as wrong as reporting a real read error as
// success. TestWalkStopSentinel covers the other half.
func TestWalkUnallocatedStopsOnCallbackError(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	sentinel := errors.New("caller gave up")
	var seen int
	err := vol.WalkUnallocated(func(uint32, uint32) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WalkUnallocated returned %v, want the callback's error", err)
	}
	if seen != 1 {
		t.Errorf("callback ran %d times after returning an error, want 1", seen)
	}
}

func TestBlockAllocatedRejectsOutOfRange(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildValidCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := vol.BlockAllocated(vol.Header().TotalBlocks); err == nil {
		t.Error("BlockAllocated past the last block returned nil error")
	}
	if _, err := vol.BlockAllocated(^uint32(0)); err == nil {
		t.Error("BlockAllocated on a huge block number returned nil error")
	}
}

// ---------------------------------------------------------------------------
// Allocation guard
// ---------------------------------------------------------------------------

func TestReadAllRespectsMaxAlloc(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	// Find a file with some content.
	var target uint32
	var size uint64
	err := vol.WalkCatalog(func(r CatalogRecord) error {
		if target == 0 && r.Type == CatalogRecordFile && r.DataFork.LogicalSize > 1024 {
			target, size = r.CNID, r.DataFork.LogicalSize
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}
	if target == 0 {
		t.Skip("no file larger than 1 KiB in the corpus image")
	}

	fh, err := vol.OpenFileByCNID(target)
	if err != nil {
		t.Fatalf("OpenFileByCNID: %v", err)
	}

	vol.SetMaxAlloc(int64(size) - 1)
	if _, err := fh.ReadAll(); err == nil {
		t.Error("ReadAll exceeded the allocation cap without an error")
	} else if !isSizeLimit(err) {
		t.Errorf("ReadAll returned %v, want ErrSizeLimit", err)
	}

	// ReadAt is unaffected: the caller supplies the buffer.
	buf := make([]byte, 512)
	if _, err := fh.ReadAt(buf, 0); err != nil {
		t.Errorf("ReadAt under a low cap failed: %v", err)
	}

	vol.SetMaxAlloc(0) // unlimited
	data, err := fh.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll with the cap removed: %v", err)
	}
	if uint64(len(data)) != size {
		t.Errorf("read %d bytes, want %d", len(data), size)
	}

	vol.SetMaxAlloc(DefaultMaxAlloc)
	if got := vol.MaxAlloc(); got != DefaultMaxAlloc {
		t.Errorf("MaxAlloc = %d, want %d", got, DefaultMaxAlloc)
	}
}

func isSizeLimit(err error) bool {
	for err != nil {
		if err == ErrSizeLimit {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ---------------------------------------------------------------------------
// Classic HFS bitmap addressing
// ---------------------------------------------------------------------------

const (
	// classicVBMStart is drVBMSt: the bitmap begins at sector 3, immediately
	// after the MDB and comfortably before the allocation-block area the
	// classic fixture starts at sector 4.
	classicVBMStart = uint16(3)

	// classicBitmapTotalBlocks matches drNmAlBlks in buildClassicHFSTimesImage.
	classicBitmapTotalBlocks = uint32(100)

	// classicBitmapAllocated is how many blocks the pattern below marks in use.
	classicBitmapAllocated = 9
)

// buildClassicHFSBitmapImage adds a real volume bitmap to the classic fixture,
// and fills the first allocation block with 0xFF.
//
// The fill is the point of the fixture. drVBMSt and drAlBlSt are both measured
// from the start of the volume, so code that adds the allocation-block base to
// the bitmap offset lands inside the allocation-block area instead of on the
// bitmap. Making that area say "everything is allocated" while the bitmap says
// otherwise turns a silent misread into a failed assertion.
func buildClassicHFSBitmapImage(t testing.TB) []byte {
	t.Helper()

	img := buildClassicHFSTimesImage(t)

	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(mdb[hfsMDBOffVBMStart:hfsMDBOffVBMStart+2], classicVBMStart)

	// Blocks 0-7 in use, block 8 in use, everything after it free.
	bitmap := int(classicVBMStart) * hfsSectorSize
	img[bitmap] = 0xFF
	img[bitmap+1] = 0x80

	allocArea := int(classicHFSDataBase)
	for i := allocArea; i < allocArea+int(classicHFSBlockSize) && i < len(img); i++ {
		img[i] = 0xFF
	}
	return img
}

// TestClassicHFSBitmapOffset is the regression test for the bitmap address:
// the volume bitmap is found from the volume start, not from the
// allocation-block area that [Volume.BaseOffset] reports.
func TestClassicHFSBitmapOffset(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSBitmapImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("fixture opened as %s, want %s", vol.Kind(), KindHFS)
	}
	// The premise of the fixture: the two offsets really are different, so
	// adding them really would read the wrong bytes.
	if vol.BaseOffset() == 0 {
		t.Fatal("fixture has a zero base offset; it cannot distinguish the two addressings")
	}

	for _, tc := range []struct {
		block uint32
		want  bool
	}{
		{0, true}, {7, true}, {8, true},
		{9, false}, {50, false}, {99, false},
	} {
		got, err := vol.BlockAllocated(tc.block)
		if err != nil {
			t.Fatalf("BlockAllocated(%d) failed: %v", tc.block, err)
		}
		if got != tc.want {
			t.Fatalf("BlockAllocated(%d) = %v, want %v — the bitmap was read from the wrong offset",
				tc.block, got, tc.want)
		}
	}

	free, err := vol.FreeBlockCount()
	if err != nil {
		t.Fatalf("FreeBlockCount failed: %v", err)
	}
	if want := classicBitmapTotalBlocks - classicBitmapAllocated; free != want {
		t.Fatalf("FreeBlockCount = %d, want %d", free, want)
	}
}

// TestClassicHFSWalkUnallocatedRuns checks that the run boundaries the carver
// depends on come out of the same correctly addressed bitmap.
func TestClassicHFSWalkUnallocatedRuns(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSBitmapImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	type run struct{ start, count uint32 }
	var runs []run
	err = vol.WalkUnallocated(func(start, count uint32) error {
		runs = append(runs, run{start, count})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkUnallocated failed: %v", err)
	}

	want := []run{{9, classicBitmapTotalBlocks - classicBitmapAllocated}}
	if len(runs) != len(want) || runs[0] != want[0] {
		t.Fatalf("runs = %v, want %v", runs, want)
	}
}

// TestBitmapByteCountDoesNotWrap covers the rounding-up arithmetic for a
// volume declaring close to 2^32 allocation blocks.
//
// The count is computed in int64 rather than in the header's uint32 because
// total+7 wraps there, which would leave the bitmap unread and every block
// reported as in use. The reader here is far too small to hold such a bitmap,
// so a correct implementation fails trying to read it; the wrapped one reads
// nothing at all and reports success.
func TestBitmapByteCountDoesNotWrap(t *testing.T) {
	vol := &Volume{
		reader: bytes.NewReader(make([]byte, 64)),
		kind:   KindHFS,
		header: VolumeHeader{BlockSize: 512, TotalBlocks: 0xFFFFFFFF},
	}

	runs := 0
	err := vol.WalkUnallocated(func(uint32, uint32) error {
		runs++
		return nil
	})
	if err == nil {
		t.Fatalf("WalkUnallocated reported success over a bitmap it could not read, with %d runs", runs)
	}
}

// ---------------------------------------------------------------------------
// Block accounting
// ---------------------------------------------------------------------------

// blockBitset is a set of allocation block numbers.
//
// A map would be the obvious choice, but a forensic image is routinely large
// enough for one entry per block to cost gigabytes — a terabyte volume with
// 4 KiB blocks has 268 million of them — where the bitset costs 32 MiB. The
// test has to be affordable on the images it exists to be pointed at.
type blockBitset struct {
	bits  []uint64
	limit uint32
}

func newBlockBitset(limit uint32) *blockBitset {
	return &blockBitset{bits: make([]uint64, (uint64(limit)+63)/64), limit: limit}
}

// add records a block and reports whether it was newly added.
func (s *blockBitset) add(b uint32) bool {
	if b >= s.limit {
		return false
	}
	word, mask := b/64, uint64(1)<<(b%64)
	if s.bits[word]&mask != 0 {
		return false
	}
	s.bits[word] |= mask
	return true
}

func (s *blockBitset) has(b uint32) bool {
	if b >= s.limit {
		return false
	}
	return s.bits[b/64]&(uint64(1)<<(b%64)) != 0
}

// reservedBlocks returns the allocation blocks the format itself keeps in use
// even though no file owns them.
//
// HFS+ reserves the first 1024 bytes for boot blocks and the 512 after them
// for the volume header, and the last 1024 bytes for the alternate volume
// header and a final reserved sector (Apple TN1150, "Volume Header"). Those
// byte ranges lie inside allocation blocks, so the bitmap marks those blocks
// in use and the catalog will never account for them.
//
// Classic HFS has the same structures but keeps them outside the allocation
// block area — that displacement is exactly what drAlBlSt measures and what
// Volume.BaseOffset returns — so no allocation block covers them and the set
// is empty.
func reservedBlocks(vol *Volume) []uint32 {
	if vol.Kind() == KindHFS {
		return nil
	}

	h := vol.Header()
	blockSize := uint64(h.BlockSize)
	if blockSize == 0 || h.TotalBlocks == 0 {
		return nil
	}
	volumeBytes := uint64(h.TotalBlocks) * blockSize

	seen := make(map[uint32]bool)
	var out []uint32
	mark := func(from, to uint64) {
		if to > volumeBytes {
			to = volumeBytes
		}
		for off := from / blockSize * blockSize; off < to; off += blockSize {
			b := uint32(off / blockSize)
			if !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	mark(0, 1536)
	if volumeBytes >= 1024 {
		mark(volumeBytes-1024, volumeBytes)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func forkOf(rec CatalogRecord, resource bool) ForkData {
	if resource {
		return rec.RsrcFork
	}
	return rec.DataFork
}

func forkTypeOf(resource bool) uint8 {
	if resource {
		return extentKeyTypeRsrc
	}
	return extentKeyTypeData
}

// walkClaimedBlocks reports every allocation block the volume's own metadata
// says belongs to something, passing each one with the name of what claims it.
//
// A block may be offered more than once; detecting that is the point. This is
// a function rather than inline code because the caller runs it twice — once
// to find collisions cheaply, once to name both sides of the ones it found.
func walkClaimedBlocks(tb testing.TB, vol *Volume, claim func(block uint32, owner string)) {
	tb.Helper()

	h := vol.Header()
	total := h.TotalBlocks

	claimExtents := func(exts []ExtentDescriptor, owner string) {
		for _, e := range exts {
			// Widened because StartBlock+BlockCount is free to wrap on a
			// damaged volume, and a wrapped loop bound never terminates.
			end := uint64(e.StartBlock) + uint64(e.BlockCount)
			if end > uint64(total) {
				end = uint64(total)
			}
			for b := uint64(e.StartBlock); b < end; b++ {
				claim(uint32(b), owner)
			}
		}
	}

	specials := []struct {
		name string
		cnid uint32
		fork ForkData
	}{
		{"$Allocation", allocationFileCNID, h.AllocationFile},
		{"$Extents", extentsFileCNID, h.ExtentsFile},
		{"$Catalog", catalogFileCNID, h.CatalogFile},
		{"$Attributes", attributesFileCNID, h.AttributesFile},
		{"$Startup", startupFileCNID, h.StartupFile},
	}
	for _, s := range specials {
		if s.fork.TotalBlocks == 0 {
			continue
		}
		exts, err := vol.forkExtents(s.cnid, s.fork)
		if err != nil {
			tb.Fatalf("forkExtents(%s): %v", s.name, err)
		}
		claimExtents(exts, s.name)
	}

	hasAttributes := h.AttributesFile.TotalBlocks > 0

	err := vol.WalkPaths(func(path string, rec CatalogRecord) error {
		// A hard link's record is a stub: the blocks belong to the inode in
		// the private data directory, which this walk reaches on its own. The
		// fork methods resolve the link, so counting the stub as well would
		// report the inode's blocks as claimed twice over.
		if rec.Link == LinkHardFile || rec.Link == LinkHardDir {
			return nil
		}

		if hasAttributes {
			attrs, err := vol.ListXAttrs(rec.CNID)
			if err == nil {
				for _, a := range attrs {
					if a.Storage == XAttrFork {
						claimExtents(a.Extents, path+" (xattr "+a.Name+")")
					}
				}
			}
		}

		if rec.IsDirectory() {
			return nil
		}
		for _, fork := range []struct {
			label    string
			resource bool
		}{{"data", false}, {"rsrc", true}} {
			exts, err := vol.resolveForkExtentsFromFork(rec.CNID, forkOf(rec, fork.resource), forkTypeOf(fork.resource))
			if err != nil {
				tb.Errorf("resolve %s fork of %q: %v", fork.label, path, err)
				continue
			}
			claimExtents(exts, path+" ("+fork.label+")")
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("WalkPaths: %v", err)
	}
}

func firstFewBlocks(blocks []uint32) []uint32 {
	if len(blocks) > 10 {
		return blocks[:10]
	}
	return blocks
}

// The catalog and the allocation bitmap are two independent records of which
// blocks are in use. The other tests here compare them in aggregate — a free
// block count against the header's claim, a sample of live blocks against the
// bitmap — so they pass whenever two errors cancel. This reconciles the two
// block by block and in both directions.
//
// It is also the only place a cross-link can show up: two objects whose
// extents name the same block. No per-file check can see that, because each
// file is self-consistent on its own, and a change-tracking tool built on
// these ranges would attribute one file's bytes to another without complaint.
func TestCorpusBlockAccounting(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	total := vol.Header().TotalBlocks
	if total == 0 {
		t.Fatal("volume reports no allocation blocks; the test would prove nothing")
	}

	claimed := newBlockBitset(total)
	collided := newBlockBitset(total)
	var distinct, collisions int
	var reported []uint32

	walkClaimedBlocks(t, vol, func(b uint32, _ string) {
		if claimed.add(b) {
			distinct++
			return
		}
		if !collided.add(b) {
			return
		}
		collisions++
		// Only the first few are named below. A volume where everything
		// collides would otherwise produce a diagnostic far larger than the
		// volume's own catalog, which is how the first draft of this test
		// behaved under a mutated extent parser.
		if len(reported) < maxReportedCollisions {
			reported = append(reported, b)
		}
	})
	if distinct == 0 {
		t.Fatal("nothing claimed a single block; the test proved nothing")
	}

	// Name both sides of each collision. This second pass runs only on
	// failure, which is why the first one need not carry the owners.
	if collisions > 0 {
		wanted := newBlockBitset(total)
		for _, b := range reported {
			wanted.add(b)
		}
		owners := make(map[uint32][]string, len(reported))
		walkClaimedBlocks(t, vol, func(b uint32, owner string) {
			if wanted.has(b) && len(owners[b]) < maxReportedOwners {
				owners[b] = append(owners[b], owner)
			}
		})
		t.Errorf("%d allocation blocks are claimed by more than one object", collisions)
		for _, b := range reported {
			t.Errorf("  block %d claimed by %v", b, owners[b])
		}
	}

	// Free blocks per the bitmap, gathered in one chunked pass: BlockAllocated
	// costs a read per call and there is one call per block on the volume.
	free := newBlockBitset(total)
	var freeCount uint32
	if err := vol.WalkUnallocated(func(start, count uint32) error {
		for b := start; b < start+count && b < total; b++ {
			free.add(b)
			freeCount++
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkUnallocated: %v", err)
	}

	reserved := reservedBlocks(vol)
	reservedSet := newBlockBitset(total)
	for _, b := range reserved {
		reservedSet.add(b)
	}

	var claimedButFree, unaccounted []uint32
	for b := uint32(0); b < total; b++ {
		switch {
		case claimed.has(b) && free.has(b):
			claimedButFree = append(claimedButFree, b)
		case !claimed.has(b) && !free.has(b) && !reservedSet.has(b):
			unaccounted = append(unaccounted, b)
		}
	}

	t.Logf("catalog claims %d of %d blocks; bitmap says %d in use and %d free; %d reserved by the format %v",
		distinct, total, total-freeCount, freeCount, len(reserved), firstFewBlocks(reserved))

	// A block holding live content that the bitmap calls free is what makes
	// deleted-file recovery unsound: carving treats it as recoverable space
	// and hands back another file's live data.
	if len(claimedButFree) > 0 {
		t.Errorf("%d blocks hold live content but read as free, first few %v",
			len(claimedButFree), firstFewBlocks(claimedButFree))
	}

	// The other direction. With the format's own reserved blocks excluded,
	// every block the bitmap calls in use should belong to something the
	// catalog names. A remainder means either that extent resolution is losing
	// fragments — losing one is invisible to every per-file check, since the
	// file then reads short consistently everywhere — or that the volume
	// genuinely has space leaked by whatever wrote it.
	if len(unaccounted) > 0 {
		t.Errorf("%d blocks are marked in use but nothing claims them, first few %v",
			len(unaccounted), firstFewBlocks(unaccounted))
	}

	// The reserved blocks are not merely excused from the reconciliation: the
	// format requires them to be in use, so check that they are.
	for _, b := range reserved {
		if free.has(b) {
			t.Errorf("reserved block %d reads as free", b)
		}
		if claimed.has(b) {
			t.Errorf("reserved block %d is claimed by a catalog object", b)
		}
	}
}

// Caps on the block-accounting diagnostics. A failure is normally a handful of
// blocks; a systematic one is every block on the volume, and printing that is
// no more informative than printing ten of them.
const (
	maxReportedCollisions = 10
	maxReportedOwners     = 4
)

// The reserved-block derivation is arithmetic over the block size, and the only
// real image that exercised it had 4096-byte blocks — the one size where the
// answer is also the obvious one, a single block at each end. At 512 the boot
// blocks and volume header span three blocks and the tail spans two, which no
// image in the corpus would have caught.
//
// Every expectation below was read off a volume formatted by Apple's
// mkfs.hfsplus (hfsprogs 540.1) at that block size and confirmed against its
// allocation bitmap: TestCorpusBlockAccounting reported exactly these blocks as
// the residue between what the catalog claims and what the bitmap calls in use,
// with nothing left over in either direction. They are literals here rather
// than a second computation because a test that recomputes the value it is
// checking agrees with any derivation, however wrong.
func TestReservedBlocksAcrossBlockSizes(t *testing.T) {
	cases := []struct {
		name        string
		kind        FileSystemKind
		blockSize   uint32
		totalBlocks uint32
		want        []uint32
	}{
		// Boot blocks occupy 0..1023 and the volume header 1024..1535, so at
		// 512 bytes a block those three structures are three distinct blocks.
		// The alternate volume header and the final reserved sector occupy the
		// last 1024 bytes, which is two more.
		{"512-byte blocks", KindHFSP, 512, 98304, []uint32{0, 1, 2, 98302, 98303}},
		// At 1024 the header shares block 1 with the second half of the boot
		// blocks, and the whole tail fits in one block.
		{"1024-byte blocks", KindHFSP, 1024, 49152, []uint32{0, 1, 49151}},
		{"4096-byte blocks", KindHFSP, 4096, 12288, []uint32{0, 12287}},
		{"65536-byte blocks", KindHFSP, 65536, 2048, []uint32{0, 2047}},
		// HFSX differs from HFS+ only in name comparison; the reserved
		// geometry is identical, and a derivation that keyed on Kind rather
		// than on the format family would get this wrong.
		{"HFSX is HFS+ geometry", KindHFSX, 512, 98304, []uint32{0, 1, 2, 98302, 98303}},
		// Classic HFS keeps its boot blocks and MDB outside the allocation
		// area — that displacement is exactly drAlBlSt — so no allocation
		// block covers them and nothing is reserved.
		{"classic HFS reserves nothing", KindHFS, 4096, 12287, nil},
		// Degenerate geometry must not divide by zero or loop forever. Open
		// rejects these, but reservedBlocks is reached from a test helper that
		// does not.
		{"zero block size", KindHFSP, 0, 1000, nil},
		{"zero total blocks", KindHFSP, 4096, 0, nil},
		// A volume too small to hold both ends separately: the whole thing is
		// reserved, and the two ranges must merge rather than report block 0
		// twice.
		{"single block volume", KindHFSP, 4096, 1, []uint32{0}},
		{"volume shorter than the tail", KindHFSP, 512, 1, []uint32{0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := &Volume{
				kind:   tc.kind,
				header: VolumeHeader{BlockSize: tc.blockSize, TotalBlocks: tc.totalBlocks},
			}
			got := reservedBlocks(vol)
			if len(got) != len(tc.want) {
				t.Fatalf("reservedBlocks = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("reservedBlocks = %v, want %v", got, tc.want)
				}
			}
			// Every reserved block must be addressable, or the accounting
			// would exclude a block that does not exist and quietly hide a
			// real one.
			for _, b := range got {
				if b >= tc.totalBlocks {
					t.Errorf("reserved block %d is past the end of a %d-block volume", b, tc.totalBlocks)
				}
			}
		})
	}
}
