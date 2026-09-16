package libhfs

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Corpus tests run against a real disk image rather than a synthetic fixture.
// They are skipped unless LIBHFS_CORPUS_IMAGE points at one, so the default
// `go test ./...` stays hermetic.
//
// These matter because synthetic fixtures are built from the same reading of
// the spec as the parser, so they cannot catch a misreading. Every bug in this
// file's history was found by a real volume and missed by the fixtures.
const corpusEnvVar = "LIBHFS_CORPUS_IMAGE"

func corpusVolume(tb testing.TB) (*Volume, func()) {
	tb.Helper()
	vol, _, cleanup := corpusImage(tb)
	return vol, cleanup
}

// corpusImage is corpusVolume plus the raw file the volume was opened on.
//
// A test that checks a byte offset the library computed must read the image
// itself to check it. Reading back through the volume would only prove the
// library agrees with itself, which is the one thing a wrong base offset does
// not disturb.
func corpusImage(tb testing.TB) (*Volume, *os.File, func()) {
	tb.Helper()

	path := os.Getenv(corpusEnvVar)
	if path == "" {
		tb.Skipf("set %s to a raw HFS/HFS+ image to run corpus tests", corpusEnvVar)
	}
	f, err := os.Open(path)
	if err != nil {
		tb.Fatalf("open corpus image %s: %v", path, err)
	}
	vol, err := Open(f)
	if err != nil {
		f.Close()
		tb.Fatalf("Open(%s): %v", path, err)
	}
	return vol, f, func() { f.Close() }
}

// skipEmptyCorpus skips when the corpus image genuinely holds no files.
//
// Several corpus tests end in a guard that fails when they examined nothing —
// "the test proved nothing" — because a test which walks zero objects and
// reports success would pass just as readily against a parser that returns
// nothing at all, and that failure mode is silent. The guard is right for an
// image that holds objects the code failed to find. It is wrong for an image
// that holds none: a freshly formatted volume is a legitimate thing to hand a
// forensic tool, and reporting failures against one that fsck and every parse
// agree is healthy teaches the reader to ignore the suite.
//
// The volume header's own FileCount separates the two cases. It is the
// writer's claim rather than a count this package derived, so it cannot be
// wrong in the same direction as the walk being checked —
// TestCorpusCountsMatchVolumeHeader exists precisely because the two
// disagreeing is itself a bug. A volume whose files are all zero-length would
// still trip the guard, which is deliberate: that is rare enough to be worth a
// look, where an empty volume is not.
func skipEmptyCorpus(tb testing.TB, vol *Volume, what string) {
	tb.Helper()
	if vol.Header().FileCount == 0 {
		tb.Skipf("volume holds no files, so it has no %s to check", what)
	}
}

func TestCorpusOpen(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	h := vol.Header()
	t.Logf("kind=%s blockSize=%d totalBlocks=%d freeBlocks=%d files=%d folders=%d",
		vol.Kind(), h.BlockSize, h.TotalBlocks, h.FreeBlocks, h.FileCount, h.FolderCount)

	if h.BlockSize == 0 || h.TotalBlocks == 0 {
		t.Fatalf("implausible geometry: blockSize=%d totalBlocks=%d", h.BlockSize, h.TotalBlocks)
	}
	if h.FreeBlocks > h.TotalBlocks {
		t.Errorf("freeBlocks %d exceeds totalBlocks %d", h.FreeBlocks, h.TotalBlocks)
	}
	if _, err := vol.GetRootDirectory(); err != nil {
		t.Fatalf("GetRootDirectory: %v", err)
	}
}

// The volume header's own file and folder counts are an independent check on
// the walker. They disagreed while node free space was being returned as live
// records, which is exactly the kind of error a synthetic fixture — whose free
// space is zeroed — cannot produce.
func TestCorpusCountsMatchVolumeHeader(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	var files, folders, threads int
	err := vol.WalkCatalog(func(r CatalogRecord) error {
		switch r.Type {
		case CatalogRecordFile:
			files++
		case CatalogRecordFolder:
			folders++
		default:
			threads++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}

	h := vol.Header()
	t.Logf("walked files=%d folders=%d threads=%d; header files=%d folders=%d",
		files, folders, threads, h.FileCount, h.FolderCount)

	if files != int(h.FileCount) {
		t.Errorf("walked %d files, volume header says %d (delta %+d)",
			files, h.FileCount, files-int(h.FileCount))
	}

	// Per TN1150 the volume header's folderCount excludes the root folder,
	// which the catalog walk does see. One more than the header is correct;
	// any other delta is not.
	if wantFolders := int(h.FolderCount) + 1; folders != wantFolders {
		t.Errorf("walked %d folders, want %d (header folderCount %d, which excludes root); delta %+d",
			folders, wantFolders, h.FolderCount, folders-wantFolders)
	}
}

// TestCorpusNodeRecordExtraction checks that record extraction yields exactly
// the number of records the node descriptor declares.
//
// extractNodeRecords derives records from the offset array, which holds
// NumRecords+1 entries — the last being the start of free space, not a record.
// Handing that region back as a record made deleted files reappear as live.
func TestCorpusNodeRecordExtraction(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	hdr, err := vol.CatalogBTreeHeader()
	if err != nil {
		t.Fatalf("CatalogBTreeHeader: %v", err)
	}
	s, err := vol.catalogSearcher()
	if err != nil {
		t.Fatalf("catalogSearcher: %v", err)
	}

	var leaves, declared, viaExtract, viaOrdered, phantom int

	node := hdr.FirstLeafNode
	seen := map[uint32]struct{}{}
	for node != 0 {
		if _, dup := seen[node]; dup {
			break
		}
		seen[node] = struct{}{}

		buf, desc, err := s.readNode(node)
		if err != nil {
			t.Fatalf("readNode(%d): %v", node, err)
		}
		leaves++
		declared += int(desc.NumRecords)

		loose := extractNodeRecords(buf, desc)
		viaExtract += len(loose)

		strict, err := orderedNodeRecords(buf, desc)
		if err != nil {
			t.Fatalf("orderedNodeRecords(%d): %v", node, err)
		}
		viaOrdered += len(strict)

		for i := len(strict); i < len(loose); i++ {
			key, consumed, kerr := parseCatalogKeyForKind(vol.kind, loose[i])
			if kerr != nil || consumed > len(loose[i]) {
				continue
			}
			if _, rerr := vol.decodeCatalogRecord(key, loose[i][consumed:]); rerr == nil {
				phantom++
			}
		}
		node = desc.ForwardLink
	}

	t.Logf("leaf nodes=%d declared=%d extract=%d ordered=%d phantom=%d",
		leaves, declared, viaExtract, viaOrdered, phantom)

	if viaOrdered != declared {
		t.Errorf("orderedNodeRecords returned %d records, descriptors declare %d", viaOrdered, declared)
	}
	if viaExtract != declared {
		t.Errorf("extractNodeRecords returned %d records, descriptors declare %d", viaExtract, declared)
	}
	if phantom > 0 {
		t.Errorf("%d free-space regions decoded as live catalog records", phantom)
	}
}

// A well-formed volume written by macOS must not trip any fallback. An
// anomaly here means keyed descent misread a structure the OS considers valid.
func TestCorpusNoAnomalies(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	if err := vol.WalkCatalog(func(CatalogRecord) error { return nil }); err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}
	entries, err := vol.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir(/): %v", err)
	}
	for _, e := range entries {
		if e.IsDirectory {
			if _, err := vol.ReadDirCNID(e.CNID); err != nil {
				t.Fatalf("ReadDirCNID(%d): %v", e.CNID, err)
			}
		} else {
			if _, err := vol.ResolveDataForkExtents(e.CNID); err != nil {
				t.Fatalf("ResolveDataForkExtents(%d): %v", e.CNID, err)
			}
		}
	}

	if n := vol.AnomalyCount(); n != 0 {
		for _, a := range vol.Anomalies() {
			t.Errorf("anomaly: %s @%d: %s", a.Op, a.Offset, a.Detail)
		}
		t.Fatalf("%d anomalies on a healthy volume", n)
	}
}

// Keyed descent must agree with the exhaustive walk for every record on a real
// volume, not just on a fixture built to suit it.
func TestCorpusKeyedMatchesLinear(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	if !vol.supportsKeyedSearch() {
		t.Skip("volume kind does not use keyed search")
	}

	type node struct {
		cnid uint32
		dir  bool
	}
	var cnids []node
	err := vol.WalkCatalog(func(r CatalogRecord) error {
		switch r.Type {
		case CatalogRecordFile:
			cnids = append(cnids, node{r.CNID, false})
		case CatalogRecordFolder:
			cnids = append(cnids, node{r.CNID, true})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}
	if len(cnids) == 0 {
		t.Fatal("no records found")
	}

	var compared, threadless, dirsCompared int
	for _, n := range cnids {
		cnid := n.cnid
		keyed, kerr := vol.lookupCNIDViaThread(cnid)
		linear, lerr := vol.lookupCNIDLinear(cnid)

		if (kerr == nil) != (lerr == nil) {
			// Classic HFS writes a thread record for every directory and, per
			// Inside Macintosh: Files, only writes one for a file when
			// something asks for a file ID reference. Volumes written by
			// System 7 and by hfsutils have none at all, so the keyed path
			// legitimately cannot see a file that the linear scan finds.
			// HFS+ makes threads mandatory (TN1150), and a directory thread is
			// required on every variant, so neither excuse applies to those:
			// both stay hard failures.
			if vol.Kind() == KindHFS && !n.dir && errors.Is(kerr, ErrNotFound) && lerr == nil {
				threadless++
				continue
			}
			t.Errorf("CNID %d: keyed err=%v, linear err=%v", cnid, kerr, lerr)
			continue
		}
		if kerr != nil {
			continue
		}
		compared++
		if n.dir {
			dirsCompared++
		}
		if keyed.CNID != linear.CNID || keyed.Name != linear.Name ||
			keyed.ParentCNID != linear.ParentCNID || keyed.Type != linear.Type ||
			keyed.DataFork.LogicalSize != linear.DataFork.LogicalSize ||
			!keyed.Times.Created.Equal(linear.Times.Created) {
			t.Errorf("CNID %d disagreement:\n keyed=%+v\nlinear=%+v", cnid, keyed, linear)
		}
	}
	t.Logf("compared %d records via both paths (%d directories); %d files had no thread record",
		compared, dirsCompared, threadless)
	if compared == 0 {
		t.Fatal("no records compared; the test proved nothing")
	}
	// The excuse above is scoped to files. If it ever swallowed the whole
	// volume there would be nothing left comparing keyed descent against the
	// exhaustive walk, which is the only thing this test exists to do.
	if threadless > 0 && dirsCompared == 0 {
		t.Fatal("every comparison was excused as a threadless file; the test proved nothing")
	}
}

func TestCorpusPathRoundTrip(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	var checked, skipped int
	err := vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Type != CatalogRecordFile && r.Type != CatalogRecordFolder {
			return nil
		}
		path, perr := vol.PathForCNID(r.CNID)
		if perr != nil {
			skipped++
			return nil
		}
		// Names holding a path separator cannot round-trip through a
		// slash-delimited path, which is a property of the API, not a bug.
		if strings.Contains(r.Name, "/") {
			skipped++
			return nil
		}
		rec, oerr := vol.OpenPath(path)
		if oerr != nil {
			t.Errorf("OpenPath(%q) for CNID %d: %v", path, r.CNID, oerr)
			return nil
		}
		if rec.CNID != r.CNID {
			t.Errorf("OpenPath(%q).CNID = %d, want %d", path, rec.CNID, r.CNID)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}
	t.Logf("round-tripped %d paths (%d skipped)", checked, skipped)
	if checked == 0 {
		t.Fatal("no paths round-tripped; the test proved nothing")
	}
}

// Reading every file end to end exercises extent resolution against real
// fragmentation, and catches short reads that size checks alone would miss.
func TestCorpusReadAllFiles(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	var files []CatalogRecord
	err := vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Type == CatalogRecordFile {
			files = append(files, r)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}

	var read, empty, fragmented int
	var totalBytes int64
	for _, r := range files {
		if r.DataFork.LogicalSize == 0 {
			empty++
			continue
		}
		exts, eerr := vol.ResolveDataForkExtents(r.CNID)
		if eerr != nil {
			t.Errorf("ResolveDataForkExtents(%d): %v", r.CNID, eerr)
			continue
		}
		if len(exts) > 1 {
			fragmented++
		}

		fh, ferr := vol.OpenFileByCNID(r.CNID)
		if ferr != nil {
			t.Errorf("OpenFileByCNID(%d): %v", r.CNID, ferr)
			continue
		}
		data, rerr := fh.ReadAll()
		if rerr != nil {
			t.Errorf("ReadAll(cnid %d, %q): %v", r.CNID, r.Name, rerr)
			continue
		}
		if int64(len(data)) != int64(r.DataFork.LogicalSize) {
			t.Errorf("cnid %d (%q): read %d bytes, LogicalSize is %d",
				r.CNID, r.Name, len(data), r.DataFork.LogicalSize)
			continue
		}
		read++
		totalBytes += int64(len(data))
	}
	t.Logf("read %d files (%d bytes), %d empty, %d fragmented", read, totalBytes, empty, fragmented)
	if read == 0 {
		skipEmptyCorpus(t, vol, "file contents")
		t.Fatal("no files read; the test proved nothing")
	}
}

// Apple's reserved names use leading NULs so they cannot be typed. Matching
// only on a printable prefix leaves the private-data store looking like
// ordinary user content.
func TestCorpusSystemFilesFlagged(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	entries, err := vol.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir(/): %v", err)
	}

	var reserved, flagged int
	for _, e := range entries {
		looksReserved := strings.Contains(e.Name, "HFS+ Private") ||
			(len(e.Name) > 0 && e.Name[0] == 0x00)
		if !looksReserved {
			continue
		}
		reserved++
		if e.IsSystem {
			flagged++
		} else {
			t.Errorf("reserved name %q not flagged as a system file", e.Name)
		}
	}
	t.Logf("root entries=%d reserved=%d flagged=%d", len(entries), reserved, flagged)
	if reserved == 0 {
		t.Skip("image has no reserved-name entries in the root")
	}
}

func TestCorpusTimestampsPopulated(t *testing.T) {
	vol, cleanup := corpusVolume(t)
	defer cleanup()

	wantSource := TimeSourceHFSPlusGMT
	if vol.Kind() == KindHFS {
		wantSource = TimeSourceHFSLocal
	}

	var total, zeroCreated, zeroModified, badSource int
	var earliest, latest time.Time
	err := vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Type != CatalogRecordFile && r.Type != CatalogRecordFolder {
			return nil
		}
		total++
		if r.Times.Source != wantSource {
			badSource++
		}
		if r.Times.Created.IsZero() {
			zeroCreated++
		} else {
			if earliest.IsZero() || r.Times.Created.Before(earliest) {
				earliest = r.Times.Created
			}
			if r.Times.Created.After(latest) {
				latest = r.Times.Created
			}
		}
		if r.Times.ContentModified.IsZero() {
			zeroModified++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCatalog: %v", err)
	}

	t.Logf("records=%d zeroCreated=%d zeroModified=%d earliest=%v latest=%v",
		total, zeroCreated, zeroModified, earliest, latest)

	if total == 0 {
		t.Fatal("no records")
	}
	if badSource != 0 {
		t.Errorf("%d records carry the wrong TimeSource, want %v", badSource, wantSource)
	}
	// A real volume has creation dates. All-zero would mean the offsets are wrong.
	if zeroCreated == total {
		t.Error("every record has a zero creation date; date offsets are likely wrong")
	}
	if !earliest.IsZero() {
		floor := time.Date(1984, time.January, 1, 0, 0, 0, 0, time.UTC)
		ceiling := time.Now().AddDate(5, 0, 0)
		if earliest.Before(floor) {
			t.Errorf("earliest creation date %v predates the Macintosh", earliest)
		}
		if latest.After(ceiling) {
			t.Errorf("latest creation date %v is implausibly far in the future", latest)
		}
	}
}

func BenchmarkCorpusOpenPath(b *testing.B) {
	vol, cleanup := corpusVolume(b)
	defer cleanup()

	var deepest string
	var depth int
	_ = vol.WalkCatalog(func(r CatalogRecord) error {
		if r.Type != CatalogRecordFile {
			return nil
		}
		if p, err := vol.PathForCNID(r.CNID); err == nil {
			if d := strings.Count(p, "/"); d > depth {
				depth, deepest = d, p
			}
		}
		return nil
	})
	if deepest == "" {
		b.Skip("no path found")
	}
	b.Logf("deepest path (%d components): %s", depth, deepest)

	b.ResetTimer()
	for b.Loop() {
		if _, err := vol.OpenPath(deepest); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCorpusWalkCatalog(b *testing.B) {
	vol, cleanup := corpusVolume(b)
	defer cleanup()

	b.ResetTimer()
	for b.Loop() {
		if err := vol.WalkCatalog(func(CatalogRecord) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

// TestCorpusPathsAndRanges is the acceptance test for the physical-addressing
// and path-resolving APIs against a real volume.
//
// Its decisive assertion is the last one: the bytes read straight out of the
// image at a DiskOffset this package computed must be the file's bytes. Every
// other check here compares the library with itself and so cannot fail on a
// wrong base offset, a wrong block size, or an off-by-one extent — all of which
// return plausible content from elsewhere in the image rather than an error.
func TestCorpusPathsAndRanges(t *testing.T) {
	vol, img, cleanup := corpusImage(t)
	defer cleanup()

	blockSize := int64(vol.Header().BlockSize)
	base := vol.BaseOffset()
	t.Logf("baseOffset=%d blockSize=%d", base, blockSize)

	// --- H2: paths ---

	var walked []PathRecord
	var roots, orphans int
	if err := vol.WalkPaths(func(path string, rec CatalogRecord) error {
		walked = append(walked, PathRecord{Path: path, Record: rec})
		switch {
		case rec.CNID == rootFolderCNID:
			roots++
			if path != "/" {
				t.Errorf("root emitted as %q, want %q", path, "/")
			}
		case path == "":
			orphans++
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkPaths: %v", err)
	}

	h := vol.Header()
	wantRecords := int(h.FileCount) + int(h.FolderCount) + 1 // +1: root, which folderCount excludes
	t.Logf("walked %d paths (%d orphans); header implies %d live records", len(walked), orphans, wantRecords)

	if len(walked) != wantRecords {
		t.Errorf("WalkPaths emitted %d records, want %d", len(walked), wantRecords)
	}
	if roots != 1 {
		t.Errorf("root emitted %d times, want once", roots)
	}
	if orphans != 0 {
		t.Errorf("%d orphaned records on a healthy volume", orphans)
	}

	for _, pr := range walked {
		if pr.Record.CNID == rootFolderCNID {
			continue
		}
		want, err := vol.PathForCNID(pr.Record.CNID)
		if err != nil {
			t.Errorf("PathForCNID(%d): %v", pr.Record.CNID, err)
			continue
		}
		if pr.Path != want {
			t.Errorf("CNID %d: WalkPaths says %q, PathForCNID says %q", pr.Record.CNID, pr.Path, want)
		}
	}

	// The slice sibling must agree with the walk it wraps, in order.
	slice, err := vol.PathRecords()
	if err != nil {
		t.Fatalf("PathRecords: %v", err)
	}
	if len(slice) != len(walked) {
		t.Errorf("PathRecords returned %d records, WalkPaths emitted %d", len(slice), len(walked))
	} else {
		for i := range slice {
			if slice[i].Path != walked[i].Path || slice[i].Record.CNID != walked[i].Record.CNID {
				t.Errorf("PathRecords[%d] = (%q, %d), walk gave (%q, %d)",
					i, slice[i].Path, slice[i].Record.CNID, walked[i].Path, walked[i].Record.CNID)
				break
			}
		}
	}

	// --- H1: ranges ---

	imgSize, err := img.Stat()
	if err != nil {
		t.Fatalf("stat image: %v", err)
	}
	imageLen := imgSize.Size()

	var checked, compressed, empty, multiExtent int
	var dataBytes, slackBytes int64

	for _, pr := range walked {
		rec := pr.Record
		if rec.Type != CatalogRecordFile {
			continue
		}

		ranges, err := vol.DataForkRanges(rec.CNID)
		if err != nil {
			t.Errorf("DataForkRanges(%d, %q): %v", rec.CNID, pr.Path, err)
			continue
		}

		// A decmpfs file keeps its content in an attribute, so it has no data
		// fork to address even though OpenFileByCNID returns bytes for it.
		if rec.Compressed {
			compressed++
			if len(ranges) != 0 {
				t.Errorf("compressed file %q returned %d data-fork ranges, want none", pr.Path, len(ranges))
			}
			continue
		}
		if rec.DataFork.LogicalSize == 0 {
			empty++
			if len(ranges) != 0 {
				t.Errorf("empty file %q returned %d ranges, want none", pr.Path, len(ranges))
			}
			continue
		}
		if len(ranges) == 0 {
			t.Errorf("file %q has LogicalSize %d but no ranges", pr.Path, rec.DataFork.LogicalSize)
			continue
		}
		if len(ranges) > 1 {
			multiExtent++
		}

		var wantForkOffset, sumLength int64
		for i, r := range ranges {
			if r.ForkOffset != wantForkOffset {
				t.Errorf("%q range %d: ForkOffset %d, want %d", pr.Path, i, r.ForkOffset, wantForkOffset)
			}
			if got, want := r.AllocatedLength(), int64(r.BlockCount)*blockSize; got != want {
				t.Errorf("%q range %d: Length+Slack = %d, want BlockCount*BlockSize = %d", pr.Path, i, got, want)
			}
			if want := base + int64(r.StartBlock)*blockSize; r.DiskOffset != want {
				t.Errorf("%q range %d: DiskOffset %d, want base+block = %d", pr.Path, i, r.DiskOffset, want)
			}
			if r.DiskOffset < 0 || r.DiskOffset+r.AllocatedLength() > imageLen {
				t.Errorf("%q range %d: [%d,%d) falls outside the %d-byte image",
					pr.Path, i, r.DiskOffset, r.DiskOffset+r.AllocatedLength(), imageLen)
			}
			switch alloc, err := vol.BlockAllocated(r.StartBlock); {
			case err != nil:
				t.Errorf("%q range %d: BlockAllocated(%d): %v", pr.Path, i, r.StartBlock, err)
			case !alloc:
				t.Errorf("%q range %d: start block %d is not marked allocated", pr.Path, i, r.StartBlock)
			}
			wantForkOffset += r.AllocatedLength()
			sumLength += r.Length
			slackBytes += r.Slack
		}
		if sumLength != int64(rec.DataFork.LogicalSize) {
			t.Errorf("%q: ranges cover %d data bytes, LogicalSize is %d", pr.Path, sumLength, rec.DataFork.LogicalSize)
		}

		// The decisive check: assemble the file from the raw image using only
		// the offsets this package returned, and compare with what the fork
		// reader produces. These two disagree the moment the base offset, the
		// block size or the extent walk is wrong.
		raw := sha256.New()
		for _, r := range ranges {
			if r.Length == 0 {
				continue
			}
			if _, err := io.CopyN(raw, io.NewSectionReader(img, r.DiskOffset, r.Length), r.Length); err != nil {
				t.Errorf("%q: raw read at %d: %v", pr.Path, r.DiskOffset, err)
				raw = nil
				break
			}
		}
		if raw == nil {
			continue
		}

		fh, err := vol.OpenFileByCNID(rec.CNID)
		if err != nil {
			t.Errorf("OpenFileByCNID(%d): %v", rec.CNID, err)
			continue
		}
		via := sha256.New()
		if _, err := io.Copy(via, fh); err != nil {
			t.Errorf("%q: read through the fork reader: %v", pr.Path, err)
			continue
		}
		if !bytes.Equal(raw.Sum(nil), via.Sum(nil)) {
			t.Errorf("%q (cnid %d): bytes read at the returned DiskOffsets differ from the fork reader's",
				pr.Path, rec.CNID)
		}

		checked++
		dataBytes += sumLength
	}

	t.Logf("verified %d files against the raw image (%d data bytes, %d slack bytes); %d multi-extent, %d compressed, %d empty",
		checked, dataBytes, slackBytes, multiExtent, compressed, empty)

	if checked == 0 {
		skipEmptyCorpus(t, vol, "file contents")
		t.Fatal("no file was read back from the raw image; the test proved nothing")
	}
	if n := vol.AnomalyCount(); n != 0 {
		for _, a := range vol.Anomalies() {
			t.Errorf("anomaly: %s @%d: %s", a.Op, a.Offset, a.Detail)
		}
	}
}

// shiftedReaderAt presents r as though it began shiftBy bytes later.
//
// It exists so a corpus test can open a half-gigabyte image at a non-zero base
// offset without copying it: the bytes before shiftBy read as 0xA5, and
// everything at or after it comes from r. That is the same shape as a volume
// sitting in a partition of a larger disk image, which is the case
// Config.BaseOffset exists for.
type shiftedReaderAt struct {
	inner   io.ReaderAt
	shiftBy int64
}

func (s shiftedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	n := 0
	for n < len(p) && off+int64(n) < s.shiftBy {
		p[n] = 0xA5
		n++
	}
	if n == len(p) {
		return n, nil
	}
	got, err := s.inner.ReadAt(p[n:], off+int64(n)-s.shiftBy)
	return n + got, err
}

// TestCorpusBaseOffsetComposes is the H8 acceptance test against a real volume.
//
// The hermetic fixtures are built from the same reading of the spec as the
// parser, so they cannot catch a misreading — and for classic HFS in particular
// the suite has been demonstrably blind to a field read from the wrong offset.
// This opens the corpus image twice, once at the start of the reader and once
// at a non-zero base offset, and asserts the two agree on everything except the
// offsets, which must differ by exactly the shift.
func TestCorpusBaseOffsetComposes(t *testing.T) {
	plain, f, cleanup := corpusImage(t)
	defer cleanup()

	const shift = int64(0x2A07)
	shiftedReader := shiftedReaderAt{inner: f, shiftBy: shift}
	shifted, err := OpenWithConfig(shiftedReader, Config{BaseOffset: shift})
	if err != nil {
		t.Fatalf("OpenWithConfig(BaseOffset=%d) failed: %v", shift, err)
	}

	if plain.Kind() != shifted.Kind() {
		t.Fatalf("Kind differs: %s vs %s", plain.Kind(), shifted.Kind())
	}
	if got, want := shifted.BaseOffset(), plain.BaseOffset()+shift; got != want {
		t.Fatalf("BaseOffset() = %d, want %d — the supplied offset must add to the derived %d, not replace it",
			got, want, plain.BaseOffset())
	}

	// A real classic HFS volume has a non-zero derived base offset, and a real
	// wrapped volume has a large one. On those images this assertion is the
	// whole point: replace-semantics would report exactly shift.
	if plain.BaseOffset() != 0 && shifted.BaseOffset() == shift {
		t.Fatalf("BaseOffset() = %d, which is the supplied offset alone; the derived %d was discarded",
			shifted.BaseOffset(), plain.BaseOffset())
	}

	if pn, sn := plainName(t, plain), plainName(t, shifted); pn != sn {
		t.Fatalf("VolumeName differs: %q vs %q", pn, sn)
	}
	if pf, sf := plain.Header().FileCount, shifted.Header().FileCount; pf != sf {
		t.Fatalf("FileCount differs: %d vs %d", pf, sf)
	}

	// Allocation state comes from the bitmap, which on classic HFS is addressed
	// from the volume start rather than from the allocation-block area — the
	// one place the two offsets must compose differently.
	pFree, err := plain.FreeBlockCount()
	if err != nil {
		t.Fatalf("FreeBlockCount failed: %v", err)
	}
	sFree, err := shifted.FreeBlockCount()
	if err != nil {
		t.Fatalf("FreeBlockCount on the shifted volume failed: %v", err)
	}
	if pFree != sFree {
		t.Fatalf("FreeBlockCount differs: %d vs %d — the bitmap was read from the wrong origin", pFree, sFree)
	}

	skipEmptyCorpus(t, plain, "files to compare byte ranges for")

	// Every reported range must move by exactly the shift, and the bytes at the
	// shifted offset must still be the file's. Bounded, because this runs
	// against half-gigabyte images.
	checked := 0
	err = plain.WalkPaths(func(_ string, rec CatalogRecord) error {
		if rec.Type != CatalogRecordFile || rec.DataFork.LogicalSize == 0 {
			return nil
		}
		pr, err := plain.DataForkRanges(rec.CNID)
		if err != nil || len(pr) == 0 {
			return nil
		}
		sr, err := shifted.DataForkRanges(rec.CNID)
		if err != nil {
			t.Fatalf("cnid %d: DataForkRanges on the shifted volume failed: %v", rec.CNID, err)
		}
		if len(pr) != len(sr) {
			t.Fatalf("cnid %d: range counts differ: %d vs %d", rec.CNID, len(pr), len(sr))
		}
		for i := range pr {
			if sr[i].DiskOffset-pr[i].DiskOffset != shift {
				t.Fatalf("cnid %d range %d: DiskOffset moved by %d, want %d",
					rec.CNID, i, sr[i].DiskOffset-pr[i].DiskOffset, shift)
			}
			if pr[i].Length != sr[i].Length {
				t.Fatalf("cnid %d range %d: Length changed: %d vs %d", rec.CNID, i, pr[i].Length, sr[i].Length)
			}
		}

		// Read the first range from the image itself at both offsets. This is
		// the criterion the request states: identical content at offsets that
		// differ by exactly the base offset.
		if n := pr[0].Length; n > 0 && n <= 4096 {
			want := make([]byte, n)
			if _, err := f.ReadAt(want, pr[0].DiskOffset); err != nil {
				return nil
			}
			got := make([]byte, n)
			if _, err := shiftedReader.ReadAt(got, sr[0].DiskOffset); err != nil {
				t.Fatalf("cnid %d: reading the shifted image at DiskOffset failed: %v", rec.CNID, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("cnid %d: content at the shifted DiskOffset differs from the original", rec.CNID)
			}
		}

		checked++
		if checked >= 64 {
			return ErrStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrStopWalk) {
		t.Fatalf("WalkPaths failed: %v", err)
	}
	if checked == 0 {
		t.Fatal("no file with a data fork was compared; the test proved nothing")
	}
	t.Logf("compared %d files at baseOffset %d (derived %d + supplied %d)",
		checked, shifted.BaseOffset(), plain.BaseOffset(), shift)
}

func plainName(tb testing.TB, vol *Volume) string {
	tb.Helper()
	name, err := vol.VolumeName()
	if err != nil {
		return ""
	}
	return name
}
