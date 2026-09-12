package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestOpenParsesHFSPlusHeader(t *testing.T) {
	img := make([]byte, volumeHeaderOffset+volumeHeaderSize)
	hdr := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]

	binary.BigEndian.PutUint16(hdr[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(hdr[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(hdr[40:44], 4096)
	binary.BigEndian.PutUint32(hdr[44:48], 100)
	binary.BigEndian.PutUint32(hdr[48:52], 25)
	binary.BigEndian.PutUint32(hdr[16:20], hfsEpochDeltaSeconds+123)

	binary.BigEndian.PutUint64(hdr[272:280], 8192)
	binary.BigEndian.PutUint32(hdr[272+16:272+20], 10)
	binary.BigEndian.PutUint32(hdr[272+20:272+24], 4)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if vol.Kind() != KindHFSP {
		t.Fatalf("unexpected kind: %s", vol.Kind())
	}
	got := vol.Header()
	if got.BlockSize != 4096 {
		t.Fatalf("unexpected block size: %d", got.BlockSize)
	}
	if got.TotalBlocks != 100 || got.FreeBlocks != 25 {
		t.Fatalf("unexpected block counters: total=%d free=%d", got.TotalBlocks, got.FreeBlocks)
	}
	if got.CatalogFile.Extents[0].StartBlock != 10 || got.CatalogFile.Extents[0].BlockCount != 4 {
		t.Fatalf("unexpected catalog extent: %#v", got.CatalogFile.Extents[0])
	}
}

func TestOpenInvalidSignature(t *testing.T) {
	img := make([]byte, volumeHeaderOffset+volumeHeaderSize)
	hdr := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(hdr[0:2], 0xFFFF)
	binary.BigEndian.PutUint16(hdr[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(hdr[40:44], 4096)
	binary.BigEndian.PutUint32(hdr[44:48], 100)

	_, err := Open(bytes.NewReader(img))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestOpenUnsupportedVersion(t *testing.T) {
	img := make([]byte, volumeHeaderOffset+volumeHeaderSize)
	hdr := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(hdr[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(hdr[2:4], 0x9999)
	binary.BigEndian.PutUint32(hdr[40:44], 4096)
	binary.BigEndian.PutUint32(hdr[44:48], 100)

	_, err := Open(bytes.NewReader(img))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrUnsupportedVer) {
		t.Fatalf("expected ErrUnsupportedVer, got %v", err)
	}
}

func TestOpenStandaloneHFS(t *testing.T) {
	img := make([]byte, volumeHeaderOffset+volumeHeaderSize)
	hdr := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(hdr[0:2], signatureHFS)
	binary.BigEndian.PutUint32(hdr[20:24], 4096)
	binary.BigEndian.PutUint16(hdr[18:20], 100)
	binary.BigEndian.PutUint16(hdr[34:36], 25)
	binary.BigEndian.PutUint16(hdr[12:14], 3)
	binary.BigEndian.PutUint16(hdr[82:84], 2)
	binary.BigEndian.PutUint32(hdr[30:34], 77)
	binary.BigEndian.PutUint16(hdr[28:30], 2)
	binary.BigEndian.PutUint32(hdr[0x92:0x96], 8192)
	binary.BigEndian.PutUint16(hdr[0x96:0x98], 10)
	binary.BigEndian.PutUint16(hdr[0x98:0x9A], 2)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("expected HFS volume to parse, got %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("unexpected kind: %s", vol.Kind())
	}
	got := vol.Header()
	if got.BlockSize != 4096 || got.TotalBlocks != 100 || got.FreeBlocks != 25 {
		t.Fatalf("unexpected HFS header: %#v", got)
	}
	if got.CatalogFile.Extents[0].StartBlock != 10 || got.CatalogFile.Extents[0].BlockCount != 2 {
		t.Fatalf("unexpected HFS catalog extent: %#v", got.CatalogFile.Extents[0])
	}
}

func TestOpenShortRead(t *testing.T) {
	img := make([]byte, volumeHeaderOffset+20)
	_, err := Open(bytes.NewReader(img))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrShortRead) && !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected short read/corrupt error, got %v", err)
	}
}

func TestOpenHFSWrapperEmbeddedHFSPlus(t *testing.T) {
	img := buildWrappedHFSPlusImage(t)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if vol.Kind() != KindHFSP {
		t.Fatalf("unexpected kind: %s", vol.Kind())
	}

	catHdr, err := vol.CatalogBTreeHeader()
	if err != nil {
		t.Fatalf("CatalogBTreeHeader failed: %v", err)
	}
	if catHdr.NodeSize != wrapperNodeSize || catHdr.TotalNodes != wrapperTotalNodes {
		t.Fatalf("unexpected catalog btree header: %#v", catHdr)
	}
}

// ---------------------------------------------------------------------------
// Wrapped HFS+ fixture
//
// An HFS wrapper carrying an embedded HFS+ volume is the case where every block
// number in the volume is offset from the start of the image, so a reader that
// ignores the base offset reads plausible bytes from the wrong place rather
// than failing. This builds one with a real catalog tree and a readable file,
// so that addressing can actually be exercised at a non-zero base.
// ---------------------------------------------------------------------------

const (
	wrapperAllocBlockSize = uint32(4096)
	wrapperAlBlSt         = uint16(1)
	wrapperEmbedStart     = uint16(2)
	wrapperEmbedCount     = uint16(20)

	wrapperBlockSize    = uint32(4096)
	wrapperNodeSize     = uint16(1024)
	wrapperTotalNodes   = uint32(3)
	wrapperCatalogBlock = uint32(2)
	wrapperDataBlock    = uint32(6)
	wrapperTotalBlocks  = uint32(400)
	wrapperFreeBlocks   = uint32(200)

	wrapperFileCNID = uint32(101)
	wrapperVolName  = "wrapped"
	wrapperFileName = "hosts"
)

// wrapperEmbeddedOffset is drAlBlSt*512 + drEmbedExtent.startBlock*drAlBlkSiz.
const wrapperEmbeddedOffset = int64(wrapperAlBlSt)*hfsSectorSize +
	int64(wrapperEmbedStart)*int64(wrapperAllocBlockSize)

// wrapperPayload is deliberately shorter than one allocation block, so the
// file's only byte range carries slack.
var wrapperPayload = []byte("embedded volumes are addressed from their own start")

func buildWrappedHFSPlusImage(tb testing.TB) []byte {
	tb.Helper()

	imgLen := int(wrapperEmbeddedOffset) + int(wrapperDataBlock+1)*int(wrapperBlockSize)
	img := make([]byte, imgLen)

	// Wrapper MDB, at the fixed HFS header offset from the start of the image.
	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(mdb[volHdrSignature:volHdrSignature+2], signatureHFS)
	binary.BigEndian.PutUint32(mdb[hfsMDBOffBlockSize:hfsMDBOffBlockSize+4], wrapperAllocBlockSize)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffAlBlSt:hfsMDBOffAlBlSt+2], wrapperAlBlSt)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffEmbedSigWord:hfsMDBOffEmbedSigWord+2], signatureHFSP)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffEmbedExtent:hfsMDBOffEmbedExtent+2], wrapperEmbedStart)
	binary.BigEndian.PutUint16(mdb[hfsMDBOffEmbedExtent+2:hfsMDBOffEmbedExtent+4], wrapperEmbedCount)

	// Embedded HFS+ volume header, and its catalog fork. Every offset below is
	// relative to the embedded volume, never to the image.
	embedded := img[wrapperEmbeddedOffset:]
	writeCatalogVolumeHeader(embedded, wrapperBlockSize, uint64(wrapperBlockSize), 1,
		[]ExtentDescriptor{{StartBlock: wrapperCatalogBlock, BlockCount: 1}})
	vh := embedded[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint32(vh[44:48], wrapperTotalBlocks)
	binary.BigEndian.PutUint32(vh[48:52], wrapperFreeBlocks)

	// Catalog: header node, index root, one leaf holding the root folder, its
	// thread, the file, and the file's thread — in B-tree key order.
	leafRecords := [][]byte{
		concatBytes(buildCatalogKey(1, wrapperVolName), buildFolderRecord(rootFolderCNID, 1)),
		concatBytes(buildCatalogKey(rootFolderCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, 1, wrapperVolName)),
		concatBytes(buildCatalogKey(rootFolderCNID, wrapperFileName),
			buildFileRecordWithFork(wrapperFileCNID, uint64(len(wrapperPayload)), wrapperDataBlock, 1, 1)),
		concatBytes(buildCatalogKey(wrapperFileCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, wrapperFileName)),
	}

	used := btreeNodeDescSize + 2
	for _, r := range leafRecords {
		used += len(r) + 2
	}
	if used > int(wrapperNodeSize) {
		tb.Fatalf("fixture leaf needs %d bytes, exceeds node size %d", used, wrapperNodeSize)
	}

	hdrRec := buildBTreeHeaderRecordBytesAt(wrapperNodeSize, wrapperTotalNodes, 1, 2)
	binary.BigEndian.PutUint16(hdrRec[0:2], 2) // depth: index root + one leaf

	nodes := [][]byte{
		makeNode(wrapperNodeSize, btreeNodeTypeHead, [][]byte{hdrRec}),
		makeNode(wrapperNodeSize, btreeNodeTypeIdx, [][]byte{
			concatBytes(buildCatalogKey(1, wrapperVolName), u32be(2)),
		}),
		makeNode(wrapperNodeSize, btreeNodeTypeLeaf, leafRecords),
	}

	catBase := int(wrapperEmbeddedOffset) + int(wrapperCatalogBlock*wrapperBlockSize)
	for i, node := range nodes {
		off := catBase + i*int(wrapperNodeSize)
		copy(img[off:off+int(wrapperNodeSize)], node)
	}

	// The file's content, at an allocation block of the embedded volume.
	dataOff := int(wrapperEmbeddedOffset) + int(wrapperDataBlock*wrapperBlockSize)
	copy(img[dataOff:], wrapperPayload)

	return img
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
