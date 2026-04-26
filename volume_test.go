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
