package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestResolveDataForkExtentsOverflow(t *testing.T) {
	img := buildExtentsOverflowTestImage(t, true)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	exts, err := vol.ResolveDataForkExtents(101)
	if err != nil {
		t.Fatalf("ResolveDataForkExtents failed: %v", err)
	}
	if len(exts) != 2 {
		t.Fatalf("expected 2 extents, got %d", len(exts))
	}
	if exts[0].StartBlock != 50 || exts[0].BlockCount != 1 {
		t.Fatalf("unexpected first extent: %#v", exts[0])
	}
	if exts[1].StartBlock != 60 || exts[1].BlockCount != 2 {
		t.Fatalf("unexpected second extent: %#v", exts[1])
	}
}

func TestResolveDataForkExtentsMissingOverflow(t *testing.T) {
	img := buildExtentsOverflowTestImage(t, false)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ResolveDataForkExtents(101)
	if !errors.Is(err, ErrMissingExtent) {
		t.Fatalf("expected ErrMissingExtent, got %v", err)
	}
}

func TestOpenFileByPathReadAcrossOverflowExtents(t *testing.T) {
	img := buildExtentsOverflowTestImage(t, true)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	f, err := vol.OpenFileByPath("/etc/hosts")
	if err != nil {
		t.Fatalf("OpenFileByPath failed: %v", err)
	}

	if f.Size() != 4104 {
		t.Fatalf("unexpected logical size: %d", f.Size())
	}

	buf := make([]byte, 12)
	n, err := f.ReadAt(buf, 4092)
	if err != io.EOF && err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != 12 {
		t.Fatalf("unexpected bytes read: %d", n)
	}
	if got := string(buf); got != "ABCDEFGHIJKL" {
		t.Fatalf("unexpected data: %q", got)
	}

	all, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if len(all) != 4104 {
		t.Fatalf("unexpected ReadAll length: %d", len(all))
	}
	if got := string(all[4092:4104]); got != "ABCDEFGHIJKL" {
		t.Fatalf("unexpected ReadAll boundary data: %q", got)
	}
}

func TestOpenFileByPathOnDirectory(t *testing.T) {
	img := buildExtentsOverflowTestImage(t, true)
	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.OpenFileByPath("/etc")
	if !errors.Is(err, ErrNotFile) {
		t.Fatalf("expected ErrNotFile, got %v", err)
	}
}

func buildExtentsOverflowTestImage(t *testing.T, includeOverflow bool) []byte {
	t.Helper()

	blockSize := uint32(4096)
	catalogStartBlock := uint32(2)
	extentsStartBlock := uint32(3)
	nodeSize := uint16(512)

	img := make([]byte, int(80*blockSize))
	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], blockSize)
	binary.BigEndian.PutUint32(vh[44:48], 400)
	binary.BigEndian.PutUint32(vh[48:52], 250)

	// catalog fork in block 2
	binary.BigEndian.PutUint64(vh[272:280], uint64(blockSize))
	binary.BigEndian.PutUint32(vh[272+12:272+16], 1)
	binary.BigEndian.PutUint32(vh[272+16:272+20], catalogStartBlock)
	binary.BigEndian.PutUint32(vh[272+20:272+24], 1)

	// extents fork in block 3
	binary.BigEndian.PutUint64(vh[192:200], uint64(blockSize))
	binary.BigEndian.PutUint32(vh[192+12:192+16], 1)
	binary.BigEndian.PutUint32(vh[192+16:192+20], extentsStartBlock)
	binary.BigEndian.PutUint32(vh[192+20:192+24], 1)

	catalogBase := int(catalogStartBlock * blockSize)
	writeCatalogNode := func(nodeNum uint32, node []byte) {
		off := catalogBase + int(nodeNum)*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}

	catalogHeader := makeNode(nodeSize, btreeNodeTypeHead, [][]byte{buildBTreeHeaderRecordBytes(nodeSize, 4, 1)})
	writeCatalogNode(0, catalogHeader)
	idxRec1 := append(buildCatalogKey(rootFolderCNID, ""), u32be(2)...)
	idxRec2 := append(buildCatalogKey(rootFolderCNID, "etc"), u32be(3)...)
	writeCatalogNode(1, makeNode(nodeSize, btreeNodeTypeIdx, [][]byte{idxRec1, idxRec2}))
	leafRoot := append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecord(rootFolderCNID, 2)...)
	leafEtc := append(buildCatalogKey(rootFolderCNID, "etc"), buildFolderRecord(100, 1)...)
	writeCatalogNode(2, makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leafRoot, leafEtc}))
	leafHosts := append(buildCatalogKey(100, "hosts"), buildFileRecordWithFork(101, 4104, 50, 1, 3)...)
	writeCatalogNode(3, makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leafHosts}))

	extentsBase := int(extentsStartBlock * blockSize)
	writeExtentsNode := func(nodeNum uint32, node []byte) {
		off := extentsBase + int(nodeNum)*int(nodeSize)
		copy(img[off:off+int(nodeSize)], node)
	}

	extHeader := makeNode(nodeSize, btreeNodeTypeHead, [][]byte{buildBTreeHeaderRecordBytes(nodeSize, 3, 1)})
	writeExtentsNode(0, extHeader)
	if includeOverflow {
		idxKey := append(buildExtentsKey(101, extentKeyTypeData, 1), u32be(2)...)
		writeExtentsNode(1, makeNode(nodeSize, btreeNodeTypeIdx, [][]byte{idxKey}))
		leafPayload := buildExtentsPayload([]ExtentDescriptor{{StartBlock: 60, BlockCount: 2}})
		leafRec := append(buildExtentsKey(101, extentKeyTypeData, 1), leafPayload...)
		writeExtentsNode(2, makeNode(nodeSize, btreeNodeTypeLeaf, [][]byte{leafRec}))

		// Logical boundary marker between extent[0] and overflow extent[1].
		copy(img[int(50*blockSize)+4092:int(50*blockSize)+4096], []byte("ABCD"))
		copy(img[int(60*blockSize):int(60*blockSize)+8], []byte("EFGHIJKL"))
	} else {
		writeExtentsNode(1, makeNode(nodeSize, btreeNodeTypeLeaf, nil))
	}

	return img
}

func buildExtentsKey(fileID uint32, forkType uint8, startBlock uint32) []byte {
	k := make([]byte, 12)
	binary.BigEndian.PutUint16(k[0:2], 10)
	k[2] = forkType
	k[3] = 0
	binary.BigEndian.PutUint32(k[4:8], fileID)
	binary.BigEndian.PutUint32(k[8:12], startBlock)
	return k
}

func buildExtentsPayload(exts []ExtentDescriptor) []byte {
	p := make([]byte, 64)
	for i, e := range exts {
		if i >= 8 {
			break
		}
		base := i * 8
		binary.BigEndian.PutUint32(p[base:base+4], e.StartBlock)
		binary.BigEndian.PutUint32(p[base+4:base+8], e.BlockCount)
	}
	return p
}

func buildFileRecordWithFork(cnid uint32, logicalSize uint64, startBlock uint32, firstLen uint32, totalBlocks uint32) []byte {
	r := make([]byte, 244)
	binary.BigEndian.PutUint16(r[0:2], catalogRecordFile)
	binary.BigEndian.PutUint32(r[8:12], cnid)

	// Data fork starts after the 88-byte shared file/folder header.
	binary.BigEndian.PutUint64(r[88:96], logicalSize)
	binary.BigEndian.PutUint32(r[88+12:88+16], totalBlocks)
	binary.BigEndian.PutUint32(r[88+16:88+20], startBlock)
	binary.BigEndian.PutUint32(r[88+20:88+24], firstLen)
	return r
}
