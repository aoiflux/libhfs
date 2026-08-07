// Package hfs reads Apple HFS, HFS+ and HFSX volumes from disk images.
//
// The library is strictly read-only and never writes to the volume it is given.
// It is built for tooling and forensic analysis, so it favours reporting what is
// actually on disk — including the parts that are damaged, deleted or
// contradictory — over presenting a tidy view of it.
//
// # Getting started
//
// Everything begins with [Open], which takes an [io.ReaderAt] over a raw image
// or device:
//
//	img, err := os.Open("disk.dd")
//	if err != nil {
//		return err
//	}
//	defer img.Close()
//
//	vol, err := hfs.Open(img)
//	if err != nil {
//		return err
//	}
//	defer vol.Close()
//
//	entries, err := vol.ReadDir("/")
//
// A volume nested inside a partition or an HFS wrapper can be reached by
// wrapping the reader in an [io.SectionReader]; wrappers containing an embedded
// HFS+ filesystem are detected and followed automatically.
//
// # Reading the filesystem
//
// Records are addressed either by path or by catalog node ID (CNID), the
// number HFS uses internally to identify a file or folder:
//
//   - [Volume.GetRootDirectory], [Volume.OpenPath], [Volume.OpenCNID]
//   - [Volume.ReadDir], [Volume.ReadDirCNID], [Volume.WalkDir], [Volume.WalkCatalog]
//   - [Volume.PathForCNID] reconstructs a path from a CNID
//   - [Volume.OpenFileByPath], [Volume.OpenFileByCNID] and the
//     OpenResourceFork equivalents return an [io.ReaderAt] over fork contents
//   - [Volume.ResolveDataForkExtents] and [Volume.ResolveResourceForkExtents]
//     expose the on-disk fragments a fork occupies, for callers that need block
//     addresses rather than bytes
//
// # Things that are easy to get wrong
//
// Several behaviours matter for forensic use and are not obvious from the
// signatures alone.
//
// Timestamps. Every [CatalogRecord] carries a [CatalogTimes] holding the MACB
// set. A zero time.Time means the field was unset on disk — not 1904, and not
// 1970 — so test with IsZero before using a value. Check [CatalogTimes.Source]
// before comparing across volumes: HFS+ and HFSX catalog dates are GMT, while
// classic HFS dates are local wall-clock readings with no offset recorded
// anywhere on the volume. Classic HFS has no access or attribute-modification
// date at all. Volume-level dates on [VolumeHeader] follow different rules
// again: CreateTime is stored in local time while the others are GMT, and unset
// fields there read back as the Unix epoch rather than a zero time.
//
// Hard links. [Volume.OpenCNID] resolves a hard link to its target inode and
// reports the target's metadata, matching what stat would show. The record as
// it literally appears in the catalog is not currently exposed.
//
// Damaged volumes. Lookups descend the B-tree by key. When the tree does not
// permit that — an unreadable node, an unparseable key, or index keys that
// disagree with the leaves they index — the lookup falls back to an exhaustive
// walk rather than returning an incomplete answer, and records the problem.
// [Volume.AnomalyCount] and [Volume.Anomalies] report these. A non-zero count
// is a finding about the volume, not merely a performance note.
//
// Deleted data. HFS+ does not zero a B-tree node's free space when a record is
// deleted, so stale records frequently survive there. This package deliberately
// excludes that region from live results: a deleted file must never appear in a
// directory listing. Surfacing those records is the job of a separate recovery
// API.
//
// Compression. Files compressed with decmpfs are decompressed transparently
// when the payload is stored inline in the attribute. Payloads held in the
// resource fork are not yet decoded; the raw resource fork remains readable via
// the OpenResourceFork methods in every case.
//
// # Concurrency
//
// A [Volume] is safe for concurrent use by multiple goroutines, provided the
// io.ReaderAt it was opened with honours the standard contract permitting
// parallel ReadAt calls — *os.File, *bytes.Reader and *io.SectionReader all do.
//
// A [File] is not safe for concurrent use, because Read advances a per-handle
// offset. Give each goroutine its own handle, or use ReadAt, which does not
// touch that offset.
//
// # Errors
//
// Failures wrap sentinel errors and a [ParseError] carrying the operation and
// byte offset, so both errors.Is and errors.As work as expected:
//
//	rec, err := vol.OpenPath("/missing")
//	if errors.Is(err, hfs.ErrNotFound) {
//		// ...
//	}
//
//	var pErr *hfs.ParseError
//	if errors.As(err, &pErr) {
//		log.Printf("op=%s offset=%d", pErr.Op, pErr.Offset)
//	}
package hfs

import "io"

// Open reads the volume header at the start of r and returns a handle to the
// filesystem it describes.
//
// r is read from but never written to, and Open does not take ownership of it:
// closing r remains the caller's responsibility, and r must stay open for as
// long as the returned Volume is used. [Volume.Close] currently releases
// nothing and exists so callers can defer it without depending on that.
//
// HFS, HFS+ and HFSX volumes are all recognised. An HFS wrapper carrying an
// embedded HFS+ filesystem is followed automatically, and all subsequent offsets
// are resolved relative to the embedded volume, so callers need not know whether
// a wrapper was present.
//
// The volume must begin at offset 0 of r. To read one inside a partition, pass
// an [io.SectionReader] covering it.
//
// A malformed or unrecognised volume yields a [ParseError] wrapping
// [ErrInvalidSignature], [ErrUnsupportedVer] or [ErrCorrupt]; use [IsCorrupt]
// to test for the group.
func Open(r io.ReaderAt) (*Volume, error) {
	if r == nil {
		return nil, &ParseError{Op: "open", Offset: 0, Err: ErrCorrupt}
	}

	buf := make([]byte, volumeHeaderSize)
	if err := readAtExact(r, volumeHeaderOffset, buf); err != nil {
		return nil, err
	}

	baseOffset := int64(0)
	if be16(buf[0:2]) == signatureHFS {
		embeddedOffset, ok := parseHFSWrapperEmbeddedOffset(buf)
		if !ok {
			// A plain classic HFS volume: the MDB is the volume header.
			hdr, hfsBase, err := parseHFSMasterDirectoryBlock(buf)
			if err != nil {
				return nil, err
			}
			return &Volume{
				reader:       r,
				kind:         KindHFS,
				header:       hdr,
				baseOffset:   hfsBase,
				cacheMax:     DefaultCacheSize,
				nodeCacheMax: DefaultNodeCacheSize,
			}, nil
		}
		// An HFS wrapper around an embedded HFS+ volume: re-read the header
		// from the embedded volume and treat its start as the base offset.
		if err := readAtExact(r, embeddedOffset+volumeHeaderOffset, buf); err != nil {
			return nil, err
		}
		baseOffset = embeddedOffset
	}

	hdr, kind, err := parseVolumeHeader(buf)
	if err != nil {
		return nil, err
	}

	return &Volume{
		reader:       r,
		kind:         kind,
		header:       hdr,
		baseOffset:   baseOffset,
		cacheMax:     DefaultCacheSize,
		nodeCacheMax: DefaultNodeCacheSize,
	}, nil
}
