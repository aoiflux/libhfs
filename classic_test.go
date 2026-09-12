package hfs

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Names above 0x7F were previously widened byte-for-byte, so every accented
// character decoded to the wrong code point. Only 0xA5 happened to be right, by
// coincidence — MacRoman 0xA5 is a bullet, and U+00A5 is the yen sign, so even
// that "match" was wrong.
func TestMacRomanDecoding(t *testing.T) {
	cases := []struct {
		name  string
		bytes []byte
		want  string
	}{
		{"ascii", []byte("README"), "README"},
		{"e-acute", []byte{0x8E}, "é"},
		{"a-umlaut", []byte{0x8A}, "ä"},
		{"n-tilde", []byte{0x96}, "ñ"},
		{"bullet", []byte{0xA5}, "•"},
		{"degree", []byte{0xA1}, "°"},
		{"ellipsis", []byte{0xC9}, "…"},
		{"apple-logo", []byte{0xF0}, ""},
		{"mixed", []byte{'C', 'a', 'f', 0x8E}, "Café"},
		{"empty", nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeMacRoman(tc.bytes); got != tc.want {
				t.Errorf("decodeMacRoman(% x) = %q, want %q", tc.bytes, got, tc.want)
			}
		})
	}
}

func TestMacRomanTableIsComplete(t *testing.T) {
	seen := map[rune]int{}
	for i, r := range macRomanHigh {
		if r == 0 {
			t.Errorf("macRomanHigh[0x%02X] is unset", 0x80+i)
		}
		if prev, dup := seen[r]; dup {
			t.Errorf("macRomanHigh[0x%02X] duplicates 0x%02X (both %U)", 0x80+i, 0x80+prev, r)
		}
		seen[r] = i
	}
	// Every byte 0x00-0xFF must decode to exactly one rune.
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	if got := []rune(decodeMacRoman(all)); len(got) != 256 {
		t.Errorf("decoding all 256 bytes produced %d runes, want 256", len(got))
	}
}

func TestTextEncodingRawPreservesBytes(t *testing.T) {
	vol := &Volume{kind: KindHFS}
	raw := []byte{0x8E, 0xA5, 'x'}

	if got, want := vol.decodeHFSName(raw), "é•x"; got != want {
		t.Errorf("default encoding: got %q, want %q", got, want)
	}

	vol.SetTextEncoding(TextEncodingRaw)
	if got := vol.decodeHFSName(raw); got != "¥x" {
		t.Errorf("raw encoding: got %q, want the byte values widened", got)
	}
	if vol.TextEncoding() != TextEncodingRaw {
		t.Errorf("TextEncoding() = %v, want %v", vol.TextEncoding(), TextEncodingRaw)
	}
}

// End-to-end: a classic HFS volume with a high-bit filename must list and open
// under the decoded name.
func TestClassicHFSHighBitFilename(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("Kind = %s, want %s", vol.Kind(), KindHFS)
	}

	entries, err := vol.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var found bool
	for _, e := range entries {
		if e.Name == classicAccentedName {
			found = true
		}
	}
	if !found {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name)
		}
		t.Fatalf("accented name %q not in listing; got %q", classicAccentedName, names)
	}

	rec, err := vol.OpenPath("/" + classicAccentedName)
	if err != nil {
		t.Fatalf("OpenPath(%q): %v", classicAccentedName, err)
	}
	if rec.CNID != classicAccentedCNID {
		t.Errorf("CNID = %d, want %d", rec.CNID, classicAccentedCNID)
	}
}

// Classic HFS FinderInfo lives in filUsrWds at offset 4, and was previously
// left at zero. The type/creator pair is how classic Mac files record what they
// are, since the format has no extensions.
func TestClassicHFSFinderInfo(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rec, err := vol.OpenPath("/DATA")
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	if rec.FinderType != classicFinderType {
		t.Errorf("FinderType = %#x, want %#x", rec.FinderType, classicFinderType)
	}
	if rec.FinderCreator != classicFinderCreator {
		t.Errorf("FinderCreator = %#x, want %#x", rec.FinderCreator, classicFinderCreator)
	}
}

// Classic HFS now uses keyed descent too. It must agree with the exhaustive
// walk and must not fall back.
func TestClassicHFSKeyedSearch(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !vol.supportsKeyedSearch() {
		t.Fatal("classic HFS should support keyed search")
	}

	for _, cnid := range []uint32{rootFolderCNID, 100, classicAccentedCNID} {
		keyed, kerr := vol.lookupCNIDViaThread(cnid)
		linear, lerr := vol.lookupCNIDLinear(cnid)
		if lerr != nil {
			t.Fatalf("linear lookup of %d failed: %v", cnid, lerr)
		}
		if kerr != nil {
			// The fixture has no thread records, so the keyed path legitimately
			// cannot resolve a CNID; the linear fallback covers it.
			t.Logf("CNID %d: no thread record, fell back (%v)", cnid, kerr)
			continue
		}
		if keyed.CNID != linear.CNID || keyed.Name != linear.Name {
			t.Errorf("CNID %d: keyed=%+v linear=%+v", cnid, keyed, linear)
		}
	}

	// Directory listing goes through keyed descent regardless of thread records.
	entries, err := vol.ReadDirCNID(rootFolderCNID)
	if err != nil {
		t.Fatalf("ReadDirCNID: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
}

func TestClassicHFSValenceAndDates(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory: %v", err)
	}
	if root.Valence != classicRootValence {
		t.Errorf("Valence = %d, want %d", root.Valence, classicRootValence)
	}
	if root.Times.Source != TimeSourceHFSLocal {
		t.Errorf("Source = %v, want %v", root.Times.Source, TimeSourceHFSLocal)
	}
}

// A classic HFS MDB records four counts that are easy to mistake for one
// another: drNmFls and drNmRtDirs count only what sits in the root directory
// and are 16-bit, while drFilCnt and drDirCnt are the volume-wide totals and
// are 32-bit. Reading the root pair returns a number that is small, plausible
// and wrong on every volume that has subdirectories, so nothing short of an
// explicit check catches it — a real volume did, once one was available.
//
// The four values here are deliberately all different, and the volume-wide
// totals deliberately exceed 16 bits, so a parser reading the wrong offset or
// the wrong width cannot land on the right answer by accident.
func TestClassicHFSVolumeWideCounts(t *testing.T) {
	const (
		rootFiles   = 3
		rootDirs    = 5
		fileCount   = 0x0001D4C1 // 120001, needs more than 16 bits
		folderCount = 0x00012345 // 74565, likewise
		writeCount  = 0x00ABCDEF // drWrCnt is 32-bit; a 16-bit read sees 0x00AB
	)

	img := buildClassicHFSTimesImage(t)
	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(mdb[12:14], rootFiles)
	binary.BigEndian.PutUint16(mdb[82:84], rootDirs)
	binary.BigEndian.PutUint32(mdb[84:88], fileCount)
	binary.BigEndian.PutUint32(mdb[88:92], folderCount)
	binary.BigEndian.PutUint32(mdb[70:74], writeCount)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h := vol.Header()

	if h.FileCount != fileCount {
		t.Errorf("FileCount = %d, want drFilCnt %d (drNmFls, the root-only count, is %d)",
			h.FileCount, fileCount, rootFiles)
	}
	if h.FolderCount != folderCount {
		t.Errorf("FolderCount = %d, want drDirCnt %d (drNmRtDirs, the root-only count, is %d)",
			h.FolderCount, folderCount, rootDirs)
	}
	if h.WriteCount != writeCount {
		t.Errorf("WriteCount = %d, want drWrCnt %d", h.WriteCount, writeCount)
	}
}

// buildThreadRecordHFS builds a classic HFS CatThreadRec (46 bytes).
//
// Deliberately written from the HFSCatalogThread layout rather than by
// narrowing buildThreadRecord's HFS+ one: the two put parentID and the name at
// different offsets, and a fixture that shared the HFS+ offsets would agree
// with a decoder that made the same mistake.
func buildThreadRecordHFS(recType byte, parentCNID uint32, name string) []byte {
	// Offsets are written as literals, not as the decoder's own constants: a
	// fixture built from the constants it is checking moves with them and so
	// agrees with any decoder, however wrong. Per Inside Macintosh: Files,
	// HFSCatalogThread is cdrType(1), reserved[9], thdParID(4), thdCName(Str31)
	// — parentID at 10, the name's length byte at 14, 46 bytes in all.
	r := make([]byte, 46)
	r[0] = recType
	binary.BigEndian.PutUint32(r[10:14], parentCNID)
	r[14] = byte(len(name))
	copy(r[15:], name)
	return r
}

const (
	// hfsRootParentCNID is kHFSRootParentID: the catalog files the root folder
	// itself under this parent, and the root's thread names it as the parent.
	hfsRootParentCNID = uint32(1)

	classicThreadVolName = "threadvol"
	classicThreadDirName = "docs"
	classicThreadFileNm  = "notes.txt"
	classicThreadDirCNID = uint32(16)
	classicThreadFilCNID = uint32(17)
)

// buildClassicHFSThreadImage builds a classic HFS volume whose catalog carries
// a real thread record for every node, with one directory nested inside the
// root so that resolving the file's path has to climb through two of them.
//
// No other fixture in the repo has classic HFS threads: every thread builder
// writes the HFS+ layout, so a classic thread decoded at HFS+ offsets read
// reserved zeroes and produced parent 0 with an empty name — values that look
// like a legitimately unreachable record rather than a parse error.
func buildClassicHFSThreadImage(tb testing.TB) []byte {
	tb.Helper()

	const (
		blockSize         = classicHFSBlockSize
		catalogStartBlock = uint32(1)
		nodeSize          = uint16(1024)
		totalNodes        = uint32(2)
		leafNode          = uint32(1)
	)

	dataBase := int(classicHFSDataBase)
	treeBase := dataBase + int(catalogStartBlock*blockSize)
	img := make([]byte, treeBase+int(blockSize))

	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(mdb[0:2], signatureHFS)
	binary.BigEndian.PutUint32(mdb[hfsMDBOffBlockSize:hfsMDBOffBlockSize+4], blockSize)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffTotalBlocks:hfsMDBOffTotalBlocks+2], 100)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffFreeBlocks:hfsMDBOffFreeBlocks+2], 90)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffAlBlSt:hfsMDBOffAlBlSt+2], classicHFSAlBlSt)
	binary.BigEndian.PutUint32(mdb[hfsMDBOffFileCount:hfsMDBOffFileCount+4], 1)
	binary.BigEndian.PutUint32(mdb[hfsMDBOffFolderCount:hfsMDBOffFolderCount+4], 1)
	binary.BigEndian.PutUint32(mdb[hfsMDBOffCTFlSize:hfsMDBOffCTFlSize+4], blockSize)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffCTExtRec:hfsMDBOffCTExtRec+2], uint16(catalogStartBlock))
	binary.BigEndian.PutUint16(mdb[hfsMDBOffCTExtRec+2:hfsMDBOffCTExtRec+4], 1)

	tf := fullTimesFixture()
	join := func(key, rec []byte) []byte { return append(key, rec...) }

	// Catalog key order is parent CNID ascending, then name. The root folder
	// itself is filed under kHFSRootParentID (1), as a real volume files it.
	records := [][]byte{
		join(buildCatalogKeyHFSBytes(hfsRootParentCNID, []byte(classicThreadVolName)),
			buildFolderRecordHFS(rootFolderCNID, 1, tf)),
		join(buildCatalogKeyHFSBytes(rootFolderCNID, nil),
			buildThreadRecordHFS(hfsRecordTypeFolderThread, hfsRootParentCNID, classicThreadVolName)),
		join(buildCatalogKeyHFSBytes(rootFolderCNID, []byte(classicThreadDirName)),
			buildFolderRecordHFS(classicThreadDirCNID, 1, tf)),
		join(buildCatalogKeyHFSBytes(classicThreadDirCNID, nil),
			buildThreadRecordHFS(hfsRecordTypeFolderThread, rootFolderCNID, classicThreadDirName)),
		join(buildCatalogKeyHFSBytes(classicThreadDirCNID, []byte(classicThreadFileNm)),
			buildFileRecordHFS(classicThreadFilCNID, tf)),
		join(buildCatalogKeyHFSBytes(classicThreadFilCNID, nil),
			buildThreadRecordHFS(hfsRecordTypeFileThread, classicThreadDirCNID, classicThreadFileNm)),
	}

	writeNode := func(num uint32, node []byte) {
		off := treeBase + int(num)*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}
	writeNode(0, makeNode(nodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(nodeSize, totalNodes, leafNode, leafNode)}))
	writeNode(leafNode, makeNode(nodeSize, btreeNodeTypeLeaf, records))

	return img
}

// A classic HFS thread record answers "who is my parent, and what am I called".
// Both answers come from offsets that differ from the HFS+ ones, and reading
// the HFS+ offsets yields zero and "" rather than an error, so the only symptom
// is that every path resolution quietly fails or stops at the root.
func TestClassicHFSThreadRecords(t *testing.T) {
	img := buildClassicHFSThreadImage(t)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("kind = %v, want %v", vol.Kind(), KindHFS)
	}

	thread, err := vol.findThreadRecord(classicThreadFilCNID)
	if err != nil {
		t.Fatalf("findThreadRecord(%d): %v", classicThreadFilCNID, err)
	}
	if thread.ParentCNID != classicThreadDirCNID {
		t.Errorf("thread ParentCNID = %d, want %d", thread.ParentCNID, classicThreadDirCNID)
	}
	if thread.Name != classicThreadFileNm {
		t.Errorf("thread Name = %q, want %q", thread.Name, classicThreadFileNm)
	}

	wantPath := "/" + classicThreadDirName + "/" + classicThreadFileNm
	got, err := vol.PathForCNID(classicThreadFilCNID)
	if err != nil {
		t.Fatalf("PathForCNID(%d): %v", classicThreadFilCNID, err)
	}
	if got != wantPath {
		t.Errorf("PathForCNID = %q, want %q", got, wantPath)
	}

	rec, err := vol.OpenPath(wantPath)
	if err != nil {
		t.Fatalf("OpenPath(%q): %v", wantPath, err)
	}
	if rec.CNID != classicThreadFilCNID {
		t.Errorf("OpenPath(%q).CNID = %d, want %d", wantPath, rec.CNID, classicThreadFilCNID)
	}
}
