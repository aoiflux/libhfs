# libhfs

libhfs reads HFS+, HFSX and classic HFS volumes and disk images with strong
validation, typed errors, and an API designed for tooling, forensics workflows,
and systems integration.

## Why libhfs

- Zero external dependencies (Go standard library only)
- Read-only, parser-first API for safe integration into analysis tools
- Strong bounds checking and corruption-aware parsing
- Typed errors for robust caller-side handling
- Practical examples for metadata, traversal, and extraction workflows

## Installation

```bash
go get github.com/aoiflux/libhfs
```

## Quick Start

```go
package main

import (
	"fmt"
	"log"
	"os"

	hfs "github.com/aoiflux/libhfs"
)

func main() {
	img, err := os.Open("disk.img") // or raw device path
	if err != nil {
		log.Fatal(err)
	}
	defer img.Close()

	vol, err := hfs.Open(img)
	if err != nil {
		log.Fatal(err)
	}
	defer vol.Close()

	fmt.Printf("Volume kind: %s\n", vol.Kind())

	entries, err := vol.ReadDir("/")
	if err != nil {
		log.Fatal(err)
	}

	for _, e := range entries {
		kind := "FILE"
		if e.IsDirectory {
			kind = "DIR"
		}
		fmt.Printf("[%s] %s (CNID %d)\n", kind, e.Name, e.CNID)
	}
}
```

## Feature Support

Implemented:

- HFS+, HFSX and classic HFS volume parsing and validation
- HFS wrapper volumes with an embedded HFS+ filesystem
- Catalog and Extents B-tree header parsing
- Catalog traversal (full walk, path lookup, CNID lookup)
- Directory listing by path and CNID
- Data fork and resource fork extent resolution (including overflow extents)
- File reads via `Read`, `ReadAt`, and `ReadAll`
- Path reconstruction via `PathForCNID`
- Per-file MACB timestamps on every catalog record
- HFS+ Unicode name comparison semantics and HFSX case-sensitive mode handling
- Inline `com.apple.decmpfs` decompression support (raw + zlib inline payloads)
- Typed error model using standard Go wrapping (`errors.Is` / `errors.As`)

- Keyed B-tree descent for catalog, extents and attributes lookups, with
  fragmented B-tree files addressed correctly
- Safe for concurrent readers, with bounded record and B-tree node caches
- Extended attributes: listing, reading and whole-volume walking, including
  fork-backed values spanning extension records
- decmpfs decompression for inline and resource-fork payloads, with a registry
  for codecs outside the standard library
- Deleted-record recovery from B-tree node slack, free nodes and unallocated
  blocks, with confidence grading and stale-copy filtering
- POSIX ownership and mode, Finder metadata, hard links and symbolic links
- Allocation bitmap access
- Byte-range addressing: extents resolved to image offsets, with file slack
  reported separately, correct on wrapper and classic HFS volumes
- Whole-catalog traversal with paths, resolved once per directory rather than
  once per record
- Composite file identity and the volume's stored identifier and derived UUID,
  for correlating two readings of a volume
- JSON-encodable volume report
- Mac OS Roman decoding for classic HFS names
- Structural anomaly reporting for damaged volumes
- Allocation guard against implausible on-disk sizes

Current limitations:

- Read-only library (no write or repair operations)
- The journal is not read, so pre-commit metadata on a journaled volume is not
  examined
- LZVN, LZFSE and LZBITMAP decmpfs codecs are not built in — register your own
- Resource-fork decompression is implemented from published descriptions and
  has not been validated against a macOS-produced compressed file
- The displayed volume UUID is derived from the published algorithm and has not
  been checked against a UUID produced by macOS itself; the stored identifier
  from `VolumeIdentifier()` carries no such doubt
- ACLs in `com.apple.system.Security` are returned as opaque bytes
- Classic HFS script encodings other than Mac OS Roman are not decoded
- Classic HFS carries no extended attributes, access dates, attribute
  modification dates, POSIX permissions, hard links or compression — these are
  properties of the format, not gaps in the parser, and `Capabilities()`
  reports them

The sections below cover the semantics that matter when this output becomes
evidence. The package documentation repeats them under "Things that are easy to
get wrong", so they are visible from `go doc` too.

## Timestamps

Every `CatalogRecord` carries a `Times` field holding the on-disk MACB set:

```go
rec, err := vol.OpenPath("/etc/hosts")
if err != nil {
	log.Fatal(err)
}

fmt.Println("created:  ", rec.Times.Created)
fmt.Println("modified: ", rec.Times.ContentModified)
fmt.Println("attr mod: ", rec.Times.AttrModified)
fmt.Println("accessed: ", rec.Times.Accessed)
fmt.Println("backup:   ", rec.Times.Backup)
```

Two rules matter when using these values:

- **A zero `time.Time` means the field was unset on disk.** It does not mean
  1904 and it does not mean 1970. Test with `IsZero()` before using a value.
- **Check `Times.Source` before comparing across volumes.** HFS+ and HFSX
  catalog dates are GMT (`TimeSourceHFSPlusGMT`). Classic HFS dates are local
  wall-clock readings with no offset stored anywhere on the volume
  (`TimeSourceHFSLocal`), so they are not absolute instants.

Classic HFS records only creation, modification and backup dates; `Accessed`
and `AttrModified` are always zero there.

Note that the volume-level dates on `VolumeHeader` follow different rules: per
the HFS+ specification `CreateTime` is stored in local time while the other
volume dates are GMT, and unset fields read back as the Unix epoch rather than
a zero time.

## API Highlights

Volume-level:

- `Open(r io.ReaderAt) (*Volume, error)`
- `(*Volume).Kind() FileSystemKind`
- `(*Volume).Header() VolumeHeader`
- `(*Volume).GetRootDirectory() (CatalogRecord, error)`
- `(*Volume).OpenPath(path string) (CatalogRecord, error)`
- `(*Volume).OpenCNID(cnid uint32) (CatalogRecord, error)`
- `(*Volume).ReadDir(path string) ([]DirEntry, error)`
- `(*Volume).ReadDirCNID(cnid uint32) ([]DirEntry, error)`
- `(*Volume).WalkDir(path string, cb func(DirEntry) error) error`
- `(*Volume).WalkCatalog(cb func(CatalogRecord) error) error`
- `(*Volume).PathForCNID(cnid uint32) (string, error)`
- `(*Volume).WalkPaths(cb func(path string, rec CatalogRecord) error) error`
- `(*Volume).WalkPathsContext(ctx context.Context, cb func(path string, rec CatalogRecord) error) error`
- `(*Volume).PathRecords() ([]PathRecord, error)`
- `(*Volume).GetTimes(cnid uint32) (CatalogTimes, error)`
- `(*Volume).GetTimesByPath(path string) (CatalogTimes, error)`
- `(*Volume).OpenCNIDRaw(cnid uint32) (CatalogRecord, error)`
- `(*Volume).ReadLink(cnid uint32) (string, error)`
- `(*Volume).Capabilities() Capabilities`
- `(*Volume).VolumeIdentifier() (VolumeIdentifier, error)`
- `(*Volume).UUID() (string, error)`
- `(*Volume).IdentityByCNID(cnid uint32) (FileIdentity, error)`
- `(*Volume).IdentityByPath(path string) (FileIdentity, error)`
- `(CatalogRecord).Identity() FileIdentity`
- `(*Volume).Report(opts *ReportOptions) (Report, error)`
- `(*Volume).ReportContext(ctx context.Context, opts *ReportOptions) (Report, error)`
- `(*Volume).SetCacheSize(n int)` / `SetNodeCacheSize(n int)` / `SetMaxAlloc(n int64)`
- `(*Volume).SetTextEncoding(e TextEncoding)`
- `(*Volume).Anomalies() []Anomaly`
- `(*Volume).AnomalyCount() int`

Extended attributes:

- `(*Volume).ListXAttrs(cnid uint32) ([]XAttr, error)`
- `(*Volume).ReadXAttr(cnid uint32, name string) ([]byte, error)`
- `(*Volume).OpenXAttr(cnid uint32, name string) (*File, error)`
- `(*Volume).WalkXAttrs(cb func(XAttr) error) error`

Allocation state:

- `(*Volume).BlockAllocated(block uint32) (bool, error)`
- `(*Volume).WalkUnallocated(cb func(start, count uint32) error) error`
- `(*Volume).FreeBlockCount() (uint32, error)`

Block addressing — read "Block addressing and slack" below before using these,
in particular the distinction between `Length` and `Slack`:

- `(*Volume).BaseOffset() int64`
- `(*Volume).BlockOffset(block uint32) (int64, error)`
- `(*Volume).DataForkRanges(cnid uint32) ([]ByteRange, error)`
- `(*Volume).ResourceForkRanges(cnid uint32) ([]ByteRange, error)`
- `(*Volume).XAttrRanges(cnid uint32, name string) ([]ByteRange, error)`
- `(*Volume).ExtentRanges(exts []ExtentDescriptor, logicalSize int64) ([]ByteRange, error)`
- `(*Volume).ResolveDataForkExtents(cnid uint32) ([]ExtentDescriptor, error)`
- `(*Volume).ResolveResourceForkExtents(cnid uint32) ([]ExtentDescriptor, error)`

Deleted-record recovery — read "Deleted-record recovery" below before relying on
these, in particular the caveat about `Overwritten`:

- `(*Volume).RecoverDeleted(opts *RecoveryOptions) ([]DeletedRecord, error)`
- `(*Volume).WalkDeleted(opts *RecoveryOptions, cb func(DeletedRecord) error) error`
- `(*Volume).OpenDeleted(rec DeletedRecord) (*File, error)`

File-level:

- `(*Volume).OpenFileByPath(path string) (*File, error)`
- `(*Volume).OpenFileByCNID(cnid uint32) (*File, error)`
- `(*Volume).OpenResourceForkByPath(path string) (*File, error)`
- `(*Volume).OpenResourceForkByCNID(cnid uint32) (*File, error)`
- `(*File).Read(p []byte) (int, error)`
- `(*File).ReadAt(p []byte, off int64) (int, error)`
- `(*File).ReadAll() ([]byte, error)`

## Block addressing and slack

An `ExtentDescriptor` counts in allocation blocks. A block number is not a byte
offset, and converting one needs both the block size and the volume's base
offset:

```go
ranges, err := vol.DataForkRanges(rec.CNID)
if err != nil {
    log.Fatal(err)
}
for _, r := range ranges {
    buf := make([]byte, r.Length)
    if _, err := image.ReadAt(buf, r.DiskOffset); err != nil {
        log.Fatal(err)
    }
    // r.DiskOffset+r.Length is where this block's file slack begins,
    // and r.Slack is how much of it there is.
}
```

Four things to understand before treating the output as evidence:

- **Block numbers are not byte offsets.** `BaseOffset()` is the image byte
  offset that allocation block 0 maps to. It is zero only for a plain HFS+ or
  HFSX volume at the start of the reader; it is non-zero for classic HFS and
  for any HFS+ volume embedded in an HFS wrapper. Getting it wrong is silent —
  the read succeeds and returns some other part of the image.
- **`Length` excludes slack.** It stops at the fork's logical size, so reading
  `Length` bytes at `DiskOffset` never picks up what the previous occupant of
  the block left behind. `Slack` counts the allocated bytes after it, beginning
  at `DiskOffset+Length`, and `AllocatedLength()` is the two together.
- **A compressed file's data fork is empty on disk.** decmpfs keeps the payload
  in an extended attribute or the resource fork, so `DataForkRanges` correctly
  returns nothing while `OpenFileByCNID` returns the decompressed contents.
  Check `CatalogRecord.Compressed`.
- **Recovered records carry stale pointers.** `ExtentRanges` will happily
  convert the fork data on a `DeletedRecord`, but those blocks may since have
  been reallocated — see `DeletedRecord.Overwritten` and "Deleted-record
  recovery" below.

Attribute extents work slightly differently from fork extents: they come from
the attributes tree alone, carried in extension records beside the fork-data
record rather than in the extents-overflow tree, and they are not trimmed
against a block total because an attribute record does not record one.

## Catalog walks with paths

`WalkPaths` enumerates the catalog with each record's path, resolving parents
once per directory instead of once per record:

```go
err := vol.WalkPaths(func(path string, rec CatalogRecord) error {
    fmt.Printf("%s\t%d\t%d\n", path, rec.CNID, rec.DataFork.LogicalSize)
    return nil
})
```

- **Order is B-tree key order**, parent CNID then name — not directory order,
  and not parents before children. Do not build a tree by attaching each record
  to a parent already seen; use the path.
- **Thread records are not emitted**, unlike `WalkCatalog`. A thread record is
  path information rather than a thing with a path.
- **Orphans are emitted with an empty path.** A record whose parent chain
  cannot be followed to the root is still reported, and the condition is
  recorded through `Anomalies()`. Suppressing those would hide exactly the
  damage worth finding, and inventing a path would be a claim the volume does
  not support.

## Stopping a walk

Every `Walk*` method takes a callback, and returning `ErrStopWalk` from one ends
the walk successfully:

```go
var found hfs.CatalogRecord
err := vol.WalkCatalog(func(r hfs.CatalogRecord) error {
    if r.Name == "secrets.txt" {
        found = r
        return hfs.ErrStopWalk
    }
    return nil
})
```

- **Any other error ends the walk too, and comes back unchanged.** Stopping
  because you found what you wanted and stopping because the image is unreadable
  both leave you with a partial answer; only one of them is a finding about the
  volume.
- **It is matched with `errors.Is`**, so a callback can wrap it to carry a
  reason back out of the traversal.
- **`WalkDeleted` stops early in the expensive sense too.** It runs three scans
  in sequence and a stop skips the ones that have not started, including the
  unallocated scan that reads every free block on the volume.
- libhfs never returns `ErrStopWalk` as a failure of its own.

## Identity across two readings

A CNID is reused once the volume wraps around `NextCatalogID`, so it does not
by itself identify a file between two readings of a volume. `FileIdentity`
pairs it with the creation date:

```go
before := recBefore.Identity()
after := recAfter.Identity()
if before.Comparable() && before.Equal(after) {
    // same file, as far as the volume can say
}
```

- **This is evidence of sameness, not proof.** Anything that can write the
  volume can write both halves.
- **Hard links collapse.** `IdentityByCNID` resolves to the target inode, so
  every path pointing at one file yields one identity. Use `OpenCNIDRaw` with
  `CatalogRecord.Identity()` when the link itself is what is being tracked.
- **Creation dates survive copying.** A file copied with `cp -p` keeps its birth
  date but gains a new CNID, so a non-match is not proof of difference either.
- **Classic HFS dates are not anchored.** When `Source` is `TimeSourceHFSLocal`
  the date is a wall-clock reading with no recorded offset, so it is comparable
  only within readings of the same volume. `Equal` refuses to match across
  clock kinds.

For the volume itself, `VolumeIdentifier()` returns the 64-bit value stored in
the volume header and `UUID()` returns the RFC 4122 string macOS displays. They
are **different values** — the second is an MD5 derivation of the first, not a
reformatting of it — and only the second will match `diskutil` or a system log.

## JSON report

`Report` assembles a JSON-encodable summary. The library does not marshal it;
the caller does.

```go
rep, err := vol.Report(&hfs.ReportOptions{IncludeFiles: true, MaxFiles: 5000})
if err != nil {
    log.Fatal(err)
}
out, _ := json.MarshalIndent(rep, "", "  ")
```

- **Null means absent, not the epoch.** A zero `CatalogTimes` field and a
  clamped volume-header date both encode as `null`. The cost is that a volume
  genuinely created at the Unix epoch reports no creation date — dates before
  1970 are already lost when the header is parsed.
- **The file listing is off by default and bounded when on.** Building it is a
  full catalog walk. `MaxFiles` defaults to `DefaultReportMaxFiles`; a negative
  value is unbounded, and `FilesTruncated` says whether the bound was reached.
- **Block counts are the header's claim, not the bitmap's.** That is what a
  report should quote; `FreeBlockCount()` counts the bits instead, and a
  mismatch between the two is itself a finding.
- **The report projects, it does not extend.** Every field is reachable through
  the ordinary API; the type exists so a tool can emit one document rather than
  assemble one.

## Concurrency

A `*Volume` is safe for concurrent use by multiple goroutines. This assumes the
`io.ReaderAt` it was opened with honours the standard contract that parallel
`ReadAt` calls are permitted — `*os.File`, `*bytes.Reader` and
`*io.SectionReader` all do.

A `*File` is **not** safe for concurrent use, because `Read` advances a
per-handle offset. Give each goroutine its own handle, or use `ReadAt`, which
does not touch that offset.

## Caching

Lookups descend the B-tree by key rather than scanning it, and two bounded
caches sit behind that. Both are on by default and both are safe to leave alone.

- `SetCacheSize(n)` — catalog records, default `DefaultCacheSize` (4096).
- `SetNodeCacheSize(n)` — B-tree nodes, default `DefaultNodeCacheSize` (128).

Pass 0 to either to disable it. Since the volume is read-only, cached data
cannot go stale.

## Damaged Volumes

Keyed descent needs a structurally sound B-tree. When it encounters one that is
not — an unreadable node, an unparseable key, or index keys that disagree with
the leaves they index — it falls back to a full linear walk rather than
returning a wrong or incomplete answer, and records the inconsistency:

```go
entries, err := vol.ReadDir("/")
if err != nil {
	log.Fatal(err)
}

if vol.AnomalyCount() > 0 {
	for _, a := range vol.Anomalies() {
		fmt.Printf("anomaly: %s at %d: %s\n", a.Op, a.Offset, a.Detail)
	}
}
```

A non-zero `AnomalyCount` is a finding about the volume, not merely a
performance note: it means part of the filesystem metadata is inconsistent.
Results remain correct, because the fallback path is an exhaustive walk of every
node in the tree.

## Deleted-record recovery

Deleting a file on HFS+ removes its catalog record and frees its blocks; it does
not erase anything. Records therefore survive in B-tree node slack, in nodes the
tree has stopped using, and in blocks the volume no longer considers allocated.

```go
recs, err := vol.RecoverDeleted(nil)
if err != nil {
	log.Fatal(err)
}
for _, r := range recs {
	fmt.Printf("%s  source=%v  confidence=%v  overwritten=%v\n",
		r.Record.Name, r.Source, r.Confidence, r.Overwritten)
}
```

Four things to understand before treating the output as evidence:

- **`Overwritten` is the field that matters.** A recovered record's extents are
  stale pointers. If those blocks have since been reallocated, reading them
  returns another file's data, not the deleted one's. It is computed against the
  current allocation bitmap.
- **Not everything found is a deletion.** B-tree inserts leave stale copies of
  records that are still live. Those are filtered out by default; set
  `IncludeStaleCopies` to see them.
- **`Confidence` is advisory triage metadata, never a guarantee.** It grades how
  much of a record was independently corroborated — whether its parent resolves,
  whether its extents fall inside the volume. The default surfaces everything
  with grades attached rather than silently dropping low-confidence findings,
  because for forensic use a discarded record is worse than a graded one.
- **Recovery quality depends on reuse**, which correlates with time since
  deletion and volume pressure — not with anything this library can measure.

Unallocated-block carving is off by default because it reads the entire free
area of the volume; enable it with `RecoveryOptions.ScanUnallocated`. Use
`WalkDeletedContext` if you need to be able to cancel it.

## Testing

The default suite is hermetic and runs against synthetic images:

```bash
go test ./...
```

Synthetic fixtures are built from the same reading of the format as the parser,
so they cannot catch a misreading of it. Point the suite at a real raw image to
run the corpus tier as well:

```bash
LIBHFS_CORPUS_IMAGE=/path/to/image.dd go test ./...
```

Those tests reconcile the catalog walk against the volume header's own file and
folder counts, check keyed lookups against an exhaustive walk for every record,
round-trip every path, and read every file verifying the byte count against the
recorded logical size.

## Error Handling

libhfs uses standard wrapping semantics so `errors.Is` and `errors.As` work
reliably.

```go
package main

import (
	"errors"
	"fmt"

	hfs "github.com/aoiflux/libhfs"
)

func handlePath(vol *hfs.Volume) {
	rec, err := vol.OpenPath("/missing/file")
	if err != nil {
		if errors.Is(err, hfs.ErrNotFound) {
			fmt.Println("not found")
			return
		}

		var pErr *hfs.ParseError
		if errors.As(err, &pErr) {
			fmt.Printf("op=%s offset=%d\n", pErr.Op, pErr.Offset)
		}
		return
	}

	_ = rec
}
```

## Platform Notes

Raw volume access usually requires elevated privileges.

Windows:

- Run terminal as Administrator
- Use paths like `\\.\C:` or `\\.\PhysicalDrive0`

macOS:

- Use device paths like `/dev/disk2s1`
- Prefer read-only/forensic-safe workflows

Linux:

- Use block-device paths like `/dev/sda1`
- Prefer read-only/forensic-safe workflows

You can also use disk image files directly on all platforms.

## Examples

- `examples/basic`: open volume, show metadata, list root directory
- `examples/traverse`: recursive traversal and size statistics
- `examples/extract`: extract a file from HFS to local output

Run one example:

```bash
cd examples/basic
go run . <hfs_volume_or_image>
```

## Performance Notes

- Sequential catalog scans use leaf-chain traversal for efficient full walks
- Extent resolution avoids redundant work and trims to logical fork size
- API is designed around `io.ReaderAt` for deterministic random-access reads

## Development

Run checks:

```bash
go test ./...
go vet ./...
```
