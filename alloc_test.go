package hfs

import (
	"bytes"
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

func TestWalkUnallocatedStopsOnCallbackError(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	sentinel := errStopWalk
	var seen int
	err := vol.WalkUnallocated(func(uint32, uint32) error {
		seen++
		return sentinel
	})
	if err != sentinel {
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
