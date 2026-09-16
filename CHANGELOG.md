# Changelog

All notable changes to libhfs are recorded here. Versions follow semantic
versioning; while the module is pre-1.0 a minor bump may break compatibility,
and every such break is listed under **Breaking** below.

## v0.4.0

Two breaking changes to the JSON report's wire format, shipped together so
consumers absorb one break rather than two, and one additive open-time option.

### Breaking — JSON keys are now snake_case

Every JSON tag in the package was camelCase; all 25 are now snake_case, which is
what the other five libraries in this family already use. A consumer parsing
libhfs reports must rename these keys. Nothing else about the documents changed:
no field was added, removed, retyped, or given a different meaning.

| type | old key | new key |
| --- | --- | --- |
| `Report` | `anomalyTotal` | `anomaly_total` |
| `Report` | `filesTruncated` | `files_truncated` |
| `VolumeSummary` | `baseOffset` | `base_offset` |
| `VolumeSummary` | `blockSize` | `block_size` |
| `VolumeSummary` | `totalBlocks` | `total_blocks` |
| `VolumeSummary` | `freeBlocks` | `free_blocks` |
| `VolumeSummary` | `totalBytes` | `total_bytes` |
| `VolumeSummary` | `freeBytes` | `free_bytes` |
| `VolumeSummary` | `fileCount` | `file_count` |
| `VolumeSummary` | `folderCount` | `folder_count` |
| `VolumeSummary` | `nextCatalogID` | `next_catalog_id` |
| `FileSummary` | `parentCNID` | `parent_cnid` |
| `FileSummary` | `resourceSize` | `resource_size` |
| `FileSummary` | `compressionType` | `compression_type` |
| `FileSummary` | `linkTarget` | `link_target` |
| `FileSummary` | `finderInfo` | `finder_info` |
| `TimeSummary` | `contentModified` | `content_modified` |
| `TimeSummary` | `attrModified` | `attr_modified` |
| `Capabilities` | `extendedAttributes` | `extended_attributes` |
| `Capabilities` | `hardLinks` | `hard_links` |
| `Capabilities` | `accessTimes` | `access_times` |
| `Capabilities` | `attrModTimes` | `attr_mod_times` |
| `Capabilities` | `posixPermissions` | `posix_permissions` |
| `Capabilities` | `caseSensitive` | `case_sensitive` |
| `Capabilities` | `unicodeNames` | `unicode_names` |

`Anomaly`'s keys (`op`, `offset`, `detail`) were already single words and did not
change.

### Breaking — the report's two version fields are now distinguishable

`Report.Version` and `VolumeSummary.Version` are unrelated facts one level apart
in the same document — the report's schema version and the volume's on-disk
format version — and both were serialised as `version`. A consumer reading the
JSON had no way to tell which was which.

| type | old | new |
| --- | --- | --- |
| `Report` | `Version int` / `version` | `SchemaVersion int` / `schema_version` |
| `VolumeSummary` | `Version uint16` / `version` | unchanged |

The `ReportVersion` constant keeps its name and its value of `1`; only the field
that carries it was renamed.

### Added

- **`Config.BaseOffset`** — the byte of the reader at which the volume begins,
  for callers whose reader is a whole disk image rather than one scoped to a
  partition. Every offset the package reports then stays absolute against that
  image. The alternative, an `io.SectionReader` over the partition, also reads
  the volume correctly but makes every reported offset partition-relative with
  nothing at the point of use to say so.

  It **adds to** the offset `Open` derives rather than replacing it. For an
  image holding a partition at byte N containing an HFS wrapper whose embedded
  HFS+ volume starts W bytes in, set it to N and `Volume.BaseOffset()` reports
  N+W. Note that `Config.BaseOffset` and `Volume.BaseOffset()` are named alike
  for consistency with this library's siblings but are not the same quantity:
  the first says where the volume begins, the second where its allocation block
  0 begins, and on classic HFS they differ by the MDB and the bitmap.

  A negative value is rejected at open with `ErrInvalidOffset`; one past the end
  of the reader yields `ErrShortRead` naming the supplied offset. The zero value
  reproduces the previous behaviour exactly, so existing callers are unaffected.

- **`Report.LibraryVersion`** / `library_version` — the release of this package
  that produced the document, from the new exported `LibraryVersion` constant.
  It is maintained by hand and bumped with the tag.

### Fixed

- A `ParseError` raised while parsing an **embedded** HFS+ volume header
  reported offset 1024 even though the read that fetched those bytes went to
  `embeddedOffset+1024`. Errors from the open path now name an offset the caller
  can seek to.
- `Volume.BlockOffset`'s overflow guard converted the base offset to `uint64`,
  so a negative one wrapped and the guard never fired, returning a garbage
  offset rather than an error. It now rejects a negative base offset explicitly.
  Unreachable before this release, since the base offset was always derived.

## v0.3.1

### Breaking

- Renamed the package clause from `hfs` to `libhfs`, matching the module path
  and the other five libraries in this family.

### Added

- `ErrStopWalk`, exported and honoured by every `Walk` method.
- HFS+ hard-link resolution by iNode name, including directory links, and
  Finder aliases no longer misclassified as links.
- `Volume.VolumeName`; `IsCorrupt` widened.
- Nil-receiver guards on every exported method.

### Fixed

- Classic HFS file paths resolved without a thread record, which classic HFS
  files do not have.
- Classic HFS MDB volume counts and thread-record decoding, both of which were
  read at the wrong offsets.
- Range end overflow, compressed-file reporting, and the bitmap byte count.

## v0.3.0

### Added

- Physical addressing: `Volume.BaseOffset`, `ByteRange`,
  `Volume.DataForkRanges`, `Volume.ResourceForkRanges`, `Volume.XAttrRanges`.
- Path-resolving walks: `Volume.WalkPaths`, `Volume.WalkPathsContext`.
- Identity: `FileIdentity` with `Equal`, `Comparable`, `IdentityByCNID`,
  `IdentityByPath`, and `CatalogRecord.Identity`.
- `Volume.UUID` and the JSON `Report`.

### Fixed

- The classic HFS volume bitmap was read at a double-counted offset.
