# Changelog

All notable changes to this project are documented here. This project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Roadmap and phase
planning live in [PLAN.md](PLAN.md).

## [Unreleased]

Phase 1: keyed B-tree search. No breaking API changes — lookups that previously
scanned the whole catalog now descend by key, with a linear fallback whenever
the tree structure does not permit descent.

### Added

- `*Volume` is now safe for concurrent use by multiple goroutines. This requires
  the supplied `io.ReaderAt` to honour the standard contract allowing parallel
  `ReadAt` calls. A single `*File` remains unsafe for concurrent use because
  `Read` advances a per-handle offset; `ReadAt` is safe.
- Catalog record cache, bounded by `SetCacheSize` (default `DefaultCacheSize`,
  4096 records; 0 disables).
- B-tree node cache, bounded by `SetNodeCacheSize` (default
  `DefaultNodeCacheSize`, 128 nodes; 0 disables).
- `Anomaly`, `(*Volume).Anomalies()` and `(*Volume).AnomalyCount()` report
  structural inconsistencies encountered during parsing that did not stop the
  operation — currently, any search that had to fall back to a linear walk.
  Anomalies are deduplicated on `(Op, Detail)`; the count includes repeats.

### Changed

- `OpenPath`, `OpenCNID`, `ReadDir`, `WalkDir`, `PathForCNID`, fork extent
  resolution and decmpfs attribute lookup all descend the relevant B-tree by key
  instead of scanning it end to end. On the test fixture a deepest-path
  `OpenPath` went from 114,808 bytes read to 600 with default settings.
- CNID lookup now resolves through the record's thread record rather than
  scanning for a matching CNID.

### Notes

- Classic HFS still uses the linear walk for lookups: its catalog keys collate
  under a MacRoman ordering this package does not model yet. Both caches still
  apply. Keyed search for classic HFS arrives with the MacRoman work.
- Descent validates that it landed where a well-formed tree says it should. A
  B-tree whose index keys disagree with its leaves would otherwise cause
  searches to silently under-report; instead such a volume falls back to the
  linear walk and records an anomaly.
- Name collation is deliberately not reproduced. Every descent target uses an
  empty name, which sorts first under any comparator, so descent depends only on
  the parent CNID; name matching is done by scanning the parent's child run.
  This removes any possibility of a comparator mismatch hiding a file that is
  present.

## [0.2.0] — 2026-08-06

Phase 0: per-file MACB timestamps.

### Added

- `CatalogRecord.Times` (`CatalogTimes`) exposes the on-disk MACB set —
  `Created`, `ContentModified`, `AttrModified`, `Accessed` and `Backup` — for
  every catalog file and folder record, on HFS+, HFSX and classic HFS.
- `CatalogTimes.Source` (`TimeSource`) records how the values must be
  interpreted: `TimeSourceHFSPlusGMT` for HFS+/HFSX catalog dates, or
  `TimeSourceHFSLocal` for classic HFS, whose dates are local wall-clock
  readings with no offset stored on the volume.
- `CatalogTimes.IsZero()` reports whether no timestamp in the set was present.
- `(*Volume).GetTimes(cnid uint32) (CatalogTimes, error)` and
  `(*Volume).GetTimesByPath(path string) (CatalogTimes, error)`.

### Fixed

- Classic HFS folder valence was read from offset 10 of `CatDirRec`, which is
  `dirCrDat`. It is now read from offset 4 (`dirVal`), so folder valence on
  classic HFS volumes was previously the high half of the creation date.

### Notes

- A zero `time.Time` in `CatalogTimes` means the field was unset on disk, not
  1904 and not 1970. Pre-1970 dates are preserved as negative Unix times rather
  than clamped.
- `VolumeHeader` date behaviour is deliberately unchanged, including its
  clamping of unset fields to the Unix epoch, so existing callers see no
  difference. A regression test guards this.
- Classic HFS has no access date and no attribute-modification date; those two
  fields are always zero when `Source` is `TimeSourceHFSLocal`.
- The minimum classic-HFS folder record length accepted by the parser rose from
  14 to 22 bytes to cover the three date fields. Records shorter than that were
  already malformed.

## [0.1.1]

Initial published release: HFS+/HFSX parsing, classic HFS parsing, catalog and
extents B-tree traversal, fork extent resolution, file and resource fork reads,
path reconstruction, and inline decmpfs support.
