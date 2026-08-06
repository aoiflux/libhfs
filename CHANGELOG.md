# Changelog

All notable changes to this project are documented here. This project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Roadmap and phase
planning live in [PLAN.md](PLAN.md).

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
