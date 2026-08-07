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
- ACLs in `com.apple.system.Security` are returned as opaque bytes
- Classic HFS script encodings other than Mac OS Roman are not decoded
- Classic HFS carries no extended attributes, access dates, attribute
  modification dates, POSIX permissions, hard links or compression — these are
  properties of the format, not gaps in the parser, and `Capabilities()`
  reports them

See [FORENSICS.md](FORENSICS.md) for the semantics that matter when this
output becomes evidence, and [PLAN.md](PLAN.md) for development history.

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
- `(*Volume).GetTimes(cnid uint32) (CatalogTimes, error)`
- `(*Volume).GetTimesByPath(path string) (CatalogTimes, error)`
- `(*Volume).OpenCNIDRaw(cnid uint32) (CatalogRecord, error)`
- `(*Volume).ReadLink(cnid uint32) (string, error)`
- `(*Volume).Capabilities() Capabilities`
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

Deleted-record recovery — see [FORENSICS.md](FORENSICS.md) §6 before relying on
these:

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
