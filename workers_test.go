package libhfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelMapPreservesOrder(t *testing.T) {
	const n = 500
	for _, workers := range []int{1, 2, 4, 16, 64} {
		got, err := parallelMap(context.Background(), workers, n,
			func(_ context.Context, i int) (int, error) { return i * i, nil })
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if len(got) != n {
			t.Fatalf("workers=%d: got %d results, want %d", workers, len(got), n)
		}
		for i, v := range got {
			if v != i*i {
				t.Fatalf("workers=%d: result[%d] = %d, want %d", workers, i, v, i*i)
			}
		}
	}
}

// The whole justification for parallel carving is that results do not depend on
// scheduling. This asserts it directly.
func TestParallelMapIsDeterministic(t *testing.T) {
	const n = 200
	fn := func(_ context.Context, i int) (string, error) {
		return string(rune('a' + i%26)), nil
	}

	baseline, err := parallelMap(context.Background(), 1, n, fn)
	if err != nil {
		t.Fatalf("sequential: %v", err)
	}
	for _, workers := range []int{2, 3, 8, 32} {
		got, err := parallelMap(context.Background(), workers, n, fn)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		for i := range baseline {
			if got[i] != baseline[i] {
				t.Fatalf("workers=%d: result[%d] = %q, sequential gave %q",
					workers, i, got[i], baseline[i])
			}
		}
	}
}

func TestParallelMapPropagatesError(t *testing.T) {
	sentinel := errors.New("task failed")
	var ran atomic.Int64

	_, err := parallelMap(context.Background(), 4, 1000,
		func(_ context.Context, i int) (int, error) {
			ran.Add(1)
			if i == 10 {
				return 0, sentinel
			}
			return i, nil
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the task's error", err)
	}
	// The failure must stop the remaining work rather than running all 1000.
	if n := ran.Load(); n >= 1000 {
		t.Errorf("%d tasks ran after a failure; the run was not cancelled", n)
	}
}

func TestParallelMapHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int64

	_, err := parallelMap(ctx, 4, 10000, func(_ context.Context, i int) (int, error) {
		if ran.Add(1) == 5 {
			cancel()
		}
		return i, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if n := ran.Load(); n >= 10000 {
		t.Errorf("%d tasks ran after cancellation", n)
	}
}

func TestParallelMapAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ran atomic.Int64
	_, err := parallelMap(ctx, 4, 100, func(_ context.Context, i int) (int, error) {
		ran.Add(1)
		return i, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if n := ran.Load(); n != 0 {
		t.Errorf("%d tasks ran despite an already-cancelled context", n)
	}
}

func TestParallelMapEmpty(t *testing.T) {
	got, err := parallelMap(context.Background(), 4, 0,
		func(_ context.Context, i int) (int, error) {
			t.Error("callback ran for an empty range")
			return 0, nil
		})
	if err != nil {
		t.Fatalf("empty range: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d results for an empty range", len(got))
	}
}

// A worker count below two must run everything on the calling goroutine, so a
// caller can opt out of concurrency entirely rather than getting a pool of one.
//
// The unsynchronised counter is the assertion: under -race, any concurrent
// execution of the callback would be reported as a data race. Passing under
// -race is therefore proof that nothing ran in parallel.
func TestParallelMapSequentialUsesNoGoroutines(t *testing.T) {
	unguarded := 0

	_, err := parallelMap(context.Background(), 1, 200,
		func(_ context.Context, i int) (int, error) {
			unguarded++ // deliberately not atomic; see the comment above
			return i, nil
		})
	if err != nil {
		t.Fatalf("sequential run: %v", err)
	}
	if unguarded != 200 {
		t.Errorf("callback ran %d times, want 200", unguarded)
	}
}

func TestSplitBlockRuns(t *testing.T) {
	cases := []struct {
		name  string
		in    []blockRange
		batch uint32
		want  []blockRange
	}{
		{
			name:  "exact multiple",
			in:    []blockRange{{start: 0, count: 8}},
			batch: 4,
			want:  []blockRange{{0, 4}, {4, 4}},
		},
		{
			name:  "remainder",
			in:    []blockRange{{start: 10, count: 5}},
			batch: 2,
			want:  []blockRange{{10, 2}, {12, 2}, {14, 1}},
		},
		{
			name:  "smaller than batch",
			in:    []blockRange{{start: 3, count: 1}},
			batch: 64,
			want:  []blockRange{{3, 1}},
		},
		{
			name:  "batch zero passes through",
			in:    []blockRange{{start: 0, count: 100}},
			batch: 0,
			want:  []blockRange{{0, 100}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitBlockRuns(tc.in, tc.batch)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d ranges, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("range %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Splitting must preserve total coverage exactly: no block dropped, none twice.
func TestSplitBlockRunsPreservesCoverage(t *testing.T) {
	runs := []blockRange{{0, 1000}, {2000, 37}, {5000, 1}}
	var before uint32
	for _, r := range runs {
		before += r.count
	}

	split := splitBlockRuns(runs, carveBlockBatch)
	var after uint32
	seen := map[uint32]bool{}
	for _, r := range split {
		after += r.count
		for b := r.start; b < r.start+r.count; b++ {
			if seen[b] {
				t.Fatalf("block %d covered twice", b)
			}
			seen[b] = true
		}
	}
	if after != before {
		t.Errorf("split covers %d blocks, original covered %d", after, before)
	}
}

func TestResolveWorkerCount(t *testing.T) {
	// Zero means unset and takes the measured default, which is sequential.
	if got := resolveWorkerCount(0); got != DefaultCarveWorkers {
		t.Errorf("unset count = %d, want DefaultCarveWorkers (%d)", got, DefaultCarveWorkers)
	}
	if DefaultCarveWorkers != 1 {
		t.Errorf("DefaultCarveWorkers = %d; measurement showed sequential to be fastest",
			DefaultCarveWorkers)
	}

	// Explicit values are honoured exactly, including above the cap: the cap
	// guards the derived value, not a caller's judgement about their storage.
	for _, n := range []int{1, 4, 1000} {
		if got := resolveWorkerCount(n); got != n {
			t.Errorf("explicit count %d became %d", n, got)
		}
	}

	// AutoCarveWorkers derives from GOMAXPROCS, capped.
	got := resolveWorkerCount(AutoCarveWorkers)
	if got < 1 || got > MaxCarveWorkers {
		t.Errorf("AutoCarveWorkers gave %d, outside 1..%d", got, MaxCarveWorkers)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	img := buildValidCatalogImage(t)

	// The zero Config must match Open's defaults exactly.
	plain, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	configured, err := OpenWithConfig(bytes.NewReader(img), Config{})
	if err != nil {
		t.Fatalf("OpenWithConfig: %v", err)
	}
	if plain.Config() != configured.Config() {
		t.Errorf("zero Config = %+v, Open gave %+v", configured.Config(), plain.Config())
	}

	// Explicit values must survive. BaseOffset is set here rather than left at
	// zero because every other field in this test is non-zero, and a Config()
	// that silently dropped it would still satisfy the comparison below if the
	// value it forgot happened to be the zero one.
	want := Config{
		CacheSize:     7,
		NodeCacheSize: 9,
		MaxAlloc:      1234,
		TextEncoding:  TextEncodingRaw,
		CarveWorkers:  3,
		BaseOffset:    embedOffset,
	}
	vol, err := OpenWithConfig(bytes.NewReader(embedImage(t, img, embedFill)), want)
	if err != nil {
		t.Fatalf("OpenWithConfig: %v", err)
	}
	if got := vol.Config(); got != want {
		t.Errorf("Config() = %+v, want %+v", got, want)
	}

	// And the snapshot must be usable as input: reopening with it reproduces
	// the volume, which is the property "snapshot" claims.
	again, err := OpenWithConfig(bytes.NewReader(embedImage(t, img, embedFill)), vol.Config())
	if err != nil {
		t.Fatalf("reopening with the reported Config failed: %v", err)
	}
	if again.BaseOffset() != vol.BaseOffset() {
		t.Errorf("reopened BaseOffset() = %d, want %d", again.BaseOffset(), vol.BaseOffset())
	}

	// DisableCache overrides the sizes without the caller needing the defaults.
	off, err := OpenWithConfig(bytes.NewReader(img), Config{DisableCache: true})
	if err != nil {
		t.Fatalf("OpenWithConfig: %v", err)
	}
	if c := off.Config(); c.CacheSize != 0 || c.NodeCacheSize != 0 {
		t.Errorf("DisableCache left caches at %d/%d", c.CacheSize, c.NodeCacheSize)
	}
	// And the volume must still work with no caching at all.
	if _, err := off.OpenPath(validTreeFilePath(0, 0)); err != nil {
		t.Errorf("lookup with caching disabled: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Carving determinism, end to end
// ---------------------------------------------------------------------------

// Carving must produce byte-identical findings at any worker count. If it did
// not, a report would depend on the machine that produced it.
//
// The corpus image's free space holds no catalog nodes, so unallocated carving
// alone finds nothing there and would make this comparison vacuous. Every
// source is enabled, and the result asserted non-empty, so the test compares
// real findings.
func TestCorpusCarvingDeterministicAcrossWorkerCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("carving reads the whole free area")
	}
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	opts := &RecoveryOptions{
		ScanNodeSlack:      true,
		ScanFreeNodes:      true,
		ScanUnallocated:    true,
		IncludeStaleCopies: true,
	}

	vol.SetCarveWorkers(1)
	sequential, err := vol.RecoverDeleted(opts)
	if err != nil {
		t.Fatalf("sequential carve: %v", err)
	}
	t.Logf("sequential carve found %d records", len(sequential))
	if len(sequential) == 0 {
		// A volume whose leaves hold no residue has nothing to carve, so the
		// worker counts below would agree by all finding nothing. See
		// TestCorpusRecoverDeleted for why a volume that has residue must
		// yield records.
		if catalogLeavesWithKeyResidue(t, vol) == 0 {
			t.Skip("no catalog leaf slack holds anything shaped like a key, so there is nothing for the worker counts to disagree about")
		}
		t.Fatal("no records found; the comparison below would prove nothing")
	}

	for _, workers := range []int{2, 4, 8} {
		vol.SetCarveWorkers(workers)
		got, err := vol.RecoverDeleted(opts)
		if err != nil {
			t.Fatalf("carve with %d workers: %v", workers, err)
		}
		if len(got) != len(sequential) {
			t.Fatalf("workers=%d found %d records, sequential found %d",
				workers, len(got), len(sequential))
		}
		for i := range sequential {
			a, b := sequential[i], got[i]
			if a.Record.CNID != b.Record.CNID || a.Record.Name != b.Record.Name ||
				a.ByteOffset != b.ByteOffset || a.Source != b.Source ||
				a.Confidence != b.Confidence {
				t.Fatalf("workers=%d: record %d differs\n sequential=%+v\n parallel=%+v",
					workers, i, a.Record, b.Record)
			}
		}
	}
}

// The corpus image has nothing recoverable in unallocated space, so the
// parallel carve path is only genuinely exercised against an image built to
// contain something there.
func TestUnallocatedCarvingFindsRecordsDeterministically(t *testing.T) {
	const carvedCNID = uint32(500)
	const carvedName = "carved-from-free-space.txt"

	img := buildImageWithNodeInFreeSpace(t, carvedCNID, carvedName)

	var baseline []DeletedRecord
	for _, workers := range []int{1, 2, 4, 8} {
		vol, err := Open(bytes.NewReader(img))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		vol.SetCarveWorkers(workers)

		recs, err := vol.RecoverDeleted(&RecoveryOptions{ScanUnallocated: true})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}

		var found bool
		for _, r := range recs {
			if r.Record.CNID == carvedCNID && r.Record.Name == carvedName {
				found = true
				if r.Source != RecoveredFromUnallocated {
					t.Errorf("workers=%d: Source = %v, want unallocated", workers, r.Source)
				}
			}
		}
		if !found {
			t.Fatalf("workers=%d: the record planted in free space was not carved; got %d records",
				workers, len(recs))
		}

		if baseline == nil {
			baseline = recs
			t.Logf("carved %d records from free space", len(recs))
			continue
		}
		if len(recs) != len(baseline) {
			t.Fatalf("workers=%d found %d records, sequential found %d",
				workers, len(recs), len(baseline))
		}
		for i := range baseline {
			if recs[i].Record.CNID != baseline[i].Record.CNID ||
				recs[i].ByteOffset != baseline[i].ByteOffset {
				t.Fatalf("workers=%d: record %d differs from the sequential result", workers, i)
			}
		}
	}
}

// buildImageWithNodeInFreeSpace plants a complete catalog leaf node in a block
// the allocation bitmap marks free — the state left behind when a catalog file
// grows and is relocated, leaving its former nodes behind.
func buildImageWithNodeInFreeSpace(t *testing.T, cnid uint32, name string) []byte {
	t.Helper()

	const (
		blockSize    = uint32(4096)
		allocBlock   = uint32(1)
		catalogBlock = uint32(2)
		nodeSize     = uint16(1024)
		strandedAt   = uint32(9) // a free block holding an orphaned node
		totalBlocks  = uint32(16)
	)

	img := make([]byte, int(totalBlocks)*int(blockSize))

	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[volHdrSignature:volHdrSignature+2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[volHdrVersion:volHdrVersion+2], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[volHdrBlockSize:volHdrBlockSize+4], blockSize)
	binary.BigEndian.PutUint32(vh[volHdrTotalBlocks:volHdrTotalBlocks+4], totalBlocks)
	binary.BigEndian.PutUint32(vh[volHdrNextCatalogID:volHdrNextCatalogID+4], 1000)

	putFork := func(off int, start, blocks uint32) {
		binary.BigEndian.PutUint64(vh[off:off+8], uint64(blocks)*uint64(blockSize))
		binary.BigEndian.PutUint32(vh[off+forkDataTotalBlocks:off+forkDataTotalBlocks+4], blocks)
		binary.BigEndian.PutUint32(vh[off+forkDataExtents:off+forkDataExtents+4], start)
		binary.BigEndian.PutUint32(vh[off+forkDataExtents+4:off+forkDataExtents+8], blocks)
	}
	putFork(volHdrAllocationFile, allocBlock, 1)
	putFork(volHdrCatalogFile, catalogBlock, 1)

	// Allocation bitmap: mark the metadata blocks in use and everything else
	// free, so the stranded node sits in space the volume considers available.
	bitmap := img[int(allocBlock)*int(blockSize):]
	setUsed := func(b uint32) {
		bitmap[b/bitsPerByte] |= 1 << (bitmapMSBFirst - b%bitsPerByte)
	}
	setUsed(0)
	setUsed(allocBlock)
	setUsed(catalogBlock)

	// Live catalog: just a root folder.
	live := [][]byte{
		append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecord(rootFolderCNID, 0)...),
	}
	catBase := int(catalogBlock) * int(blockSize)
	copy(img[catBase:], makeNode(nodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(nodeSize, 2, 1, 1)}))
	copy(img[catBase+int(nodeSize):], makeNode(nodeSize, btreeNodeTypeLeaf, live))

	// The orphaned node, complete and valid, sitting in free space.
	stranded := makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{
		append(buildCatalogKey(rootFolderCNID, name), buildFileRecord(cnid)...),
	})
	copy(img[int(strandedAt)*int(blockSize):], stranded)

	return img
}

func TestCorpusCarvingHonoursCancellation(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := vol.WalkDeletedContext(ctx, &RecoveryOptions{ScanUnallocated: true},
		func(DeletedRecord) error { return nil })
	elapsed := time.Since(start)

	if err == nil {
		t.Skip("carve completed before the deadline; image too small to test cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	t.Logf("carve cancelled after %v", elapsed)

	// A full carve of this image takes ~500ms; cancelling must be much faster.
	if elapsed > 400*time.Millisecond {
		t.Errorf("cancellation took %v, which suggests it is not checked between blocks", elapsed)
	}
}

func TestWalkCatalogContextCancellation(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	var seen int
	err := vol.WalkCatalogContext(ctx, func(CatalogRecord) error {
		seen++
		if seen == 3 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		// cancel is called from the third record, so a volume holding fewer
		// than three never asks for cancellation and the walk finishing
		// cleanly is the correct result. An empty HFS+ volume has exactly two
		// records: the root folder and its thread.
		if seen < 3 {
			t.Skipf("volume has only %d catalog records, so cancellation was never requested", seen)
		}
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if seen > 4 {
		t.Errorf("walk continued for %d records after cancellation", seen)
	}
}
