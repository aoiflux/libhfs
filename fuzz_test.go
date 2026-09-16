package libhfs

import (
	"bytes"
	"math"
	"testing"
)

// Fuzz targets assert one property: arbitrary bytes must never panic and never
// provoke an unbounded allocation. Wrong answers on garbage input are
// acceptable; crashing on it is not, because a forensic tool is routinely
// pointed at damaged and hostile images.
//
// Run longer than the seed corpus with, for example:
//
//	go test -run '^$' -fuzz FuzzOpen -fuzztime 60s
//
// FuzzOpen's progress line freezes a few seconds in — "execs: N (0/sec)" for
// the rest of the run — and that is an accounting artefact of go test -fuzz,
// not a hang here. Tracing every call showed the worker executing 24,380
// inputs while the coordinator still reported 101; a ten-second watchdog
// around the body never fired in seventy seconds; and every seed, every
// testdata entry and the whole cached corpus each run in about a millisecond.
// Clearing the cache does not change it, and a small-input target in this file
// sustains 180k execs/sec unaffected. Fuzzing is working; only the counter is
// wrong, so judge a run by whether it reports a failure, not by the rate.

func FuzzOpen(f *testing.F) {
	f.Add(buildValidCatalogImage(f))
	f.Add(buildClassicHFSTimesImage(f))
	f.Add(buildXAttrImage(f))
	f.Add(buildWrappedHFSPlusImage(f))
	// A volume whose extents B-tree header names a root node the tree does not
	// contain. No corpus image has that shape, and it is the one that sends
	// fork resolution down the degradation fallback.
	f.Add(buildClassicHFSOverflowImage(f, 99))
	// A volume whose records point at each other: a hard-link stub, the inode
	// it names, and two symlinks. Every other seed is a tree of self-contained
	// records, so nothing reaches the resolution loop that follows one record
	// to another.
	f.Add(buildLinkImage(f))
	// Both private hard-link stores on one volume, so the fuzzer can corrupt
	// the directory-link path as well as the file-link one.
	f.Add(buildDirLinkImage(f, dlOpts{stubFlags: hfsHasLinkChainMask}))
	f.Add(make([]byte, volumeHeaderOffset+volumeHeaderSize))
	f.Add([]byte("not a filesystem"))

	f.Fuzz(func(t *testing.T, data []byte) {
		vol, err := Open(bytes.NewReader(data))
		if err != nil {
			return
		}
		// A volume that opened must survive being used.
		vol.SetMaxAlloc(1 << 20)

		_, _ = vol.GetRootDirectory()
		_, _ = vol.ReadDir("/")
		_ = vol.WalkCatalog(func(CatalogRecord) error { return nil })
		_, _ = vol.OpenPath("/anything")
		_, _ = vol.PathForCNID(rootFolderCNID)
		_ = vol.Capabilities()

		_ = vol.BaseOffset()
		_, _ = vol.BlockOffset(0)
		_, _ = vol.DataForkRanges(rootFolderCNID)
		_, _ = vol.ResourceForkRanges(rootFolderCNID)
		_, _ = vol.VolumeIdentifier()
		_, _ = vol.UUID()
		// VolumeName reads a length-prefixed field out of the MDB on classic
		// HFS, so it is the kind of thing an adversarial length byte breaks.
		_, _ = vol.VolumeName()
		_ = vol.WalkPaths(func(string, CatalogRecord) error { return nil })
		// Report(nil) omits the file listing by default, which is what keeps
		// this affordable to fuzz; the listing itself is covered separately.
		_, _ = vol.Report(nil)
	})
}

func FuzzParseCatalogRecord(f *testing.F) {
	f.Add(buildFolderRecord(100, 3), true)
	f.Add(buildFileRecord(101), true)
	f.Add(buildThreadRecord(catalogRecordFileThread, 2, "name"), true)
	f.Add(buildFolderRecordHFS(100, 2, fullTimesFixture()), false)
	f.Add(buildFileRecordHFS(101, fullTimesFixture()), false)
	f.Add([]byte{}, true)

	f.Fuzz(func(t *testing.T, payload []byte, hfsPlus bool) {
		vol := &Volume{kind: KindHFSP, maxAlloc: DefaultMaxAlloc}
		if !hfsPlus {
			vol.kind = KindHFS
			vol.header.BlockSize = 4096
		}
		key := CatalogKey{ParentCNID: 2, NameBytes: []byte("x")}
		_, _ = vol.decodeCatalogRecord(key, payload)
	})
}

func FuzzParseBTreeNode(f *testing.F) {
	f.Add(makeNode(512, btreeNodeTypeLeaf, [][]byte{
		append(buildCatalogKey(2, "a"), buildFolderRecord(100, 0)...),
	}))
	f.Add(makeNode(512, btreeNodeTypeIdx, [][]byte{
		append(buildCatalogKey(2, ""), u32be(3)...),
	}))
	f.Add(make([]byte, 512))
	f.Add([]byte{0, 1, 2})

	f.Fuzz(func(t *testing.T, node []byte) {
		desc, err := parseBTreeNodeDescriptor(node)
		if err != nil {
			return
		}
		// Both extraction paths must tolerate anything.
		recs := extractNodeRecords(node, desc)
		if got := len(recs); got > int(desc.NumRecords) {
			t.Fatalf("extractNodeRecords returned %d records, more than the declared %d",
				got, desc.NumRecords)
		}
		if strict, err := orderedNodeRecords(node, desc); err == nil {
			if len(strict) != int(desc.NumRecords) {
				t.Fatalf("orderedNodeRecords returned %d, want %d", len(strict), desc.NumRecords)
			}
		}
		_, _ = nodeSlackRegion(node, desc)
		_, _ = parseNodeRecordOffsets(node, desc.NumRecords)
	})
}

func FuzzParseKeys(f *testing.F) {
	f.Add(buildCatalogKey(2, "etc"))
	f.Add(buildCatalogKeyHFS(2, "ETC"))
	f.Add(buildAttrKey(100, 0, "com.apple.decmpfs"))
	f.Add([]byte{0xFF, 0xFF})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, raw []byte) {
		for _, kind := range []FileSystemKind{KindHFSP, KindHFSX, KindHFS} {
			if key, consumed, err := parseCatalogKeyForKind(kind, raw); err == nil {
				if consumed < 0 || consumed > len(raw) {
					t.Fatalf("catalog key consumed %d of %d bytes", consumed, len(raw))
				}
				_ = key.NameString()
			}
			if _, consumed, err := parseExtentsKeyForKind(kind, raw); err == nil {
				if consumed < 0 || consumed > len(raw) {
					t.Fatalf("extents key consumed %d of %d bytes", consumed, len(raw))
				}
			}
		}
		if _, consumed, err := parseAttributesKey(raw); err == nil {
			if consumed < 0 || consumed > len(raw) {
				t.Fatalf("attributes key consumed %d of %d bytes", consumed, len(raw))
			}
		}
	})
}

func FuzzDecmpfs(f *testing.F) {
	f.Add(buildDecmpfsAttr(CompressionZlibInline, 16, []byte{0x0F, 'a'}), uint64(16))
	f.Add(buildDecmpfsAttr(CompressionRawInline, 4, []byte("data")), uint64(4))
	f.Add([]byte("fpmc"), uint64(0))
	f.Add([]byte{}, uint64(1<<40))
	// A well-formed resource fork, so the fork branch below starts from an
	// input with a real chunk table. Every other seed here is an inline
	// attribute or junk, and those die at decodeDecmpfsResourceFork's first
	// length guard, leaving the chunk-table arithmetic unreached.
	f.Add(buildDecmpfsResourceFork(f, bytes.Repeat([]byte("chunk payload; "), 40)), uint64(600))

	f.Fuzz(func(t *testing.T, attr []byte, size uint64) {
		vol := &Volume{maxAlloc: 1 << 20}

		if h, ok := parseDecmpfsHeader(attr); ok {
			payload := attr[min(len(attr), decmpfsHeaderSize):]
			out, err := vol.decodeInlinePayload(h, payload)
			if err == nil && uint64(len(out)) > h.UncompressedSize {
				t.Fatalf("decoded %d bytes for a declared size of %d", len(out), h.UncompressedSize)
			}
		}

		// The resource-fork path takes the raw fork bytes and a size.
		if size < 1<<24 {
			_, _ = decodeDecmpfsResourceFork(attr, size, inflateZlib)
		}
		_, _ = inflateZlib(attr, 4096)
	})
}

func FuzzWalkDeleted(f *testing.F) {
	f.Add(buildImageWithDeletedRecord(f, 400, "gone.txt"))
	f.Add(buildValidCatalogImage(f))
	f.Add(make([]byte, volumeHeaderOffset+volumeHeaderSize))

	f.Fuzz(func(t *testing.T, data []byte) {
		vol, err := Open(bytes.NewReader(data))
		if err != nil {
			return
		}
		vol.SetMaxAlloc(1 << 20)

		// Carving is the path most exposed to malformed input: it decodes bytes
		// that were never meant to be records.
		_ = vol.WalkDeleted(&RecoveryOptions{
			ScanNodeSlack:      true,
			ScanFreeNodes:      true,
			IncludeStaleCopies: true,
		}, func(DeletedRecord) error { return nil })
	})
}

func FuzzXAttr(f *testing.F) {
	f.Add(buildXAttrImage(f))
	f.Add(buildValidCatalogImage(f))

	f.Fuzz(func(t *testing.T, data []byte) {
		vol, err := Open(bytes.NewReader(data))
		if err != nil {
			return
		}
		vol.SetMaxAlloc(1 << 20)

		_ = vol.WalkXAttrs(func(XAttr) error { return nil })
		for _, cnid := range []uint32{rootFolderCNID, 100, 101} {
			attrs, err := vol.ListXAttrs(cnid)
			if err != nil {
				continue
			}
			for _, a := range attrs {
				_, _ = vol.ReadXAttr(cnid, a.Name)
				_, _ = vol.XAttrRanges(cnid, a.Name)
			}
		}
	})
}

// buildValidCatalogImage and friends take a testing.TB, so the fuzz seeds can
// reuse the same fixtures the unit tests do.
var (
	_ = func() bool { return true }()
)

// Non-fuzz guards for the same properties, so they run in the ordinary suite.
func TestParsersRejectTruncatedInput(t *testing.T) {
	full := buildValidCatalogImage(t)

	// Every truncation of a valid image must either open or fail cleanly.
	for _, n := range []int{0, 1, 512, 1023, 1024, 1500, 2048, 4096, len(full) / 2, len(full) - 1} {
		if n > len(full) {
			continue
		}
		vol, err := Open(bytes.NewReader(full[:n]))
		if err != nil {
			continue
		}
		_, _ = vol.GetRootDirectory()
		_ = vol.WalkCatalog(func(CatalogRecord) error { return nil })
		_, _ = vol.ReadDir("/")
		_ = vol.WalkPaths(func(string, CatalogRecord) error { return nil })
		_, _ = vol.DataForkRanges(rootFolderCNID)
		_, _ = vol.Report(nil)
	}
}

func TestParsersRejectCorruptedBytes(t *testing.T) {
	base := buildValidCatalogImage(t)

	// Flip bytes at structurally significant offsets and confirm nothing panics.
	offsets := []int{
		volumeHeaderOffset,      // signature
		volumeHeaderOffset + 40, // block size
		volumeHeaderOffset + 44, // total blocks
		volumeHeaderOffset + 272 + 16,
		int(2 * 4096),          // catalog header node
		int(2*4096) + 14,       // node descriptor
		int(2*4096) + 2048,     // index node
		int(2*4096) + 2048 + 8, // node type
	}
	for _, off := range offsets {
		if off >= len(base) {
			continue
		}
		for _, val := range []byte{0x00, 0xFF, 0x7F} {
			img := append([]byte(nil), base...)
			img[off] = val

			vol, err := Open(bytes.NewReader(img))
			if err != nil {
				continue
			}
			vol.SetMaxAlloc(1 << 20)
			_, _ = vol.GetRootDirectory()
			_ = vol.WalkCatalog(func(CatalogRecord) error { return nil })
			_, _ = vol.ReadDirCNID(rootFolderCNID)
			_, _ = vol.RecoverDeleted(nil)
			_ = vol.WalkPaths(func(string, CatalogRecord) error { return nil })
			_, _ = vol.DataForkRanges(rootFolderCNID)
			_, _ = vol.Report(&ReportOptions{IncludeFiles: true, MaxFiles: 64})
		}
	}
}

// FuzzOpenAt fuzzes the path a caller-supplied base offset takes.
//
// FuzzOpen only ever opens at offset zero, where volumeStart is zero and the
// composition in newVolume is a no-op. Here the same adversarial bytes are
// placed part-way through a larger buffer, so baseOffset is non-zero for every
// input — which is where BlockOffset's overflow arithmetic lives, and where a
// geometry that would merely be rejected at offset zero can instead produce an
// offset that overflows.
//
// The offset itself is fuzzed rather than fixed, because a value near MaxInt64
// is the one that matters and no fixed seed would find it.
func FuzzOpenAt(f *testing.F) {
	for _, seed := range [][]byte{
		buildValidCatalogImage(f),
		buildClassicHFSTimesImage(f),
		buildWrappedHFSPlusImage(f),
		buildXAttrImage(f),
		[]byte("not a filesystem"),
	} {
		f.Add(seed, int64(0))
		f.Add(seed, embedOffset)
		f.Add(seed, int64(-1))
		f.Add(seed, int64(math.MaxInt64))
		f.Add(seed, int64(math.MaxInt64)-embedOffset)
	}

	f.Fuzz(func(t *testing.T, data []byte, off int64) {
		// The volume is placed at a bounded offset in a real buffer, while the
		// offset handed to the library is whatever the fuzzer chose. The two
		// agreeing is the ordinary case; them disagreeing is what exercises the
		// rejection paths.
		padded := make([]byte, int(embedOffset)+len(data))
		for i := range int(embedOffset) {
			padded[i] = embedFill
		}
		copy(padded[embedOffset:], data)

		vol, err := OpenWithConfig(bytes.NewReader(padded), Config{BaseOffset: off})
		if err != nil {
			return
		}
		if off < 0 {
			t.Fatalf("a negative BaseOffset (%d) opened a volume", off)
		}
		if vol.BaseOffset() < 0 {
			t.Fatalf("BaseOffset() = %d is negative for a volume that opened at %d",
				vol.BaseOffset(), off)
		}

		vol.SetMaxAlloc(1 << 20)
		_, _ = vol.GetRootDirectory()
		_, _ = vol.ReadDir("/")
		_ = vol.WalkCatalog(func(CatalogRecord) error { return nil })
		_ = vol.WalkPaths(func(string, CatalogRecord) error { return nil })
		_, _ = vol.BlockOffset(0)
		_, _ = vol.BlockOffset(^uint32(0))
		_, _ = vol.DataForkRanges(rootFolderCNID)
		_, _ = vol.ResourceForkRanges(rootFolderCNID)
		_, _ = vol.FreeBlockCount()
		_, _ = vol.VolumeName()
		_, _ = vol.Report(nil)
	})
}
