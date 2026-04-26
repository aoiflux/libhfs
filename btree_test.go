package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestParseBTreeNodeDescriptor(t *testing.T) {
	n := make([]byte, btreeNodeDescSize)
	binary.BigEndian.PutUint32(n[0:4], 7)
	binary.BigEndian.PutUint32(n[4:8], 3)
	n[8] = byte(btreeNodeTypeHead)
	n[9] = 0
	binary.BigEndian.PutUint16(n[10:12], 2)

	d, err := parseBTreeNodeDescriptor(n)
	if err != nil {
		t.Fatalf("parseBTreeNodeDescriptor: %v", err)
	}
	if d.ForwardLink != 7 || d.BackwardLink != 3 || d.Type != btreeNodeTypeHead || d.NumRecords != 2 {
		t.Fatalf("unexpected descriptor: %#v", d)
	}
}

func TestParseBTreeHeaderRecord(t *testing.T) {
	r := make([]byte, btreeHeaderRecSize)
	binary.BigEndian.PutUint16(r[0:2], 3)
	binary.BigEndian.PutUint32(r[2:6], 1)
	binary.BigEndian.PutUint32(r[6:10], 10)
	binary.BigEndian.PutUint32(r[10:14], 2)
	binary.BigEndian.PutUint32(r[14:18], 8)
	binary.BigEndian.PutUint16(r[18:20], 4096)
	binary.BigEndian.PutUint16(r[20:22], 520)
	binary.BigEndian.PutUint32(r[22:26], 64)
	binary.BigEndian.PutUint32(r[26:30], 4)
	binary.BigEndian.PutUint32(r[32:36], 65536)
	r[36] = 0
	r[37] = 0xC7
	binary.BigEndian.PutUint32(r[38:42], 0x00000004)

	h, err := parseBTreeHeaderRecord(r)
	if err != nil {
		t.Fatalf("parseBTreeHeaderRecord: %v", err)
	}
	if h.NodeSize != 4096 || h.TotalNodes != 64 || h.Attributes != 0x00000004 {
		t.Fatalf("unexpected header: %#v", h)
	}
}

func TestParseCatalogAndExtentsKeys(t *testing.T) {
	cat := make([]byte, 14)
	binary.BigEndian.PutUint16(cat[0:2], 12)
	binary.BigEndian.PutUint32(cat[2:6], 42)
	binary.BigEndian.PutUint16(cat[6:8], 3)
	binary.BigEndian.PutUint16(cat[8:10], 'a')
	binary.BigEndian.PutUint16(cat[10:12], 'b')
	binary.BigEndian.PutUint16(cat[12:14], 'c')

	ck, n, err := parseCatalogKey(cat)
	if err != nil {
		t.Fatalf("parseCatalogKey: %v", err)
	}
	if n != 14 || ck.ParentCNID != 42 || ck.NameString() != "abc" {
		t.Fatalf("unexpected catalog key: %#v n=%d", ck, n)
	}

	ext := make([]byte, 12)
	binary.BigEndian.PutUint16(ext[0:2], 10)
	ext[2] = extentKeyTypeData
	binary.BigEndian.PutUint32(ext[4:8], 99)
	binary.BigEndian.PutUint32(ext[8:12], 123)

	ek, m, err := parseExtentsKey(ext)
	if err != nil {
		t.Fatalf("parseExtentsKey: %v", err)
	}
	if m != 12 || ek.FileID != 99 || ek.StartBlock != 123 {
		t.Fatalf("unexpected extents key: %#v m=%d", ek, m)
	}
}

func TestVolumeBTreeHeaders(t *testing.T) {
	blockSize := uint32(4096)
	catalogStart := uint32(2)
	extentsStart := uint32(3)

	img := make([]byte, int((extentsStart+1)*blockSize))
	h := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(h[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(h[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(h[40:44], blockSize)
	binary.BigEndian.PutUint32(h[44:48], 400)
	binary.BigEndian.PutUint32(h[48:52], 200)

	// Catalog fork descriptor at [272:352].
	binary.BigEndian.PutUint64(h[272:280], 8192)
	binary.BigEndian.PutUint32(h[272+12:272+16], 2)
	binary.BigEndian.PutUint32(h[272+16:272+20], catalogStart)
	binary.BigEndian.PutUint32(h[272+20:272+24], 1)

	// Extents fork descriptor at [192:272].
	binary.BigEndian.PutUint64(h[192:200], 4096)
	binary.BigEndian.PutUint32(h[192+12:192+16], 1)
	binary.BigEndian.PutUint32(h[192+16:192+20], extentsStart)
	binary.BigEndian.PutUint32(h[192+20:192+24], 1)

	writeBTreeHeaderNode := func(block uint32, nodeSize uint16, totalNodes uint32) {
		off := int(block * blockSize)
		node := img[off : off+btreeNodeDescSize+btreeHeaderRecSize]
		node[8] = byte(btreeNodeTypeHead)
		binary.BigEndian.PutUint16(node[10:12], 3)
		rec := node[btreeNodeDescSize:]
		binary.BigEndian.PutUint16(rec[0:2], 2)
		binary.BigEndian.PutUint32(rec[2:6], 1)
		binary.BigEndian.PutUint16(rec[18:20], nodeSize)
		binary.BigEndian.PutUint16(rec[20:22], 520)
		binary.BigEndian.PutUint32(rec[22:26], totalNodes)
		binary.BigEndian.PutUint32(rec[26:30], 1)
	}

	writeBTreeHeaderNode(catalogStart, uint16(blockSize), 33)
	writeBTreeHeaderNode(extentsStart, uint16(blockSize), 44)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	catHdr, err := vol.CatalogBTreeHeader()
	if err != nil {
		t.Fatalf("CatalogBTreeHeader failed: %v", err)
	}
	if catHdr.NodeSize != uint16(blockSize) || catHdr.TotalNodes != 33 {
		t.Fatalf("unexpected catalog btree header: %#v", catHdr)
	}

	extHdr, err := vol.ExtentsBTreeHeader()
	if err != nil {
		t.Fatalf("ExtentsBTreeHeader failed: %v", err)
	}
	if extHdr.NodeSize != uint16(blockSize) || extHdr.TotalNodes != 44 {
		t.Fatalf("unexpected extents btree header: %#v", extHdr)
	}
}

func TestVolumeBTreeHeaderMissingExtent(t *testing.T) {
	img := make([]byte, volumeHeaderOffset+volumeHeaderSize)
	h := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(h[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(h[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(h[40:44], 4096)
	binary.BigEndian.PutUint32(h[44:48], 100)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	_, err = vol.CatalogBTreeHeader()
	if !errors.Is(err, ErrMissingExtent) {
		t.Fatalf("expected ErrMissingExtent, got %v", err)
	}
}
