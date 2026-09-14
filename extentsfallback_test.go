package libhfs

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The extents overflow tree has two readers, and until these tests only one of
// them had ever run.
//
// Keyed descent (btreeSearcher.scanFrom) serves a healthy volume. The linear
// walk in btree_walk.go — walkExtentsBTree, walkExtentsLeafChain and the
// extentsWalkState methods — is the fallback walkExtentsForFork drops to when
// descent degrades. Measured across the hermetic suite and all 14 corpus
// images, every one of those functions sat at 0% coverage, because
// supportsKeyedSearch answers true for every volume kind, so descent always won
// and the fallback was never asked for.
//
// That is the wrong half to leave dark. btree_search.go opens by saying partial
// corruption is the normal case on forensic images and that a search which
// gives up is worse than a slow search that succeeds. The fallback is what
// makes that claim true, and it runs only on volumes that are already damaged —
// which is exactly when a silently wrong extent list becomes a wrong
// extraction.
//
// Each fallback test is paired with a control on the same fixture with the
// damage removed. The control proves the extent list is reachable at all, so a
// fallback test cannot pass by agreeing with a reader that found nothing.

const (
	ovfFileCNID  = uint32(17)
	ovfVolName   = "ovfvol"
	ovfFileName  = "big.bin"
	ovfBlockSize = uint32(4096)
	ovfNodeSize  = uint16(512)

	ovfCatalogBlock = uint32(1)
	ovfExtentsBlock = uint32(2)

	// The fork claims five allocation blocks while filExtRec names three, so
	// resolving it has to read the overflow tree. A result of three extents
	// means the overflow record was skipped rather than read.
	ovfForkBlocks  = uint32(5)
	ovfLogicalSize = uint32(5*4096 - 100)
)

// ovfWantExtents is the fork's complete extent list: the first two come from
// the catalog record, the third from the overflow record keyed at start block 3.
var ovfWantExtents = []ExtentDescriptor{
	{StartBlock: 10, BlockCount: 2},
	{StartBlock: 20, BlockCount: 1},
	{StartBlock: 30, BlockCount: 2},
}

// buildClassicHFSOverflowImage builds a classic HFS volume holding one file
// whose data fork spills past the three extents a CatFilRec can carry.
//
// extentsRoot becomes the overflow tree's root node. Pointing it past
// TotalNodes degrades keyed descent without touching the leaf the linear walk
// reads: the searcher refuses a node number it cannot address, while
// walkExtentsLeafChain starts from FirstLeafNode and never consults the root.
// That asymmetry is the reason the classic fallback can recover a fork descent
// cannot reach at all.
//
// Offsets are written as literals rather than through the decoder's own
// constants, so the fixture cannot drift into agreement with a decoder whose
// constants have moved.
func buildClassicHFSOverflowImage(tb testing.TB, extentsRoot uint32) []byte {
	tb.Helper()

	const (
		totalBlocks = 100
		alBlSt      = uint16(4) // drAlBlSt, in 512-byte sectors
		dataBase    = int(alBlSt) * 512
	)
	img := make([]byte, dataBase+totalBlocks*int(ovfBlockSize))

	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(mdb[0:2], signatureHFS)
	binary.BigEndian.PutUint16(mdb[14:16], 3)            // drVBMSt
	binary.BigEndian.PutUint16(mdb[18:20], totalBlocks)  // drNmAlBlks
	binary.BigEndian.PutUint32(mdb[20:24], ovfBlockSize) // drAlBlkSiz
	binary.BigEndian.PutUint16(mdb[28:30], alBlSt)       // drAlBlSt
	binary.BigEndian.PutUint16(mdb[34:36], 80)           // drFreeBks
	binary.BigEndian.PutUint32(mdb[84:88], 1)            // drFilCnt
	binary.BigEndian.PutUint32(mdb[88:92], 1)            // drDirCnt

	// drXTFlSize / drXTExtRec, then drCTFlSize / drCTExtRec. Classic HFS records
	// these fork sizes in bytes and their extents as uint16 pairs.
	binary.BigEndian.PutUint32(mdb[0x82:0x86], ovfBlockSize)
	binary.BigEndian.PutUint16(mdb[0x86:0x88], uint16(ovfExtentsBlock))
	binary.BigEndian.PutUint16(mdb[0x88:0x8A], 1)
	binary.BigEndian.PutUint32(mdb[0x92:0x96], ovfBlockSize)
	binary.BigEndian.PutUint16(mdb[0x96:0x98], uint16(ovfCatalogBlock))
	binary.BigEndian.PutUint16(mdb[0x98:0x9A], 1)

	tf := fullTimesFixture()
	join := func(key, rec []byte) []byte { return append(key, rec...) }

	// Catalog key order is parent CNID ascending, then name.
	catalogRecords := [][]byte{
		join(buildCatalogKeyHFSBytes(hfsRootParentCNID, []byte(ovfVolName)),
			buildFolderRecordHFS(rootFolderCNID, 1, tf)),
		join(buildCatalogKeyHFSBytes(rootFolderCNID, nil),
			buildThreadRecordHFS(hfsRecordTypeFolderThread, hfsRootParentCNID, ovfVolName)),
		join(buildCatalogKeyHFSBytes(rootFolderCNID, []byte(ovfFileName)),
			buildOverflowFileRecordHFS(ovfFileCNID, tf)),
		// A file thread is optional on classic HFS, but writing one keeps
		// OpenCNID a direct lookup so these tests can only fail for extent
		// reasons. The missing-thread fallback has its own test.
		join(buildCatalogKeyHFSBytes(ovfFileCNID, nil),
			buildThreadRecordHFS(hfsRecordTypeFileThread, rootFolderCNID, ovfFileName)),
	}

	writeNode := func(base int, num uint32, node []byte) {
		off := base + int(num)*int(ovfNodeSize)
		copy(img[off:off+int(ovfNodeSize)], node)
	}

	catBase := dataBase + int(ovfCatalogBlock*ovfBlockSize)
	writeNode(catBase, 0, makeNode(ovfNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(ovfNodeSize, 2, 1, 1)}))
	writeNode(catBase, 1, makeNode(ovfNodeSize, btreeNodeTypeLeaf, catalogRecords))

	// One overflow record, keyed at the first logical block the catalog record
	// does not describe.
	extBase := dataBase + int(ovfExtentsBlock*ovfBlockSize)
	overflowRec := append(
		buildExtentsKeyHFS(ovfFileCNID, extentKeyTypeData, 3),
		buildExtentsPayloadHFS(ovfWantExtents[2:])...)
	writeNode(extBase, 0, makeNode(ovfNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(ovfNodeSize, 2, extentsRoot, 1)}))
	writeNode(extBase, 1, makeNode(ovfNodeSize, btreeNodeTypeLeaf, [][]byte{overflowRec}))

	return img
}

// buildExtentsKeyHFS builds an HFSExtentKey: xkrKeyLen(1), xkrFkType(1),
// xkrFNum(4), xkrFABN(2). Eight bytes, already even, so the parser's
// odd-length padding rule never applies here.
func buildExtentsKeyHFS(fileID uint32, forkType uint8, startBlock uint16) []byte {
	k := make([]byte, 8)
	k[0] = 7 // key length, not counting the length byte itself
	k[1] = forkType
	binary.BigEndian.PutUint32(k[2:6], fileID)
	binary.BigEndian.PutUint16(k[6:8], startBlock)
	return k
}

// buildExtentsPayloadHFS builds an HFSExtentRecord: three (startBlock,
// blockCount) pairs of uint16 — half the width of the HFS+ record that
// buildExtentsPayload writes.
func buildExtentsPayloadHFS(exts []ExtentDescriptor) []byte {
	p := make([]byte, 12)
	for i, e := range exts {
		if i >= 3 {
			break
		}
		binary.BigEndian.PutUint16(p[i*4:i*4+2], uint16(e.StartBlock))
		binary.BigEndian.PutUint16(p[i*4+2:i*4+4], uint16(e.BlockCount))
	}
	return p
}

// buildOverflowFileRecordHFS builds a CatFilRec whose physical size claims five
// allocation blocks while filExtRec names only three of them. The unused third
// extent pair stays zero, which is how a classic record says the rest lives in
// the overflow file.
func buildOverflowFileRecordHFS(cnid uint32, tf timesFixture) []byte {
	r := make([]byte, 102)
	r[0] = 0x02                                                      // cdrType: file
	binary.BigEndian.PutUint32(r[20:24], cnid)                       // filFlNum
	binary.BigEndian.PutUint32(r[26:30], ovfLogicalSize)             // filLgLen
	binary.BigEndian.PutUint32(r[30:34], ovfForkBlocks*ovfBlockSize) // filPyLen
	binary.BigEndian.PutUint32(r[44:48], tf.created)                 // filCrDat
	binary.BigEndian.PutUint32(r[48:52], tf.contentMod)              // filMdDat
	binary.BigEndian.PutUint32(r[52:56], tf.backup)                  // filBkDat
	copy(r[74:86], buildExtentsPayloadHFS(ovfWantExtents[:2]))       // filExtRec
	return r
}

func openOverflowVolume(t *testing.T, extentsRoot uint32) *Volume {
	t.Helper()
	vol, err := Open(bytes.NewReader(buildClassicHFSOverflowImage(t, extentsRoot)))
	if err != nil {
		t.Fatalf("Open(extentsRoot=%d): %v", extentsRoot, err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("Kind = %s, want %s", vol.Kind(), KindHFS)
	}
	return vol
}

func assertExtents(t *testing.T, got, want []ExtentDescriptor) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("resolved %d extents %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("extent %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func hasAnomaly(v *Volume, op string) bool {
	for _, a := range v.Anomalies() {
		if a.Op == op {
			return true
		}
	}
	return false
}

// TestClassicHFSExtentsOverflowResolves is the control for the fallback test
// below: with a healthy root, keyed descent reads the overflow record, so the
// expected extent list is known to be reachable before anything asks the
// fallback for it.
func TestClassicHFSExtentsOverflowResolves(t *testing.T) {
	vol := openOverflowVolume(t, 1)

	got, err := vol.ResolveDataForkExtents(ovfFileCNID)
	if err != nil {
		t.Fatalf("ResolveDataForkExtents: %v", err)
	}
	assertExtents(t, got, ovfWantExtents)

	if n := vol.AnomalyCount(); n != 0 {
		t.Errorf("AnomalyCount = %d on an undamaged volume, want 0; anomalies: %v",
			n, vol.Anomalies())
	}
}

// TestClassicHFSExtentsOverflowFallsBackToLeafChain drives walkExtentsLeafChain,
// the classic-HFS half of the fallback, by making the overflow tree's root node
// unreadable while leaving its leaf chain intact.
//
// A classic volume is where this matters most. HFS+ descends the fallback tree
// from the same root the searcher failed on, so a bad root defeats both;
// walkExtentsLeafChain ignores the root and walks FirstLeafNode forward, which
// is the one shape where the fallback recovers a fork descent cannot reach.
func TestClassicHFSExtentsOverflowFallsBackToLeafChain(t *testing.T) {
	const unreachableRoot = 99 // >= TotalNodes, so the searcher cannot read it
	vol := openOverflowVolume(t, unreachableRoot)

	// The premise: descent really is broken on this fixture. Without this the
	// test would still pass if the searcher quietly succeeded, and the code it
	// exists to cover would go on never running.
	hdr, err := vol.ExtentsBTreeHeader()
	if err != nil {
		t.Fatalf("ExtentsBTreeHeader: %v", err)
	}
	if hdr.RootNode < hdr.TotalNodes {
		t.Fatalf("fixture root node %d is inside TotalNodes %d, so descent will not degrade",
			hdr.RootNode, hdr.TotalNodes)
	}
	if hdr.FirstLeafNode == 0 || hdr.FirstLeafNode >= hdr.TotalNodes {
		t.Fatalf("fixture FirstLeafNode %d is unusable, so the fallback cannot be what is under test",
			hdr.FirstLeafNode)
	}

	got, err := vol.ResolveDataForkExtents(ovfFileCNID)
	if err != nil {
		t.Fatalf("ResolveDataForkExtents through the fallback: %v", err)
	}
	assertExtents(t, got, ovfWantExtents)

	// A silent recovery is the wrong outcome: an examiner reading the report
	// needs to know the volume was damaged enough to need the slow path.
	if !hasAnomaly(vol, "extents_search") {
		t.Errorf("no extents_search anomaly recorded; anomalies: %v", vol.Anomalies())
	}
}

// TestExtentsOverflowFallsBackOnUnorderedNode drives the HFS+ half of the
// fallback — walkExtentsBTree's descent from the root, and both branches of
// extentsWalkState.walkNode — by zeroing the free-space offset in the overflow
// tree's index node.
//
// That single value is what separates the two readers. orderedNodeRecords, used
// by descent, needs each record to end where the next begins and rejects the
// node outright. extractNodeRecords, used by the linear walk, treats an
// out-of-range free-space marker as absent and salvages the record anyway. The
// tolerance is deliberate and documented at extractNodeRecords; this is the
// first test that depends on it.
func TestExtentsOverflowFallsBackOnUnorderedNode(t *testing.T) {
	// The fixture's geometry, repeated here as literals: extents fork in block
	// 3 of a 4096-byte-block volume, 512-byte nodes, index node 1.
	const (
		extentsBase   = 3 * 4096
		nodeSize      = 512
		indexNode     = 1
		overflowCNID  = 101
		freeSpaceSlot = extentsBase + indexNode*nodeSize + nodeSize - 4
	)
	want := []ExtentDescriptor{{StartBlock: 50, BlockCount: 1}, {StartBlock: 60, BlockCount: 2}}

	// Control first: undamaged, the same extents resolve by descent.
	clean := buildExtentsOverflowTestImage(t, true)
	volClean, err := Open(bytes.NewReader(clean))
	if err != nil {
		t.Fatalf("Open(clean): %v", err)
	}
	got, err := volClean.ResolveDataForkExtents(overflowCNID)
	if err != nil {
		t.Fatalf("ResolveDataForkExtents(clean): %v", err)
	}
	assertExtents(t, got, want)
	if n := volClean.AnomalyCount(); n != 0 {
		t.Fatalf("AnomalyCount = %d on the undamaged fixture, want 0; anomalies: %v",
			n, volClean.Anomalies())
	}

	// The premise: the slot really does hold the end of the index node's one
	// record, so zeroing it is the mutation intended and not a stray write.
	if off := binary.BigEndian.Uint16(clean[freeSpaceSlot : freeSpaceSlot+2]); off <= btreeNodeDescSize {
		t.Fatalf("free-space offset is %d, not past the node descriptor; the fixture layout has moved", off)
	}

	damaged := buildExtentsOverflowTestImage(t, true)
	binary.BigEndian.PutUint16(damaged[freeSpaceSlot:freeSpaceSlot+2], 0)

	vol, err := Open(bytes.NewReader(damaged))
	if err != nil {
		t.Fatalf("Open(damaged): %v", err)
	}
	got, err = vol.ResolveDataForkExtents(overflowCNID)
	if err != nil {
		t.Fatalf("ResolveDataForkExtents through the fallback: %v", err)
	}
	assertExtents(t, got, want)

	if !hasAnomaly(vol, "extents_search") {
		t.Errorf("no extents_search anomaly recorded; anomalies: %v", vol.Anomalies())
	}
}
