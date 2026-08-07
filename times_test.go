package hfs

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// Distinct, non-round raw date words so a decoder reading the wrong offset
// cannot accidentally produce a passing value.
const (
	rawCreated    = hfsEpochDeltaSeconds + 1_600_000_000
	rawContentMod = hfsEpochDeltaSeconds + 1_600_000_100
	rawAttrMod    = hfsEpochDeltaSeconds + 1_600_000_200
	rawAccessed   = hfsEpochDeltaSeconds + 1_600_000_300
	rawBackup     = hfsEpochDeltaSeconds + 1_600_000_400
)

func wantTime(raw uint32) time.Time {
	return time.Unix(int64(raw)-int64(hfsEpochDeltaSeconds), 0).UTC()
}

func assertTime(t *testing.T, label string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

// ---------------------------------------------------------------------------
// HFS+
// ---------------------------------------------------------------------------

func TestCatalogTimesHFSPlusFile(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildTimesTestImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	rec, err := vol.OpenPath("/file.txt")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}

	assertTime(t, "Created", rec.Times.Created, wantTime(rawCreated))
	assertTime(t, "ContentModified", rec.Times.ContentModified, wantTime(rawContentMod))
	assertTime(t, "AttrModified", rec.Times.AttrModified, wantTime(rawAttrMod))
	assertTime(t, "Accessed", rec.Times.Accessed, wantTime(rawAccessed))
	assertTime(t, "Backup", rec.Times.Backup, wantTime(rawBackup))

	if rec.Times.Source != TimeSourceHFSPlusGMT {
		t.Errorf("Source = %v, want TimeSourceHFSPlusGMT", rec.Times.Source)
	}
	if rec.Times.IsZero() {
		t.Error("IsZero() = true for a fully populated set")
	}
}

func TestCatalogTimesHFSPlusFolder(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildTimesTestImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// The root folder record carries the same five dates at the same offsets.
	rec, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}

	assertTime(t, "Created", rec.Times.Created, wantTime(rawCreated))
	assertTime(t, "ContentModified", rec.Times.ContentModified, wantTime(rawContentMod))
	assertTime(t, "AttrModified", rec.Times.AttrModified, wantTime(rawAttrMod))
	assertTime(t, "Accessed", rec.Times.Accessed, wantTime(rawAccessed))
	assertTime(t, "Backup", rec.Times.Backup, wantTime(rawBackup))

	if rec.Times.Source != TimeSourceHFSPlusGMT {
		t.Errorf("Source = %v, want TimeSourceHFSPlusGMT", rec.Times.Source)
	}
}

func TestGetTimesAccessors(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildTimesTestImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	byCNID, err := vol.GetTimes(100)
	if err != nil {
		t.Fatalf("GetTimes failed: %v", err)
	}
	byPath, err := vol.GetTimesByPath("/file.txt")
	if err != nil {
		t.Fatalf("GetTimesByPath failed: %v", err)
	}

	assertTime(t, "byCNID.Created", byCNID.Created, wantTime(rawCreated))
	assertTime(t, "byPath.Created", byPath.Created, byCNID.Created)
	assertTime(t, "byPath.ContentModified", byPath.ContentModified, byCNID.ContentModified)
	assertTime(t, "byPath.AttrModified", byPath.AttrModified, byCNID.AttrModified)
	assertTime(t, "byPath.Accessed", byPath.Accessed, byCNID.Accessed)
	assertTime(t, "byPath.Backup", byPath.Backup, byCNID.Backup)
	if byPath.Source != byCNID.Source {
		t.Errorf("Source: byPath = %v, byCNID = %v", byPath.Source, byCNID.Source)
	}

	if _, err := vol.GetTimes(999999); err == nil {
		t.Error("GetTimes on a missing CNID returned nil error")
	}
	if _, err := vol.GetTimesByPath("/nope"); err == nil {
		t.Error("GetTimesByPath on a missing path returned nil error")
	}
}

// A raw date word of zero means "never set". It must not decode to 1904, and
// it must not decode to the Unix epoch — a caller building a timeline has to
// be able to tell an absent timestamp from a real one.
func TestCatalogTimesUnsetFieldsAreZero(t *testing.T) {
	img := buildTimesTestImageWith(t, timesFixture{
		created: rawCreated,
		// every other field left at zero on disk
	})

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	rec, err := vol.OpenPath("/file.txt")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}

	assertTime(t, "Created", rec.Times.Created, wantTime(rawCreated))
	for _, tc := range []struct {
		name string
		got  time.Time
	}{
		{"ContentModified", rec.Times.ContentModified},
		{"AttrModified", rec.Times.AttrModified},
		{"Accessed", rec.Times.Accessed},
		{"Backup", rec.Times.Backup},
	} {
		if !tc.got.IsZero() {
			t.Errorf("%s = %v, want zero time.Time", tc.name, tc.got)
		}
	}
}

// Pre-1970 dates are real evidence on old media. They must survive as negative
// Unix times rather than being clamped to the epoch.
func TestCatalogTimesPreUnixEpochPreserved(t *testing.T) {
	const raw = uint32(1) // one second after the 1904 HFS epoch

	img := buildTimesTestImageWith(t, timesFixture{created: raw})
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	rec, err := vol.OpenPath("/file.txt")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}

	want := time.Date(1904, time.January, 1, 0, 0, 1, 0, time.UTC)
	assertTime(t, "Created", rec.Times.Created, want)
	if rec.Times.Created.Unix() >= 0 {
		t.Errorf("Created.Unix() = %d, want negative", rec.Times.Created.Unix())
	}
}

// Thread records carry no dates at all; they must not claim a TimeSource.
func TestCatalogTimesThreadRecordsHaveNoSource(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildCatalogThreadedTestImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	var checked int
	err = vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Type != CatalogRecordFolderThread && r.Type != CatalogRecordFileThread {
			return nil
		}
		checked++
		if r.Times.Source != TimeSourceUnknown {
			t.Errorf("thread record %q: Source = %v, want TimeSourceUnknown", r.Name, r.Times.Source)
		}
		if !r.Times.IsZero() {
			t.Errorf("thread record %q: Times = %+v, want all zero", r.Name, r.Times)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog failed: %v", err)
	}
	if checked == 0 {
		t.Fatal("no thread records seen; fixture is not exercising the case")
	}
}

// ---------------------------------------------------------------------------
// Classic HFS
// ---------------------------------------------------------------------------

func TestCatalogTimesClassicHFSFile(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("Kind = %s, want %s", vol.Kind(), KindHFS)
	}

	rec, err := vol.OpenPath("/DATA")
	if err != nil {
		t.Fatalf("OpenPath failed: %v", err)
	}

	assertTime(t, "Created", rec.Times.Created, wantTime(rawCreated))
	assertTime(t, "ContentModified", rec.Times.ContentModified, wantTime(rawContentMod))
	assertTime(t, "Backup", rec.Times.Backup, wantTime(rawBackup))

	// Classic HFS records neither of these.
	if !rec.Times.Accessed.IsZero() {
		t.Errorf("Accessed = %v, want zero (classic HFS has no access date)", rec.Times.Accessed)
	}
	if !rec.Times.AttrModified.IsZero() {
		t.Errorf("AttrModified = %v, want zero (classic HFS has no attr-mod date)", rec.Times.AttrModified)
	}
	if rec.Times.Source != TimeSourceHFSLocal {
		t.Errorf("Source = %v, want TimeSourceHFSLocal", rec.Times.Source)
	}
}

func TestCatalogTimesClassicHFSFolder(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	rec, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}

	assertTime(t, "Created", rec.Times.Created, wantTime(rawCreated))
	assertTime(t, "ContentModified", rec.Times.ContentModified, wantTime(rawContentMod))
	assertTime(t, "Backup", rec.Times.Backup, wantTime(rawBackup))
	if rec.Times.Source != TimeSourceHFSLocal {
		t.Errorf("Source = %v, want TimeSourceHFSLocal", rec.Times.Source)
	}
}

// Regression for the CatDirRec offset bug: dirVal lives at offset 4, not 10.
// Offset 10 is dirCrDat, so the old code reported the top half of the creation
// date as the valence.
func TestClassicHFSFolderValenceOffset(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSTimesImage(t)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	rec, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory failed: %v", err)
	}
	if rec.Valence != classicRootValence {
		t.Fatalf("Valence = %d, want %d (reading dirCrDat instead of dirVal yields %d)",
			rec.Valence, classicRootValence, uint32(rawCreated>>16))
	}
}

// ---------------------------------------------------------------------------
// Volume-level dates must be untouched by the catalog work.
// ---------------------------------------------------------------------------

func TestVolumeHeaderTimesUnchanged(t *testing.T) {
	img := make([]byte, volumeHeaderOffset+volumeHeaderSize)
	hdr := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(hdr[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(hdr[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(hdr[40:44], 4096)
	binary.BigEndian.PutUint32(hdr[44:48], 100)
	binary.BigEndian.PutUint32(hdr[16:20], hfsEpochDeltaSeconds+123) // CreateTime
	binary.BigEndian.PutUint32(hdr[20:24], 0)                        // ModifyTime, unset

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	got := vol.Header()

	if want := time.Unix(123, 0).UTC(); !got.CreateTime.Equal(want) {
		t.Errorf("CreateTime = %v, want %v", got.CreateTime, want)
	}
	// Unset volume fields clamp to the Unix epoch. That behaviour is
	// deliberately retained so existing callers see no change.
	if want := time.Unix(0, 0).UTC(); !got.ModifyTime.Equal(want) {
		t.Errorf("ModifyTime = %v, want %v (clamping behaviour must be preserved)", got.ModifyTime, want)
	}
}

func TestHFSCatalogTimeHelper(t *testing.T) {
	if got := hfsCatalogTime(0); !got.IsZero() {
		t.Errorf("hfsCatalogTime(0) = %v, want zero time.Time", got)
	}
	if got, want := hfsCatalogTime(hfsEpochDeltaSeconds), time.Unix(0, 0).UTC(); !got.Equal(want) {
		t.Errorf("hfsCatalogTime(epoch delta) = %v, want %v", got, want)
	}
	if got := hfsCatalogTime(hfsEpochDeltaSeconds - 1); got.Unix() != -1 {
		t.Errorf("hfsCatalogTime(delta-1).Unix() = %d, want -1", got.Unix())
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type timesFixture struct {
	created    uint32
	contentMod uint32
	attrMod    uint32
	accessed   uint32
	backup     uint32
}

func fullTimesFixture() timesFixture {
	return timesFixture{
		created:    rawCreated,
		contentMod: rawContentMod,
		attrMod:    rawAttrMod,
		accessed:   rawAccessed,
		backup:     rawBackup,
	}
}

func buildTimesTestImage(t *testing.T) []byte {
	t.Helper()
	return buildTimesTestImageWith(t, fullTimesFixture())
}

// buildTimesTestImageWith lays out a minimal HFS+ volume holding a single leaf
// node with the root folder and one file, both stamped with tf.
func buildTimesTestImageWith(t *testing.T, tf timesFixture) []byte {
	t.Helper()

	const (
		blockSize         = uint32(4096)
		catalogStartBlock = uint32(2)
		nodeSize          = uint16(512)
		totalNodes        = uint32(2) // 0 header, 1 leaf
		leafNode          = uint32(1)
	)

	img := make([]byte, int((catalogStartBlock+1)*blockSize))
	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], blockSize)
	binary.BigEndian.PutUint32(vh[44:48], 200)
	binary.BigEndian.PutUint32(vh[48:52], 120)

	// Catalog fork: one allocation block at catalogStartBlock.
	binary.BigEndian.PutUint64(vh[272:280], uint64(blockSize))
	binary.BigEndian.PutUint32(vh[272+12:272+16], 1)
	binary.BigEndian.PutUint32(vh[272+16:272+20], catalogStartBlock)
	binary.BigEndian.PutUint32(vh[272+20:272+24], 1)

	treeBase := int(catalogStartBlock * blockSize)
	writeNode := func(num uint32, node []byte) {
		off := treeBase + int(num)*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}

	writeNode(0, makeNode(nodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(nodeSize, totalNodes, leafNode, leafNode)}))

	leafRoot := append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecordWithTimes(rootFolderCNID, 1, tf)...)
	leafFile := append(buildCatalogKey(rootFolderCNID, "file.txt"), buildFileRecordWithTimes(100, tf)...)
	writeNode(leafNode, makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leafRoot, leafFile}))

	return img
}

// buildBTreeHeaderRecordBytesAt is buildBTreeHeaderRecordBytes with the leaf
// chain endpoints made explicit, for fixtures that use a single leaf node.
func buildBTreeHeaderRecordBytesAt(nodeSize uint16, totalNodes, rootNode, leafNode uint32) []byte {
	r := buildBTreeHeaderRecordBytes(nodeSize, totalNodes, rootNode)
	binary.BigEndian.PutUint32(r[10:14], leafNode) // FirstLeafNode
	binary.BigEndian.PutUint32(r[14:18], leafNode) // LastLeafNode
	return r
}

func putTimesHFSPlus(r []byte, tf timesFixture) {
	binary.BigEndian.PutUint32(r[12:16], tf.created)
	binary.BigEndian.PutUint32(r[16:20], tf.contentMod)
	binary.BigEndian.PutUint32(r[20:24], tf.attrMod)
	binary.BigEndian.PutUint32(r[24:28], tf.accessed)
	binary.BigEndian.PutUint32(r[28:32], tf.backup)
}

func buildFolderRecordWithTimes(cnid uint32, valence uint32, tf timesFixture) []byte {
	r := buildFolderRecord(cnid, valence)
	putTimesHFSPlus(r, tf)
	return r
}

func buildFileRecordWithTimes(cnid uint32, tf timesFixture) []byte {
	r := buildFileRecord(cnid)
	putTimesHFSPlus(r, tf)
	return r
}

// ---------------------------------------------------------------------------
// Classic HFS fixture
// ---------------------------------------------------------------------------

const (
	classicRootValence = uint32(2)

	// A name with a high-bit MacRoman byte: "CAF" + 0x8E, which decodes to
	// "CAFé". Widening the byte instead would yield U+008E, an unprintable
	// control character.
	classicAccentedName  = "CAFé"
	classicAccentedCNID  = uint32(101)
	classicFinderType    = uint32(0x54455854) // "TEXT"
	classicFinderCreator = uint32(0x4d505320) // "MPS "
)

// classicAccentedBytes is the on-disk form of classicAccentedName.
var classicAccentedBytes = []byte{'C', 'A', 'F', 0x8E}

// buildClassicHFSTimesImage lays out a minimal classic HFS volume: an MDB, and
// a catalog B-tree of one header node plus one leaf node holding the root
// folder and a single file.
func buildClassicHFSTimesImage(t testing.TB) []byte {
	t.Helper()

	const (
		blockSize         = uint32(4096)
		alBlSt            = uint16(4) // allocation blocks start at byte 2048
		catalogStartBlock = uint32(1)
		nodeSize          = uint16(512)
		totalNodes        = uint32(2)
		leafNode          = uint32(1)
	)

	dataBase := int(alBlSt) * 512
	treeBase := dataBase + int(catalogStartBlock*blockSize)
	img := make([]byte, treeBase+int(blockSize))

	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(mdb[0:2], signatureHFS)
	binary.BigEndian.PutUint32(mdb[hfsMDBOffBlockSize:hfsMDBOffBlockSize+4], blockSize)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffTotalBlocks:hfsMDBOffTotalBlocks+2], 100)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffFreeBlocks:hfsMDBOffFreeBlocks+2], 90)
	binary.BigEndian.PutUint16(mdb[28:30], alBlSt)
	// Catalog file: one allocation block starting at catalogStartBlock.
	binary.BigEndian.PutUint32(mdb[hfsMDBOffCTFlSize:hfsMDBOffCTFlSize+4], blockSize)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffCTExtRec:hfsMDBOffCTExtRec+2], uint16(catalogStartBlock))
	binary.BigEndian.PutUint16(mdb[hfsMDBOffCTExtRec+2:hfsMDBOffCTExtRec+4], 1)

	writeNode := func(num uint32, node []byte) {
		off := treeBase + int(num)*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}

	writeNode(0, makeNode(nodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(nodeSize, totalNodes, leafNode, leafNode)}))

	tf := fullTimesFixture()
	leafRoot := append(buildCatalogKeyHFSBytes(rootFolderCNID, nil),
		buildFolderRecordHFS(rootFolderCNID, classicRootValence, tf)...)
	leafFile := append(buildCatalogKeyHFSBytes(rootFolderCNID, []byte("DATA")),
		buildFileRecordHFS(100, tf)...)
	// A high-bit MacRoman name, to prove decoding rather than byte-widening.
	leafAccent := append(buildCatalogKeyHFSBytes(rootFolderCNID, classicAccentedBytes),
		buildFileRecordHFS(classicAccentedCNID, tf)...)

	// Keys must be in on-disk byte order: "" first, then "CAF\x8e" ('C' = 0x43),
	// then "DATA" ('D' = 0x44).
	writeNode(leafNode, makeNode(nodeSize, btreeNodeTypeLeaf,
		[][]byte{leafRoot, leafAccent, leafFile}))

	return img
}

// buildCatalogKeyHFS builds a classic HFS CatKeyRec: a length byte, a reserved
// byte, the parent CNID, then a Str31 name. The record is padded to an even
// length, matching what parseCatalogKeyHFS expects to skip.
func buildCatalogKeyHFS(parent uint32, name string) []byte {
	return buildCatalogKeyHFSBytes(parent, []byte(name))
}

// buildCatalogKeyHFSBytes takes the name as raw bytes, so a fixture can hold a
// high-bit MacRoman name that is not valid UTF-8 as a Go string.
func buildCatalogKeyHFSBytes(parent uint32, nameBytes []byte) []byte {
	keyLen := 6 + len(nameBytes) // resrv1 + parID + nameLen byte + name
	total := keyLen + 1
	if total%2 != 0 {
		total++
	}

	out := make([]byte, total)
	out[0] = byte(keyLen)
	// out[1] is ckrResrv1, left zero.
	binary.BigEndian.PutUint32(out[2:6], parent)
	out[6] = byte(len(nameBytes))
	copy(out[7:], nameBytes)
	return out
}

// buildFolderRecordHFS builds a classic HFS CatDirRec (70 bytes).
func buildFolderRecordHFS(cnid uint32, valence uint32, tf timesFixture) []byte {
	r := make([]byte, 70)
	r[0] = 0x01
	binary.BigEndian.PutUint16(r[4:6], uint16(valence)) // dirVal
	binary.BigEndian.PutUint32(r[6:10], cnid)           // dirDirID
	binary.BigEndian.PutUint32(r[10:14], tf.created)    // dirCrDat
	binary.BigEndian.PutUint32(r[14:18], tf.contentMod) // dirMdDat
	binary.BigEndian.PutUint32(r[18:22], tf.backup)     // dirBkDat
	return r
}

// buildFileRecordHFS builds a classic HFS CatFilRec (102 bytes).
func buildFileRecordHFS(cnid uint32, tf timesFixture) []byte {
	r := make([]byte, 102)
	r[0] = 0x02
	binary.BigEndian.PutUint32(r[4:8], classicFinderType)     // filUsrWds.fdType
	binary.BigEndian.PutUint32(r[8:12], classicFinderCreator) // filUsrWds.fdCreator
	binary.BigEndian.PutUint32(r[20:24], cnid)                // filFlNum
	binary.BigEndian.PutUint32(r[26:30], 1234)                // filLgLen
	binary.BigEndian.PutUint32(r[30:34], 4096)                // filPyLen
	binary.BigEndian.PutUint32(r[44:48], tf.created)          // filCrDat
	binary.BigEndian.PutUint32(r[48:52], tf.contentMod)       // filMdDat
	binary.BigEndian.PutUint32(r[52:56], tf.backup)           // filBkDat
	binary.BigEndian.PutUint16(r[74:76], 5)                   // filExtRec[0].startBlock
	binary.BigEndian.PutUint16(r[76:78], 1)                   // filExtRec[0].blockCount
	return r
}
