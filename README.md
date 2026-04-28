# libhfs

libhfs reads HFS+ and HFSX volumes and disk images with strong validation, typed
errors, and an API designed for tooling, forensics workflows, and systems
integration.

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

## Go Version

- Requires Go 1.25+

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

- HFS+ and HFSX volume header parsing and validation
- Catalog and Extents B-tree header parsing
- Catalog traversal (full walk, path lookup, CNID lookup)
- Directory listing by path and CNID
- Data fork and resource fork extent resolution (including overflow extents)
- File reads via `Read`, `ReadAt`, and `ReadAll`
- Path reconstruction via `PathForCNID`
- HFS+ Unicode name comparison semantics and HFSX case-sensitive mode handling
- Inline `com.apple.decmpfs` decompression support (raw + zlib inline payloads)
- Typed error model using standard Go wrapping (`errors.Is` / `errors.As`)

Current limitations:

- Classic HFS volumes are detected but not supported (`ErrUnsupportedFormat`)
- Read-only library (no write or repair operations)
- Compression support is limited to inline decmpfs attribute payloads

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

File-level:

- `(*Volume).OpenFileByPath(path string) (*File, error)`
- `(*Volume).OpenFileByCNID(cnid uint32) (*File, error)`
- `(*Volume).OpenResourceForkByPath(path string) (*File, error)`
- `(*Volume).OpenResourceForkByCNID(cnid uint32) (*File, error)`
- `(*File).Read(p []byte) (int, error)`
- `(*File).ReadAt(p []byte, off int64) (int, error)`
- `(*File).ReadAll() ([]byte, error)`

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
