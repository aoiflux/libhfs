package libhfs

import (
	"bytes"
	"testing"
	"time"
)

func timeoutAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}

// A catalog B-tree split across two non-adjacent extents must read identically
// to the same tree laid out contiguously.
//
// Node offsets are logical positions within the fork, so they only map to the
// right blocks when resolved through the fork's extent list. Computing them
// from the first extent alone works until the tree crosses the extent boundary,
// then reads unrelated blocks — which on a real volume means wrong records
// rather than an error. The fixture puts a poison-filled gap between the two
// extents so that failure is unmistakable.
func TestFragmentedCatalogMatchesContiguous(t *testing.T) {
	contig, err := Open(bytes.NewReader(buildValidCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open contiguous: %v", err)
	}
	frag, err := Open(bytes.NewReader(buildFragmentedCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open fragmented: %v", err)
	}

	// The fixture must actually be fragmented, or this test proves nothing.
	exts, err := frag.forkExtents(catalogFileCNID, frag.Header().CatalogFile)
	if err != nil {
		t.Fatalf("forkExtents: %v", err)
	}
	if len(exts) < 2 {
		t.Fatalf("fragmented fixture has %d extent(s), want >= 2: %+v", len(exts), exts)
	}
	t.Logf("catalog spans %d extents: %+v", len(exts), exts)

	// Every record must decode the same from both layouts.
	var want []CatalogRecord
	if err := contig.WalkCatalog(func(r CatalogRecord) error {
		want = append(want, r)
		return nil
	}); err != nil {
		t.Fatalf("WalkCatalog contiguous: %v", err)
	}

	var got []CatalogRecord
	if err := frag.WalkCatalog(func(r CatalogRecord) error {
		got = append(got, r)
		return nil
	}); err != nil {
		t.Fatalf("WalkCatalog fragmented: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("fragmented walk yielded %d records, contiguous yielded %d", len(got), len(want))
	}
	for i := range want {
		if got[i].CNID != want[i].CNID || got[i].Name != want[i].Name ||
			got[i].ParentCNID != want[i].ParentCNID || got[i].Type != want[i].Type {
			t.Fatalf("record %d differs:\n contiguous=%+v\n fragmented=%+v", i, want[i], got[i])
		}
	}
	t.Logf("%d records identical across both layouts", len(got))

	// Keyed lookups must also cross the boundary correctly. Records late in key
	// order live in the second extent, so these are the ones that fail when node
	// addressing ignores fragmentation.
	for d := range validTreeDirCount {
		for f := range validTreeFilesPerDir {
			cnid := validTreeFileCNID(d, f)
			path := validTreeFilePath(d, f)

			rec, err := frag.OpenPath(path)
			if err != nil {
				t.Fatalf("fragmented OpenPath(%q): %v", path, err)
			}
			if rec.CNID != cnid {
				t.Errorf("fragmented OpenPath(%q).CNID = %d, want %d", path, rec.CNID, cnid)
			}
			if got, err := frag.PathForCNID(cnid); err != nil || got != path {
				t.Errorf("fragmented PathForCNID(%d) = %q, %v; want %q", cnid, got, err, path)
			}
		}
	}

	assertNoFallback(t, frag)
}

// The extents overflow file cannot record its own overflow, so its extent list
// is whatever the volume header holds. Resolving it through the overflow tree
// would recurse.
func TestExtentsFileResolvesWithoutRecursion(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildValidCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := vol.forkExtents(extentsFileCNID, vol.Header().ExtentsFile); err != nil {
			t.Errorf("forkExtents(extents file): %v", err)
		}
	}()

	select {
	case <-done:
	case <-timeoutAfterSeconds(10):
		t.Fatal("resolving the extents file's own extents did not terminate")
	}
}

func TestForkExtentsRejectsEmptyFork(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildValidCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// A fork with no extents cannot yield a node reader.
	if _, err := vol.nodeReaderFor(catalogFileCNID, ForkData{}, 4096); err == nil {
		t.Error("nodeReaderFor on an empty fork returned nil error")
	}
	// Nor can a zero node size.
	if _, err := vol.nodeReaderFor(catalogFileCNID, vol.Header().CatalogFile, 0); err == nil {
		t.Error("nodeReaderFor with nodeSize 0 returned nil error")
	}
}
