// Package libhfs reads Apple HFS, HFS+ and HFSX volumes from disk images.
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
//	vol, err := libhfs.Open(img)
//	if err != nil {
//		return err
//	}
//	defer vol.Close()
//
//	entries, err := vol.ReadDir("/")
//
// A volume nested inside a partition can be reached either by setting
// [Config.BaseOffset] to the byte it begins at, or by wrapping the reader in an
// [io.SectionReader]. Prefer the first when the offsets this package reports
// must be absolute against the whole image: a SectionReader makes every
// reported offset partition-relative, and nothing says so at the point of use.
// Wrappers containing an embedded HFS+ filesystem are detected and followed
// automatically either way.
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
//     OpenResourceFork equivalents return readers over fork contents
//   - [Volume.ResolveDataForkExtents] and [Volume.ResolveResourceForkExtents]
//     expose the on-disk fragments a fork occupies, for callers that need block
//     addresses rather than bytes
//   - [Volume.DataForkRanges] and [Volume.ResourceForkRanges] give the same
//     fragments as offsets into the image, ready to read; [Volume.BaseOffset]
//     is the origin they already account for, for callers converting block
//     numbers themselves
//   - [Volume.WalkPaths] enumerates the catalog with each record's path,
//     sharing the resolution work rather than repeating it per record
//   - [CatalogRecord.Identity] and [Volume.VolumeIdentifier] give handles for
//     matching a file, and a volume, across two readings
//   - [Volume.Report] assembles a JSON-encodable summary of all of the above
//   - [Volume.ListXAttrs], [Volume.ReadXAttr] and [Volume.WalkXAttrs] read
//     extended attributes
//   - [Volume.RecoverDeleted] and [Volume.WalkDeleted] surface records the
//     filesystem no longer lists
//   - [Volume.Capabilities] reports what the volume's format can hold, so
//     callers branch on a value rather than on [Volume.Kind]
//   - every Walk method stops early when its callback returns [ErrStopWalk]
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
// date at all, which [Volume.Capabilities] reports — that is a property of the
// format, not a missing value. Volume-level dates on [VolumeHeader] follow
// different rules again: CreateTime is stored in local time while the others
// are GMT, and unset fields there read back as the Unix epoch rather than a
// zero time.
//
// Links. [Volume.OpenCNID] resolves a hard link to its target inode and reports
// the target's metadata, matching what stat would show, while keeping the link's
// name and parent. [CatalogRecord.Link] makes that visible; [Volume.OpenCNIDRaw]
// returns the link record untouched, which is often what an examiner wants.
// Directories can be hard links too — Time Machine builds its backups out of
// them — and they resolve the same way, so [Volume.WalkDirCNID] on a directory
// link lists the target's children instead of reporting an empty directory.
// Both kinds are stored as file records pointing into a private folder at the
// volume root, a different folder for each kind, which is why [LinkHardDir] can
// appear on a record whose type is a file. An ordinary Finder alias to a folder
// carries the same Finder type and creator as a directory hard link and is not
// one: the two are separated by the link-chain flag, as the kernel separates
// them, and an alias is left unresolved because its target is a document's
// contents rather than a catalog reference.
//
// Damaged volumes. Lookups descend the B-tree by key. When the tree does not
// permit that — an unreadable node, an unparseable key, or index keys that
// disagree with the leaves they index — the lookup falls back to an exhaustive
// walk rather than returning an incomplete answer, and records the problem.
// [Volume.AnomalyCount] and [Volume.Anomalies] report these. A non-zero count
// is a finding about the volume, not merely a performance note.
//
// Deleted data. HFS+ does not zero a B-tree node's free space when a record is
// deleted, so stale records frequently survive there. Those are excluded from
// live results — a deleted file must never appear in a directory listing — and
// surfaced only through [Volume.WalkDeleted]. Note that not every record found
// that way is a deletion: B-tree inserts leave stale copies of records that are
// still live, and those are filtered out by default. Check
// [DeletedRecord.Overwritten] before trusting recovered content, since a
// recovered record's extents are stale pointers.
//
// Compression. Files compressed with decmpfs are decompressed transparently,
// whether the payload sits in the attribute or in the resource fork. zlib and
// stored payloads are built in; LZVN, LZFSE and LZBITMAP are not in the Go
// standard library and can be supplied through [RegisterDecompressor]. A file
// needing an unavailable codec yields [ErrUnsupportedCompression] rather than
// wrong bytes, and its raw resource fork stays readable through the
// OpenResourceFork methods so the artifact can still be preserved.
//
// Block addressing. An [ExtentDescriptor] counts in allocation blocks, and a
// block number is not a byte offset. The byte a block begins at is
// [Volume.BaseOffset] plus the block number times the block size, and the base
// offset is non-zero for classic HFS, for any HFS+ volume embedded in an HFS
// wrapper, and for any volume opened with a [Config.BaseOffset] — exactly the
// volumes where getting it wrong matters. It is wrong silently: the read
// succeeds and returns another part of the image. Prefer
// [Volume.DataForkRanges] and [Volume.ExtentRanges], which do the conversion.
// In a [ByteRange], Length stops at the fork's logical size and Slack counts
// the allocated bytes after it, so reading Length bytes never picks up what the
// previous occupant of the block left behind — and so that slack, which is
// usually the point of looking, is still addressable at DiskOffset+Length.
// Ranges describe a fork as stored, so a decmpfs-compressed file reports an
// empty data fork even though [Volume.OpenFileByCNID] returns its contents.
//
// Walk order and orphans. [Volume.WalkPaths] and [Volume.WalkCatalog] traverse
// in catalog B-tree key order — parent CNID ascending, then name — which is
// neither directory order nor parents before children, so a tree cannot be
// built by attaching each record to a parent already seen. A record whose
// parent chain cannot be followed to the root is still emitted by WalkPaths,
// with an empty path and an anomaly recorded: on a damaged volume those records
// are usually the point, and inventing a path for one would be a claim the
// volume does not support.
//
// Stopping a walk. Returning [ErrStopWalk] from any Walk callback ends the
// traversal and makes the Walk method return nil; any other error also ends it
// but is returned unchanged. A caller that stops at the first match and a
// caller whose walk died partway through an image both hold a partial answer,
// and on a forensic tool those two must not be reported the same way. For
// WalkDeleted the distinction is also a cost: a stop skips the scan phases that
// have not run yet, and the last of them reads every unallocated block on the
// volume.
//
// Identity across readings. A CNID is reused once the volume wraps around
// VolumeHeader.NextCatalogID, so it does not by itself identify a file between
// two readings. [FileIdentity] pairs it with the creation date, which nothing
// in normal use rewrites. That is evidence of sameness rather than proof:
// anything that can write the volume can write both halves, a hard link
// resolves to its inode so two links share one identity, and a copied file
// keeps its birth date while gaining a new CNID.
//
// Volume identifiers. [Volume.VolumeIdentifier] returns the 64-bit value stored
// in the volume header. [Volume.UUID] returns the RFC 4122 string macOS
// displays, which is derived from those bytes by hashing rather than by
// reformatting them — the two share no digits, and only the second will match
// what diskutil or a system log records.
//
// Hostile input. Sizes come from the volume, so a corrupt image can declare an
// enormous fork. [Volume.SetMaxAlloc] caps any single buffer sized from an
// on-disk field and yields [ErrSizeLimit] instead of attempting the allocation.
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
//	if errors.Is(err, libhfs.ErrNotFound) {
//		// ...
//	}
//
//	var pErr *libhfs.ParseError
//	if errors.As(err, &pErr) {
//		log.Printf("op=%s offset=%d", pErr.Op, pErr.Offset)
//	}
package libhfs

import (
	"errors"
	"io"
	"math"
)

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
// The volume must begin at offset 0 of r. To read one inside a partition of a
// whole-disk image, use [OpenWithConfig] with [Config.BaseOffset] set to the
// byte the volume begins at — every offset reported then stays absolute against
// the image. Wrapping the partition in an [io.SectionReader] also works, but
// makes every reported offset partition-relative, which is silent at the point
// of use.
//
// A malformed or unrecognised volume yields a [ParseError] wrapping
// [ErrInvalidSignature], [ErrUnsupportedVer] or [ErrCorrupt]; use [IsCorrupt]
// to test for the group.
func Open(r io.ReaderAt) (*Volume, error) {
	return openAt(r, 0)
}

// openAt is Open with the volume placed at an arbitrary byte of r.
//
// volumeStart is where the volume's own header lives — for a wrapped volume,
// where the *wrapper* begins, since that is what a caller can see from outside.
// The offset the wrapper's embedded volume adds on top is discovered here, not
// supplied, and the two compose rather than conflict: see [Config.BaseOffset].
func openAt(r io.ReaderAt, volumeStart int64) (*Volume, error) {
	// The nil guard lives here rather than in Open because OpenWithConfig
	// reaches this function directly; leaving it upstream would turn
	// OpenWithConfig(nil, cfg) into a panic inside readAtExact.
	if r == nil {
		return nil, &ParseError{Op: "open", Offset: 0, Err: ErrCorrupt}
	}
	if volumeStart < 0 {
		return nil, &ParseError{Op: "open", Offset: volumeStart, Err: ErrInvalidOffset}
	}

	buf := make([]byte, volumeHeaderSize)
	if err := readVolumeStartHeader(r, volumeStart, buf); err != nil {
		return nil, err
	}

	derived := int64(0)
	if be16(buf[0:2]) == signatureHFS {
		embeddedOffset, ok := parseHFSWrapperEmbeddedOffset(buf)
		if !ok {
			// Either a plain classic HFS volume, or a wrapper whose embedded
			// extent is unusable. Both are read as classic HFS: the MDB is a
			// complete, valid volume header in its own right, so the wrapper's
			// own filesystem is still readable even when the HFS+ volume it
			// points at is not. Reporting an error instead would discard
			// recoverable data.
			hdr, hfsBase, vbmStart, err := parseHFSMasterDirectoryBlock(buf, volumeStart)
			if err != nil {
				return nil, err
			}
			if err := checkBaseOffsetFits(volumeStart, hfsBase); err != nil {
				return nil, err
			}
			vol := newVolume(r, KindHFS, hdr, volumeStart, hfsBase)
			vol.hfsVBMStart = vbmStart
			return vol, nil
		}
		// An HFS wrapper around an embedded HFS+ volume: re-read the header
		// from the embedded volume and treat its start as the derived offset.
		if err := checkBaseOffsetFits(volumeStart, embeddedOffset); err != nil {
			return nil, err
		}
		// Not readVolumeStartHeader: a short read here means the wrapper's
		// embedded extent points past the end of the image, which is a fact
		// about the volume. Attributing it to the caller's offset would send
		// them looking in the wrong place.
		if err := readAtExact(r, volumeStart+embeddedOffset+volumeHeaderOffset, buf); err != nil {
			return nil, err
		}
		derived = embeddedOffset
	}

	hdr, kind, err := parseVolumeHeader(buf, volumeStart+derived)
	if err != nil {
		return nil, err
	}

	return newVolume(r, kind, hdr, volumeStart, derived), nil
}

// readVolumeStartHeader reads the header at the start of the volume, reporting
// a short read against volumeStart rather than against the header itself.
//
// This is the read that establishes whether volumeStart points anywhere useful,
// so it is the one that should say so. A caller who mistypes
// [Config.BaseOffset] and lands past the end of the image otherwise gets
// ErrShortRead attributed to the header — "this acquisition is truncated",
// which is a misleading forensic finding when the image is intact and only the
// offset was wrong. io.ReaderAt exposes no size, and asserting for one would
// narrow the reader contract this package documents, so a short read is the
// only signal available; naming volumeStart in the error is what makes it
// legible.
//
// A volume at the start of the reader keeps the original error, because there
// the short read really is about the image and there is no supplied offset to
// blame.
func readVolumeStartHeader(r io.ReaderAt, volumeStart int64, buf []byte) error {
	err := readAtExact(r, volumeStart+volumeHeaderOffset, buf)
	if err != nil && volumeStart != 0 && errors.Is(err, ErrShortRead) {
		return &ParseError{Op: "open", Offset: volumeStart, Err: ErrShortRead}
	}
	return err
}

// checkBaseOffsetFits rejects a base offset that would overflow int64.
//
// A derived offset is bounded by the volume's own fields — at most about
// 2.8e14 for an embedded extent — but a caller-supplied volumeStart is not, and
// a baseOffset that wrapped negative would defeat the overflow guard in
// [Volume.BlockOffset] rather than trip it.
func checkBaseOffsetFits(volumeStart, derived int64) error {
	if derived < 0 || volumeStart > math.MaxInt64-derived {
		return &ParseError{Op: "open", Offset: volumeStart, Err: ErrInvalidOffset}
	}
	return nil
}

// newVolume builds a Volume with default configuration.
//
// openAt reaches this from two paths — a classic HFS master directory block and
// an HFS+ volume header — which must not drift apart in what they initialise.
// Adding a field to Volume and forgetting one of them is exactly the kind of
// omission a single constructor prevents.
//
// derived is the offset the volume itself implies for allocation block 0,
// measured from the volume's own start. It is added to volumeStart rather than
// replacing it, which is the whole of the composition rule.
func newVolume(r io.ReaderAt, kind FileSystemKind, hdr VolumeHeader, volumeStart, derived int64) *Volume {
	cfg := DefaultConfig().normalise()
	return &Volume{
		reader:       r,
		kind:         kind,
		header:       hdr,
		volumeStart:  volumeStart,
		baseOffset:   volumeStart + derived,
		cacheMax:     cfg.CacheSize,
		nodeCacheMax: cfg.NodeCacheSize,
		maxAlloc:     cfg.MaxAlloc,
		textEncoding: cfg.TextEncoding,
		carveWorkers: cfg.CarveWorkers,
	}
}
