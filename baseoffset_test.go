package libhfs

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

// embedOffset is where these tests place a volume inside a larger image.
//
// It is deliberately not a multiple of any fixture's block size, so no
// implementation can express it in blocks and accidentally arrive at the right
// answer by a different route.
const embedOffset = int64(0x2A07)

// embedFill is the byte the space before the volume is filled with.
//
// It must not be zero. With zero padding a site that forgot to add the volume
// start usually reads a run of zeros, which degrades into "found nothing" —
// ErrNotFound, an empty directory, a zero count — and a weak assertion passes.
// With 0xA5 the same omission yields a wrong value or a hard parse error, which
// is what the assertions below are written to catch.
const embedFill = byte(0xA5)

// embedImage places img at embedOffset inside a larger buffer.
func embedImage(tb testing.TB, img []byte, fill byte) []byte {
	tb.Helper()
	out := make([]byte, int(embedOffset)+len(img))
	for i := range int(embedOffset) {
		out[i] = fill
	}
	copy(out[embedOffset:], img)
	return out
}

// openEmbedded opens img at embedOffset in a padded buffer, returning both the
// volume and the padded image so a test can read the reported offsets back out
// of the bytes the library was given.
func openEmbedded(tb testing.TB, img []byte, fill byte) (*Volume, []byte) {
	tb.Helper()
	padded := embedImage(tb, img, fill)
	vol, err := OpenWithConfig(bytes.NewReader(padded), Config{BaseOffset: embedOffset})
	if err != nil {
		tb.Fatalf("OpenWithConfig(BaseOffset=%d) failed: %v", embedOffset, err)
	}
	return vol, padded
}

// TestBaseOffsetComposesWithWrapper is the acceptance test for H8, and the one
// test that separates composing from replacing.
//
// A wrapped volume has two offsets: where the caller put the wrapper, and where
// the wrapper's embedded HFS+ volume begins inside it. An implementation that
// treats Config.BaseOffset as a replacement reports the first and is wrong by
// exactly the second — a small enough error to look plausible, which is why it
// gets an explicit assertion rather than only a byte comparison.
func TestBaseOffsetComposesWithWrapper(t *testing.T) {
	vol, img := openEmbedded(t, buildWrappedHFSPlusImage(t), embedFill)

	want := embedOffset + wrapperEmbeddedOffset
	if got := vol.BaseOffset(); got != want {
		t.Fatalf("BaseOffset() = %d, want %d (partition start %d plus wrapper offset %d)",
			got, want, embedOffset, wrapperEmbeddedOffset)
	}
	if vol.BaseOffset() == embedOffset {
		t.Fatal("BaseOffset() replaced the derived wrapper offset instead of adding to it")
	}
	if vol.BaseOffset() == wrapperEmbeddedOffset {
		t.Fatal("BaseOffset() ignored Config.BaseOffset entirely")
	}

	// Config reports what the caller supplied, not the composed value. The two
	// being different numbers under similar names is exactly what the doc
	// comments warn about, so pin it.
	if got := vol.Config().BaseOffset; got != embedOffset {
		t.Fatalf("Config().BaseOffset = %d, want the supplied %d", got, embedOffset)
	}

	ranges, err := vol.DataForkRanges(wrapperFileCNID)
	if err != nil {
		t.Fatalf("DataForkRanges failed: %v", err)
	}
	if len(ranges) != 1 {
		t.Fatalf("got %d ranges, want 1: %#v", len(ranges), ranges)
	}
	r := ranges[0]

	wantOffset := embedOffset + wrapperEmbeddedOffset + int64(wrapperDataBlock)*int64(wrapperBlockSize)
	if r.DiskOffset != wantOffset {
		t.Fatalf("DiskOffset = %d, want %d", r.DiskOffset, wantOffset)
	}
	if r.Length != int64(len(wrapperPayload)) {
		t.Fatalf("Length = %d, want %d", r.Length, len(wrapperPayload))
	}

	// The criterion the request states: read Length bytes at the reported
	// offset straight from the underlying image, not back through the volume.
	// Reading through the volume would only prove the library agrees with
	// itself, which is the one thing a wrong base offset does not disturb.
	buf := make([]byte, r.Length)
	if _, err := bytes.NewReader(img).ReadAt(buf, r.DiskOffset); err != nil {
		t.Fatalf("reading the image at DiskOffset failed: %v", err)
	}
	if !bytes.Equal(buf, wrapperPayload) {
		t.Fatalf("bytes at DiskOffset = %q, want %q", buf, wrapperPayload)
	}

	// And the B-tree must have been reachable at all, which is a separate
	// failure from the arithmetic being wrong.
	f, err := vol.OpenFileByCNID(wrapperFileCNID)
	if err != nil {
		t.Fatalf("OpenFileByCNID failed: %v", err)
	}
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(got, wrapperPayload) {
		t.Fatalf("ReadAll = %q, want %q", got, wrapperPayload)
	}
}

// TestBaseOffsetComposesOnClassicHFS covers the other composition arm, and the
// bitmap in particular.
//
// Classic HFS addresses its bitmap from the volume start and its allocation
// blocks from drAlBlSt*512, so the two must compose with the supplied offset
// differently. The fixture fills the first allocation block with 0xFF precisely
// so that reading the bitmap from the wrong origin is a failed assertion rather
// than a silent misread; the 0xA5 padding extends that to the third possible
// wrong origin, the space before the volume.
func TestBaseOffsetComposesOnClassicHFS(t *testing.T) {
	vol, _ := openEmbedded(t, buildClassicHFSBitmapImage(t), embedFill)

	if vol.Kind() != KindHFS {
		t.Fatalf("fixture opened as %s, want %s", vol.Kind(), KindHFS)
	}
	want := embedOffset + classicHFSDataBase
	if got := vol.BaseOffset(); got != want {
		t.Fatalf("BaseOffset() = %d, want %d (partition start %d plus drAlBlSt*512 %d)",
			got, want, embedOffset, classicHFSDataBase)
	}

	off, err := vol.BlockOffset(3)
	if err != nil {
		t.Fatalf("BlockOffset failed: %v", err)
	}
	if wantOff := want + 3*int64(vol.Header().BlockSize); off != wantOff {
		t.Fatalf("BlockOffset(3) = %d, want %d", off, wantOff)
	}

	// The same table TestClassicHFSBitmapOffset asserts at offset zero. Reading
	// the bitmap from v.baseOffset lands in the 0xFF block and says everything
	// is allocated; reading it from zero lands in the 0xA5 padding and says
	// something else again. Only the volume start gives this answer.
	for _, tc := range []struct {
		block uint32
		want  bool
	}{
		{0, true}, {7, true}, {8, true},
		{9, false}, {50, false}, {99, false},
	} {
		got, err := vol.BlockAllocated(tc.block)
		if err != nil {
			t.Fatalf("BlockAllocated(%d) failed: %v", tc.block, err)
		}
		if got != tc.want {
			t.Fatalf("BlockAllocated(%d) = %v, want %v — the bitmap was read from the wrong offset",
				tc.block, got, tc.want)
		}
	}

	free, err := vol.FreeBlockCount()
	if err != nil {
		t.Fatalf("FreeBlockCount failed: %v", err)
	}
	if wantFree := classicBitmapTotalBlocks - classicBitmapAllocated; free != wantFree {
		t.Fatalf("FreeBlockCount = %d, want %d", free, wantFree)
	}
}

// TestBaseOffsetClassicVolumeName pins hfsVolumeName, which re-reads the MDB
// from the volume start and is the only caller in the package that does.
//
// The padding here is 0x41 rather than 0xA5 on purpose: 0x41 is both a
// printable 'A' and a plausible Str27 length byte, so an implementation that
// reads the MDB from offset zero returns a 27-character run of "A" instead of
// failing. A test that only checked for an error would pass against that.
func TestBaseOffsetClassicVolumeName(t *testing.T) {
	vol, _ := openEmbedded(t, buildClassicHFSNamedImage(t, vnClassicName), 0x41)

	name, err := vol.VolumeName()
	if err != nil {
		t.Fatalf("VolumeName failed: %v", err)
	}
	if name != vnClassicName {
		t.Fatalf("VolumeName() = %q, want %q — the MDB was read from the wrong offset", name, vnClassicName)
	}
}

// TestBaseOffsetIgnoresDecoyAtReaderStart proves the header probe itself is
// redirected.
//
// Without this, an implementation that honours the volume start everywhere
// except the initial read still passes the tests above whenever the padding
// happens to be unparseable. Here the padding is a different, valid volume, so
// reading from the wrong origin succeeds and returns the wrong filesystem.
func TestBaseOffsetIgnoresDecoyAtReaderStart(t *testing.T) {
	decoy := buildValidCatalogImage(t)
	wrapped := buildWrappedHFSPlusImage(t)

	img := make([]byte, len(decoy)+len(wrapped))
	copy(img, decoy)
	copy(img[len(decoy):], wrapped)

	atZero, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open at the reader start failed: %v", err)
	}
	atOffset, err := OpenWithConfig(bytes.NewReader(img), Config{BaseOffset: int64(len(decoy))})
	if err != nil {
		t.Fatalf("OpenWithConfig failed: %v", err)
	}

	if atZero.Kind() == atOffset.Kind() && atZero.BaseOffset() == atOffset.BaseOffset() {
		t.Fatal("the offset volume is indistinguishable from the one at the reader start; the header probe was not redirected")
	}
	if atOffset.Kind() != KindHFSP {
		t.Fatalf("offset volume opened as %s, want %s — it parsed the decoy", atOffset.Kind(), KindHFSP)
	}
	if want := int64(len(decoy)) + wrapperEmbeddedOffset; atOffset.BaseOffset() != want {
		t.Fatalf("BaseOffset() = %d, want %d", atOffset.BaseOffset(), want)
	}
	if _, err := atOffset.OpenFileByCNID(wrapperFileCNID); err != nil {
		t.Fatalf("the offset volume does not hold the wrapped fixture's file: %v", err)
	}
}

// TestBaseOffsetShiftsEveryReportedOffset is the generalising test.
//
// The others pin named sites. This one asserts the property those sites exist
// to uphold — open the same image twice, once scoped to the volume and once
// with a base offset, and every reported disk offset must differ by exactly
// that offset while everything else is identical. It is the only test here that
// catches a site nobody thought to enumerate.
func TestBaseOffsetShiftsEveryReportedOffset(t *testing.T) {
	for _, tc := range []struct {
		name string
		img  []byte
	}{
		{"hfsplus", buildValidCatalogImage(t)},
		{"classic", buildClassicHFSBitmapImage(t)},
		{"wrapped", buildWrappedHFSPlusImage(t)},
		{"xattr", buildXAttrImage(t)},
		{"links", buildLinkImage(t)},
		{"decmpfs", buildDecmpfsImage(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain, err := Open(bytes.NewReader(tc.img))
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			shifted, _ := openEmbedded(t, tc.img, embedFill)

			if plain.Kind() != shifted.Kind() {
				t.Fatalf("Kind differs: %s vs %s", plain.Kind(), shifted.Kind())
			}
			if got, want := shifted.BaseOffset(), plain.BaseOffset()+embedOffset; got != want {
				t.Fatalf("BaseOffset() = %d, want %d", got, want)
			}
			if plain.AnomalyCount() != shifted.AnomalyCount() {
				t.Fatalf("AnomalyCount differs: %d vs %d", plain.AnomalyCount(), shifted.AnomalyCount())
			}

			// Walk both catalogs in the same order and compare each record's
			// paths, then every byte range it reports. A missed site shows up
			// either as a walk that diverges or as an offset that moved by
			// something other than embedOffset.
			plainPaths := walkPathList(t, plain)
			shiftedPaths := walkPathList(t, shifted)
			if len(plainPaths) == 0 {
				t.Fatal("fixture yielded no records; the comparison would prove nothing")
			}
			if len(plainPaths) != len(shiftedPaths) {
				t.Fatalf("walk lengths differ: %d vs %d", len(plainPaths), len(shiftedPaths))
			}
			compared := 0
			for i := range plainPaths {
				if plainPaths[i] != shiftedPaths[i] {
					t.Fatalf("walk diverged at %d: %q vs %q", i, plainPaths[i], shiftedPaths[i])
				}
			}
			for _, cnid := range walkCNIDList(t, plain) {
				pr, perr := plain.DataForkRanges(cnid)
				sr, serr := shifted.DataForkRanges(cnid)
				if (perr == nil) != (serr == nil) {
					t.Fatalf("cnid %d: DataForkRanges errors differ: %v vs %v", cnid, perr, serr)
				}
				if perr != nil {
					continue
				}
				if len(pr) != len(sr) {
					t.Fatalf("cnid %d: range counts differ: %d vs %d", cnid, len(pr), len(sr))
				}
				for j := range pr {
					if sr[j].DiskOffset-pr[j].DiskOffset != embedOffset {
						t.Fatalf("cnid %d range %d: DiskOffset moved by %d, want %d",
							cnid, j, sr[j].DiskOffset-pr[j].DiskOffset, embedOffset)
					}
					if pr[j].Length != sr[j].Length || pr[j].ForkOffset != sr[j].ForkOffset || pr[j].Slack != sr[j].Slack {
						t.Fatalf("cnid %d range %d: fork-relative fields changed: %#v vs %#v", cnid, j, pr[j], sr[j])
					}
					compared++
				}
			}
			if compared == 0 {
				t.Log("no data-fork ranges on this fixture; the walk comparison still applies")
			}
		})
	}
}

func walkPathList(tb testing.TB, vol *Volume) []string {
	tb.Helper()
	var out []string
	err := vol.WalkPaths(func(path string, _ CatalogRecord) error {
		out = append(out, path)
		return nil
	})
	if err != nil {
		tb.Fatalf("WalkPaths failed: %v", err)
	}
	return out
}

func walkCNIDList(tb testing.TB, vol *Volume) []uint32 {
	tb.Helper()
	var out []uint32
	err := vol.WalkPaths(func(_ string, rec CatalogRecord) error {
		if rec.Type == CatalogRecordFile {
			out = append(out, rec.CNID)
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("WalkPaths failed: %v", err)
	}
	return out
}

// TestBaseOffsetRejectsInvalidValues covers the contract's validation clause.
func TestBaseOffsetRejectsInvalidValues(t *testing.T) {
	img := buildValidCatalogImage(t)

	t.Run("negative", func(t *testing.T) {
		_, err := OpenWithConfig(bytes.NewReader(img), Config{BaseOffset: -1})
		if err == nil {
			t.Fatal("a negative BaseOffset was accepted")
		}
		if !errors.Is(err, ErrInvalidOffset) {
			t.Fatalf("err = %v, want ErrInvalidOffset", err)
		}
		// A caller's sign error is not evidence that the image is damaged.
		if IsCorrupt(err) {
			t.Fatalf("a caller-side offset error was reported as corruption: %v", err)
		}
	})

	t.Run("past the end", func(t *testing.T) {
		_, err := OpenWithConfig(bytes.NewReader(img), Config{BaseOffset: int64(len(img))})
		if err == nil {
			t.Fatal("an offset past the end of the reader was accepted")
		}
		if !errors.Is(err, ErrShortRead) {
			t.Fatalf("err = %v, want ErrShortRead", err)
		}
		var pErr *ParseError
		if !errors.As(err, &pErr) {
			t.Fatalf("err = %v, want a *ParseError", err)
		}
		// The error must name the offset the caller supplied, not the header's
		// 1024, or it reads as "your acquisition is truncated".
		if pErr.Offset != int64(len(img)) {
			t.Fatalf("ParseError.Offset = %d, want the supplied %d", pErr.Offset, len(img))
		}
	})

	t.Run("overflows int64", func(t *testing.T) {
		vol, err := OpenWithConfig(bytes.NewReader(img), Config{BaseOffset: math.MaxInt64})
		if err == nil {
			t.Fatalf("MaxInt64 was accepted, yielding BaseOffset() = %d", vol.BaseOffset())
		}
	})
}

// TestBlockOffsetRejectsNegativeBaseOffset pins the invariant BlockOffset's
// overflow guard depends on.
//
// The guard converts baseOffset to uint64, so a negative one wraps to ~2^64,
// the subtraction underflows, and the check never fires — it would return a
// garbage offset rather than an error. openAt makes that unreachable, which is
// exactly why the guard needs a direct test: nothing else can reach it.
func TestBlockOffsetRejectsNegativeBaseOffset(t *testing.T) {
	vol := &Volume{baseOffset: -1, header: VolumeHeader{BlockSize: 4096}}
	off, err := vol.BlockOffset(1)
	if err == nil {
		t.Fatalf("BlockOffset returned %d for a negative base offset, want an error", off)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// TestBaseOffsetZeroIsUnchanged is the additive guarantee: the zero value must
// reproduce Open exactly.
func TestBaseOffsetZeroIsUnchanged(t *testing.T) {
	img := buildWrappedHFSPlusImage(t)

	plain, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	zero, err := OpenWithConfig(bytes.NewReader(img), Config{})
	if err != nil {
		t.Fatalf("OpenWithConfig with a zero Config failed: %v", err)
	}

	if plain.BaseOffset() != zero.BaseOffset() {
		t.Fatalf("BaseOffset() differs: %d vs %d", plain.BaseOffset(), zero.BaseOffset())
	}
	if zero.BaseOffset() != wrapperEmbeddedOffset {
		t.Fatalf("BaseOffset() = %d, want the derived %d", zero.BaseOffset(), wrapperEmbeddedOffset)
	}
	if zero.Config().BaseOffset != 0 {
		t.Fatalf("Config().BaseOffset = %d, want 0", zero.Config().BaseOffset)
	}
}

// TestBaseOffsetCarvedRecordOffset pins the one exported offset whose meaning
// depends on how the record was found.
//
// DeletedRecord.ByteOffset is documented as directly seekable in the reader
// passed to Open when Source is RecoveredFromUnallocated. That promise is only
// kept if carving composes the base offset like everything else, and carving
// reaches the reader by a different route from every other read — it scans raw
// blocks rather than following a fork.
func TestBaseOffsetCarvedRecordOffset(t *testing.T) {
	const (
		cnid = uint32(500)
		name = "carved-from-free-space.txt"
	)
	vol, img := openEmbedded(t, buildImageWithNodeInFreeSpace(t, cnid, name), embedFill)

	recs, err := vol.RecoverDeleted(&RecoveryOptions{ScanUnallocated: true})
	if err != nil {
		t.Fatalf("RecoverDeleted failed: %v", err)
	}

	var found *DeletedRecord
	for i := range recs {
		if recs[i].Record.CNID == cnid && recs[i].Source == RecoveredFromUnallocated {
			found = &recs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the record planted in free space was not carved from the shifted image; got %d records", len(recs))
	}

	if found.ByteOffset < embedOffset {
		t.Fatalf("ByteOffset = %d, which is before the volume even begins (%d) — the base offset was dropped",
			found.ByteOffset, embedOffset)
	}
	if found.ByteOffset >= int64(len(img)) {
		t.Fatalf("ByteOffset = %d is past the end of the %d-byte image", found.ByteOffset, len(img))
	}

	// The criterion: the bytes at the reported offset must be the node the
	// record was carved out of. Anything else means the offset is arithmetic
	// that happens to look plausible.
	node := make([]byte, btreeNodeDescSize)
	if _, err := bytes.NewReader(img).ReadAt(node, found.ByteOffset); err != nil {
		t.Fatalf("reading the image at ByteOffset failed: %v", err)
	}
	if _, err := parseBTreeNodeDescriptor(node); err != nil {
		t.Fatalf("bytes at ByteOffset %d are not a B-tree node descriptor: %v", found.ByteOffset, err)
	}
}
