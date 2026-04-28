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
	const (
		wrapperAllocBlockSize = uint32(4096)
		wrapperAlBlSt         = uint16(1)
		embedStartBlock       = uint16(2)
	)
	embeddedOffset := int64(wrapperAlBlSt)*512 + int64(embedStartBlock)*int64(wrapperAllocBlockSize)

	blockSize := uint32(4096)
	catalogStart := uint32(2)

	img := make([]byte, int(embeddedOffset)+int(volumeHeaderOffset)+int(catalogStart+1)*int(blockSize))

	// Wrapper MDB at fixed HFS offset (1024 bytes).
	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(mdb[0:2], signatureHFS)
	binary.BigEndian.PutUint32(mdb[20:24], wrapperAllocBlockSize)
	binary.BigEndian.PutUint16(mdb[28:30], wrapperAlBlSt)
	binary.BigEndian.PutUint16(mdb[0x7C:0x7E], signatureHFSP)
	binary.BigEndian.PutUint16(mdb[0x7E:0x80], embedStartBlock)
	binary.BigEndian.PutUint16(mdb[0x80:0x82], 20)

	// Embedded HFS+ volume header.
	vhOff := int(embeddedOffset) + volumeHeaderOffset
	h := img[vhOff : vhOff+volumeHeaderSize]
	binary.BigEndian.PutUint16(h[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(h[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(h[40:44], blockSize)
	binary.BigEndian.PutUint32(h[44:48], 400)
	binary.BigEndian.PutUint32(h[48:52], 200)
	binary.BigEndian.PutUint64(h[272:280], 8192)
	binary.BigEndian.PutUint32(h[272+12:272+16], 2)
	binary.BigEndian.PutUint32(h[272+16:272+20], catalogStart)
	binary.BigEndian.PutUint32(h[272+20:272+24], 1)

	// Catalog B-tree header node located relative to the embedded volume.
	catOff := int(embeddedOffset) + int(catalogStart*blockSize)
	node := img[catOff : catOff+btreeNodeDescSize+btreeHeaderRecSize]
	node[8] = byte(btreeNodeTypeHead)
	binary.BigEndian.PutUint16(node[10:12], 3)
	rec := node[btreeNodeDescSize:]
	binary.BigEndian.PutUint16(rec[0:2], 1)
	binary.BigEndian.PutUint32(rec[2:6], 0)
	binary.BigEndian.PutUint16(rec[18:20], uint16(blockSize))
	binary.BigEndian.PutUint16(rec[20:22], 520)
	binary.BigEndian.PutUint32(rec[22:26], 12)
	binary.BigEndian.PutUint32(rec[26:30], 1)

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
	if catHdr.NodeSize != uint16(blockSize) || catHdr.TotalNodes != 12 {
		t.Fatalf("unexpected catalog btree header: %#v", catHdr)
	}
}
