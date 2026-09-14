package libhfs

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// Fixture layout. The corpus image has an empty attributes tree, so extended
// attributes can only be exercised synthetically.
const (
	xaBlockSize = uint32(4096)
	xaNodeSize  = uint16(1024)

	xaCatalogBlock = uint32(2)
	xaAttrBlock    = uint32(4)

	xaInlineCNID = uint32(100) // one inline attribute
	xaForkCNID   = uint32(101) // one fork-backed attribute, 8 extents + 1 extension
	xaManyCNID   = uint32(102) // three inline attributes
	xaNoneCNID   = uint32(103) // no attributes

	xaInlineName  = "user.small"
	xaInlineValue = "hello extended attributes"
	xaForkName    = "user.big"
)

// The fork-backed attribute's nine extents, deliberately non-contiguous so a
// reader that assumes adjacency produces wrong bytes rather than an error.
var xaForkExtents = []ExtentDescriptor{
	{StartBlock: 10, BlockCount: 1},
	{StartBlock: 12, BlockCount: 1},
	{StartBlock: 14, BlockCount: 1},
	{StartBlock: 16, BlockCount: 1},
	{StartBlock: 18, BlockCount: 1},
	{StartBlock: 20, BlockCount: 1},
	{StartBlock: 22, BlockCount: 1},
	{StartBlock: 24, BlockCount: 1}, // eighth: fills the fork record
	{StartBlock: 26, BlockCount: 1}, // ninth: needs an extension record
}

// xaForkSize is deliberately not a whole number of blocks, so a reader that
// returns whole extents rather than honouring the logical size is caught.
var xaForkSize = uint64(len(xaForkExtents))*uint64(xaBlockSize) - 1234

func TestXAttrListInline(t *testing.T) {
	vol := openXAttrFixture(t)

	attrs, err := vol.ListXAttrs(xaInlineCNID)
	if err != nil {
		t.Fatalf("ListXAttrs: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("got %d attributes, want 1: %+v", len(attrs), attrs)
	}
	a := attrs[0]
	if a.Name != xaInlineName {
		t.Errorf("Name = %q, want %q", a.Name, xaInlineName)
	}
	if a.Storage != XAttrInline {
		t.Errorf("Storage = %v, want inline", a.Storage)
	}
	if a.Size != uint64(len(xaInlineValue)) {
		t.Errorf("Size = %d, want %d", a.Size, len(xaInlineValue))
	}
	if a.Extents != nil {
		t.Errorf("inline attribute has extents: %+v", a.Extents)
	}
	if a.CNID != xaInlineCNID {
		t.Errorf("CNID = %d, want %d", a.CNID, xaInlineCNID)
	}
}

func TestXAttrReadInline(t *testing.T) {
	vol := openXAttrFixture(t)

	got, err := vol.ReadXAttr(xaInlineCNID, xaInlineName)
	if err != nil {
		t.Fatalf("ReadXAttr: %v", err)
	}
	if string(got) != xaInlineValue {
		t.Errorf("value = %q, want %q", got, xaInlineValue)
	}

	if _, err := vol.ReadXAttr(xaInlineCNID, "no.such.attr"); err == nil {
		t.Error("ReadXAttr on a missing name returned nil error")
	}
}

// A fork-backed attribute whose value spans nine extents needs the ninth
// stitched in from a 0x30 extension record. Dropping it silently truncates the
// value.
func TestXAttrForkBackedWithExtension(t *testing.T) {
	vol := openXAttrFixture(t)

	attrs, err := vol.ListXAttrs(xaForkCNID)
	if err != nil {
		t.Fatalf("ListXAttrs: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("got %d attributes, want 1: %+v", len(attrs), attrs)
	}
	a := attrs[0]

	if a.Storage != XAttrFork {
		t.Fatalf("Storage = %v, want fork", a.Storage)
	}
	if a.Size != xaForkSize {
		t.Errorf("Size = %d, want %d", a.Size, xaForkSize)
	}
	if len(a.Extents) != len(xaForkExtents) {
		t.Fatalf("got %d extents, want %d (the extension record was not stitched in): %+v",
			len(a.Extents), len(xaForkExtents), a.Extents)
	}
	for i, e := range a.Extents {
		if e != xaForkExtents[i] {
			t.Errorf("extent %d = %+v, want %+v", i, e, xaForkExtents[i])
		}
	}
}

// Reading the fork-backed value must concatenate the extents in order and stop
// at the logical size.
func TestXAttrReadForkBacked(t *testing.T) {
	vol := openXAttrFixture(t)

	got, err := vol.ReadXAttr(xaForkCNID, xaForkName)
	if err != nil {
		t.Fatalf("ReadXAttr: %v", err)
	}
	if uint64(len(got)) != xaForkSize {
		t.Fatalf("read %d bytes, want %d", len(got), xaForkSize)
	}

	// Each extent's block was filled with its own marker byte, so the value
	// must read as nine runs in extent order.
	for i := range xaForkExtents {
		off := i * int(xaBlockSize)
		if off >= len(got) {
			break
		}
		want := xaMarkerByte(i)
		if got[off] != want {
			t.Fatalf("byte at offset %d (extent %d) = %#x, want %#x; extents are out of order or misread",
				off, i, got[off], want)
		}
	}
}

func TestXAttrOpenStreaming(t *testing.T) {
	vol := openXAttrFixture(t)

	fh, err := vol.OpenXAttr(xaForkCNID, xaForkName)
	if err != nil {
		t.Fatalf("OpenXAttr: %v", err)
	}
	if fh.Size() != int64(xaForkSize) {
		t.Errorf("Size() = %d, want %d", fh.Size(), xaForkSize)
	}

	// Read across an extent boundary.
	buf := make([]byte, 16)
	at := int64(xaBlockSize) - 8
	if _, err := fh.ReadAt(buf, at); err != nil {
		t.Fatalf("ReadAt across an extent boundary: %v", err)
	}
	for i := range 8 {
		if buf[i] != xaMarkerByte(0) {
			t.Errorf("byte %d before the boundary = %#x, want %#x", i, buf[i], xaMarkerByte(0))
		}
		if buf[8+i] != xaMarkerByte(1) {
			t.Errorf("byte %d after the boundary = %#x, want %#x", i, buf[8+i], xaMarkerByte(1))
		}
	}

	// An inline attribute also opens, served from memory.
	ih, err := vol.OpenXAttr(xaInlineCNID, xaInlineName)
	if err != nil {
		t.Fatalf("OpenXAttr inline: %v", err)
	}
	data, err := ih.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll inline: %v", err)
	}
	if string(data) != xaInlineValue {
		t.Errorf("inline value = %q, want %q", data, xaInlineValue)
	}
}

func TestXAttrMultipleAndNone(t *testing.T) {
	vol := openXAttrFixture(t)

	attrs, err := vol.ListXAttrs(xaManyCNID)
	if err != nil {
		t.Fatalf("ListXAttrs: %v", err)
	}
	want := []string{"a.attr", "b.attr", "c.attr"}
	if len(attrs) != len(want) {
		t.Fatalf("got %d attributes, want %d: %+v", len(attrs), len(want), attrs)
	}
	for i, a := range attrs {
		if a.Name != want[i] {
			t.Errorf("attribute %d = %q, want %q (ordering)", i, a.Name, want[i])
		}
	}

	none, err := vol.ListXAttrs(xaNoneCNID)
	if err != nil {
		t.Fatalf("ListXAttrs on a node with none: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("node with no attributes returned %d: %+v", len(none), none)
	}
}

// A volume with no attributes file must report no attributes, not an error.
func TestXAttrVolumeWithoutAttributesFile(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildValidCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	attrs, err := vol.ListXAttrs(validTreeFileCNID(0, 0))
	if err != nil {
		t.Fatalf("ListXAttrs on a volume with no attributes file: %v", err)
	}
	if len(attrs) != 0 {
		t.Errorf("got %d attributes, want 0", len(attrs))
	}
	if err := vol.WalkXAttrs(func(XAttr) error {
		t.Error("WalkXAttrs yielded an attribute on a volume with no attributes file")
		return nil
	}); err != nil {
		t.Errorf("WalkXAttrs: %v", err)
	}
}

func TestWalkXAttrs(t *testing.T) {
	vol := openXAttrFixture(t)

	seen := map[uint32][]string{}
	var forkAttr *XAttr
	err := vol.WalkXAttrs(func(a XAttr) error {
		seen[a.CNID] = append(seen[a.CNID], a.Name)
		if a.Storage == XAttrFork {
			cp := a
			forkAttr = &cp
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkXAttrs: %v", err)
	}

	if got := len(seen[xaManyCNID]); got != 3 {
		t.Errorf("cnid %d has %d attributes in the walk, want 3", xaManyCNID, got)
	}
	if got := len(seen[xaInlineCNID]); got != 1 {
		t.Errorf("cnid %d has %d attributes in the walk, want 1", xaInlineCNID, got)
	}
	if _, ok := seen[xaNoneCNID]; ok {
		t.Errorf("cnid %d has no attributes but appeared in the walk", xaNoneCNID)
	}

	// The walk must stitch extension records too, not just ListXAttrs.
	if forkAttr == nil {
		t.Fatal("walk did not yield the fork-backed attribute")
	}
	if len(forkAttr.Extents) != len(xaForkExtents) {
		t.Errorf("walked fork attribute has %d extents, want %d", len(forkAttr.Extents), len(xaForkExtents))
	}
}

func TestWalkXAttrsStopsOnCallbackError(t *testing.T) {
	vol := openXAttrFixture(t)

	var seen int
	err := vol.WalkXAttrs(func(XAttr) error {
		seen++
		return ErrNotFound
	})
	if err != ErrNotFound {
		t.Fatalf("WalkXAttrs returned %v, want the callback's error", err)
	}
	if seen != 1 {
		t.Errorf("callback ran %d times after erroring, want 1", seen)
	}
}

func TestReadXAttrRespectsMaxAlloc(t *testing.T) {
	vol := openXAttrFixture(t)
	vol.SetMaxAlloc(8)

	if _, err := vol.ReadXAttr(xaForkCNID, xaForkName); err == nil {
		t.Error("fork-backed ReadXAttr ignored the allocation cap")
	} else if !isSizeLimit(err) {
		t.Errorf("got %v, want ErrSizeLimit", err)
	}
	if _, err := vol.ReadXAttr(xaInlineCNID, xaInlineName); err == nil {
		t.Error("inline ReadXAttr ignored the allocation cap")
	} else if !isSizeLimit(err) {
		t.Errorf("got %v, want ErrSizeLimit", err)
	}
}

// ---------------------------------------------------------------------------
// Corpus
// ---------------------------------------------------------------------------

func TestCorpusXAttrsConsistent(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	var total int
	byStorage := map[XAttrStorage]int{}
	names := map[string]int{}

	err := vol.WalkXAttrs(func(a XAttr) error {
		total++
		byStorage[a.Storage]++
		names[a.Name]++
		if a.Storage == XAttrFork && len(a.Extents) == 0 && a.Size > 0 {
			t.Errorf("fork-backed attribute %q on cnid %d has no extents", a.Name, a.CNID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkXAttrs: %v", err)
	}
	t.Logf("%d attributes (%d inline, %d fork) across %d distinct names",
		total, byStorage[XAttrInline], byStorage[XAttrFork], len(names))
	for n, c := range names {
		t.Logf("   %-40s %d", n, c)
	}

	// Whatever the walk found must also be reachable per-file.
	if total > 0 {
		var perFile int
		_ = vol.WalkCatalog(func(r CatalogRecord) error {
			if r.Type != CatalogRecordFile && r.Type != CatalogRecordFolder {
				return nil
			}
			attrs, err := vol.ListXAttrs(r.CNID)
			if err != nil {
				t.Errorf("ListXAttrs(%d): %v", r.CNID, err)
				return nil
			}
			perFile += len(attrs)
			return nil
		})
		if perFile != total {
			t.Errorf("WalkXAttrs found %d attributes, per-file listing found %d", total, perFile)
		}
	}
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

func xaMarkerByte(extentIndex int) byte { return byte(0xA0 + extentIndex) }

func openXAttrFixture(t *testing.T) *Volume {
	t.Helper()
	vol, err := Open(bytes.NewReader(buildXAttrImage(t)))
	if err != nil {
		t.Fatalf("Open xattr fixture: %v", err)
	}
	return vol
}

// buildXAttrImage lays out an HFS+ volume with both a catalog and a populated
// attributes B-tree.
func buildXAttrImage(t testing.TB) []byte {
	t.Helper()

	lastBlock := uint32(0)
	for _, e := range xaForkExtents {
		if end := e.StartBlock + e.BlockCount; end > lastBlock {
			lastBlock = end
		}
	}
	img := make([]byte, int(lastBlock+1)*int(xaBlockSize))

	// --- volume header ---
	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], xaBlockSize)
	binary.BigEndian.PutUint32(vh[44:48], lastBlock+1)
	binary.BigEndian.PutUint32(vh[48:52], 10)

	putFork := func(off int, blocks uint32, exts []ExtentDescriptor) {
		binary.BigEndian.PutUint64(vh[off:off+8], uint64(blocks)*uint64(xaBlockSize))
		binary.BigEndian.PutUint32(vh[off+12:off+16], blocks)
		for i, e := range exts {
			base := off + 16 + i*8
			binary.BigEndian.PutUint32(vh[base:base+4], e.StartBlock)
			binary.BigEndian.PutUint32(vh[base+4:base+8], e.BlockCount)
		}
	}
	putFork(272, 1, []ExtentDescriptor{{StartBlock: xaCatalogBlock, BlockCount: 1}}) // catalog
	putFork(352, 1, []ExtentDescriptor{{StartBlock: xaAttrBlock, BlockCount: 1}})    // attributes

	writeNodeAt := func(startBlock uint32, num uint32, node []byte) {
		off := int(startBlock)*int(xaBlockSize) + int(num)*int(xaNodeSize)
		copy(img[off:off+int(xaNodeSize)], node)
	}

	// --- catalog: root plus four files ---
	catRecords := [][]byte{
		append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecord(rootFolderCNID, 4)...),
		append(buildCatalogKey(rootFolderCNID, "forked.bin"), buildFileRecord(xaForkCNID)...),
		append(buildCatalogKey(rootFolderCNID, "inline.txt"), buildFileRecord(xaInlineCNID)...),
	}
	writeNodeAt(xaCatalogBlock, 0, makeNode(xaNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(xaNodeSize, 2, 1, 1)}))
	writeNodeAt(xaCatalogBlock, 1, makeNode(xaNodeSize, btreeNodeTypeLeaf, catRecords))

	// --- attributes tree ---
	attrRecords := [][]byte{
		// Keys sort by fileID, then name, then startBlock.
		append(buildAttrKey(xaInlineCNID, 0, xaInlineName), buildAttrInlineRecord([]byte(xaInlineValue))...),
		append(buildAttrKey(xaForkCNID, 0, xaForkName), buildAttrForkRecord(xaForkSize, xaForkExtents[:8])...),
		append(buildAttrKey(xaForkCNID, 8, xaForkName), buildAttrExtensionRecord(xaForkExtents[8:])...),
		append(buildAttrKey(xaManyCNID, 0, "a.attr"), buildAttrInlineRecord([]byte("A"))...),
		append(buildAttrKey(xaManyCNID, 0, "b.attr"), buildAttrInlineRecord([]byte("BB"))...),
		append(buildAttrKey(xaManyCNID, 0, "c.attr"), buildAttrInlineRecord([]byte("CCC"))...),
	}
	writeNodeAt(xaAttrBlock, 0, makeNode(xaNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(xaNodeSize, 2, 1, 1)}))
	writeNodeAt(xaAttrBlock, 1, makeNode(xaNodeSize, btreeNodeTypeLeaf, attrRecords))

	// --- fork-backed attribute payload: one marker byte per extent ---
	for i, e := range xaForkExtents {
		start := int(e.StartBlock) * int(xaBlockSize)
		end := start + int(e.BlockCount)*int(xaBlockSize)
		for j := start; j < end; j++ {
			img[j] = xaMarkerByte(i)
		}
	}

	return img
}

// buildAttrKey builds an HFSPlusAttrKey: keyLength, pad, fileID, startBlock,
// name length in characters, then the name as UTF-16BE.
func buildAttrKey(fileID, startBlock uint32, name string) []byte {
	u16 := utf16.Encode([]rune(name))
	keyLen := 12 + 2*len(u16)

	out := make([]byte, 2+keyLen)
	binary.BigEndian.PutUint16(out[0:2], uint16(keyLen))
	// out[2:4] is pad, left zero.
	binary.BigEndian.PutUint32(out[4:8], fileID)
	binary.BigEndian.PutUint32(out[8:12], startBlock)
	binary.BigEndian.PutUint16(out[12:14], uint16(len(u16)))
	for i, c := range u16 {
		base := 14 + i*2
		binary.BigEndian.PutUint16(out[base:base+2], c)
	}
	return out
}

func buildAttrInlineRecord(value []byte) []byte {
	out := make([]byte, attrInlineHeaderSize+len(value))
	binary.BigEndian.PutUint32(out[0:4], attrRecordTypeInlineData)
	binary.BigEndian.PutUint32(out[12:16], uint32(len(value)))
	copy(out[attrInlineHeaderSize:], value)
	return out
}

func buildAttrForkRecord(size uint64, exts []ExtentDescriptor) []byte {
	out := make([]byte, attrForkDataOffset+attrForkDataSize)
	binary.BigEndian.PutUint32(out[0:4], attrRecordTypeForkData)

	fork := out[attrForkDataOffset:]
	binary.BigEndian.PutUint64(fork[0:8], size)
	var blocks uint32
	for _, e := range exts {
		blocks += e.BlockCount
	}
	binary.BigEndian.PutUint32(fork[12:16], blocks)
	for i, e := range exts {
		base := 16 + i*8
		binary.BigEndian.PutUint32(fork[base:base+4], e.StartBlock)
		binary.BigEndian.PutUint32(fork[base+4:base+8], e.BlockCount)
	}
	return out
}

func buildAttrExtensionRecord(exts []ExtentDescriptor) []byte {
	out := make([]byte, attrExtentsOffset+attrExtentsSize)
	binary.BigEndian.PutUint32(out[0:4], attrRecordTypeExtension)
	for i, e := range exts {
		base := attrExtentsOffset + i*8
		binary.BigEndian.PutUint32(out[base:base+4], e.StartBlock)
		binary.BigEndian.PutUint32(out[base+4:base+8], e.BlockCount)
	}
	return out
}
