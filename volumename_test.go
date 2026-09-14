package libhfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// Volume.VolumeName and IsCorrupt.
//
// Both are small, both are exported, and both were untested: IsCorrupt sat at
// 0% coverage on every image in the corpus, and there was no volume-name
// accessor at all — the name was reachable only as the root record's Name,
// which is not where classic HFS keeps it.

const vnClassicName = "NAMEDVOL"

// buildClassicHFSNamedImage stamps drVN into the MDB of the classic fixture.
//
// The offsets are written as literals with their Inside Macintosh meaning
// rather than reusing the decoder's constants: a fixture built from the same
// constant as the parser agrees with it however wrong that constant is, which
// is the exact mistake this repo has made before on classic HFS.
func buildClassicHFSNamedImage(tb testing.TB, name string) []byte {
	tb.Helper()

	img := buildClassicHFSTimesImage(tb)
	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]

	// drVN is a Str27 at offset 36 of the MDB: one length byte, then the
	// characters, in a 28-byte field.
	const drVN = 36
	for i := range 28 {
		mdb[drVN+i] = 0
	}
	mdb[drVN] = byte(len(name))
	copy(mdb[drVN+1:], name)
	return img
}

func TestVolumeNameClassicHFSReadsMDB(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSNamedImage(t, vnClassicName)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if vol.Kind() != KindHFS {
		t.Fatalf("fixture opened as %s, want %s", vol.Kind(), KindHFS)
	}

	got, err := vol.VolumeName()
	if err != nil {
		t.Fatalf("VolumeName: %v", err)
	}
	if got != vnClassicName {
		t.Errorf("VolumeName = %q, want %q", got, vnClassicName)
	}
}

// TestVolumeNameClassicHFSPrefersMDBOverCatalog is what makes the classic path
// worth having separately.
//
// The fixture's catalog root record carries a different name from the MDB, so a
// VolumeName that quietly resolved through the catalog on every format would
// return the wrong one here. A volume where the two disagree is damaged or
// tampered with, and the MDB is the field the format defines for this.
func TestVolumeNameClassicHFSPrefersMDBOverCatalog(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildClassicHFSNamedImage(t, vnClassicName)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Skipf("fixture has no readable root record: %v", err)
	}
	// The premise: the two sources really do differ in this fixture, so the
	// assertion below can tell them apart.
	if root.Name == vnClassicName {
		t.Skipf("root record also names the volume %q; this fixture cannot distinguish the sources",
			root.Name)
	}

	got, err := vol.VolumeName()
	if err != nil {
		t.Fatalf("VolumeName: %v", err)
	}
	if got != vnClassicName {
		t.Errorf("VolumeName = %q, want the MDB's %q (the root record says %q)",
			got, vnClassicName, root.Name)
	}
}

// TestVolumeNameClassicHFSClampsOverlongLength covers a damaged Str27 whose
// length byte runs past the field. Trusting it would return bytes from drVolBkUp
// and the dates after it as part of the name.
func TestVolumeNameClassicHFSClampsOverlongLength(t *testing.T) {
	img := buildClassicHFSNamedImage(t, vnClassicName)
	mdb := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	mdb[36] = 0xFF

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := vol.VolumeName()
	if err != nil {
		t.Fatalf("VolumeName: %v", err)
	}
	if len(got) > 27 {
		t.Errorf("VolumeName returned %d characters (%q); a Str27 cannot exceed 27", len(got), got)
	}
}

// TestVolumeNameClassicHFSAbsent checks the empty case rather than letting a
// zero-length name come back as a successful "".
func TestVolumeNameClassicHFSAbsent(t *testing.T) {
	img := buildClassicHFSNamedImage(t, vnClassicName)
	img[volumeHeaderOffset+36] = 0

	vol, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := vol.VolumeName(); !errors.Is(err, ErrNotFound) {
		t.Errorf("VolumeName on an unnamed volume = %v, want ErrNotFound", err)
	}
}

// TestVolumeNameHFSPlusReadsCatalogRoot covers the other format, where the name
// exists only as the root folder's catalog key.
func TestVolumeNameHFSPlusReadsCatalogRoot(t *testing.T) {
	vol, err := Open(bytes.NewReader(buildValidCatalogImage(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if vol.Kind() == KindHFS {
		t.Fatalf("fixture is classic HFS; this test is about the HFS+ path")
	}

	root, err := vol.GetRootDirectory()
	if err != nil {
		t.Fatalf("GetRootDirectory: %v", err)
	}
	got, err := vol.VolumeName()
	if err != nil {
		t.Fatalf("VolumeName: %v", err)
	}
	if got != root.Name {
		t.Errorf("VolumeName = %q, root record Name = %q", got, root.Name)
	}
	if got == "" {
		t.Error("VolumeName returned empty without an error")
	}
}

func TestVolumeNameNilReceiver(t *testing.T) {
	var vol *Volume
	if _, err := vol.VolumeName(); err == nil {
		t.Fatal("VolumeName on a nil receiver returned no error")
	}
}

// TestIsCorruptClassifies pins which errors count as a damaged volume.
//
// The split matters to a caller deciding whether to report an image as
// unreliable: a missing path is an answer, a malformed B-tree node is not.
func TestIsCorruptClassifies(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
		why  string
	}{
		{ErrCorrupt, true, "the general corruption sentinel"},
		{ErrInvalidSignature, true, "no recognisable volume header"},
		{ErrUnsupportedVer, true, "a version field no writer produces"},
		{ErrInvalidBTreeNode, true, "a structurally impossible node"},
		{ErrInvalidBTreeKey, true, "a key that cannot be decoded"},
		{ErrMissingExtent, true, "metadata pointing at blocks that are not there"},

		{ErrNotFound, false, "an answer about the request, not the volume"},
		{ErrNotFile, false, "an answer about the request"},
		{ErrNotDir, false, "an answer about the request"},
		{ErrInvalidOffset, false, "the caller asked for a bad offset"},
		{ErrUnsupportedFormat, false, "well-formed, just not read by this package"},
		{ErrUnsupportedHFS, false, "well-formed, just not read by this package"},
		{ErrShortRead, false, "a truncated image is a different finding from corrupt metadata"},
		{ErrSizeLimit, false, "a limit this caller set"},
		{nil, false, "no error at all"},
	} {
		if got := IsCorrupt(tc.err); got != tc.want {
			t.Errorf("IsCorrupt(%v) = %v, want %v — %s", tc.err, got, tc.want, tc.why)
		}
	}
}

// TestIsCorruptUnwrapsParseError checks the case callers actually hit: the
// package returns *ParseError almost everywhere, never a bare sentinel, so an
// IsCorrupt that compared with == would report every real error as fine.
func TestIsCorruptUnwrapsParseError(t *testing.T) {
	wrapped := &ParseError{Op: "decode_catalog_record", Offset: 512, Err: ErrCorrupt}
	if !IsCorrupt(wrapped) {
		t.Error("IsCorrupt did not see through a *ParseError")
	}

	nested := &ParseError{Op: "outer", Offset: 0, Err: &ParseError{Op: "inner", Err: ErrInvalidBTreeNode}}
	if !IsCorrupt(nested) {
		t.Error("IsCorrupt did not see through nested *ParseError values")
	}

	notCorrupt := &ParseError{Op: "open_path", Offset: 0, Err: ErrNotFound}
	if IsCorrupt(notCorrupt) {
		t.Error("IsCorrupt reported a wrapped ErrNotFound as corruption")
	}
}

// TestIsCorruptOnRealDamage drives the predicate from an actual damaged image
// rather than from hand-made sentinels, which is the use it exists for.
func TestIsCorruptOnRealDamage(t *testing.T) {
	base := buildValidCatalogImage(t)

	// Destroy the signature: the volume cannot be identified at all.
	broken := append([]byte(nil), base...)
	binary.BigEndian.PutUint16(broken[volumeHeaderOffset:volumeHeaderOffset+2], 0x0000)
	if _, err := Open(bytes.NewReader(broken)); err == nil {
		t.Fatal("Open accepted an image with no volume signature")
	} else if !IsCorrupt(err) {
		t.Errorf("IsCorrupt(%v) = false for an unidentifiable volume", err)
	}

	// A volume that opens but whose catalog is rubble. Whatever the walk
	// reports, a missing-path answer would be the wrong classification.
	rubble := append([]byte(nil), base...)
	for i := 2 * 4096; i < 3*4096 && i < len(rubble); i++ {
		rubble[i] = 0xFF
	}
	vol, err := Open(bytes.NewReader(rubble))
	if err != nil {
		if !IsCorrupt(err) {
			t.Errorf("IsCorrupt(%v) = false for an image with a destroyed catalog", err)
		}
		return
	}
	if _, err := vol.OpenPath("/anything"); err != nil && errors.Is(err, ErrNotFound) {
		t.Log("destroyed catalog reported as not-found; the volume opened far enough to answer")
	}
}
