package libhfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// countingReaderAt records how much of the image a call actually touched, so a
// test can prove keyed descent read a fraction of the tree rather than all of
// it. Safe for concurrent use, since *Volume is.
type countingReaderAt struct {
	inner io.ReaderAt
	calls atomic.Int64
	bytes atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	c.bytes.Add(int64(len(p)))
	return c.inner.ReadAt(p, off)
}

func (c *countingReaderAt) reset() {
	c.calls.Store(0)
	c.bytes.Store(0)
}

// The fixture models a volume with real shape: several directories, each
// holding several files. A single flat directory would make "scan this
// directory's children" identical to "scan the whole catalog", hiding whether
// keyed descent does anything.
const (
	validTreeDirCount    = 10
	validTreeFilesPerDir = 10
	validTreeFileCount   = validTreeDirCount * validTreeFilesPerDir
	validTreeVolName     = "volname"
)

func validTreeDirName(d int) string  { return fmt.Sprintf("dir%02d", d) }
func validTreeFileName(f int) string { return fmt.Sprintf("file%03d", f) }
func validTreeDirCNID(d int) uint32  { return uint32(10 + d) }
func validTreeFileCNID(d, f int) uint32 {
	return uint32(100 + d*validTreeFilesPerDir + f)
}
func validTreeFilePath(d, f int) string {
	return "/" + validTreeDirName(d) + "/" + validTreeFileName(f)
}

// ---------------------------------------------------------------------------
// Keyed descent correctness
// ---------------------------------------------------------------------------

func TestKeyedSearchListsAllChildren(t *testing.T) {
	vol, _ := openValidTree(t)

	// Root holds the directories.
	roots, err := vol.ReadDirCNID(rootFolderCNID)
	if err != nil {
		t.Fatalf("ReadDirCNID(root) failed: %v", err)
	}
	if len(roots) != validTreeDirCount {
		t.Fatalf("root has %d entries, want %d", len(roots), validTreeDirCount)
	}
	for d, ent := range roots {
		if want := validTreeDirName(d); ent.Name != want {
			t.Errorf("root entry %d: Name = %q, want %q", d, ent.Name, want)
		}
		if !ent.IsDirectory {
			t.Errorf("root entry %q: IsDirectory = false, want true", ent.Name)
		}
	}

	// Each directory holds its own files and nothing else.
	for d := range validTreeDirCount {
		entries, err := vol.ReadDirCNID(validTreeDirCNID(d))
		if err != nil {
			t.Fatalf("ReadDirCNID(%s) failed: %v", validTreeDirName(d), err)
		}
		if len(entries) != validTreeFilesPerDir {
			t.Fatalf("%s has %d entries, want %d", validTreeDirName(d), len(entries), validTreeFilesPerDir)
		}
		for f, ent := range entries {
			if want := validTreeFileName(f); ent.Name != want {
				t.Errorf("%s entry %d: Name = %q, want %q", validTreeDirName(d), f, ent.Name, want)
			}
			if want := validTreeFileCNID(d, f); ent.CNID != want {
				t.Errorf("%s/%s: CNID = %d, want %d", validTreeDirName(d), ent.Name, ent.CNID, want)
			}
		}
	}
	assertNoFallback(t, vol)
}

// Keyed descent must agree with the linear walk for every record in the tree.
// This is the property that matters: a faster path that disagrees is worse
// than no fast path at all.
func TestKeyedSearchMatchesLinearWalk(t *testing.T) {
	vol, _ := openValidTree(t)

	check := func(cnid uint32) {
		t.Helper()
		keyed, err := vol.lookupCNIDViaThread(cnid)
		if err != nil {
			t.Fatalf("keyed lookup of CNID %d failed: %v", cnid, err)
		}
		linear, err := vol.lookupCNIDLinear(cnid)
		if err != nil {
			t.Fatalf("linear lookup of CNID %d failed: %v", cnid, err)
		}
		if keyed.CNID != linear.CNID || keyed.Name != linear.Name ||
			keyed.ParentCNID != linear.ParentCNID || keyed.Type != linear.Type ||
			keyed.DataFork.LogicalSize != linear.DataFork.LogicalSize {
			t.Fatalf("CNID %d: keyed = %+v, linear = %+v", cnid, keyed, linear)
		}
	}

	for d := range validTreeDirCount {
		check(validTreeDirCNID(d))
		for f := range validTreeFilesPerDir {
			check(validTreeFileCNID(d, f))
		}
	}
	assertNoFallback(t, vol)
}

// A CNID that is not present must report ErrNotFound rather than returning a
// neighbouring record that descent happened to land next to.
func TestKeyedSearchMissingCNID(t *testing.T) {
	vol, _ := openValidTree(t)

	if _, err := vol.OpenCNID(999999); err == nil {
		t.Fatal("OpenCNID on an absent CNID returned nil error")
	}
	if _, err := vol.OpenPath("/nosuchfile"); err == nil {
		t.Fatal("OpenPath on an absent name returned nil error")
	}
}

func TestKeyedSearchPathRoundTrip(t *testing.T) {
	vol, _ := openValidTree(t)

	for d := range validTreeDirCount {
		for f := range validTreeFilesPerDir {
			cnid := validTreeFileCNID(d, f)
			want := validTreeFilePath(d, f)

			got, err := vol.PathForCNID(cnid)
			if err != nil {
				t.Fatalf("PathForCNID(%d) failed: %v", cnid, err)
			}
			if got != want {
				t.Errorf("PathForCNID(%d) = %q, want %q", cnid, got, want)
			}

			rec, err := vol.OpenPath(want)
			if err != nil {
				t.Fatalf("OpenPath(%q) failed: %v", want, err)
			}
			if rec.CNID != cnid {
				t.Errorf("OpenPath(%q).CNID = %d, want %d", want, rec.CNID, cnid)
			}
		}
	}
	assertNoFallback(t, vol)
}

// Descent must read a small fraction of the tree. Without this the rest of the
// suite would still pass with the linear walker silently doing all the work.
func TestKeyedSearchReadsFewerNodesThanFullScan(t *testing.T) {
	vol, counter := openValidTree(t)
	// Measure the search itself, with both caches out of the picture.
	vol.SetCacheSize(0)
	vol.SetNodeCacheSize(0)

	counter.reset()
	if err := vol.WalkCatalog(func(CatalogRecord) error { return nil }); err != nil {
		t.Fatalf("WalkCatalog failed: %v", err)
	}
	fullScanBytes := counter.bytes.Load()

	// Deepest, last-ordered path: the worst case for descent.
	counter.reset()
	if _, err := vol.OpenPath(validTreeFilePath(validTreeDirCount-1, validTreeFilesPerDir-1)); err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	lookupBytes := counter.bytes.Load()

	ratio := float64(lookupBytes) / float64(fullScanBytes)
	t.Logf("keyed lookup read %d bytes vs %d for a full scan (%.1f%%)",
		lookupBytes, fullScanBytes, 100*ratio)

	// A generous bound: the point is to catch descent silently regressing to a
	// full walk, not to pin an exact figure that fixture changes would break.
	if ratio > 0.5 {
		t.Fatalf("keyed lookup read %.1f%% of the tree; descent is not narrowing the search",
			100*ratio)
	}
	assertNoFallback(t, vol)
}

// ---------------------------------------------------------------------------
// Degradation
// ---------------------------------------------------------------------------

// The legacy fixtures carry an index node whose keys do not match their
// children's first keys. Descent must detect the overshoot, fall back, still
// return the right answer, and record the inconsistency as a finding.
func TestInconsistentIndexFallsBackAndRecordsAnomaly(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildCatalogThreadedTestImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	got, err := vol.PathForCNID(101)
	if err != nil {
		t.Fatalf("PathForCNID failed despite fallback: %v", err)
	}
	if want := "/etc/hosts"; got != want {
		t.Fatalf("PathForCNID(101) = %q, want %q", got, want)
	}

	if vol.AnomalyCount() == 0 {
		t.Fatal("expected an anomaly to be recorded for the inconsistent index")
	}
	found := false
	for _, a := range vol.Anomalies() {
		if a.Op == "catalog_search" {
			found = true
		}
	}
	if !found {
		t.Errorf("no catalog_search anomaly recorded; got %+v", vol.Anomalies())
	}
}

// Anomalies are deduplicated so a fallback firing per-file cannot flood the
// list, but the total count must keep rising.
func TestAnomalyDeduplication(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildCatalogThreadedTestImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	vol.SetCacheSize(0)

	for range 5 {
		if _, err := vol.PathForCNID(101); err != nil {
			t.Fatalf("PathForCNID failed: %v", err)
		}
	}

	distinct := len(vol.Anomalies())
	total := vol.AnomalyCount()
	if distinct == 0 {
		t.Fatal("no anomalies recorded")
	}
	if total <= distinct {
		t.Errorf("AnomalyCount = %d, len(Anomalies) = %d; repeats should be counted but not appended",
			total, distinct)
	}
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

func TestCacheAvoidsRepeatReads(t *testing.T) {
	vol, counter := openValidTree(t)

	cnid := validTreeFileCNID(validTreeDirCount-1, validTreeFilesPerDir-1)
	if _, err := vol.OpenCNID(cnid); err != nil {
		t.Fatalf("first OpenCNID failed: %v", err)
	}

	counter.reset()
	if _, err := vol.OpenCNID(cnid); err != nil {
		t.Fatalf("second OpenCNID failed: %v", err)
	}
	cached := counter.bytes.Load()

	vol.SetCacheSize(0)
	counter.reset()
	if _, err := vol.OpenCNID(cnid); err != nil {
		t.Fatalf("uncached OpenCNID failed: %v", err)
	}
	uncached := counter.bytes.Load()

	if cached >= uncached {
		t.Fatalf("cached lookup read %d bytes, uncached read %d; cache is not serving hits",
			cached, uncached)
	}
}

// Descent re-reads the root index node for every path component, so the node
// cache is what stops a deep lookup paying for it repeatedly.
func TestNodeCacheAvoidsRereadingIndexNodes(t *testing.T) {
	vol, counter := openValidTree(t)
	vol.SetCacheSize(0) // isolate the node cache from the record cache

	path := validTreeFilePath(validTreeDirCount-1, validTreeFilesPerDir-1)

	vol.SetNodeCacheSize(0)
	counter.reset()
	if _, err := vol.OpenPath(path); err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	uncached := counter.bytes.Load()

	vol.SetNodeCacheSize(DefaultNodeCacheSize)
	if _, err := vol.OpenPath(path); err != nil { // warm
		t.Fatalf("OpenPath failed: %v", err)
	}
	counter.reset()
	if _, err := vol.OpenPath(path); err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	cached := counter.bytes.Load()

	if cached >= uncached {
		t.Fatalf("node cache read %d bytes, uncached read %d; cache is not serving hits",
			cached, uncached)
	}
	t.Logf("node-cached lookup read %d bytes vs %d uncached", cached, uncached)
	assertNoFallback(t, vol)
}

func TestNodeCacheEvictionBounded(t *testing.T) {
	vol, _ := openValidTree(t)
	vol.SetNodeCacheSize(3)

	for d := range validTreeDirCount {
		if _, err := vol.ReadDirCNID(validTreeDirCNID(d)); err != nil {
			t.Fatalf("ReadDirCNID failed: %v", err)
		}
	}

	vol.mu.RLock()
	entries, order := len(vol.nodeCache), len(vol.nodeOrder)
	vol.mu.RUnlock()

	if entries > 3 || order > 3 {
		t.Fatalf("node cache holds %d entries / %d order slots, want <= 3", entries, order)
	}
}

// Catalog and extents nodes share a node-number space, so the cache key must
// keep them apart or one tree will serve the other's nodes.
func TestNodeCacheKeysAreTreeScoped(t *testing.T) {
	a := nodeCacheKey{tree: treeCatalog, num: 1}
	b := nodeCacheKey{tree: treeExtents, num: 1}
	c := nodeCacheKey{tree: treeAttributes, num: 1}

	if a == b || a == c || b == c {
		t.Fatalf("node cache keys collide across trees: %+v %+v %+v", a, b, c)
	}
}

func TestCacheEvictionBounded(t *testing.T) {
	vol, _ := openValidTree(t)
	vol.SetCacheSize(4)

	for d := range validTreeDirCount {
		for f := range validTreeFilesPerDir {
			if _, err := vol.OpenCNID(validTreeFileCNID(d, f)); err != nil {
				t.Fatalf("OpenCNID failed: %v", err)
			}
		}
	}

	vol.mu.RLock()
	entries, order := len(vol.recCache), len(vol.recOrder)
	vol.mu.RUnlock()

	if entries > 4 || order > 4 {
		t.Fatalf("cache holds %d entries / %d order slots, want <= 4", entries, order)
	}
}

func TestCacheDisabledReturnsCorrectResults(t *testing.T) {
	vol, _ := openValidTree(t)
	vol.SetCacheSize(0)

	for d := range validTreeDirCount {
		for f := range validTreeFilesPerDir {
			cnid := validTreeFileCNID(d, f)
			rec, err := vol.OpenCNID(cnid)
			if err != nil {
				t.Fatalf("OpenCNID(%d) failed: %v", cnid, err)
			}
			if rec.Name != validTreeFileName(f) {
				t.Errorf("CNID %d: Name = %q, want %q", cnid, rec.Name, validTreeFileName(f))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency — meaningful under -race
// ---------------------------------------------------------------------------

func TestConcurrentReaders(t *testing.T) {
	vol, _ := openValidTree(t)

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*validTreeFileCount)

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for d := range validTreeDirCount {
				for f := range validTreeFilesPerDir {
					cnid := validTreeFileCNID(d, f)

					rec, err := vol.OpenCNID(cnid)
					if err != nil {
						errs <- fmt.Errorf("goroutine %d: OpenCNID(%d): %w", g, cnid, err)
						return
					}
					if rec.Name != validTreeFileName(f) {
						errs <- fmt.Errorf("goroutine %d: CNID %d name = %q, want %q",
							g, cnid, rec.Name, validTreeFileName(f))
						return
					}
					path, err := vol.PathForCNID(cnid)
					if err != nil {
						errs <- fmt.Errorf("goroutine %d: PathForCNID(%d): %w", g, cnid, err)
						return
					}
					if want := validTreeFilePath(d, f); path != want {
						errs <- fmt.Errorf("goroutine %d: PathForCNID(%d) = %q, want %q", g, cnid, path, want)
						return
					}
				}
				if _, err := vol.ReadDirCNID(validTreeDirCNID(d)); err != nil {
					errs <- fmt.Errorf("goroutine %d: ReadDirCNID: %w", g, err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// ---------------------------------------------------------------------------
// Comparators
// ---------------------------------------------------------------------------

func TestCompareCatalogKeys(t *testing.T) {
	key := func(parent uint32, name string) CatalogKey {
		k := CatalogKey{ParentCNID: parent}
		for _, r := range name {
			k.NameUTF16 = append(k.NameUTF16, uint16(r))
		}
		return k
	}

	cases := []struct {
		a, b CatalogKey
		want int
	}{
		{key(2, ""), key(2, ""), 0},
		{key(2, ""), key(2, "a"), -1},   // empty name sorts first
		{key(2, "a"), key(2, ""), 1},    // ...and this is what descent relies on
		{key(2, "zzz"), key(3, ""), -1}, // parent dominates the name
		{key(3, ""), key(2, "zzz"), 1},  //
		{key(2, "abc"), key(2, "abd"), -1},
		{key(2, "ab"), key(2, "abc"), -1}, // prefix sorts first
	}
	for _, tc := range cases {
		if got := compareCatalogKeys(tc.a, tc.b); sign(got) != tc.want {
			t.Errorf("compareCatalogKeys(%v/%q, %v/%q) = %d, want %d",
				tc.a.ParentCNID, tc.a.NameString(), tc.b.ParentCNID, tc.b.NameString(), sign(got), tc.want)
		}
	}
}

func TestCompareExtentsKeys(t *testing.T) {
	k := func(id uint32, fork uint8, start uint32) ExtentsKey {
		return ExtentsKey{FileID: id, ForkType: fork, StartBlock: start}
	}
	cases := []struct {
		a, b ExtentsKey
		want int
	}{
		{k(5, extentKeyTypeData, 0), k(5, extentKeyTypeData, 0), 0},
		{k(4, extentKeyTypeRsrc, 99), k(5, extentKeyTypeData, 0), -1}, // file ID dominates
		{k(5, extentKeyTypeData, 9), k(5, extentKeyTypeRsrc, 0), -1},  // then fork type
		{k(5, extentKeyTypeData, 0), k(5, extentKeyTypeData, 1), -1},  // then start block
	}
	for _, tc := range cases {
		if got := compareExtentsKeys(tc.a, tc.b); sign(got) != tc.want {
			t.Errorf("compareExtentsKeys(%v, %v) = %d, want %d", tc.a, tc.b, sign(got), tc.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkOpenPathKeyed(b *testing.B) {
	img := buildValidCatalogImage(b)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	vol.SetCacheSize(0)
	path := validTreeFilePath(validTreeDirCount-1, validTreeFilesPerDir-1)

	b.ResetTimer()
	for b.Loop() {
		if _, err := vol.OpenPath(path); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadDirKeyed(b *testing.B) {
	img := buildValidCatalogImage(b)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	vol.SetCacheSize(0)

	b.ResetTimer()
	for b.Loop() {
		if _, err := vol.ReadDirCNID(validTreeDirCNID(validTreeDirCount - 1)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPathForCNIDKeyed(b *testing.B) {
	img := buildValidCatalogImage(b)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	vol.SetCacheSize(0)

	cnid := validTreeFileCNID(validTreeDirCount-1, validTreeFilesPerDir-1)

	b.ResetTimer()
	for b.Loop() {
		if _, err := vol.PathForCNID(cnid); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Fixture: a structurally valid multi-level catalog B-tree
// ---------------------------------------------------------------------------

func openValidTree(t *testing.T) (*Volume, *countingReaderAt) {
	t.Helper()
	counter := &countingReaderAt{inner: bytes.NewReader(buildValidCatalogImage(t))}
	vol, err := Open(counter)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return vol, counter
}

// assertNoFallback fails if any search degraded to a linear walk, which would
// mean the test passed without exercising keyed descent at all.
func assertNoFallback(t *testing.T, vol *Volume) {
	t.Helper()
	if n := vol.AnomalyCount(); n != 0 {
		t.Errorf("%d searches fell back to a linear walk; keyed descent was not exercised: %+v",
			n, vol.Anomalies())
	}
}

// buildValidCatalogImage lays out an HFS+ catalog whose index keys genuinely
// equal each child's first key and whose leaves are chained in key order —
// unlike the older fixtures, which are only walkable linearly.
//
// Layout follows a real volume: the root folder record is keyed under the
// parent-of-root CNID, and every node carries a thread record.
func buildValidCatalogImage(tb testing.TB) []byte {
	tb.Helper()

	nodes, nodeSize := buildValidCatalogNodes(tb)

	// One contiguous extent holding the whole tree.
	const (
		blockSize         = uint32(4096)
		catalogStartBlock = uint32(2)
	)
	treeBytes := len(nodes) * int(nodeSize)
	catalogBlocks := uint32((treeBytes + int(blockSize) - 1) / int(blockSize))

	img := make([]byte, int(catalogStartBlock+catalogBlocks)*int(blockSize))
	writeCatalogVolumeHeader(img, blockSize, uint64(catalogBlocks)*uint64(blockSize), catalogBlocks,
		[]ExtentDescriptor{{StartBlock: catalogStartBlock, BlockCount: catalogBlocks}})

	base := int(catalogStartBlock * blockSize)
	for i, node := range nodes {
		off := base + i*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}
	return img
}

// buildFragmentedCatalogImage holds the same tree as buildValidCatalogImage but
// splits the catalog file across two non-adjacent extents, with a gap of
// unrelated blocks between them.
//
// Addressing nodes from the first extent alone silently reads that gap once the
// tree crosses the boundary, so every lookup landing in the second half returns
// garbage. Nothing in a single-extent fixture can catch that, and real volumes
// fragment their catalog as a matter of course.
func buildFragmentedCatalogImage(tb testing.TB) []byte {
	tb.Helper()

	nodes, nodeSize := buildValidCatalogNodes(tb)

	const (
		blockSize   = uint32(4096)
		firstStart  = uint32(2)
		gapBlocks   = uint32(5) // unrelated blocks between the two extents
		poisonByte  = byte(0xDB)
		minPerParts = 2
	)

	treeBytes := len(nodes) * int(nodeSize)
	totalBlocks := uint32((treeBytes + int(blockSize) - 1) / int(blockSize))
	if totalBlocks < minPerParts*2 {
		tb.Fatalf("fixture tree of %d blocks is too small to split meaningfully", totalBlocks)
	}
	firstCount := totalBlocks / 2
	secondCount := totalBlocks - firstCount
	secondStart := firstStart + firstCount + gapBlocks

	exts := []ExtentDescriptor{
		{StartBlock: firstStart, BlockCount: firstCount},
		{StartBlock: secondStart, BlockCount: secondCount},
	}

	img := make([]byte, int(secondStart+secondCount)*int(blockSize))
	writeCatalogVolumeHeader(img, blockSize, uint64(totalBlocks)*uint64(blockSize), totalBlocks, exts)

	// Fill the gap with a recognisable pattern. A reader that walks off the end
	// of the first extent lands here and must not silently succeed.
	gapStart := int((firstStart + firstCount) * blockSize)
	gapEnd := int(secondStart * blockSize)
	for i := gapStart; i < gapEnd; i++ {
		img[i] = poisonByte
	}

	// Map each node's logical offset onto the extent list.
	for i, node := range nodes {
		logical := int64(i) * int64(nodeSize)
		if err := writeAtLogicalOffset(img, exts, blockSize, logical, node); err != nil {
			tb.Fatalf("writing node %d: %v", i, err)
		}
	}
	return img
}

// writeAtLogicalOffset writes buf at a logical offset within a fork, following
// the fork's extent list — the inverse of readFromExtents.
func writeAtLogicalOffset(img []byte, exts []ExtentDescriptor, blockSize uint32, off int64, buf []byte) error {
	remaining := buf
	logicalBase := int64(0)
	cur := off

	for _, e := range exts {
		extBytes := int64(e.BlockCount) * int64(blockSize)
		if cur >= logicalBase+extBytes {
			logicalBase += extBytes
			continue
		}
		inExt := cur - logicalBase
		can := extBytes - inExt
		if can > int64(len(remaining)) {
			can = int64(len(remaining))
		}
		phys := int64(e.StartBlock)*int64(blockSize) + inExt
		copy(img[phys:phys+can], remaining[:can])

		remaining = remaining[can:]
		cur += can
		logicalBase += extBytes
		if len(remaining) == 0 {
			return nil
		}
	}
	return errShortFixtureWrite
}

var errShortFixtureWrite = errors.New("fixture: extents too small for the tree")

// writeCatalogVolumeHeader stamps an HFS+ volume header describing a catalog
// fork with the given extents.
func writeCatalogVolumeHeader(img []byte, blockSize uint32, logicalSize uint64, totalBlocks uint32, exts []ExtentDescriptor) {
	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], blockSize)
	binary.BigEndian.PutUint32(vh[44:48], 100000)
	binary.BigEndian.PutUint32(vh[48:52], 50000)

	const catalogForkOff = 272
	binary.BigEndian.PutUint64(vh[catalogForkOff:catalogForkOff+8], logicalSize)
	binary.BigEndian.PutUint32(vh[catalogForkOff+12:catalogForkOff+16], totalBlocks)
	for i, e := range exts {
		if i >= 8 {
			break
		}
		base := catalogForkOff + 16 + i*8
		binary.BigEndian.PutUint32(vh[base:base+4], e.StartBlock)
		binary.BigEndian.PutUint32(vh[base+4:base+8], e.BlockCount)
	}
}

// buildValidCatalogNodes builds the catalog B-tree node images, indexed by node
// number. Physical placement is left to the caller so the same tree can be laid
// out contiguously or across several extents.
func buildValidCatalogNodes(tb testing.TB) ([][]byte, uint16) {
	tb.Helper()

	const (
		nodeSize       = uint16(2048)
		recordsPerLeaf = 4
	)

	var entries []catalogEntry

	// Records must be emitted in B-tree key order: parent CNID ascending, then
	// name. CNIDs are assigned so that ordering by parent puts the root's
	// parent first, then root, then the directories, then the file threads.
	//
	// (1, volname) -> the root folder record itself.
	entries = append(entries, catalogEntry{
		key: buildCatalogKey(1, validTreeVolName),
		rec: buildFolderRecord(rootFolderCNID, validTreeDirCount),
	})
	// (2, "") -> root's thread, then (2, dirNN) -> the directory records.
	entries = append(entries, catalogEntry{
		key: buildCatalogKey(rootFolderCNID, ""),
		rec: buildThreadRecord(catalogRecordFolderThread, 1, validTreeVolName),
	})
	for d := range validTreeDirCount {
		entries = append(entries, catalogEntry{
			key: buildCatalogKey(rootFolderCNID, validTreeDirName(d)),
			rec: buildFolderRecord(validTreeDirCNID(d), validTreeFilesPerDir),
		})
	}
	// Per directory: its own thread, then its file records.
	for d := range validTreeDirCount {
		entries = append(entries, catalogEntry{
			key: buildCatalogKey(validTreeDirCNID(d), ""),
			rec: buildThreadRecord(catalogRecordFolderThread, rootFolderCNID, validTreeDirName(d)),
		})
		for f := range validTreeFilesPerDir {
			entries = append(entries, catalogEntry{
				key: buildCatalogKey(validTreeDirCNID(d), validTreeFileName(f)),
				rec: buildFileRecord(validTreeFileCNID(d, f)),
			})
		}
	}
	// Finally every file's thread record, keyed on its own CNID.
	for d := range validTreeDirCount {
		for f := range validTreeFilesPerDir {
			entries = append(entries, catalogEntry{
				key: buildCatalogKey(validTreeFileCNID(d, f), ""),
				rec: buildThreadRecord(catalogRecordFileThread, validTreeDirCNID(d), validTreeFileName(f)),
			})
		}
	}

	return packCatalogTree(tb, entries, nodeSize, recordsPerLeaf)
}

// catalogEntry is one catalog record with the key it is filed under, in the
// form packCatalogTree expects.
type catalogEntry struct {
	key []byte
	rec []byte
}

// packCatalogTree lays a key-ordered run of catalog entries out as a header
// node, an index root and a chain of leaves, and returns the node images
// indexed by node number.
//
// The entries must already be in B-tree key order — parent CNID ascending, then
// name — because nothing here sorts them, and a fixture that is out of order
// tests the degraded full-scan path rather than the keyed descent it looks like
// it is testing. Physical placement is left to the caller so the same tree can
// be laid out contiguously or across several extents.
func packCatalogTree(tb testing.TB, entries []catalogEntry, nodeSize uint16, recordsPerLeaf int) ([][]byte, uint16) {
	tb.Helper()

	// Pack into leaves, remembering each leaf's first key for the index node.
	type leaf struct {
		records  [][]byte
		firstKey []byte
	}
	var leaves []leaf
	for i := 0; i < len(entries); i += recordsPerLeaf {
		end := min(i+recordsPerLeaf, len(entries))

		l := leaf{firstKey: entries[i].key}
		used := btreeNodeDescSize
		for _, e := range entries[i:end] {
			rec := append(append([]byte{}, e.key...), e.rec...)
			used += len(rec) + 2 // record plus its offset-array slot
			l.records = append(l.records, rec)
		}
		if used+2 > int(nodeSize) {
			tb.Fatalf("fixture leaf %d needs %d bytes, exceeds node size %d", len(leaves), used+2, nodeSize)
		}
		leaves = append(leaves, l)
	}

	const (
		headerNodeNum = uint32(0)
		indexNodeNum  = uint32(1)
		firstLeafNum  = uint32(2)
	)
	totalNodes := uint32(2 + len(leaves))
	lastLeafNum := firstLeafNum + uint32(len(leaves)) - 1

	nodes := make([][]byte, totalNodes)

	hdrRec := buildBTreeHeaderRecordBytesAt(nodeSize, totalNodes, indexNodeNum, firstLeafNum)
	binary.BigEndian.PutUint32(hdrRec[14:18], lastLeafNum)
	binary.BigEndian.PutUint16(hdrRec[0:2], 2) // depth: index root + leaves
	nodes[headerNodeNum] = makeNode(nodeSize, btreeNodeTypeHead, [][]byte{hdrRec})

	// Index root: one record per leaf, key = that leaf's true first key.
	idxRecords := make([][]byte, 0, len(leaves))
	idxUsed := btreeNodeDescSize + 2
	for i, l := range leaves {
		rec := append(append([]byte{}, l.firstKey...), u32be(firstLeafNum+uint32(i))...)
		idxUsed += len(rec) + 2
		idxRecords = append(idxRecords, rec)
	}
	if idxUsed > int(nodeSize) {
		tb.Fatalf("fixture index node needs %d bytes for %d leaves, exceeds node size %d",
			idxUsed, len(leaves), nodeSize)
	}
	nodes[indexNodeNum] = makeNode(nodeSize, btreeNodeTypeIdx, idxRecords)

	for i, l := range leaves {
		node := makeNode(nodeSize, btreeNodeTypeLeaf, l.records)
		if i+1 < len(leaves) {
			setForwardLink(node, firstLeafNum+uint32(i)+1)
		}
		if i > 0 {
			binary.BigEndian.PutUint32(node[4:8], firstLeafNum+uint32(i)-1) // BackwardLink
		}
		nodes[firstLeafNum+uint32(i)] = node
	}

	return nodes, nodeSize
}
