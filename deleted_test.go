package hfs

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The corpus image carries plenty of catalog records in node slack — the B7
// investigation found 36 decoding cleanly — but every one is a stale copy of a
// file that is still live. The image was built by copying files in; nothing was
// ever deleted from it.
//
// So the correct result here is *zero* deletions, and that is the assertion
// worth making: the carving machinery finds the bytes, and the live cross-check
// correctly declines to call any of them a deletion. A library that reported
// 179 deleted files for this image would be confidently wrong.
func TestCorpusRecoverDeleted(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	// With stale copies included, carving must find records — otherwise the
	// scan itself is broken and the zero below would be meaningless.
	withStale, err := vol.RecoverDeleted(&RecoveryOptions{
		ScanNodeSlack:      true,
		ScanFreeNodes:      true,
		IncludeStaleCopies: true,
	})
	if err != nil {
		t.Fatalf("RecoverDeleted(with stale): %v", err)
	}
	if len(withStale) == 0 {
		t.Fatal("carving found nothing at all; the scan is not working")
	}

	var stale int
	for _, r := range withStale {
		if r.StaleCopy {
			stale++
		}
	}
	t.Logf("carved %d records, %d of them stale copies of live files", len(withStale), stale)

	// The default view must exclude every record that is still live.
	recs, err := vol.RecoverDeleted(nil)
	if err != nil {
		t.Fatalf("RecoverDeleted: %v", err)
	}

	bySource := map[RecoverySource]int{}
	byConfidence := map[Confidence]int{}
	var overwritten int
	for _, r := range recs {
		bySource[r.Source]++
		byConfidence[r.Confidence]++
		if r.Overwritten {
			overwritten++
		}
	}
	t.Logf("reported as deleted: %d (slack=%d freeNode=%d; low=%d medium=%d high=%d; overwritten=%d)",
		len(recs),
		bySource[RecoveredFromNodeSlack], bySource[RecoveredFromFreeNode],
		byConfidence[ConfidenceLow], byConfidence[ConfidenceMedium], byConfidence[ConfidenceHigh],
		overwritten)

	for i, r := range recs {
		if i >= 12 {
			t.Logf("   ... and %d more", len(recs)-12)
			break
		}
		t.Logf("   [%s/%s] cnid=%-7d parent=%-7d overwritten=%-5v %q",
			r.Source, r.Confidence, r.Record.CNID, r.Record.ParentCNID, r.Overwritten, r.Record.Name)
	}

	// No record reported as deleted may still be live.
	live := map[uint32]string{}
	if err := vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Type == CatalogRecordFile || r.Type == CatalogRecordFolder {
			live[r.CNID] = r.Name
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}
	for _, r := range recs {
		if name, isLive := live[r.Record.CNID]; isLive && name == r.Record.Name {
			t.Errorf("cnid %d (%q) is live but was reported as deleted", r.Record.CNID, name)
		}
		if r.StaleCopy {
			t.Errorf("cnid %d (%q) is flagged StaleCopy but was returned by the default view",
				r.Record.CNID, r.Record.Name)
		}
	}
}

// A record present in node slack but absent from the live catalog is a genuine
// deletion, and must be recovered as one. The corpus image has no deletions, so
// this is the only place that path is exercised end to end.
func TestRecoverGenuineDeletion(t *testing.T) {
	const deletedCNID = uint32(400)
	const deletedName = "deleted-secret.txt"

	vol, err := Open(bytes.NewReader(buildImageWithDeletedRecord(t, deletedCNID, deletedName)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// It must not appear in any live view.
	if _, err := vol.OpenCNID(deletedCNID); err == nil {
		t.Fatal("the deleted record is reachable through OpenCNID")
	}
	entries, err := vol.ReadDirCNID(rootFolderCNID)
	if err != nil {
		t.Fatalf("ReadDirCNID: %v", err)
	}
	for _, e := range entries {
		if e.Name == deletedName {
			t.Fatal("the deleted record appears in a directory listing")
		}
	}

	// It must appear in recovery.
	recs, err := vol.RecoverDeleted(nil)
	if err != nil {
		t.Fatalf("RecoverDeleted: %v", err)
	}

	var found *DeletedRecord
	for i := range recs {
		if recs[i].Record.CNID == deletedCNID {
			found = &recs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the deleted record was not recovered; got %d records", len(recs))
	}
	if found.Record.Name != deletedName {
		t.Errorf("recovered name = %q, want %q", found.Record.Name, deletedName)
	}
	if found.Source != RecoveredFromNodeSlack {
		t.Errorf("Source = %v, want node slack", found.Source)
	}
	if found.StaleCopy {
		t.Error("a genuinely deleted record was flagged as a stale copy")
	}
	if found.NodeNumber == 0 {
		t.Error("no node number recorded for provenance")
	}
	t.Logf("recovered %q: cnid=%d confidence=%s overwritten=%v node=%d offset=%d",
		found.Record.Name, found.Record.CNID, found.Confidence,
		found.Overwritten, found.NodeNumber, found.ByteOffset)
}

// Provenance must be usable: an examiner has to be able to go back to the bytes.
func TestCorpusDeletedProvenance(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	recs, err := vol.RecoverDeleted(nil)
	if err != nil {
		t.Fatalf("RecoverDeleted: %v", err)
	}
	if len(recs) == 0 {
		t.Skip("no deleted records in this image")
	}

	hdr, err := vol.CatalogBTreeHeader()
	if err != nil {
		t.Fatalf("CatalogBTreeHeader: %v", err)
	}

	for _, r := range recs {
		switch r.Source {
		case RecoveredFromNodeSlack:
			if r.NodeNumber == 0 || r.NodeNumber >= hdr.TotalNodes {
				t.Errorf("slack record cites node %d, outside 1..%d", r.NodeNumber, hdr.TotalNodes)
			}
			if r.ByteOffset <= 0 || r.ByteOffset >= int64(hdr.NodeSize) {
				t.Errorf("slack record cites offset %d, outside the node", r.ByteOffset)
			}
		case RecoveredFromFreeNode:
			if r.NodeNumber == 0 || r.NodeNumber >= hdr.TotalNodes {
				t.Errorf("free-node record cites node %d, outside 1..%d", r.NodeNumber, hdr.TotalNodes)
			}
		}
	}
}

// MinConfidence must actually filter, and confidence must be monotonic: raising
// the floor can only ever return fewer records.
func TestCorpusDeletedConfidenceFilter(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	all, err := vol.RecoverDeleted(&RecoveryOptions{
		ScanNodeSlack: true, ScanFreeNodes: true, MinConfidence: ConfidenceLow,
	})
	if err != nil {
		t.Fatalf("RecoverDeleted(low): %v", err)
	}
	medium, err := vol.RecoverDeleted(&RecoveryOptions{
		ScanNodeSlack: true, ScanFreeNodes: true, MinConfidence: ConfidenceMedium,
	})
	if err != nil {
		t.Fatalf("RecoverDeleted(medium): %v", err)
	}
	high, err := vol.RecoverDeleted(&RecoveryOptions{
		ScanNodeSlack: true, ScanFreeNodes: true, MinConfidence: ConfidenceHigh,
	})
	if err != nil {
		t.Fatalf("RecoverDeleted(high): %v", err)
	}

	t.Logf("low=%d medium=%d high=%d", len(all), len(medium), len(high))
	if len(medium) > len(all) || len(high) > len(medium) {
		t.Errorf("confidence filtering is not monotonic: low=%d medium=%d high=%d",
			len(all), len(medium), len(high))
	}
	for _, r := range high {
		if r.Confidence != ConfidenceHigh {
			t.Errorf("record graded %v survived a ConfidenceHigh filter", r.Confidence)
		}
	}
}

// Disabling every source must yield nothing, confirming the options are honoured
// rather than ignored.
func TestDeletedSourcesAreOptional(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	recs, err := vol.RecoverDeleted(&RecoveryOptions{})
	if err != nil {
		t.Fatalf("RecoverDeleted with no sources: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("got %d records with every source disabled", len(recs))
	}
}

func TestWalkDeletedStopsOnCallbackError(t *testing.T) {
	// Uses the synthetic image: the corpus image has no deletions, so the
	// callback would never run there and the test would pass vacuously.
	vol, err := Open(bytes.NewReader(buildImageWithDeletedRecord(t, 400, "gone.txt")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var seen int
	err = vol.WalkDeleted(nil, func(DeletedRecord) error {
		seen++
		return ErrNotFound
	})
	if err != ErrNotFound {
		t.Fatalf("WalkDeleted returned %v, want the callback's error", err)
	}
	if seen != 1 {
		t.Errorf("callback ran %d times after erroring, want 1", seen)
	}
}

// Reading a recovered record's content must work, and must be bounded by what
// its extents can actually hold rather than by a possibly corrupt size field.
func TestOpenDeletedBoundsSize(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	recs, err := vol.RecoverDeleted(nil)
	if err != nil {
		t.Fatalf("RecoverDeleted: %v", err)
	}

	var opened int
	for _, r := range recs {
		if len(compactExtents(r.Record.DataFork.Extents[:])) == 0 {
			continue
		}
		fh, err := vol.OpenDeleted(r)
		if err != nil {
			continue
		}
		capacity := vol.extentsByteCapacity(compactExtents(r.Record.DataFork.Extents[:]))
		if fh.Size() > capacity {
			t.Errorf("cnid %d: OpenDeleted size %d exceeds extent capacity %d",
				r.Record.CNID, fh.Size(), capacity)
		}
		if _, err := fh.ReadAll(); err != nil {
			t.Errorf("cnid %d: reading recovered content: %v", r.Record.CNID, err)
		}
		opened++
		if opened >= 5 {
			break
		}
	}
	t.Logf("opened %d recovered records", opened)
}

// A record with no extents cannot be read; that must be an error rather than a
// zero-length success that looks like an empty file.
func TestOpenDeletedRejectsExtentlessRecord(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildValidCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := vol.OpenDeleted(DeletedRecord{}); err == nil {
		t.Error("OpenDeleted on a record with no extents returned nil error")
	}
}

// ---------------------------------------------------------------------------
// Unallocated carving
// ---------------------------------------------------------------------------

// Carving unallocated space is opt-in because it reads a large part of the
// image. It must find at least what the cheaper sources find.
func TestCorpusUnallocatedCarving(t *testing.T) {
	if testing.Short() {
		t.Skip("unallocated carving reads the whole free area")
	}
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	withCarve, err := vol.RecoverDeleted(&RecoveryOptions{
		ScanNodeSlack:   true,
		ScanFreeNodes:   true,
		ScanUnallocated: true,
	})
	if err != nil {
		t.Fatalf("RecoverDeleted with carving: %v", err)
	}
	without, err := vol.RecoverDeleted(nil)
	if err != nil {
		t.Fatalf("RecoverDeleted without carving: %v", err)
	}

	t.Logf("with carving=%d, without=%d", len(withCarve), len(without))
	if len(withCarve) < len(without) {
		t.Errorf("carving returned fewer records (%d) than the default sources (%d)",
			len(withCarve), len(without))
	}
}

// buildImageWithDeletedRecord lays out a volume whose catalog leaf node holds
// two live records plus, in its slack, the bytes of a record that is not in the
// tree — exactly the state HFS+ leaves behind when a file is deleted.
func buildImageWithDeletedRecord(t testing.TB, cnid uint32, name string) []byte {
	t.Helper()

	const (
		blockSize    = uint32(4096)
		catalogBlock = uint32(2)
		nodeSize     = uint16(1024)
		dataBlock    = uint32(6)
	)

	img := make([]byte, int(dataBlock+2)*int(blockSize))

	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], blockSize)
	binary.BigEndian.PutUint32(vh[44:48], dataBlock+2)
	binary.BigEndian.PutUint32(vh[48:52], 2)
	binary.BigEndian.PutUint32(vh[64:68], 1000) // nextCatalogID, so CNIDs look plausible
	binary.BigEndian.PutUint64(vh[272:280], uint64(blockSize))
	binary.BigEndian.PutUint32(vh[272+12:272+16], 1)
	binary.BigEndian.PutUint32(vh[272+16:272+20], catalogBlock)
	binary.BigEndian.PutUint32(vh[272+20:272+24], 1)

	// The allocation file is absent, so every block reads as free — which makes
	// the recovered record's blocks look intact rather than reused.

	live := [][]byte{
		append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecord(rootFolderCNID, 1)...),
		append(buildCatalogKey(rootFolderCNID, "kept.txt"), buildFileRecord(300)...),
	}
	node := makeNode(nodeSize, btreeNodeTypeLeaf, live)

	// The deleted record's bytes, written into the node's free space where the
	// filesystem left them.
	deletedRec := buildFileRecord(cnid)
	binary.BigEndian.PutUint64(deletedRec[88:96], 4096)    // logical size
	binary.BigEndian.PutUint32(deletedRec[88+12:88+16], 1) // 1 block
	binary.BigEndian.PutUint32(deletedRec[88+16:88+20], dataBlock)
	binary.BigEndian.PutUint32(deletedRec[88+20:88+24], 1)
	stale := append(buildCatalogKey(rootFolderCNID, name), deletedRec...)

	desc, err := parseBTreeNodeDescriptor(node)
	if err != nil {
		t.Fatalf("parseBTreeNodeDescriptor: %v", err)
	}
	slack, base := nodeSlackRegion(node, desc)
	if len(slack) < len(stale) {
		t.Fatalf("node slack is %d bytes, need %d for the deleted record", len(slack), len(stale))
	}
	copy(node[base:], stale)

	catBase := int(catalogBlock * blockSize)
	copy(img[catBase:catBase+int(nodeSize)],
		makeNode(nodeSize, btreeNodeTypeHead,
			[][]byte{buildBTreeHeaderRecordBytesAt(nodeSize, 2, 1, 1)}))
	copy(img[catBase+int(nodeSize):catBase+2*int(nodeSize)], node)

	// Content the deleted record points at, still on disk.
	dataStart := int(dataBlock) * int(blockSize)
	copy(img[dataStart:], []byte("this content survived the deletion"))

	return img
}

func TestNodeSlackRegion(t *testing.T) {
	const nodeSize = 512
	records := [][]byte{
		append(buildCatalogKey(2, "a"), buildFolderRecord(100, 0)...),
		append(buildCatalogKey(2, "b"), buildFolderRecord(101, 0)...),
	}
	node := makeNode(nodeSize, btreeNodeTypeLeaf, records)
	desc, err := parseBTreeNodeDescriptor(node)
	if err != nil {
		t.Fatalf("parseBTreeNodeDescriptor: %v", err)
	}

	slack, base := nodeSlackRegion(node, desc)
	if len(slack) == 0 {
		t.Fatal("no slack region found in a node with room to spare")
	}

	// The slack must start after the last record and stop before the offset
	// array, and must not overlap either.
	offs, err := parseNodeRecordOffsets(node, desc.NumRecords)
	if err != nil {
		t.Fatalf("parseNodeRecordOffsets: %v", err)
	}
	wantStart := int(offs[desc.NumRecords])
	wantEnd := nodeSize - 2*(int(desc.NumRecords)+1)

	if base != wantStart {
		t.Errorf("slack starts at %d, want %d", base, wantStart)
	}
	if base+len(slack) != wantEnd {
		t.Errorf("slack ends at %d, want %d", base+len(slack), wantEnd)
	}
}
