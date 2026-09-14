package libhfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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

// buildOrphanedDirImage returns the valid-tree fixture with its first directory
// made unplaceable, and that directory's CNID.
//
// Destroying both of the things that could place the directory — its own folder
// record and its thread record — is what makes it an orphan. Removing only one
// is not enough: the walk recovers the path from whichever survives, which is
// the correct answer rather than an orphan.
//
// The directory's own children become orphans; every other directory and file
// on the volume stays fully resolvable, which is what lets a test tell a walk
// that reports the damage from one that has spread it.
func buildOrphanedDirImage(t *testing.T) ([]byte, uint32) {
	t.Helper()

	img := buildValidCatalogImage(t)
	orphanParent := validTreeDirCNID(0)

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

	return img, orphanParent
}

// TestWalkPathsEmitsOrphans covers the forensic case: a directory whose thread
// record is gone still has children on the volume, and hiding them would hide
// the damage.
func TestWalkPathsEmitsOrphans(t *testing.T) {
	img, orphanParent := buildOrphanedDirImage(t)

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

// TestWalkPathsOnDamagedVolumeMatchesPathForCNID is the anchor for the negative
// memo. When a climb fails, failPath records that failure against every node it
// touched on the way up, so that the siblings and descendants of a broken
// directory do not each repeat an exhaustive scan.
//
// Poisoning too little only costs time. Poisoning too much is a wrong answer: a
// record that the volume can place would be reported with an empty path, which
// this package's callers are told means the parent chain does not reach the
// root. TestWalkPathsEmitsOrphans cannot catch that, because it only inspects
// the records whose parent is the broken directory. This checks the other side —
// that every record outside the damage still agrees with the independent
// per-record resolver, which uses no memo and so cannot be poisoned at all.
func TestWalkPathsOnDamagedVolumeMatchesPathForCNID(t *testing.T) {
	img, orphanParent := buildOrphanedDirImage(t)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	placed, orphans := 0, 0
	err = vol.WalkPaths(func(path string, rec CatalogRecord) error {
		if rec.ParentCNID == orphanParent {
			orphans++
			return nil
		}
		placed++
		if path == "" {
			t.Fatalf("CNID %d under parent %d was reported unplaceable, but the damage is confined to %d",
				rec.CNID, rec.ParentCNID, orphanParent)
		}
		want, err := vol.PathForCNID(rec.CNID)
		if err != nil {
			t.Fatalf("PathForCNID(%d) failed on a record WalkPaths placed at %q: %v", rec.CNID, path, err)
		}
		if path != want {
			t.Fatalf("WalkPaths gave %q for CNID %d, PathForCNID gave %q", path, rec.CNID, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPaths failed: %v", err)
	}

	// The root, and every directory and file except the broken directory's own
	// record and the children it can no longer place.
	wantPlaced := 1 + (validTreeDirCount - 1) + (validTreeFileCount - validTreeFilesPerDir)
	if placed != wantPlaced {
		t.Fatalf("WalkPaths placed %d records, want %d", placed, wantPlaced)
	}
	if orphans != validTreeFilesPerDir {
		t.Fatalf("WalkPaths emitted %d orphans, want %d", orphans, validTreeFilesPerDir)
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

// The nested fixture: root -> deep01 -> deep02 -> ... -> deep08 -> leaf.txt.
//
// Every other catalog fixture in this package is flat — each folder's parent is
// the root — so a path is assembled from exactly one name and the climb in
// pathUp never iterates. That hides a whole class of defect: a reversed
// assembly loop, an off-by-one pairing names against the CNIDs they belong to,
// or a climb that stops one level early all produce the correct answer on a
// two-level tree.
const (
	deepChainLen = 8
	deepVolName  = "deepvol"
	deepFileName = "leaf.txt"
	deepFileCNID = uint32(20 + deepChainLen)
)

func deepDirName(d int) string { return fmt.Sprintf("deep%02d", d+1) }
func deepDirCNID(d int) uint32 { return uint32(20 + d) }

// deepDirPath returns the path of the directory at depth d, counting from zero.
func deepDirPath(d int) string {
	p := ""
	for i := 0; i <= d; i++ {
		p += "/" + deepDirName(i)
	}
	return p
}

func deepFilePath() string { return deepDirPath(deepChainLen-1) + "/" + deepFileName }

func buildDeepCatalogImage(tb testing.TB) []byte {
	tb.Helper()

	const (
		nodeSize       = uint16(2048)
		recordsPerLeaf = 4
	)

	// Records go out in B-tree key order: parent CNID ascending, then name.
	// CNIDs ascend with depth, so appending each directory's record (filed
	// under its parent) and then its own thread (filed under itself) produces
	// that order without a sort.
	var entries []catalogEntry
	entries = append(entries,
		catalogEntry{
			key: buildCatalogKey(1, deepVolName),
			rec: buildFolderRecord(rootFolderCNID, 1),
		},
		catalogEntry{
			key: buildCatalogKey(rootFolderCNID, ""),
			rec: buildThreadRecord(catalogRecordFolderThread, 1, deepVolName),
		},
	)

	parent := uint32(rootFolderCNID)
	for d := range deepChainLen {
		cnid, name := deepDirCNID(d), deepDirName(d)
		entries = append(entries,
			catalogEntry{
				key: buildCatalogKey(parent, name),
				rec: buildFolderRecord(cnid, 1),
			},
			catalogEntry{
				key: buildCatalogKey(cnid, ""),
				rec: buildThreadRecord(catalogRecordFolderThread, parent, name),
			},
		)
		parent = cnid
	}

	entries = append(entries,
		catalogEntry{
			key: buildCatalogKey(parent, deepFileName),
			rec: buildFileRecord(deepFileCNID),
		},
		catalogEntry{
			key: buildCatalogKey(deepFileCNID, ""),
			rec: buildThreadRecord(catalogRecordFileThread, parent, deepFileName),
		},
	)

	nodes, _ := packCatalogTree(tb, entries, nodeSize, recordsPerLeaf)

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

// TestWalkPathsResolvesNestedDirectories exercises multi-level path assembly,
// which no other fixture in this package reaches.
//
// Note what this does and does not reach. Records arrive in parent-CNID order
// and CNIDs ascend with depth here, so each directory is emitted and memoised
// before the run of its own children: WalkPaths resolves every path by one memo
// hit and pathUp's climb never runs. That is the common case and worth pinning,
// but the climb itself is covered by the moved-directory fixture below.
func TestWalkPathsResolvesNestedDirectories(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildDeepCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	got := map[uint32]string{}
	err = vol.WalkPaths(func(path string, rec CatalogRecord) error {
		got[rec.CNID] = path
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPaths failed: %v", err)
	}

	want := map[uint32]string{rootFolderCNID: "/", deepFileCNID: deepFilePath()}
	for d := range deepChainLen {
		want[deepDirCNID(d)] = deepDirPath(d)
	}

	for cnid, wantPath := range want {
		if got[cnid] != wantPath {
			t.Errorf("CNID %d: WalkPaths gave %q, want %q", cnid, got[cnid], wantPath)
		}
	}
	if len(got) != len(want) {
		t.Errorf("WalkPaths emitted %d records, want %d: %v", len(got), len(want), got)
	}
	if vol.AnomalyCount() != 0 {
		t.Errorf("an intact nested tree recorded anomalies: %#v", vol.Anomalies())
	}
}

// TestPathForCNIDResolvesNestedDirectories checks the per-record resolver on
// the same tree with no memo at all, so a defect in the memo cannot mask one in
// the climb or vice versa.
func TestPathForCNIDResolvesNestedDirectories(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildDeepCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	for d := range deepChainLen {
		got, err := vol.PathForCNID(deepDirCNID(d))
		if err != nil {
			t.Fatalf("PathForCNID(%d) failed: %v", deepDirCNID(d), err)
		}
		if want := deepDirPath(d); got != want {
			t.Errorf("PathForCNID(%d) = %q, want %q", deepDirCNID(d), got, want)
		}
	}

	got, err := vol.PathForCNID(deepFileCNID)
	if err != nil {
		t.Fatalf("PathForCNID(%d) failed: %v", deepFileCNID, err)
	}
	if want := deepFilePath(); got != want {
		t.Errorf("PathForCNID(%d) = %q, want %q", deepFileCNID, got, want)
	}
}

// TestOpenPathResolvesNestedDirectories closes the loop: a path WalkPaths
// reports must be one OpenPath accepts. Descent and reconstruction are separate
// code, and a fixture this deep is the first chance to disagree.
func TestOpenPathResolvesNestedDirectories(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildDeepCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	rec, err := vol.OpenPath(deepFilePath())
	if err != nil {
		t.Fatalf("OpenPath(%q) failed: %v", deepFilePath(), err)
	}
	if rec.CNID != deepFileCNID {
		t.Fatalf("OpenPath(%q) returned CNID %d, want %d", deepFilePath(), rec.CNID, deepFileCNID)
	}
}

// The moved-directory fixture: root -> outer -> middle -> inner -> leaf.txt,
// with the CNIDs running the other way (outer 50, middle 30, inner 20).
//
// This is what a volume looks like after a directory is moved into one created
// later: the child keeps its old, lower CNID. Because the catalog is keyed on
// (parent CNID, name), the run of inner's children is then reached before the
// record that places inner, so the memo cannot help and pathUp must climb the
// thread records to the root. Every other fixture in this package has CNIDs
// ascending with depth, so none of them reaches that code from WalkPaths.
const (
	movedVolName  = "movedvol"
	movedFileName = "leaf.txt"

	movedOuterCNID = uint32(50)
	movedMidCNID   = uint32(30)
	movedInnerCNID = uint32(20)
	movedFileCNID  = uint32(60)

	movedOuterPath = "/outer"
	movedMidPath   = movedOuterPath + "/middle"
	movedInnerPath = movedMidPath + "/inner"
	movedFilePath  = movedInnerPath + "/" + movedFileName
)

// buildMovedDirImage builds that tree. When breakOuter is set, the outermost
// directory is made unplaceable exactly as buildOrphanedDirImage does it — both
// its folder record and its thread record — which is the only way to force a
// climb that is several levels deep to fail part-way up.
func buildMovedDirImage(tb testing.TB, breakOuter bool) []byte {
	tb.Helper()

	const (
		nodeSize       = uint16(2048)
		recordsPerLeaf = 4
	)

	// In B-tree key order: parent 1, 2, 20, 30, 50, 60.
	entries := []catalogEntry{
		{buildCatalogKey(1, movedVolName), buildFolderRecord(rootFolderCNID, 1)},
		{buildCatalogKey(rootFolderCNID, ""), buildThreadRecord(catalogRecordFolderThread, 1, movedVolName)},
		{buildCatalogKey(rootFolderCNID, "outer"), buildFolderRecord(movedOuterCNID, 1)},
		{buildCatalogKey(movedInnerCNID, ""), buildThreadRecord(catalogRecordFolderThread, movedMidCNID, "inner")},
		{buildCatalogKey(movedInnerCNID, movedFileName), buildFileRecord(movedFileCNID)},
		{buildCatalogKey(movedMidCNID, ""), buildThreadRecord(catalogRecordFolderThread, movedOuterCNID, "middle")},
		{buildCatalogKey(movedMidCNID, "inner"), buildFolderRecord(movedInnerCNID, 1)},
		{buildCatalogKey(movedOuterCNID, ""), buildThreadRecord(catalogRecordFolderThread, rootFolderCNID, "outer")},
		{buildCatalogKey(movedOuterCNID, "middle"), buildFolderRecord(movedMidCNID, 1)},
		{buildCatalogKey(movedFileCNID, ""), buildThreadRecord(catalogRecordFileThread, movedInnerCNID, movedFileName)},
	}

	nodes, _ := packCatalogTree(tb, entries, nodeSize, recordsPerLeaf)

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

	if breakOuter {
		corrupt := func(key []byte, what string) {
			tb.Helper()
			idx := bytes.Index(img, key)
			if idx < 0 {
				tb.Fatalf("could not locate the %s in the fixture", what)
			}
			binary.BigEndian.PutUint16(img[idx+len(key):idx+len(key)+2], 0xFFFF)
		}
		corrupt(buildCatalogKey(rootFolderCNID, "outer"), "outer directory record")
		corrupt(buildCatalogKey(movedOuterCNID, ""), "outer directory thread record")
	}
	return img
}

// TestWalkPathsClimbsForMovedDirectory is the only test in this package that
// makes WalkPaths assemble a path from more than one name.
//
// A directory whose CNID is lower than its parent's has its children reached
// before the record that places it, so the memo misses and pathUp climbs. The
// assertion is the exact path, not merely a non-empty one: assembling the names
// in the wrong order is the defect this arrangement exists to expose.
func TestWalkPathsClimbsForMovedDirectory(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildMovedDirImage(t, false)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	got := map[uint32]string{}
	err = vol.WalkPaths(func(path string, rec CatalogRecord) error {
		got[rec.CNID] = path
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPaths failed: %v", err)
	}

	want := map[uint32]string{
		rootFolderCNID: "/",
		movedOuterCNID: movedOuterPath,
		movedMidCNID:   movedMidPath,
		movedInnerCNID: movedInnerPath,
		movedFileCNID:  movedFilePath,
	}
	for cnid, wantPath := range want {
		if got[cnid] != wantPath {
			t.Errorf("CNID %d: WalkPaths gave %q, want %q", cnid, got[cnid], wantPath)
		}
	}
	if len(got) != len(want) {
		t.Errorf("WalkPaths emitted %d records, want %d: %v", len(got), len(want), got)
	}
	if vol.AnomalyCount() != 0 {
		t.Errorf("an intact tree recorded anomalies: %#v", vol.Anomalies())
	}
}

// TestWalkPathsFailedClimbReportsEveryDescendant covers a climb that fails
// several levels up, which is the only way failPath's chain loop is reached.
//
// The loop is an optimisation — it stops each descendant of the break from
// repeating a scan that has already failed — so no assertion here can observe
// it directly. What must hold is that it costs nothing in correctness: every
// record under the break is reported unplaceable rather than given a path
// invented from the part of the chain that did resolve.
func TestWalkPathsFailedClimbReportsEveryDescendant(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildMovedDirImage(t, true)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	got := map[uint32]string{}
	err = vol.WalkPaths(func(path string, rec CatalogRecord) error {
		got[rec.CNID] = path
		return nil
	})
	if err != nil {
		t.Fatalf("WalkPaths failed: %v", err)
	}

	// The outer directory's own record is destroyed, so it is never emitted.
	// Everything beneath it is emitted with no path.
	for _, cnid := range []uint32{movedMidCNID, movedInnerCNID, movedFileCNID} {
		path, ok := got[cnid]
		if !ok {
			t.Errorf("CNID %d was not emitted at all; damage must not hide records", cnid)
			continue
		}
		if path != "" {
			t.Errorf("CNID %d got path %q, want the empty string: its chain does not reach the root",
				cnid, path)
		}
	}
	if got[rootFolderCNID] != "/" {
		t.Errorf("the root got %q, want %q", got[rootFolderCNID], "/")
	}
	if vol.AnomalyCount() == 0 {
		t.Error("an unreachable parent chain was not recorded as an anomaly")
	}
}
