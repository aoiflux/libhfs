package hfs

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Cross-implementation verification against hfsutils.
//
// Every other corpus test compares libhfs to libhfs: that its counts add up,
// that its keyed search agrees with its linear scan, that a path round-trips.
// None of those can catch a field decoded from the wrong offset, because the
// same parser produces both sides of the comparison. A volume whose file sizes
// all come from four bytes too far along is entirely self-consistent.
//
// hfsutils shares no code with this package, so where the two agree the
// agreement is evidence. testdata/gen_oracle.sh writes its listing next to the
// image as <image>.oracle.tsv; these tests run only when that file exists.
//
// What this does NOT independently verify is name decoding. The oracle records
// high-bit MacRoman bytes as octal escapes, and turning those into a string to
// compare against requires a MacRoman table — so the test uses this package's
// own decoder, and a wrong table would agree with itself. Which files exist and
// how large they are is independent; how their names render is not.

type oracleEntry struct {
	isDir     bool
	dataSize  uint64
	rsrcSize  uint64
	itemCount uint64
	rawPath   string // the path exactly as hfsutils wrote it, undecoded
}

// macRomanStandard is the Mac OS Roman mapping for the high-bit bytes this
// corpus actually contains, written out from the standard rather than read from
// the package's own table.
//
// That independence is the entire point. TestCorpusMatchesOracle compares
// decoded names, so it runs both sides through decodeHFSName and a wrong table
// agrees with itself — replacing the MacRoman mapping with a raw byte widening
// leaves that test passing. These few entries are what makes the corpus able to
// tell the difference.
// Verified against Python's independent mac_roman codec, not against this
// package. Note that byte 0xE9 is E-grave: it is easy to write 'é' here because
// that character's Unicode code point is U+00E9, but the MacRoman byte for it
// is 0x8E. testdata/gen_classic_corpus.sh made exactly that mistake in a
// comment.
var macRomanStandard = map[byte]rune{
	0x8E: 'é', // U+00E9 LATIN SMALL LETTER E WITH ACUTE
	0xA9: '©', // U+00A9 COPYRIGHT SIGN
	0xE9: 'È', // U+00C8 LATIN CAPITAL LETTER E WITH GRAVE
}

type oracleListing struct {
	path       string
	volumeName string
	freeBytes  uint64
	entries    map[string]oracleEntry
}

// decodeOraclePath converts an oracle path, whose segments are the raw catalog
// name bytes, into the string this package would produce for it.
//
// hfsutils prints name bytes verbatim rather than escaping them, which this
// test originally assumed otherwise and got wrong: it read a filename that
// genuinely contains the ASCII text "\251" as the single byte 0xA9 and then
// reported libhfs for disagreeing. The corpus names that look like octal
// escapes really are backslashes on disk — see the note in
// testdata/gen_classic_corpus.sh.
func decodeOraclePath(vol *Volume, p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" {
			continue
		}
		segs[i] = vol.decodeHFSName([]byte(s))
	}
	return strings.Join(segs, "/")
}

// loadOracle reads the listing beside the corpus image, if one was generated.
func loadOracle(tb testing.TB, vol *Volume) *oracleListing {
	tb.Helper()

	img := os.Getenv(corpusEnvVar)
	if img == "" {
		tb.Skipf("set %s to a raw HFS image to run corpus tests", corpusEnvVar)
	}
	path := img + ".oracle.tsv"
	f, err := os.Open(path)
	if err != nil {
		tb.Skipf("no oracle beside %s; generate one with testdata/gen_oracle.sh", img)
	}
	defer f.Close()

	out := &oracleListing{path: path, entries: map[string]oracleEntry{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for line := 1; sc.Scan(); line++ {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "V":
			if len(fields) < 3 {
				tb.Fatalf("%s:%d: malformed volume line", path, line)
			}
			out.volumeName = fields[1]
			out.freeBytes = mustParseUint(tb, path, line, fields[2])
		case "d":
			if len(fields) < 3 {
				tb.Fatalf("%s:%d: malformed directory line", path, line)
			}
			out.entries[decodeOraclePath(vol, fields[1])] = oracleEntry{
				isDir:     true,
				itemCount: mustParseUint(tb, path, line, fields[2]),
				rawPath:   fields[1],
			}
		case "f":
			if len(fields) < 4 {
				tb.Fatalf("%s:%d: malformed file line", path, line)
			}
			out.entries[decodeOraclePath(vol, fields[1])] = oracleEntry{
				dataSize: mustParseUint(tb, path, line, fields[2]),
				rsrcSize: mustParseUint(tb, path, line, fields[3]),
				rawPath:  fields[1],
			}
		}
	}
	if err := sc.Err(); err != nil {
		tb.Fatalf("reading %s: %v", path, err)
	}
	if len(out.entries) == 0 {
		tb.Skipf("%s lists no entries — the volume is empty, so there is nothing to cross-check", path)
	}
	return out
}

func mustParseUint(tb testing.TB, path string, line int, s string) uint64 {
	tb.Helper()
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		tb.Fatalf("%s:%d: %q is not a number: %v", path, line, s, err)
	}
	return v
}

// libhfsListing walks the catalog and returns the same shape the oracle has.
func libhfsListing(tb testing.TB, vol *Volume) map[string]oracleEntry {
	tb.Helper()

	got := map[string]oracleEntry{}
	err := vol.WalkPaths(func(path string, rec CatalogRecord) error {
		if path == "" || path == "/" {
			return nil
		}
		switch rec.Type {
		case CatalogRecordFolder:
			got[path] = oracleEntry{isDir: true, itemCount: uint64(rec.Valence)}
		case CatalogRecordFile:
			got[path] = oracleEntry{
				dataSize: rec.DataFork.LogicalSize,
				rsrcSize: rec.RsrcFork.LogicalSize,
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("WalkPaths: %v", err)
	}
	return got
}

// TestCorpusMatchesOracle is the cross-check: every file hfsutils sees, libhfs
// must see, at the same size, and vice versa.
func TestCorpusMatchesOracle(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	oracle := loadOracle(t, vol)
	got := libhfsListing(t, vol)

	// The premise: both sides actually found something. Without this the
	// comparisons below pass trivially on an empty map.
	if len(got) == 0 {
		t.Fatalf("libhfs found no catalog entries, so agreement with the oracle would be vacuous")
	}
	t.Logf("comparing %d libhfs entries against %d from %s", len(got), len(oracle.entries), oracle.path)

	var missing, extra, mismatched int
	for path, want := range oracle.entries {
		have, ok := got[path]
		if !ok {
			if missing < 10 {
				t.Errorf("hfsutils lists %q, libhfs does not", path)
			}
			missing++
			continue
		}
		if have.isDir != want.isDir {
			t.Errorf("%q: libhfs says isDir=%v, hfsutils says %v", path, have.isDir, want.isDir)
			mismatched++
			continue
		}
		if want.isDir {
			if have.itemCount != want.itemCount {
				if mismatched < 10 {
					t.Errorf("%q: libhfs valence %d, hfsutils %d items",
						path, have.itemCount, want.itemCount)
				}
				mismatched++
			}
			continue
		}
		if have.dataSize != want.dataSize || have.rsrcSize != want.rsrcSize {
			if mismatched < 10 {
				t.Errorf("%q: libhfs data=%d rsrc=%d, hfsutils data=%d rsrc=%d",
					path, have.dataSize, have.rsrcSize, want.dataSize, want.rsrcSize)
			}
			mismatched++
		}
	}
	for path := range got {
		if _, ok := oracle.entries[path]; !ok {
			if extra < 10 {
				t.Errorf("libhfs lists %q, hfsutils does not", path)
			}
			extra++
		}
	}
	if missing+extra+mismatched > 0 {
		t.Errorf("totals: %d missing, %d extra, %d mismatched of %d oracle entries",
			missing, extra, mismatched, len(oracle.entries))
	}
}

// TestCorpusHighBitNamesDecoded checks that a name a third-party writer stored
// with high-bit bytes comes back as the code points Mac OS Roman assigns them.
//
// This is the one name assertion in this file that does not go through the
// package's own table on both sides: the expected runes come from
// macRomanStandard above. Without it, swapping decodeMacRoman for a plain byte
// widening leaves every other test here passing.
func TestCorpusHighBitNamesDecoded(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	oracle := loadOracle(t, vol)
	got := libhfsListing(t, vol)

	checked := 0
	for decoded, entry := range oracle.entries {
		raw := entry.rawPath
		if !hasHighBit(raw) {
			continue
		}
		want, ok := expectedMacRoman(raw)
		if !ok {
			t.Logf("%q contains a high-bit byte outside macRomanStandard; not checked", raw)
			continue
		}
		if _, ok := got[want]; !ok {
			t.Errorf("expected MacRoman decoding %q (from bytes %q) not found among libhfs paths; "+
				"this package decoded it as %q", want, raw, decoded)
			continue
		}
		checked++
	}

	if checked == 0 {
		t.Skip("no corpus name uses a high-bit byte this test knows the mapping for")
	}
	t.Logf("checked %d high-bit name(s) against the Mac OS Roman standard", checked)
}

func hasHighBit(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// expectedMacRoman renders an oracle path using the standard table above,
// reporting false if it contains a high-bit byte that table does not cover.
func expectedMacRoman(raw string) (string, bool) {
	var b strings.Builder
	for i := range len(raw) {
		c := raw[i]
		if c < 0x80 {
			b.WriteByte(c)
			continue
		}
		r, ok := macRomanStandard[c]
		if !ok {
			return "", false
		}
		b.WriteRune(r)
	}
	return b.String(), true
}

// TestCorpusVolumeMatchesOracle cross-checks the volume header rather than the
// catalog: the name the writer recorded, and the free space it claims.
func TestCorpusVolumeMatchesOracle(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	oracle := loadOracle(t, vol)
	hdr := vol.Header()

	if oracle.volumeName != "" {
		want := vol.decodeHFSName([]byte(oracle.volumeName))

		got, err := vol.VolumeName()
		if err != nil {
			t.Fatalf("VolumeName: %v", err)
		}
		if got != want {
			t.Errorf("VolumeName = %q, hfsutils reads %q", got, want)
		}

		// The root catalog record is a second, independent copy of the name on
		// classic HFS — VolumeName reads the MDB there, not the catalog — so
		// the two agreeing is worth asserting rather than assuming.
		root, err := vol.GetRootDirectory()
		if err != nil {
			t.Fatalf("GetRootDirectory: %v", err)
		}
		if root.Name != want {
			t.Errorf("root record name = %q, hfsutils reads %q from the MDB", root.Name, want)
		}
	}

	// hfsutils reports free space as the writer's own claim: the MDB's free
	// block count times the allocation block size. That is what Header exposes,
	// so the two must agree exactly.
	headerFree := uint64(hdr.FreeBlocks) * uint64(hdr.BlockSize)
	if headerFree != oracle.freeBytes {
		t.Errorf("header free bytes = %d (%d blocks x %d), hfsutils says %d",
			headerFree, hdr.FreeBlocks, hdr.BlockSize, oracle.freeBytes)
	}
}

// TestCorpusFreeBlockCountAgreesWithHeader compares the bitmap-derived free
// count against the header's claim, with the oracle confirming which one is
// right.
//
// These are two different measurements: the header records what the writer
// believed, and FreeBlockCount counts zero bits in the allocation bitmap. On a
// cleanly unmounted volume they must agree, and the oracle pins the header side
// to a third party's reading, so a disagreement here is a bitmap-addressing
// fault rather than an argument about which number is meaningful.
func TestCorpusFreeBlockCountAgreesWithHeader(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	oracle := loadOracle(t, vol)
	hdr := vol.Header()

	counted, err := vol.FreeBlockCount()
	if err != nil {
		t.Fatalf("FreeBlockCount: %v", err)
	}
	if uint64(hdr.FreeBlocks)*uint64(hdr.BlockSize) != oracle.freeBytes {
		t.Skipf("header free space already disagrees with the oracle; " +
			"TestCorpusVolumeMatchesOracle reports that")
	}
	if counted != hdr.FreeBlocks {
		t.Errorf("FreeBlockCount = %d, header (confirmed by hfsutils) = %d; difference %d blocks",
			counted, hdr.FreeBlocks, int64(counted)-int64(hdr.FreeBlocks))
	}
}

// oracleSummary is used by the failure messages above to keep them short.
func (e oracleEntry) String() string {
	if e.isDir {
		return fmt.Sprintf("dir(%d items)", e.itemCount)
	}
	return fmt.Sprintf("file(data=%d rsrc=%d)", e.dataSize, e.rsrcSize)
}
