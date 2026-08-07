package hfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// decmpfs compression types. The value is stored little-endian in the
// com.apple.decmpfs attribute header.
//
// Odd types keep the payload in the attribute itself; even types keep it in the
// resource fork. Types 2, 5 and 6 are reserved or undocumented and are reported
// as unsupported rather than guessed at.
const (
	CompressionNoneInline  = uint32(1)  // stored uncompressed in the attribute
	CompressionZlibInline  = uint32(3)  // zlib, in the attribute
	CompressionZlibFork    = uint32(4)  // zlib, in the resource fork
	CompressionLZVNInline  = uint32(7)  // LZVN, in the attribute
	CompressionLZVNFork    = uint32(8)  // LZVN, in the resource fork
	CompressionRawInline   = uint32(9)  // stored raw in the attribute
	CompressionRawFork     = uint32(10) // stored raw in the resource fork
	CompressionLZFSEInline = uint32(11) // LZFSE, in the attribute
	CompressionLZFSEFork   = uint32(12) // LZFSE, in the resource fork
	CompressionLZBitmapIn  = uint32(13) // LZBITMAP, in the attribute
	CompressionLZBitmapFrk = uint32(14) // LZBITMAP, in the resource fork
)

// decmpfsChunkSize is the uncompressed size of every chunk but the last.
const decmpfsChunkSize = 64 << 10

// decmpfsMagic is 'fpmc' as it appears at the start of the attribute.
var decmpfsMagic = [4]byte{'f', 'p', 'm', 'c'}

// ErrUnsupportedCompression reports a decmpfs compression type this build
// cannot decode.
//
// The file's data is not lost: [Volume.OpenResourceForkByCNID] still returns
// the raw resource fork, so an artifact can be preserved even when it cannot be
// decompressed. [CatalogRecord.CompressionType] says which codec was needed.
var ErrUnsupportedCompression = errors.New("hfs: unsupported decmpfs compression type")

// Decompressor decodes one decmpfs chunk.
//
// Implementations must write the decompressed bytes to dst and return how many
// were written. dst is sized to the chunk's expected uncompressed length, so an
// implementation that would exceed it should return an error rather than
// reallocating.
type Decompressor interface {
	Decompress(dst, src []byte) (int, error)
}

// DecompressorFunc adapts a function to the Decompressor interface.
type DecompressorFunc func(dst, src []byte) (int, error)

func (f DecompressorFunc) Decompress(dst, src []byte) (int, error) { return f(dst, src) }

var (
	codecMu sync.RWMutex
	codecs  = map[uint32]Decompressor{}
)

// RegisterDecompressor installs a codec for a decmpfs compression type,
// replacing any existing registration.
//
// zlib and the uncompressed types are built in. LZVN, LZFSE and LZBITMAP are
// not in the Go standard library, so they are left to the caller to supply —
// this keeps the package dependency-free while letting a consumer that needs
// those codecs opt in:
//
//	hfs.RegisterDecompressor(hfs.CompressionLZFSEFork,
//		hfs.DecompressorFunc(func(dst, src []byte) (int, error) { ... }))
//
// Safe for concurrent use, but intended to be called during initialisation.
func RegisterDecompressor(compressionType uint32, d Decompressor) {
	codecMu.Lock()
	defer codecMu.Unlock()
	if d == nil {
		delete(codecs, compressionType)
		return
	}
	codecs[compressionType] = d
}

func lookupDecompressor(compressionType uint32) (Decompressor, bool) {
	codecMu.RLock()
	defer codecMu.RUnlock()
	d, ok := codecs[compressionType]
	return d, ok
}

// decmpfsHeader is the 16-byte header at the start of the attribute.
type decmpfsHeader struct {
	CompressionType  uint32
	UncompressedSize uint64
}

// storedInResourceFork reports whether the payload lives in the resource fork
// rather than in the attribute. Even compression types use the fork.
func (h decmpfsHeader) storedInResourceFork() bool { return h.CompressionType%2 == 0 }

func parseDecmpfsHeader(attr []byte) (decmpfsHeader, bool) {
	if len(attr) < decmpfsHeaderSize {
		return decmpfsHeader{}, false
	}
	if !bytes.Equal(attr[0:4], decmpfsMagic[:]) {
		return decmpfsHeader{}, false
	}
	return decmpfsHeader{
		CompressionType:  binary.LittleEndian.Uint32(attr[4:8]),
		UncompressedSize: binary.LittleEndian.Uint64(attr[8:16]),
	}, true
}

// inflateZlib decompresses a zlib stream, or returns the bytes unchanged when
// the stream is stored rather than deflated.
//
// A leading byte of 0x0F marks an uncompressed chunk. That is not part of the
// zlib format: Apple uses it so incompressible data can be stored without
// expansion, and a decoder that hands such a chunk to zlib gets an error rather
// than the data.
func inflateZlib(src []byte, maxOut int) ([]byte, error) {
	if len(src) == 0 {
		return nil, &ParseError{Op: "decmpfs_zlib", Offset: 0, Err: ErrCorrupt}
	}
	if src[0] == 0x0F {
		out := src[1:]
		if len(out) > maxOut {
			out = out[:maxOut]
		}
		return append([]byte(nil), out...), nil
	}

	zr, err := zlib.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, &ParseError{Op: "decmpfs_zlib", Offset: 0, Err: ErrCorrupt}
	}
	defer zr.Close()

	// Bound the read so a corrupt stream claiming to expand enormously cannot
	// exhaust memory.
	out, err := io.ReadAll(io.LimitReader(zr, int64(maxOut)))
	if err != nil {
		return nil, &ParseError{Op: "decmpfs_zlib", Offset: 0, Err: ErrCorrupt}
	}
	return out, nil
}

// decodeInlinePayload decompresses a payload held in the attribute itself.
func (v *Volume) decodeInlinePayload(h decmpfsHeader, raw []byte) ([]byte, error) {
	if err := v.checkAlloc("decmpfs_inline", int64(h.UncompressedSize)); err != nil {
		return nil, err
	}
	size := int(h.UncompressedSize)

	switch h.CompressionType {
	case CompressionNoneInline, CompressionRawInline:
		if len(raw) < size {
			return nil, &ParseError{Op: "decmpfs_inline", Offset: 0, Err: ErrCorrupt}
		}
		return append([]byte(nil), raw[:size]...), nil

	case CompressionZlibInline:
		out, err := inflateZlib(raw, size)
		if err != nil {
			return nil, err
		}
		if len(out) != size {
			return nil, &ParseError{Op: "decmpfs_inline", Offset: int64(len(out)), Err: ErrCorrupt}
		}
		return out, nil
	}

	if d, ok := lookupDecompressor(h.CompressionType); ok {
		dst := make([]byte, size)
		n, err := d.Decompress(dst, raw)
		if err != nil {
			return nil, &ParseError{Op: "decmpfs_inline", Offset: 0, Err: err}
		}
		return dst[:n], nil
	}
	return nil, unsupportedCompression(h.CompressionType)
}

func unsupportedCompression(t uint32) error {
	return &ParseError{Op: "decmpfs", Offset: int64(t), Err: ErrUnsupportedCompression}
}

// decodeResourceForkPayload decompresses a payload held in the resource fork.
//
// The layout is a chunk table followed by the chunks. At the start of the fork
// sits a 256-byte resource-fork header whose first word gives the offset of the
// resource data; at that offset is a big-endian total length, then a
// little-endian chunk count, then one (offset, length) pair per chunk. Chunk
// offsets are relative to the start of the chunk-count field.
//
// Every chunk decompresses to 64 KiB except the last, which holds the
// remainder.
func (v *Volume) decodeResourceForkPayload(h decmpfsHeader, rec CatalogRecord) ([]byte, error) {
	if err := v.checkAlloc("decmpfs_fork", int64(h.UncompressedSize)); err != nil {
		return nil, err
	}

	var chunkCodec func(src []byte, maxOut int) ([]byte, error)
	switch h.CompressionType {
	case CompressionZlibFork:
		chunkCodec = inflateZlib
	case CompressionRawFork:
		chunkCodec = func(src []byte, maxOut int) ([]byte, error) {
			if len(src) > maxOut {
				src = src[:maxOut]
			}
			return append([]byte(nil), src...), nil
		}
	default:
		d, ok := lookupDecompressor(h.CompressionType)
		if !ok {
			return nil, unsupportedCompression(h.CompressionType)
		}
		chunkCodec = func(src []byte, maxOut int) ([]byte, error) {
			dst := make([]byte, maxOut)
			n, err := d.Decompress(dst, src)
			if err != nil {
				return nil, err
			}
			return dst[:n], nil
		}
	}

	fork, err := v.openForkReader(rec, true)
	if err != nil {
		return nil, err
	}
	if fork.Size() == 0 {
		return nil, &ParseError{Op: "decmpfs_fork", Offset: 0, Err: ErrMissingExtent}
	}

	raw, err := readAllBounded(fork, v)
	if err != nil {
		return nil, err
	}
	return decodeDecmpfsResourceFork(raw, h.UncompressedSize, chunkCodec)
}

// decodeDecmpfsResourceFork parses the chunk table and decompresses each chunk.
// Split out from the volume so it can be tested directly against a fork image.
func decodeDecmpfsResourceFork(raw []byte, uncompressedSize uint64, codec func([]byte, int) ([]byte, error)) ([]byte, error) {
	const resourceHeaderSize = 0x100

	if len(raw) < resourceHeaderSize+8 {
		return nil, &ParseError{Op: "decmpfs_fork", Offset: int64(len(raw)), Err: ErrCorrupt}
	}

	// The resource-fork header's first word is the offset of the resource data.
	dataOffset := int64(be32(raw[0:4]))
	if dataOffset <= 0 || dataOffset+8 > int64(len(raw)) {
		// Fall back to the conventional 0x100 when the header is unhelpful.
		dataOffset = resourceHeaderSize
	}
	if dataOffset+8 > int64(len(raw)) {
		return nil, &ParseError{Op: "decmpfs_fork", Offset: dataOffset, Err: ErrCorrupt}
	}

	// A big-endian length precedes the chunk table; the table itself, and every
	// offset in it, are relative to the start of the chunk count.
	tableBase := dataOffset + 4
	if tableBase+4 > int64(len(raw)) {
		return nil, &ParseError{Op: "decmpfs_fork", Offset: tableBase, Err: ErrCorrupt}
	}
	numChunks := int64(binary.LittleEndian.Uint32(raw[tableBase : tableBase+4]))

	wantChunks := int64((uncompressedSize + decmpfsChunkSize - 1) / decmpfsChunkSize)
	if numChunks <= 0 || numChunks != wantChunks {
		return nil, &ParseError{Op: "decmpfs_fork_chunks", Offset: numChunks, Err: ErrCorrupt}
	}
	if tableBase+4+numChunks*8 > int64(len(raw)) {
		return nil, &ParseError{Op: "decmpfs_fork_table", Offset: numChunks, Err: ErrCorrupt}
	}

	out := make([]byte, 0, uncompressedSize)
	for i := int64(0); i < numChunks; i++ {
		entry := tableBase + 4 + i*8
		chunkOff := tableBase + int64(binary.LittleEndian.Uint32(raw[entry:entry+4]))
		chunkLen := int64(binary.LittleEndian.Uint32(raw[entry+4 : entry+8]))

		if chunkOff < 0 || chunkLen < 0 || chunkOff+chunkLen > int64(len(raw)) {
			return nil, &ParseError{Op: "decmpfs_fork_chunk", Offset: chunkOff, Err: ErrCorrupt}
		}

		want := decmpfsChunkSize
		if remaining := int(uncompressedSize) - len(out); remaining < want {
			want = remaining
		}
		if want <= 0 {
			break
		}

		chunk, err := codec(raw[chunkOff:chunkOff+chunkLen], want)
		if err != nil {
			return nil, &ParseError{Op: "decmpfs_fork_chunk", Offset: chunkOff, Err: ErrCorrupt}
		}
		out = append(out, chunk...)
	}

	if uint64(len(out)) != uncompressedSize {
		return nil, &ParseError{Op: "decmpfs_fork_size", Offset: int64(len(out)), Err: ErrCorrupt}
	}
	return out, nil
}

// readAllBounded reads a whole fork, honouring the volume's allocation cap.
func readAllBounded(f *File, v *Volume) ([]byte, error) {
	if err := v.checkAlloc("decmpfs_resource", f.Size()); err != nil {
		return nil, err
	}
	return f.ReadAll()
}
