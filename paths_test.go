package hfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

// TestWalkPathsMatchesPathForCNID is the correctness anchor for the memo: every
// path the shared traversal produces must equal the one the per-record resolver
// produces independently.
func TestWalkPathsMatchesPathForCNID(t *testing.T) {
	vol, _ := openValidTree(t)

	seen := 0
	threads := 0
	err := vol.WalkPaths(func(path string, rec CatalogRecord) error {
		seen++
		if rec.Type == CatalogRecordFolderThread || rec.Type == CatalogRecordFileThread {
			threads++
		}
		want, err := vol.PathForCNID(rec.CNID)
		if err != nil {
			t.Fatalf("PathForCNID(%d) failed: %v", rec.CNID, err)
		}
		if path != want {
			t.Fatalf("WalkPaths gave %q for CNID %d, PathForCNID gave %q", path, rec.CNID, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPaths failed: %v", err)
	}

	if threads != 0 {
		t.Fatalf("WalkPaths emitted %d thread records, want none", threads)
	}
	// The fixture's directories and files, plus the root folder itself.
	want := validTreeDirCount + validTreeFileCount + 1
	if seen != want {
		t.Fatalf("WalkPaths emitted %d records, want %d", seen, want)
	}
}

func TestWalkPathsEmitsRootOnce(t *testing.T) {
	vol, _ := openValidTree(t)

	roots := 0
	err := vol.WalkPaths(func(path string, rec CatalogRecord) error {
		if path == "/" {
			roots++
			if rec.CNID != rootFolderCNID {
				t.Fatalf(`path "/" carried CNID %d, want %d`, rec.CNID, rootFolderCNID)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPaths failed: %v", err)
	}
	if roots != 1 {
		t.Fatalf(`emitted "/" %d times, want once`, roots)
	}
}

// TestWalkPathsMemoEviction proves the memo is an optimisation and nothing
// else: a memo too small to hold the tree must produce identical output.
func TestWalkPathsMemoEviction(t *testing.T) {
	vol, _ := openValidTree(t)

	collect := func() []PathRecord {
		t.Helper()
		recs, err := vol.PathRecords()
		if err != nil {
			t.Fatalf("PathRecords failed: %v", err)
		}
		return recs
	}

	full := collect()

	original := walkPathMemoMax
	walkPathMemoMax = 1
	defer func() { walkPathMemoMax = original }()

	starved := collect()

	if len(full) != len(starved) {
		t.Fatalf("memo size changed the record count: %d vs %d", len(full), len(starved))
	}
	for i := range full {
		if full[i].Path != starved[i].Path || full[i].Record.CNID != starved[i].Record.CNID {
			t.Fatalf("record %d differs: %q/%d vs %q/%d", i,
				full[i].Path, full[i].Record.CNID, starved[i].Path, starved[i].Record.CNID)
		}
	}
}

// TestWalkPathsEmitsOrphans covers the forensic case: a directory whose thread
// record is gone still has children on the volume, and hiding them would hide
// the damage.
func TestWalkPathsEmitsOrphans(t *testing.T) {
	img := buildValidCatalogImage(t)
	orphanParent := validTreeDirCNID(0)

	// Destroy both of the things that could place the first directory: its own
	// folder record, and its thread record. Removing only one of them is not
	// enough — the walk recovers the path from whichever survives, which is the
	// correct answer rather than an orphan.
	corruptRecord := func(key []byte, what string) {
		t.Helper()
		idx := bytes.Index(img, key)
		if idx < 0 {
			t.Fatalf("could not locate the %s in the fixture", what)
		}
		binary.BigEndian.PutUint16(img[idx+len(key):idx+len(key)+2], 0xFFFF)
	}
	corruptRecord(buildCatalogKey(rootFolderCNID, validTreeDirName(0)), "directory record")
	corruptRecord(buildCatalogKey(orphanParent, ""), "directory thread record")

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	orphans := 0
	err = vol.WalkPaths(func(path string, rec CatalogRecord) error {
		if rec.ParentCNID == orphanParent {
			if path != "" {
				t.Fatalf("orphan CNID %d got path %q, want the empty string", rec.CNID, path)
			}
			orphans++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPaths failed: %v", err)
	}

	if orphans != validTreeFilesPerDir {
		t.Fatalf("emitted %d orphaned children, want %d", orphans, validTreeFilesPerDir)
	}
	if vol.AnomalyCount() == 0 {
		t.Fatal("an unreachable parent chain was not recorded as an anomaly")
	}

	var found bool
	for _, a := range vol.Anomalies() {
		if a.Op == "walk_path" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no walk_path anomaly recorded: %#v", vol.Anomalies())
	}
}

func TestWalkPathsContextCancel(t *testing.T) {
	vol, _ := openValidTree(t)

	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	err := vol.WalkPathsContext(ctx, func(string, CatalogRecord) error {
		seen++
		if seen == 3 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if seen > 4 {
		t.Fatalf("walk continued past cancellation, saw %d records", seen)
	}
}

func TestWalkPathsNilCallback(t *testing.T) {
	vol, _ := openValidTree(t)
	if err := vol.WalkPaths(nil); err != nil {
		t.Fatalf("WalkPaths(nil) = %v, want nil", err)
	}
	if err := vol.WalkPathsContext(context.Background(), nil); err != nil {
		t.Fatalf("WalkPathsContext(nil) = %v, want nil", err)
	}
}

// TestWalkPathsStopsOnCallbackError checks that a caller's own error ends the
// walk and reaches them unchanged.
func TestWalkPathsStopsOnCallbackError(t *testing.T) {
	vol, _ := openValidTree(t)

	sentinel := errors.New("stop here")
	seen := 0
	err := vol.WalkPaths(func(string, CatalogRecord) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the callback's error, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("walk continued after the callback failed, saw %d records", seen)
	}
}

func TestPathForCNIDStillReportsCycles(t *testing.T) {
	img := buildCatalogThreadedTestImage(t)

	// Point /etc's thread record at its own child, making the chain loop.
	thread := buildCatalogKey(100, "")
	idx := bytes.Index(img, thread)
	if idx < 0 {
		t.Fatal("could not locate the /etc thread record in the fixture")
	}
	binary.BigEndian.PutUint32(img[idx+len(thread)+4:idx+len(thread)+8], 100)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if _, err := vol.PathForCNID(100); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt for a looping parent chain, got %v", err)
	}
}

// TestWalkPathsClassicHFS covers the classic format end to end: MacRoman name
// decoding, and a root folder record that stores no name at all — the case
// that must still be emitted as "/".
func TestWalkPathsClassicHFS(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	got := map[string]uint32{}
	err = vol.WalkPaths(func(path string, rec CatalogRecord) error {
		got[path] = rec.CNID
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPaths failed: %v", err)
	}

	want := map[string]uint32{
		"/":     rootFolderCNID,
		"/DATA": 100,
		"/CAFé": 101,
	}
	if len(got) != len(want) {
		t.Fatalf("walked %d paths, want %d: %v", len(got), len(want), got)
	}
	for path, cnid := range want {
		if got[path] != cnid {
			t.Fatalf("path %q gave CNID %d, want %d (all: %v)", path, got[path], cnid, got)
		}
	}
	if vol.AnomalyCount() != 0 {
		t.Fatalf("classic walk recorded anomalies: %v", vol.Anomalies())
	}
}
