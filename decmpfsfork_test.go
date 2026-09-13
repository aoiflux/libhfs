package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// Resource-fork decmpfs compression had no end-to-end coverage at all.
//
// decodeDecmpfsResourceFork — the pure chunk-table parser — is well tested
// against fork images built by hand. What was not tested is everything that
// wires it to a volume: decodeResourceForkPayload sat at 22% coverage, its only
// executed statement being the first guard, and readAllBounded at 0%. Codec
// selection, opening the resource fork of a record whose data fork is empty,
// bounding the raw read, and hydrating the record's reported size had never run
// together against an image.
//
// That gap cannot be closed with a real volume. Resource-fork compression is
// how macOS stores any compressed file larger than a few hundred bytes, but
// writing one needs a macOS host: no image in the corpus contains a compressed
// file, and none can, because HFS+ cannot be populated on this machine. A
// hermetic fixture is the only way this path gets tested at all.

const (
	dcfCNID       = uint32(101)
	dcfName       = "squeezed.bin"
	dcfBlockSize  = uint32(4096)
	dcfNodeSize   = uint16(512)
	dcfCatalogBlk = uint32(2)
	dcfAttrBlk    = uint32(3)
	dcfForkBlk    = uint32(4)
)

// dcfPayload is what the compressed file must decompress to. It spans more than
// one 64 KiB chunk, so the chunk table is walked rather than short-circuited,
// and it is repetitive so the zlib fixture stays small.
func dcfPayload() []byte {
	return bytes.Repeat([]byte("decmpfs resource fork payload block; "), 2000)
}

// rawChunk stores a chunk verbatim, which is what the CompressionRawFork types
// mean by "compressed".
func rawChunk(_ testing.TB, in []byte) []byte { return append([]byte(nil), in...) }

// buildDecmpfsAttrValue builds the value of a com.apple.decmpfs attribute: a
// 16-byte header of magic, then the compression type and the uncompressed size,
// both little-endian. A fork-stored type carries no payload after the header —
// the bytes live in the resource fork.
func buildDecmpfsAttrValue(compType uint32, uncompressedSize uint64) []byte {
	v := make([]byte, 16)
	copy(v[0:4], decmpfsMagic[:])
	binary.LittleEndian.PutUint32(v[4:8], compType)
	binary.LittleEndian.PutUint64(v[8:16], uncompressedSize)
	return v
}

// buildCompressedFileRecord builds an HFSPlusCatalogFile whose data fork is
// empty and whose resource fork holds the given extents.
//
// The empty data fork is the point rather than an omission: a decmpfs file
// records a zero-length data fork, and hydrateCompressedRecord keys off exactly
// that when deciding whether to look for a decmpfs attribute. A record built by
// the ordinary buildFileRecord, which fills the data fork in, would never be
// treated as compressed however correct the attribute was.
func buildCompressedFileRecord(cnid uint32, rsrcSize uint64, rsrcExts []ExtentDescriptor) []byte {
	r := make([]byte, 248)
	binary.BigEndian.PutUint16(r[0:2], catalogRecordFile)
	binary.BigEndian.PutUint32(r[8:12], cnid)

	// Data fork at 88 stays zero. Resource fork at 168: logicalSize, clumpSize,
	// totalBlocks, then eight extent descriptors.
	rf := r[168:248]
	binary.BigEndian.PutUint64(rf[0:8], rsrcSize)
	var blocks uint32
	for _, e := range rsrcExts {
		blocks += e.BlockCount
	}
	binary.BigEndian.PutUint32(rf[12:16], blocks)
	for i, e := range rsrcExts {
		if i >= 8 {
			break
		}
		base := 16 + i*8
		binary.BigEndian.PutUint32(rf[base:base+4], e.StartBlock)
		binary.BigEndian.PutUint32(rf[base+4:base+8], e.BlockCount)
	}
	return r
}

// buildDecmpfsForkImage lays out an HFS+ volume holding one file compressed
// into its resource fork: a catalog with the file and its threads, an
// attributes tree carrying the com.apple.decmpfs header, and the fork bytes
// themselves.
func buildDecmpfsForkImage(tb testing.TB, compType uint32, payload, fork []byte) []byte {
	tb.Helper()

	forkBlocks := uint32((len(fork) + int(dcfBlockSize) - 1) / int(dcfBlockSize))
	totalBlocks := dcfForkBlk + forkBlocks
	img := make([]byte, int(totalBlocks)*int(dcfBlockSize))

	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], dcfBlockSize)
	binary.BigEndian.PutUint32(vh[44:48], totalBlocks)
	binary.BigEndian.PutUint32(vh[48:52], 1)

	// Special-file forks: catalog at 272, attributes at 352.
	putFork := func(off int, start, blocks uint32) {
		binary.BigEndian.PutUint64(vh[off:off+8], uint64(blocks)*uint64(dcfBlockSize))
		binary.BigEndian.PutUint32(vh[off+12:off+16], blocks)
		binary.BigEndian.PutUint32(vh[off+16:off+20], start)
		binary.BigEndian.PutUint32(vh[off+20:off+24], blocks)
	}
	putFork(272, dcfCatalogBlk, 1)
	putFork(352, dcfAttrBlk, 1)

	writeNode := func(block, num uint32, node []byte) {
		off := int(block)*int(dcfBlockSize) + int(num)*int(dcfNodeSize)
		copy(img[off:off+int(dcfNodeSize)], node)
	}

	// Catalog, in key order: the root folder filed under its parent, the root's
	// thread, the file, then the file's own thread.
	rsrcExts := []ExtentDescriptor{{StartBlock: dcfForkBlk, BlockCount: forkBlocks}}
	catRecords := [][]byte{
		append(buildCatalogKey(1, "compvol"), buildFolderRecord(rootFolderCNID, 1)...),
		append(buildCatalogKey(rootFolderCNID, ""),
			buildThreadRecord(catalogRecordFolderThread, 1, "compvol")...),
		append(buildCatalogKey(rootFolderCNID, dcfName),
			buildCompressedFileRecord(dcfCNID, uint64(len(fork)), rsrcExts)...),
		append(buildCatalogKey(dcfCNID, ""),
			buildThreadRecord(catalogRecordFileThread, rootFolderCNID, dcfName)...),
	}
	writeNode(dcfCatalogBlk, 0, makeNode(dcfNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(dcfNodeSize, 2, 1, 1)}))
	writeNode(dcfCatalogBlk, 1, makeNode(dcfNodeSize, btreeNodeTypeLeaf, catRecords))

	attrRecords := [][]byte{
		append(buildAttrKey(dcfCNID, 0, decmpfsAttrName),
			buildAttrInlineRecord(buildDecmpfsAttrValue(compType, uint64(len(payload))))...),
	}
	writeNode(dcfAttrBlk, 0, makeNode(dcfNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(dcfNodeSize, 2, 1, 1)}))
	writeNode(dcfAttrBlk, 1, makeNode(dcfNodeSize, btreeNodeTypeLeaf, attrRecords))

	copy(img[int(dcfForkBlk)*int(dcfBlockSize):], fork)
	return img
}

func openDecmpfsForkVolume(t *testing.T, compType uint32, payload, fork []byte) *Volume {
	t.Helper()
	vol, err := Open(bytes.NewReader(buildDecmpfsForkImage(t, compType, payload, fork)))
	if err != nil {
		t.Fatalf("Open decmpfs fork fixture: %v", err)
	}
	return vol
}

// TestDecmpfsResourceForkFileRoundTrip reads a resource-fork compressed file
// through the ordinary file API, which is the path a caller actually uses and
// the one that had never run.
func TestDecmpfsResourceForkFileRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		compType uint32
		encode   func(testing.TB, []byte) []byte
	}{
		{"zlib", CompressionZlibFork, zlibCompress},
		{"raw", CompressionRawFork, rawChunk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := dcfPayload()
			fork := buildDecmpfsResourceForkEnc(t, payload, resourceHeaderSize, tc.encode)
			vol := openDecmpfsForkVolume(t, tc.compType, payload, fork)

			// The premise: the payload really does span more than one chunk, so
			// the chunk table is walked rather than short-circuited on the first
			// entry.
			if len(payload) <= decmpfsChunkSize {
				t.Fatalf("payload of %d bytes fits in one %d-byte chunk; the chunk loop would run once",
					len(payload), decmpfsChunkSize)
			}

			rec, err := vol.OpenCNID(dcfCNID)
			if err != nil {
				t.Fatalf("OpenCNID: %v", err)
			}
			if !rec.Compressed {
				t.Errorf("record is not flagged compressed")
			}
			if rec.CompressionType != tc.compType {
				t.Errorf("CompressionType = %d, want %d", rec.CompressionType, tc.compType)
			}
			// The reported size must be the decompressed length, not the zero
			// the data fork records.
			if rec.DataFork.LogicalSize != uint64(len(payload)) {
				t.Errorf("DataFork.LogicalSize = %d, want %d (the decompressed size)",
					rec.DataFork.LogicalSize, len(payload))
			}

			f, err := vol.OpenFileByCNID(dcfCNID)
			if err != nil {
				t.Fatalf("OpenFileByCNID: %v", err)
			}
			got, err := f.ReadAll()
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("read %d bytes, want %d; equal prefix %d",
					len(got), len(payload), commonPrefixLen(got, payload))
			}

			// The resource fork must still be readable raw. A caller examining
			// how a file was stored needs the stored bytes, and opening the
			// resource fork must not decompress.
			rf, err := vol.OpenResourceForkByCNID(dcfCNID)
			if err != nil {
				t.Fatalf("OpenResourceForkByCNID: %v", err)
			}
			rawBytes, err := rf.ReadAll()
			if err != nil {
				t.Fatalf("ReadAll(resource): %v", err)
			}
			if !bytes.Equal(rawBytes, fork) {
				t.Errorf("resource fork read back %d bytes, want the %d stored", len(rawBytes), len(fork))
			}
		})
	}
}

// TestDecmpfsResourceForkBoundsBothReads pins the two allocation caps on the
// resource-fork path separately, because they guard different numbers and only
// one of them had ever been reachable.
//
// decodeResourceForkPayload caps the decompressed size before doing any work;
// readAllBounded caps the raw fork it must buffer to get there. On a compressed
// file the raw fork is the smaller of the two, so the first check always fires
// first and the second is unobservable. The raw-storage types invert that —
// stored bytes exceed the payload by the header and chunk table — which is what
// makes a limit that separates them possible at all.
func TestDecmpfsResourceForkBoundsBothReads(t *testing.T) {
	payload := dcfPayload()
	fork := buildDecmpfsResourceForkEnc(t, payload, resourceHeaderSize, rawChunk)

	// The premise: raw storage really is larger than what it decodes to, so a
	// limit can sit between them.
	if len(fork) <= len(payload) {
		t.Fatalf("raw fork of %d bytes does not exceed its %d-byte payload; no limit separates the two checks",
			len(fork), len(payload))
	}

	t.Run("raw fork over the cap", func(t *testing.T) {
		vol := openDecmpfsForkVolume(t, CompressionRawFork, payload, fork)
		// Above the decompressed size, below the stored size: the first check
		// must pass and the second must fail.
		vol.SetMaxAlloc(int64(len(payload)) + 1)

		_, err := vol.OpenFileByCNID(dcfCNID)
		assertSizeLimit(t, err, "decmpfs_resource")
	})

	t.Run("decompressed size over the cap", func(t *testing.T) {
		vol := openDecmpfsForkVolume(t, CompressionRawFork, payload, fork)
		vol.SetMaxAlloc(int64(len(payload)) - 1)

		_, err := vol.OpenFileByCNID(dcfCNID)
		assertSizeLimit(t, err, "decmpfs_fork")
	})

	t.Run("cap above both", func(t *testing.T) {
		vol := openDecmpfsForkVolume(t, CompressionRawFork, payload, fork)
		vol.SetMaxAlloc(int64(len(fork)) + 1)

		f, err := vol.OpenFileByCNID(dcfCNID)
		if err != nil {
			t.Fatalf("OpenFileByCNID under a sufficient cap: %v", err)
		}
		got, err := f.ReadAll()
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("read %d bytes, want %d", len(got), len(payload))
		}
	})
}

// TestDecmpfsResourceForkUnsupportedType checks that an unknown fork-stored
// codec is reported as unsupported rather than guessed at.
//
// It matters that this reaches the caller: a file this package cannot
// decompress must not read back as empty or as its compressed bytes, either of
// which an examiner would take at face value.
func TestDecmpfsResourceForkUnsupportedType(t *testing.T) {
	payload := dcfPayload()
	fork := buildDecmpfsResourceForkEnc(t, payload, resourceHeaderSize, zlibCompress)
	vol := openDecmpfsForkVolume(t, CompressionLZFSEFork, payload, fork)

	_, err := vol.OpenFileByCNID(dcfCNID)
	if !errors.Is(err, ErrUnsupportedCompression) {
		t.Fatalf("OpenFileByCNID = %v, want ErrUnsupportedCompression", err)
	}

	// The stored bytes stay reachable, which is the documented escape hatch for
	// exactly this case.
	rf, err := vol.OpenResourceForkByCNID(dcfCNID)
	if err != nil {
		t.Fatalf("OpenResourceForkByCNID: %v", err)
	}
	rawBytes, err := rf.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll(resource): %v", err)
	}
	if !bytes.Equal(rawBytes, fork) {
		t.Errorf("resource fork read back %d bytes, want the %d stored", len(rawBytes), len(fork))
	}
}

// TestDecmpfsResourceForkRegisteredCodec drives the registered-decompressor
// branch of codec selection, which is how a caller adds LZVN or LZFSE without
// this package taking on the dependency.
func TestDecmpfsResourceForkRegisteredCodec(t *testing.T) {
	payload := dcfPayload()
	fork := buildDecmpfsResourceForkEnc(t, payload, resourceHeaderSize, rawChunk)
	vol := openDecmpfsForkVolume(t, CompressionLZFSEFork, payload, fork)

	// Without a codec registered this type is unsupported, so a pass here
	// cannot come from the built-in paths.
	if _, err := vol.OpenFileByCNID(dcfCNID); !errors.Is(err, ErrUnsupportedCompression) {
		t.Fatalf("before registering, OpenFileByCNID = %v, want ErrUnsupportedCompression", err)
	}

	var calls int
	vol.RegisterDecompressor(CompressionLZFSEFork, DecompressorFunc(func(dst, src []byte) (int, error) {
		calls++
		return copy(dst, src), nil
	}))

	f, err := vol.OpenFileByCNID(dcfCNID)
	if err != nil {
		t.Fatalf("OpenFileByCNID with a registered codec: %v", err)
	}
	got, err := f.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("read %d bytes, want %d", len(got), len(payload))
	}
	if calls == 0 {
		t.Error("the registered decompressor was never called")
	}
}

func assertSizeLimit(t *testing.T, err error, wantOp string) {
	t.Helper()
	if !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("error = %v, want ErrSizeLimit", err)
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *ParseError, so which check rejected it cannot be told", err)
	}
	if pe.Op != wantOp {
		t.Errorf("rejected by %q, want %q — the other allocation check fired", pe.Op, wantOp)
	}
}

func commonPrefixLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
