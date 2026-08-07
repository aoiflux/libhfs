package hfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"testing"
)

func TestParseDecmpfsHeader(t *testing.T) {
	good := make([]byte, decmpfsHeaderSize)
	copy(good[0:4], decmpfsMagic[:])
	binary.LittleEndian.PutUint32(good[4:8], CompressionZlibFork)
	binary.LittleEndian.PutUint64(good[8:16], 123456)

	h, ok := parseDecmpfsHeader(good)
	if !ok {
		t.Fatal("parseDecmpfsHeader rejected a valid header")
	}
	if h.CompressionType != CompressionZlibFork || h.UncompressedSize != 123456 {
		t.Errorf("got %+v, want type %d size 123456", h, CompressionZlibFork)
	}
	if !h.storedInResourceFork() {
		t.Error("type 4 should be resource-fork backed")
	}

	// Odd types live in the attribute.
	h.CompressionType = CompressionZlibInline
	if h.storedInResourceFork() {
		t.Error("type 3 should be inline")
	}

	bad := append([]byte(nil), good...)
	bad[0] = 'x'
	if _, ok := parseDecmpfsHeader(bad); ok {
		t.Error("accepted a header with the wrong magic")
	}
	if _, ok := parseDecmpfsHeader(good[:8]); ok {
		t.Error("accepted a truncated header")
	}
}

// The 0x0F prefix marks a stored (uncompressed) chunk. It is not valid zlib, so
// a decoder that hands it to the zlib reader fails on incompressible data.
func TestInflateZlibStoredChunk(t *testing.T) {
	payload := []byte("incompressible enough")
	src := append([]byte{0x0F}, payload...)

	got, err := inflateZlib(src, len(payload))
	if err != nil {
		t.Fatalf("inflateZlib on a stored chunk: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestInflateZlibRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("compress me "), 500)
	compressed := zlibCompress(t, payload)

	got, err := inflateZlib(compressed, len(payload))
	if err != nil {
		t.Fatalf("inflateZlib: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round trip produced %d bytes, want %d", len(got), len(payload))
	}
}

// A stream claiming to expand enormously must be bounded rather than allowed to
// exhaust memory.
func TestInflateZlibBoundsOutput(t *testing.T) {
	payload := bytes.Repeat([]byte{0}, 1<<20)
	compressed := zlibCompress(t, payload)

	got, err := inflateZlib(compressed, 4096)
	if err != nil {
		t.Fatalf("inflateZlib: %v", err)
	}
	if len(got) > 4096 {
		t.Errorf("returned %d bytes despite a 4096-byte bound", len(got))
	}
}

func TestInflateZlibRejectsGarbage(t *testing.T) {
	if _, err := inflateZlib([]byte{0x01, 0x02, 0x03}, 128); err == nil {
		t.Error("accepted a non-zlib stream")
	}
	if _, err := inflateZlib(nil, 128); err == nil {
		t.Error("accepted an empty stream")
	}
}

// ---------------------------------------------------------------------------
// Inline payloads
// ---------------------------------------------------------------------------

func TestDecmpfsInlineTypes(t *testing.T) {
	payload := []byte("the quick brown fox jumps over the lazy dog")
	vol := &Volume{maxAlloc: DefaultMaxAlloc}

	cases := []struct {
		name string
		typ  uint32
		raw  []byte
	}{
		{"raw type 9", CompressionRawInline, payload},
		{"uncompressed type 1", CompressionNoneInline, payload},
		{"zlib type 3", CompressionZlibInline, zlibCompress(t, payload)},
		{"zlib stored", CompressionZlibInline, append([]byte{0x0F}, payload...)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := decmpfsHeader{CompressionType: tc.typ, UncompressedSize: uint64(len(payload))}
			got, err := vol.decodeInlinePayload(h, tc.raw)
			if err != nil {
				t.Fatalf("decodeInlinePayload: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("got %q, want %q", got, payload)
			}
		})
	}
}

func TestDecmpfsUnsupportedInlineType(t *testing.T) {
	vol := &Volume{maxAlloc: DefaultMaxAlloc}
	h := decmpfsHeader{CompressionType: CompressionLZFSEInline, UncompressedSize: 16}

	_, err := vol.decodeInlinePayload(h, []byte("whatever"))
	if !errors.Is(err, ErrUnsupportedCompression) {
		t.Fatalf("got %v, want ErrUnsupportedCompression", err)
	}
}

// A caller can supply codecs the standard library does not provide.
func TestRegisterDecompressor(t *testing.T) {
	const fakeType = uint32(9999)
	t.Cleanup(func() { RegisterDecompressor(fakeType, nil) })

	RegisterDecompressor(fakeType, DecompressorFunc(func(dst, src []byte) (int, error) {
		// A trivial codec: reverse the bytes.
		for i := range src {
			if i >= len(dst) {
				break
			}
			dst[i] = src[len(src)-1-i]
		}
		return min(len(src), len(dst)), nil
	}))

	vol := &Volume{maxAlloc: DefaultMaxAlloc}
	h := decmpfsHeader{CompressionType: fakeType, UncompressedSize: 5}
	got, err := vol.decodeInlinePayload(h, []byte("olleh"))
	if err != nil {
		t.Fatalf("decodeInlinePayload with a registered codec: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}

	RegisterDecompressor(fakeType, nil)
	if _, err := vol.decodeInlinePayload(h, []byte("olleh")); !errors.Is(err, ErrUnsupportedCompression) {
		t.Errorf("after deregistering, got %v, want ErrUnsupportedCompression", err)
	}
}

// ---------------------------------------------------------------------------
// Resource-fork payloads
//
// NOTE: the chunk-table layout implemented here is taken from published
// descriptions of the format and has NOT been validated against a
// macOS-produced compressed file, because no image containing one was
// available. These tests confirm the decoder is self-consistent and correctly
// bounded; they do not confirm the layout matches what macOS writes. See
// PLAN.md §6.2.
// ---------------------------------------------------------------------------

func TestDecmpfsResourceForkChunks(t *testing.T) {
	// Two chunks: a full one and a partial one.
	payload := make([]byte, decmpfsChunkSize+1000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	fork := buildDecmpfsResourceFork(t, payload)

	got, err := decodeDecmpfsResourceFork(fork, uint64(len(payload)), inflateZlib)
	if err != nil {
		t.Fatalf("decodeDecmpfsResourceFork: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestDecmpfsResourceForkRejectsBadInput(t *testing.T) {
	payload := make([]byte, decmpfsChunkSize+10)
	fork := buildDecmpfsResourceFork(t, payload)

	cases := []struct {
		name string
		fork []byte
		size uint64
	}{
		{"truncated fork", fork[:64], uint64(len(payload))},
		{"empty fork", nil, uint64(len(payload))},
		{"chunk count disagrees with size", fork, uint64(len(payload)) * 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeDecmpfsResourceFork(tc.fork, tc.size, inflateZlib); err == nil {
				t.Error("accepted malformed input")
			}
		})
	}

	// A chunk offset pointing outside the fork must be rejected, not followed.
	bad := append([]byte(nil), fork...)
	entry := 0x100 + 4 + 4 // tableBase + chunk count
	binary.LittleEndian.PutUint32(bad[entry:entry+4], 0xFFFFFF)
	if _, err := decodeDecmpfsResourceFork(bad, uint64(len(payload)), inflateZlib); err == nil {
		t.Error("accepted a chunk offset past the end of the fork")
	}
}

// ---------------------------------------------------------------------------
// End-to-end through a volume
// ---------------------------------------------------------------------------

func TestCompressedFileEndToEnd(t *testing.T) {
	vol := openDecmpfsFixture(t)

	rec, err := vol.OpenCNID(dcInlineCNID)
	if err != nil {
		t.Fatalf("OpenCNID: %v", err)
	}
	if !rec.Compressed {
		t.Error("record not marked compressed")
	}
	if rec.CompressionType != CompressionZlibInline {
		t.Errorf("CompressionType = %d, want %d", rec.CompressionType, CompressionZlibInline)
	}
	if rec.DataFork.LogicalSize != uint64(len(dcPayload)) {
		t.Errorf("LogicalSize = %d, want %d (uncompressed size)",
			rec.DataFork.LogicalSize, len(dcPayload))
	}

	fh, err := vol.OpenFileByCNID(dcInlineCNID)
	if err != nil {
		t.Fatalf("OpenFileByCNID: %v", err)
	}
	got, err := fh.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, dcPayload) {
		t.Errorf("read %d bytes, want %d", len(got), len(dcPayload))
	}
}

// A file compressed with a codec this build cannot decode must fail clearly and
// still allow the raw resource fork to be preserved.
func TestUnsupportedCompressionStillExposesResourceFork(t *testing.T) {
	vol := openDecmpfsFixture(t)

	rec, err := vol.OpenCNID(dcUnsupportedCNID)
	if err != nil {
		t.Fatalf("OpenCNID: %v", err)
	}
	if !rec.Compressed {
		t.Error("record not marked compressed")
	}
	if rec.CompressionType != CompressionLZFSEFork {
		t.Errorf("CompressionType = %d, want %d", rec.CompressionType, CompressionLZFSEFork)
	}

	if _, err := vol.OpenFileByCNID(dcUnsupportedCNID); !errors.Is(err, ErrUnsupportedCompression) {
		t.Errorf("OpenFileByCNID returned %v, want ErrUnsupportedCompression", err)
	}

	// The artifact is still recoverable in raw form.
	rf, err := vol.OpenResourceForkByCNID(dcUnsupportedCNID)
	if err != nil {
		t.Fatalf("OpenResourceForkByCNID: %v", err)
	}
	raw, err := rf.ReadAll()
	if err != nil {
		t.Fatalf("reading the raw resource fork: %v", err)
	}
	if len(raw) == 0 {
		t.Error("raw resource fork is empty; the artifact would be unrecoverable")
	}
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	dcBlockSize        = uint32(4096)
	dcNodeSize         = uint16(1024)
	dcCatalogBlock     = uint32(2)
	dcAttrBlock        = uint32(4)
	dcRsrcBlock        = uint32(8)
	dcInlineCNID       = uint32(200)
	dcUnsupportedCNID  = uint32(202)
	dcUnsupportedBytes = 512
)

var dcPayload = bytes.Repeat([]byte("decmpfs payload! "), 64)

func zlibCompress(t testing.TB, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(in); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

// buildDecmpfsResourceFork lays out the chunked resource fork used by
// even-numbered decmpfs types.
func buildDecmpfsResourceFork(t testing.TB, payload []byte) []byte {
	t.Helper()

	numChunks := (len(payload) + decmpfsChunkSize - 1) / decmpfsChunkSize
	chunks := make([][]byte, numChunks)
	for i := range chunks {
		start := i * decmpfsChunkSize
		end := min(start+decmpfsChunkSize, len(payload))
		chunks[i] = zlibCompress(t, payload[start:end])
	}

	const dataOffset = 0x100
	tableBase := dataOffset + 4 // chunk count sits here
	dataStart := tableBase + 4 + numChunks*8

	total := dataStart
	for _, c := range chunks {
		total += len(c)
	}

	out := make([]byte, total)
	binary.BigEndian.PutUint32(out[0:4], dataOffset)         // resource header
	binary.BigEndian.PutUint32(out[dataOffset:dataOffset+4], // total length
		uint32(total-dataOffset))
	binary.LittleEndian.PutUint32(out[tableBase:tableBase+4], uint32(numChunks))

	pos := dataStart
	for i, c := range chunks {
		entry := tableBase + 4 + i*8
		// Offsets in the table are relative to tableBase.
		binary.LittleEndian.PutUint32(out[entry:entry+4], uint32(pos-tableBase))
		binary.LittleEndian.PutUint32(out[entry+4:entry+8], uint32(len(c)))
		copy(out[pos:], c)
		pos += len(c)
	}
	return out
}

func openDecmpfsFixture(t *testing.T) *Volume {
	t.Helper()
	vol, err := Open(bytes.NewReader(buildDecmpfsImage(t)))
	if err != nil {
		t.Fatalf("Open decmpfs fixture: %v", err)
	}
	return vol
}

func buildDecmpfsImage(t *testing.T) []byte {
	t.Helper()

	const lastBlock = uint32(12)
	img := make([]byte, int(lastBlock+1)*int(dcBlockSize))

	vh := img[volumeHeaderOffset : volumeHeaderOffset+volumeHeaderSize]
	binary.BigEndian.PutUint16(vh[0:2], signatureHFSP)
	binary.BigEndian.PutUint16(vh[2:4], versionHFSPlus)
	binary.BigEndian.PutUint32(vh[40:44], dcBlockSize)
	binary.BigEndian.PutUint32(vh[44:48], lastBlock+1)
	binary.BigEndian.PutUint32(vh[48:52], 4)

	putFork := func(off int, blocks uint32, start uint32) {
		binary.BigEndian.PutUint64(vh[off:off+8], uint64(blocks)*uint64(dcBlockSize))
		binary.BigEndian.PutUint32(vh[off+12:off+16], blocks)
		binary.BigEndian.PutUint32(vh[off+16:off+20], start)
		binary.BigEndian.PutUint32(vh[off+20:off+24], blocks)
	}
	putFork(272, 1, dcCatalogBlock) // catalog
	putFork(352, 1, dcAttrBlock)    // attributes

	writeNodeAt := func(startBlock, num uint32, node []byte) {
		off := int(startBlock)*int(dcBlockSize) + int(num)*int(dcNodeSize)
		copy(img[off:off+int(dcNodeSize)], node)
	}

	// Catalog: a compressed file with no data fork, and one with an
	// unsupported codec whose payload sits in a resource fork.
	inlineRec := buildFileRecord(dcInlineCNID)
	binary.BigEndian.PutUint64(inlineRec[88:96], 0) // empty data fork

	unsupportedRec := buildFileRecord(dcUnsupportedCNID)
	binary.BigEndian.PutUint64(unsupportedRec[88:96], 0)                    // empty data fork
	binary.BigEndian.PutUint64(unsupportedRec[168:176], dcUnsupportedBytes) // resource fork size
	binary.BigEndian.PutUint32(unsupportedRec[168+12:168+16], 1)            // 1 block
	binary.BigEndian.PutUint32(unsupportedRec[168+16:168+20], dcRsrcBlock)
	binary.BigEndian.PutUint32(unsupportedRec[168+20:168+24], 1)

	catRecords := [][]byte{
		append(buildCatalogKey(rootFolderCNID, ""), buildFolderRecord(rootFolderCNID, 2)...),
		append(buildCatalogKey(rootFolderCNID, "big.bin"), unsupportedRec...),
		append(buildCatalogKey(rootFolderCNID, "small.txt"), inlineRec...),
	}
	writeNodeAt(dcCatalogBlock, 0, makeNode(dcNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(dcNodeSize, 2, 1, 1)}))
	writeNodeAt(dcCatalogBlock, 1, makeNode(dcNodeSize, btreeNodeTypeLeaf, catRecords))

	// Attributes: a decmpfs record for each.
	attrRecords := [][]byte{
		append(buildAttrKey(dcInlineCNID, 0, decmpfsAttrName),
			buildAttrInlineRecord(buildDecmpfsAttr(CompressionZlibInline,
				uint64(len(dcPayload)), zlibCompress(t, dcPayload)))...),
		append(buildAttrKey(dcUnsupportedCNID, 0, decmpfsAttrName),
			buildAttrInlineRecord(buildDecmpfsAttr(CompressionLZFSEFork, 4096, nil))...),
	}
	writeNodeAt(dcAttrBlock, 0, makeNode(dcNodeSize, btreeNodeTypeHead,
		[][]byte{buildBTreeHeaderRecordBytesAt(dcNodeSize, 2, 1, 1)}))
	writeNodeAt(dcAttrBlock, 1, makeNode(dcNodeSize, btreeNodeTypeLeaf, attrRecords))

	// Some bytes in the unsupported file's resource fork, so preserving the
	// raw artifact is possible.
	rsrcStart := int(dcRsrcBlock) * int(dcBlockSize)
	for i := range dcUnsupportedBytes {
		img[rsrcStart+i] = byte(i % 256)
	}

	return img
}

// buildDecmpfsAttr assembles the com.apple.decmpfs attribute value.
func buildDecmpfsAttr(compressionType uint32, uncompressedSize uint64, payload []byte) []byte {
	out := make([]byte, decmpfsHeaderSize+len(payload))
	copy(out[0:4], decmpfsMagic[:])
	binary.LittleEndian.PutUint32(out[4:8], compressionType)
	binary.LittleEndian.PutUint64(out[8:16], uncompressedSize)
	copy(out[decmpfsHeaderSize:], payload)
	return out
}
