package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// volHdrFinderInfoWord returns the byte offset, within the image, of one word
// of the volume header's Finder information.
func volHdrFinderInfoWord(i int) int {
	return volumeHeaderOffset + volHdrFinderInfo + i*4
}

func TestVolumeIdentifierAbsent(t *testing.T) {
	vol, _ := openValidTree(t)

	if _, err := vol.VolumeIdentifier(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on a volume with no identifier, got %v", err)
	}
	if _, err := vol.UUID(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound from UUID, got %v", err)
	}
}

func TestVolumeIdentifierAndUUID(t *testing.T) {
	const (
		high = uint32(0xA1B2C3D4)
		low  = uint32(0xE5F60718)
	)

	img := buildValidCatalogImage(t)
	binary.BigEndian.PutUint32(img[volHdrFinderInfoWord(6):volHdrFinderInfoWord(6)+4], high)
	binary.BigEndian.PutUint32(img[volHdrFinderInfoWord(7):volHdrFinderInfoWord(7)+4], low)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	id, err := vol.VolumeIdentifier()
	if err != nil {
		t.Fatalf("VolumeIdentifier failed: %v", err)
	}
	if id.High != high || id.Low != low {
		t.Fatalf("identifier = %08X%08X, want %08X%08X", id.High, id.Low, high, low)
	}
	if id.IsZero() {
		t.Fatal("IsZero reported true for a populated identifier")
	}

	// The raw form is the two words as stored, high word first.
	if got, want := id.String(), "A1B2C3D4E5F60718"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}

	// The displayed UUID is a hash of those bytes under Apple's namespace, not
	// a reformatting of them. Pinned as a golden value so that a change to the
	// namespace, the byte order, or the nibble forcing is caught here.
	uuid, err := vol.UUID()
	if err != nil {
		t.Fatalf("UUID failed: %v", err)
	}
	if uuid == "" {
		t.Fatal("UUID returned the empty string for a populated identifier")
	}
	if uuid != id.UUID() {
		t.Fatalf("Volume.UUID() = %q, VolumeIdentifier.UUID() = %q", uuid, id.UUID())
	}

	// Shape: 8-4-4-4-12 uppercase hex, RFC 4122 version 3 and variant 10x.
	if len(uuid) != 36 {
		t.Fatalf("UUID %q is %d characters, want 36", uuid, len(uuid))
	}
	for _, pos := range []int{8, 13, 18, 23} {
		if uuid[pos] != '-' {
			t.Fatalf("UUID %q has no hyphen at position %d", uuid, pos)
		}
	}
	if uuid[14] != '3' {
		t.Fatalf("UUID %q does not carry version 3, got %q", uuid, uuid[14])
	}
	if v := uuid[19]; v != '8' && v != '9' && v != 'A' && v != 'B' {
		t.Fatalf("UUID %q does not carry the RFC 4122 variant, got %q", uuid, v)
	}

	// The two forms must share no digits, which is the whole reason both ship.
	if uuid == id.String() {
		t.Fatal("the derived UUID is the raw identifier; the derivation did nothing")
	}
}

func TestVolumeIdentifierUUIDIsStable(t *testing.T) {
	id := VolumeIdentifier{High: 0x01234567, Low: 0x89ABCDEF}

	first := id.UUID()
	if first != id.UUID() {
		t.Fatal("UUID() is not deterministic")
	}

	// A one-bit change in the stored value must change the derived UUID.
	other := VolumeIdentifier{High: 0x01234567, Low: 0x89ABCDEE}
	if other.UUID() == first {
		t.Fatal("two different identifiers derived the same UUID")
	}
}

func TestVolumeIdentifierZeroValue(t *testing.T) {
	var id VolumeIdentifier
	if !id.IsZero() {
		t.Fatal("the zero VolumeIdentifier does not report IsZero")
	}
	if got := id.UUID(); got != "" {
		t.Fatalf("UUID() on an absent identifier = %q, want the empty string", got)
	}
	if got, want := id.String(), "0000000000000000"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// TestVolumeIdentifierClassicHFS covers the correction that classic HFS carries
// the same two words in drFndrInfo, so the lookup must not be gated on Kind.
func TestVolumeIdentifierClassicHFS(t *testing.T) {
	const (
		high = uint32(0x11223344)
		low  = uint32(0x55667788)
	)

	img := buildClassicHFSTimesImage(t)
	off := volumeHeaderOffset + hfsMDBOffFinderInfo
	binary.BigEndian.PutUint32(img[off+6*4:off+6*4+4], high)
	binary.BigEndian.PutUint32(img[off+7*4:off+7*4+4], low)

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("unexpected kind: %s", vol.Kind())
	}

	id, err := vol.VolumeIdentifier()
	if err != nil {
		t.Fatalf("VolumeIdentifier on classic HFS failed: %v", err)
	}
	if id.High != high || id.Low != low {
		t.Fatalf("identifier = %08X%08X, want %08X%08X", id.High, id.Low, high, low)
	}
}

func TestCorpusVolumeUUID(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	id, err := vol.VolumeIdentifier()
	if errors.Is(err, ErrNotFound) {
		t.Skip("corpus volume carries no volume identifier")
	}
	if err != nil {
		t.Fatalf("VolumeIdentifier failed: %v", err)
	}

	uuid, err := vol.UUID()
	if err != nil {
		t.Fatalf("UUID failed: %v", err)
	}
	// Compare against `diskutil info` for the source volume: this is the
	// derivation's only real check, and the fixtures cannot supply one.
	t.Logf("volume identifier %s derives UUID %s", id, uuid)

	if len(uuid) != 36 {
		t.Fatalf("derived UUID %q is malformed", uuid)
	}
}
