package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestCatalogTraversalAndLookup(t *testing.T) {
	img := buildCatalogTestImage(t)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	recs, err := vol.CatalogRecords()
	if err != nil {
		t.Fatalf("CatalogRecords failed: %v", err)
	}
	if len(recs) < 3 {
		t.Fatalf("expected at least 3 records, got %d", len(recs))
	}

	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}
	if root.CNID != rootFolderCNID || root.Name != "" {
		t.Fatalf("unexpected root: %#v", root)
	}

	etc, err := vol.OpenPath("/etc")
	if err != nil {
		t.Fatalf("OpenPath(/etc) failed: %v", err)
	}
	if !etc.IsDirectory() || etc.CNID != 100 {
		t.Fatalf("unexpected /etc record: %#v", etc)
	}

	hosts, err := vol.OpenPath("/etc/hosts")
	if err != nil {
		t.Fatalf("OpenPath(/etc/hosts) failed: %v", err)
	}
	if hosts.CNID != 101 || hosts.Type != CatalogRecordFile {
		t.Fatalf("unexpected /etc/hosts record: %#v", hosts)
	}

	_, err = vol.OpenPath("/etc/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing entry, got %v", err)
	}
}

func TestOpenPathHFSPCaseInsensitive(t *testing.T) {
	img := buildCatalogTestImageWithOptions(t, signatureHFSP, versionHFSPlus, btreeCompTypeInsensitive, "etc")
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	rec, err := vol.OpenPath("/EtC/HoStS")
	if err != nil {
		t.Fatalf("OpenPath case-insensitive failed: %v", err)
	}
	if rec.CNID != 101 {
		t.Fatalf("unexpected record: %#v", rec)
	}
}

func TestOpenPathHFSXCaseSensitive(t *testing.T) {
	img := buildCatalogTestImageWithOptions(t, signatureHFSX, versionHFSX, btreeCompTypeSensitive, "Etc")
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.OpenPath("/etc/hosts")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected case-sensitive miss, got %v", err)
	}

	rec, err := vol.OpenPath("/Etc/hosts")
	if err != nil {
		t.Fatalf("OpenPath exact-case failed: %v", err)
	}
	if rec.CNID != 101 {
		t.Fatalf("unexpected record: %#v", rec)
	}
}

func TestReadDir(t *testing.T) {
	img := buildCatalogTestImage(t)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	rootEntries, err := vol.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir(/) failed: %v", err)
	}
	if len(rootEntries) != 1 || rootEntries[0].Name != "etc" || !rootEntries[0].IsDirectory {
		t.Fatalf("unexpected root entries: %#v", rootEntries)
	}

	etcEntries, err := vol.ReadDir("/etc")
	if err != nil {
		t.Fatalf("ReadDir(/etc) failed: %v", err)
	}
	if len(etcEntries) != 1 || etcEntries[0].Name != "hosts" || etcEntries[0].IsDirectory {
		t.Fatalf("unexpected /etc entries: %#v", etcEntries)
	}

	_, err = vol.ReadDir("/etc/hosts")
	if !errors.Is(err, ErrNotDir) {
		t.Fatalf("expected ErrNotDir, got %v", err)
	}
}

func TestPathForCNIDUsingThreadRecords(t *testing.T) {
	img := buildCatalogThreadedTestImage(t)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	root, err := vol.PathForCNID(rootFolderCNID)
	if err != nil {
		t.Fatalf("PathForCNID(root) failed: %v", err)
	}
	if root != "/" {
		t.Fatalf("unexpected root path: %q", root)
	}

	etc, err := vol.PathForCNID(100)
	if err != nil {
		t.Fatalf("PathForCNID(100) failed: %v", err)
	}
	if etc != "/etc" {
		t.Fatalf("unexpected /etc path: %q", etc)
	}

	hosts, err := vol.PathForCNID(101)
	if err != nil {
		t.Fatalf("PathForCNID(101) failed: %v", err)
	}
	if hosts != "/etc/hosts" {
		t.Fatalf("unexpected /etc/hosts path: %q", hosts)
	}
}

func TestPathCNIDRoundTrip(t *testing.T) {
	img := buildCatalogThreadedTestImage(t)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	rec, err := vol.OpenPath("/etc/hosts")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}
	path, err := vol.PathForCNID(rec.CNID)
	if err != nil {
		t.Fatalf("PathForCNID failed: %v", err)
	}
	if path != "/etc/hosts" {
		t.Fatalf("unexpected roundtrip path: %q", path)
	}
}

func TestWalkDirAndWalkCatalog(t *testing.T) {
	img := buildCatalogTestImage(t)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	seenDir := make([]string, 0)
	err = vol.WalkDir("/etc", func(ent DirEntry) error {
		seenDir = append(seenDir, ent.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir failed: %v", err)
	}
	if len(seenDir) != 1 || seenDir[0] != "hosts" {
		t.Fatalf("unexpected WalkDir entries: %#v", seenDir)
	}

	seenCNIDs := map[uint32]bool{}
	err = vol.WalkCatalog(func(r CatalogRecord) error {
		if r.CNID != 0 {
			seenCNIDs[r.CNID] = true
		}
		if r.CNID == 101 {
			return errStopWalk
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog failed: %v", err)
	}
	if !seenCNIDs[rootFolderCNID] || !seenCNIDs[100] || !seenCNIDs[101] {
		t.Fatalf("missing expected CNIDs in walk: %#v", seenCNIDs)
	}
}

func buildCatalogTestImage(t *testing.T) []byte {
	return buildCatalogTestImageWithOptions(t, signatureHFSP, versionHFSPlus, btreeCompTypeInsensitive, "etc")
}

func buildCatalogTestImageWithOptions(t *testing.T, signature uint16, version uint16, compType uint8, dirName string) []byte {
	t.Helper()

	blockSize := uint32(4096)
	catalogStartBlock := uint32(2)
	nodeSize := uint16(512)
	totalNodes := uint32(4) // 0 header, 1 root index, 2 leaf, 3 leaf

	img := make([]byte, int((catalogStartBlock+1)*blockSize))
	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signature)
	binary.BigEndian.PutUint16(vh[2:4], version)
	binary.BigEndian.PutUint32(vh[40:44], blockSize)
	binary.BigEndian.PutUint32(vh[44:48], 200)
	binary.BigEndian.PutUint32(vh[48:52], 120)

	// catalog fork
	binary.BigEndian.PutUint64(vh[272:280], uint64(blockSize))
	binary.BigEndian.PutUint32(vh[272+12:272+16], 1)
	binary.BigEndian.PutUint32(vh[272+16:272+20], catalogStartBlock)
	binary.BigEndian.PutUint32(vh[272+20:272+24], 1)

	treeBase := int(catalogStartBlock * blockSize)

	writeNode := func(nodeNum uint32, node []byte) {
		off := treeBase + int(nodeNum)*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}

	// Node 0: header node
	headerNode := makeNode(nodeSize, btreeNodeTypeHead, [][]byte{buildBTreeHeaderRecordBytesWithCompType(nodeSize, totalNodes, 1, compType)})
	writeNode(0, headerNode)

	// Node 1: index root -> children 2 and 3
	idxRec1 := append(buildCatalogKey(rootFolderCNID, ""), u32be(2)...)
	idxRec2 := append(buildCatalogKey(rootFolderCNID, dirName), u32be(3)...)
	writeNode(1, makeNode(nodeSize, btreeNodeTypeIdx, [][]byte{idxRec1, idxRec2}))

	// Node 2: leaf containing root folder and /etc folder
	leafRoot := append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecord(rootFolderCNID, 2)...)
	leafEtc := append(buildCatalogKey(rootFolderCNID, dirName), buildFolderRecord(100, 1)...)
	node2 := makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leafRoot, leafEtc})
	setForwardLink(node2, 3) // chain leaf 2 → leaf 3
	writeNode(2, node2)

	// Node 3: leaf containing /etc/hosts file
	leafHosts := append(buildCatalogKey(100, "hosts"), buildFileRecord(101)...)
	writeNode(3, makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leafHosts}))

	return img
}

func buildCatalogThreadedTestImage(t *testing.T) []byte {
	t.Helper()

	blockSize := uint32(4096)
	catalogStartBlock := uint32(2)
	nodeSize := uint16(512)
	totalNodes := uint32(4)

	img := make([]byte, int((catalogStartBlock+1)*blockSize))
	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], blockSize)
	binary.BigEndian.PutUint32(vh[44:48], 200)
	binary.BigEndian.PutUint32(vh[48:52], 120)

	binary.BigEndian.PutUint64(vh[272:280], uint64(blockSize))
	binary.BigEndian.PutUint32(vh[272+12:272+16], 1)
	binary.BigEndian.PutUint32(vh[272+16:272+20], catalogStartBlock)
	binary.BigEndian.PutUint32(vh[272+20:272+24], 1)

	treeBase := int(catalogStartBlock * blockSize)
	writeNode := func(nodeNum uint32, node []byte) {
		off := treeBase + int(nodeNum)*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}

	headerNode := makeNode(nodeSize, btreeNodeTypeHead, [][]byte{buildBTreeHeaderRecordBytes(nodeSize, totalNodes, 1)})
	writeNode(0, headerNode)

	idxRec1 := append(buildCatalogKey(rootFolderCNID, ""), u32be(2)...)
	idxRec2 := append(buildCatalogKey(rootFolderCNID, "etc"), u32be(3)...)
	writeNode(1, makeNode(nodeSize, btreeNodeTypeIdx, [][]byte{idxRec1, idxRec2}))

	leafRoot := append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecord(rootFolderCNID, 2)...)
	leafEtc := append(buildCatalogKey(rootFolderCNID, "etc"), buildFolderRecord(100, 1)...)
	leafEtcThread := append(buildCatalogKey(100, ""), buildThreadRecord(catalogRecordFolderThread, rootFolderCNID, "etc")...)
	node2t := makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leafRoot, leafEtc, leafEtcThread})
	setForwardLink(node2t, 3) // chain leaf 2 → leaf 3
	writeNode(2, node2t)

	leafHosts := append(buildCatalogKey(100, "hosts"), buildFileRecord(101)...)
	leafHostsThread := append(buildCatalogKey(101, ""), buildThreadRecord(catalogRecordFileThread, 100, "hosts")...)
	writeNode(3, makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leafHosts, leafHostsThread}))

	return img
}

func buildBTreeHeaderRecordBytesWithCompType(nodeSize uint16, totalNodes uint32, rootNode uint32, compType uint8) []byte {
	r := buildBTreeHeaderRecordBytes(nodeSize, totalNodes, rootNode)
	r[37] = compType
	return r
}

func buildBTreeHeaderRecordBytes(nodeSize uint16, totalNodes uint32, rootNode uint32) []byte {
	r := make([]byte, btreeHeaderRecSize)
	binary.BigEndian.PutUint16(r[0:2], 2)
	binary.BigEndian.PutUint32(r[2:6], rootNode)
	binary.BigEndian.PutUint32(r[6:10], 3)
	binary.BigEndian.PutUint32(r[10:14], 2)
	binary.BigEndian.PutUint32(r[14:18], 3)
	binary.BigEndian.PutUint16(r[18:20], nodeSize)
	binary.BigEndian.PutUint16(r[20:22], 520)
	binary.BigEndian.PutUint32(r[22:26], totalNodes)
	binary.BigEndian.PutUint32(r[26:30], 0)
	binary.BigEndian.PutUint32(r[32:36], 0)
	r[36] = 0
	r[37] = 0xC7
	binary.BigEndian.PutUint32(r[38:42], 0x00000004)
	return r
}

func makeNode(nodeSize uint16, nodeType int8, records [][]byte) []byte {
	node := make([]byte, int(nodeSize))
	node[8] = byte(nodeType)
	binary.BigEndian.PutUint16(node[10:12], uint16(len(records)))

	starts := make([]int, 0, len(records)+1)
	cur := btreeNodeDescSize
	for _, rec := range records {
		copy(node[cur:], rec)
		starts = append(starts, cur)
		cur += len(rec)
	}
	starts = append(starts, cur)

	for i, start := range starts {
		pos := int(nodeSize) - 2*(i+1)
		binary.BigEndian.PutUint16(node[pos:pos+2], uint16(start))
	}
	return node
}

func buildCatalogKey(parent uint32, name string) []byte {
	nameRunes := []rune(name)
	body := make([]byte, 6+len(nameRunes)*2)
	binary.BigEndian.PutUint32(body[0:4], parent)
	binary.BigEndian.PutUint16(body[4:6], uint16(len(nameRunes)))
	for i, r := range nameRunes {
		base := 6 + i*2
		binary.BigEndian.PutUint16(body[base:base+2], uint16(r))
	}
	out := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(body)))
	copy(out[2:], body)
	return out
}

func buildFolderRecord(cnid uint32, valence uint32) []byte {
	r := make([]byte, 88)
	binary.BigEndian.PutUint16(r[0:2], catalogRecordFolder)
	binary.BigEndian.PutUint32(r[4:8], valence)
	binary.BigEndian.PutUint32(r[8:12], cnid)
	return r
}

func buildFileRecord(cnid uint32) []byte {
	r := make([]byte, 244)
	binary.BigEndian.PutUint16(r[0:2], catalogRecordFile)
	binary.BigEndian.PutUint32(r[8:12], cnid)
	binary.BigEndian.PutUint64(r[88:96], 1234)
	binary.BigEndian.PutUint32(r[88+12:88+16], 1)
	binary.BigEndian.PutUint32(r[88+16:88+20], 50)
	binary.BigEndian.PutUint32(r[88+20:88+24], 2)
	return r
}

func buildThreadRecord(recType uint16, parentCNID uint32, name string) []byte {
	nameRunes := []rune(name)
	r := make([]byte, 10+len(nameRunes)*2)
	binary.BigEndian.PutUint16(r[0:2], recType)
	binary.BigEndian.PutUint32(r[4:8], parentCNID)
	binary.BigEndian.PutUint16(r[8:10], uint16(len(nameRunes)))
	for i, rn := range nameRunes {
		base := 10 + i*2
		binary.BigEndian.PutUint16(r[base:base+2], uint16(rn))
	}
	return r
}

func u32be(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// setForwardLink writes the B-tree ForwardLink (bytes 0-3) into an existing node buffer.
func setForwardLink(node []byte, link uint32) {
	binary.BigEndian.PutUint32(node[0:4], link)
}

// ---------------------------------------------------------------------------
// Tests for HFS Unicode case-folding and leaf-chain iteration
// ---------------------------------------------------------------------------

func TestHFSUnicodeEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// ASCII case-insensitive (A→a via Table 1 row 0x40)
		{"Hello", "hello", true},
		{"HELLO", "hello", true},
		{"hello", "Hello", true},
		{"Hello", "World", false},
		// exact same
		{"test", "test", true},
		// Æ (U+00C6) folds to æ (U+00E6) — explicitly in Table 1 row 0xC0
		{"\u00C6", "\u00E6", true},
		// Ø (U+00D8) folds to ø (U+00F8) — Table 1 row 0xD0
		{"\u00D8", "\u00F8", true},
		// NOTE: HFS+ table does NOT fold Ä↔ä or Ö↔ö (both map to themselves)
		{"\u00C4", "\u00E4", false}, // Ä ≠ ä per Apple TN1150 table
		{"\u00D6", "\u00F6", false}, // Ö ≠ ö per Apple TN1150 table
		// Greek alpha: U+0391 Α → U+03B1 α (Table 3 row 0x90)
		{"\u0391", "\u03B1", true},
		// Cyrillic А (U+0410) → а (U+0430) via Table 4 row 0x10
		{"\u0410", "\u0430", true},
		// Fullwidth Latin (Table 10: FF21 A → FF41 a)
		{"\uFF21", "\uFF41", true},
		// Different chars
		{"abc", "abd", false},
		// Empty strings
		{"", "", true},
	}

	for _, tc := range cases {
		got := hfsUnicodeEqual(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("hfsUnicodeEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestWalkCatalogUsesLeafChain(t *testing.T) {
	// Build a 3-leaf image: node 2 → node 3 → node 4 via ForwardLink.
	// The tree traversal from root would normally only visit node 2 (if the
	// index only has one pointer). With the leaf-chain walker all three are visited.

	blockSize := uint32(4096)
	catalogStartBlock := uint32(2)
	nodeSize := uint16(512)
	totalNodes := uint32(5) // 0 header, 1 root-idx, 2 leaf, 3 leaf, 4 leaf

	img := make([]byte, int((catalogStartBlock+1)*blockSize))
	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], blockSize)
	binary.BigEndian.PutUint32(vh[44:48], 200)
	binary.BigEndian.PutUint32(vh[48:52], 120)
	binary.BigEndian.PutUint64(vh[272:280], uint64(blockSize))
	binary.BigEndian.PutUint32(vh[272+12:272+16], 1)
	binary.BigEndian.PutUint32(vh[272+16:272+20], catalogStartBlock)
	binary.BigEndian.PutUint32(vh[272+20:272+24], 1)

	treeBase := int(catalogStartBlock * blockSize)
	writeNode := func(nodeNum uint32, node []byte) {
		off := treeBase + int(nodeNum)*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}

	// Header with FirstLeafNode=2, LastLeafNode=4
	hdrRec := buildBTreeHeaderRecordBytes(nodeSize, totalNodes, 1)
	// override LastLeafNode to 4
	binary.BigEndian.PutUint32(hdrRec[14:18], 4)
	writeNode(0, makeNode(nodeSize, btreeNodeTypeHead, [][]byte{hdrRec}))

	// Index root: only pointers to leaves 2 and 3 (leaf 4 would be unreachable by tree walk)
	idxRec1 := append(buildCatalogKey(rootFolderCNID, ""), u32be(2)...)
	idxRec2 := append(buildCatalogKey(rootFolderCNID, "b"), u32be(3)...)
	writeNode(1, makeNode(nodeSize, btreeNodeTypeIdx, [][]byte{idxRec1, idxRec2}))

	// Leaf 2: root dir + "a" dir
	leaf2a := append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecord(rootFolderCNID, 2)...)
	leaf2b := append(buildCatalogKey(rootFolderCNID, "a"), buildFolderRecord(100, 0)...)
	node2 := makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leaf2a, leaf2b})
	setForwardLink(node2, 3)
	writeNode(2, node2)

	// Leaf 3: "b" dir
	leaf3 := append(buildCatalogKey(rootFolderCNID, "b"), buildFolderRecord(101, 0)...)
	node3 := makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leaf3})
	setForwardLink(node3, 4)
	writeNode(3, node3)

	// Leaf 4: "c" dir — NOT reachable via tree walk from the index above
	leaf4 := append(buildCatalogKey(rootFolderCNID, "c"), buildFolderRecord(102, 0)...)
	writeNode(4, makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leaf4}))

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	seen := map[uint32]bool{}
	if err := vol.WalkCatalog(func(r CatalogRecord) error {
		if r.CNID != 0 {
			seen[r.CNID] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}

	// All three directories must be seen, including CNID 102 from leaf 4
	// which is only reachable via the leaf-chain (ForwardLink), not tree recursion.
	for _, cnid := range []uint32{rootFolderCNID, 100, 101, 102} {
		if !seen[cnid] {
			t.Errorf("WalkCatalog did not visit CNID %d (leaf-chain traversal failed)", cnid)
		}
	}
}
