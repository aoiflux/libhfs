package libhfs

import (
	"context"
	"encoding/hex"
	"errors"
	"time"
)

// ReportVersion is the schema version stamped into every [Report]. It changes
// when a field changes meaning or disappears, so a consumer can refuse a
// document it does not understand rather than misread one.
const ReportVersion = 1

// LibraryVersion is the release of this package stamped into every [Report].
//
// It is maintained by hand and must be bumped with the tag. Nothing in the test
// suite can detect a forgotten bump — the module knows nothing about its own
// version at compile time — so a report from an untagged build of the tree
// claims whatever this said when it was last edited. Treat it as the answer to
// "which release was this written against", not as a proof of provenance.
const LibraryVersion = "v0.4.0"

// DefaultReportMaxFiles bounds the file listing when [ReportOptions] asks for
// one without saying how many.
const DefaultReportMaxFiles = 10000

// Report is a self-contained, JSON-encodable summary of a volume.
//
// It exists because the types this package returns describe on-disk structure
// rather than a document: a [CatalogRecord]'s Finder information is 32 raw
// bytes, a [VolumeHeader]'s unset dates read back as the Unix epoch, and a zero
// [CatalogTimes] field means absent rather than the year 1. Encoding those
// directly produces output that is either unreadable or quietly wrong, so this
// type projects them into forms that survive the round trip: bytes as hex,
// absent dates as null, enumerations as their String form.
//
// Nothing here is a new fact about the volume. Every field is reachable through
// the ordinary API, and this type exists only so that a tool can emit one
// document instead of assembling one.
//
// Two things mislead if taken at face value. A null date means the volume
// recorded none, and because dates before 1970 are clamped away when the header
// is parsed, a volume genuinely created at the Unix epoch is indistinguishable
// from one with no creation date. And the block counts are the volume header's
// own claim rather than a count of the allocation bitmap — see
// [Volume.FreeBlockCount] for the observed figure, and note that a mismatch
// between the two is itself a finding.
type Report struct {
	// SchemaVersion is [ReportVersion], the schema this document follows.
	//
	// It is named for the schema rather than simply "version" because the
	// volume's own on-disk format version lives one level down at
	// [VolumeSummary.Version]. The two are unrelated facts, and while both were
	// serialised as "version" a consumer could not tell which it was reading
	// without the Go source in front of it.
	SchemaVersion int `json:"schema_version"`

	// LibraryVersion is [LibraryVersion], the release of this package that
	// produced the document. A report is evidence, and evidence has to stay
	// re-readable by a tool built against a different release; this says which
	// one wrote it.
	LibraryVersion string `json:"library_version"`

	// Generated is when the report was built, not a fact about the volume.
	Generated time.Time `json:"generated"`

	Volume       VolumeSummary `json:"volume"`
	Capabilities Capabilities  `json:"capabilities"`

	// Anomalies lists the distinct structural inconsistencies the volume has
	// recorded, and AnomalyTotal counts every occurrence including repeats.
	//
	// They are read from the volume rather than collected by the report, so
	// they cover everything since [Open] — including whatever a caller's own
	// earlier reads provoked — not only what building the report found. A
	// report built without a file listing walks much less of the volume and so
	// contributes correspondingly little of its own.
	Anomalies    []Anomaly `json:"anomalies"`
	AnomalyTotal int       `json:"anomaly_total"`

	// Files is present only when [ReportOptions].IncludeFiles asked for it.
	// FilesTruncated says the listing stopped at the bound rather than at the
	// end of the catalog.
	Files          []FileSummary `json:"files"`
	FilesTruncated bool          `json:"files_truncated"`
}

// VolumeSummary is the volume's identity and geometry.
type VolumeSummary struct {
	Kind string `json:"kind"`

	// Version is the volume format version recorded in the volume header —
	// 4 for HFS+, 5 for HFSX, 0 for classic HFS, which has no such field. It
	// is a fact about the volume on disk and has nothing to do with
	// [Report.SchemaVersion], which describes this document.
	Version uint16 `json:"version"`

	// Name is the volume's name. It is empty when the volume has none that
	// could be read — a classic HFS volume with a zero-length drVN, or an
	// HFS+ volume whose root catalog record is unreachable — rather than the
	// report failing over a cosmetic field. See [Volume.VolumeName].
	Name string `json:"name"`

	// Identifier is the 64-bit value stored in the volume header, as hex, and
	// UUID is the RFC 4122 string macOS derives from it. They are different
	// values; see [VolumeIdentifier]. Both are empty when the volume carries
	// no identifier.
	Identifier string `json:"identifier"`
	UUID       string `json:"uuid"`

	// BaseOffset is the image byte offset of allocation block 0. It is
	// reported because every block number in this document is meaningless
	// without it. See [Volume.BaseOffset], whose value this is — note that it
	// is not the same quantity as [Config.BaseOffset], which says where the
	// volume begins rather than where its allocation blocks are numbered from.
	BaseOffset int64 `json:"base_offset"`

	BlockSize   uint32 `json:"block_size"`
	TotalBlocks uint32 `json:"total_blocks"`
	FreeBlocks  uint32 `json:"free_blocks"`
	TotalBytes  uint64 `json:"total_bytes"`
	FreeBytes   uint64 `json:"free_bytes"`

	FileCount     uint32 `json:"file_count"`
	FolderCount   uint32 `json:"folder_count"`
	NextCatalogID uint32 `json:"next_catalog_id"`
	Journaled     bool   `json:"journaled"`

	Created  *time.Time `json:"created"`
	Modified *time.Time `json:"modified"`
	Backup   *time.Time `json:"backup"`
	Checked  *time.Time `json:"checked"`
}

// FileSummary is one catalog record with its path.
type FileSummary struct {
	Path       string `json:"path"`
	CNID       uint32 `json:"cnid"`
	ParentCNID uint32 `json:"parent_cnid"`
	Name       string `json:"name"`
	Type       string `json:"type"`

	// Identity is [FileIdentity.String], the composite handle for matching
	// this record against another reading of the same volume.
	Identity string `json:"identity"`

	Size         uint64 `json:"size"`
	ResourceSize uint64 `json:"resource_size"`

	Compressed      bool   `json:"compressed"`
	CompressionType uint32 `json:"compression_type"`

	Link       string `json:"link"`
	LinkTarget uint32 `json:"link_target"`

	Mode uint16 `json:"mode"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`

	System bool        `json:"system"`
	Times  TimeSummary `json:"times"`

	// FinderInfo is the record's 32 bytes of Finder metadata as hex. It is the
	// empty string when they are all zero, which is the case on classic HFS.
	FinderInfo string `json:"finder_info"`
}

// TimeSummary is a [CatalogTimes] with absent stamps encoded as null rather
// than as the zero time.
type TimeSummary struct {
	Created         *time.Time `json:"created"`
	ContentModified *time.Time `json:"content_modified"`
	AttrModified    *time.Time `json:"attr_modified"`
	Accessed        *time.Time `json:"accessed"`
	Backup          *time.Time `json:"backup"`
	Source          string     `json:"source"`
}

// ReportOptions selects what a report covers. A nil *ReportOptions means the
// values from [DefaultReportOptions].
type ReportOptions struct {
	// IncludeFiles adds a per-file listing. It is off by default because
	// building one is a full catalog walk, which on a volume of any size is
	// orders of magnitude more work than everything else in the report put
	// together — a caller asking for a volume summary should not pay for it by
	// accident.
	IncludeFiles bool

	// MaxFiles bounds the listing. Zero means [DefaultReportMaxFiles]; a
	// negative value means unbounded, which on a large volume holds the whole
	// catalog in memory. Report.FilesTruncated says whether the bound was hit.
	MaxFiles int

	// IncludeSystemFiles keeps the filesystem's own metadata in the listing,
	// such as the private directories hard links are stored in. Off by
	// default, since a listing of user data is the usual intent.
	IncludeSystemFiles bool
}

// DefaultReportOptions returns the options [Volume.Report] uses for a nil
// argument: a volume summary with no file listing.
func DefaultReportOptions() ReportOptions {
	return ReportOptions{
		IncludeFiles:       false,
		MaxFiles:           DefaultReportMaxFiles,
		IncludeSystemFiles: false,
	}
}

// errReportFull stops the catalog walk once the file bound is reached.
var errReportFull = errors.New("libhfs: report file limit reached")

// Report builds a summary of the volume.
//
// See [Report] for what it contains and how to read it, and [ReportOptions] for
// what it costs. This package does not marshal the result; the caller does,
// with encoding/json or anything else.
func (v *Volume) Report(opts *ReportOptions) (Report, error) {
	return v.ReportContext(context.Background(), opts)
}

// ReportContext is [Volume.Report] with cancellation.
//
// Only the file listing takes meaningful time, so the context is checked once
// per record while it is being built; its error is returned.
func (v *Volume) ReportContext(ctx context.Context, opts *ReportOptions) (Report, error) {
	if v == nil {
		return Report{}, &ParseError{Op: "report", Offset: 0, Err: ErrCorrupt}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cfg := DefaultReportOptions()
	if opts != nil {
		cfg = *opts
	}
	if cfg.MaxFiles == 0 {
		cfg.MaxFiles = DefaultReportMaxFiles
	}

	rep := Report{
		SchemaVersion:  ReportVersion,
		LibraryVersion: LibraryVersion,
		Generated:      time.Now().UTC(),
		Volume:         v.volumeSummary(),
		Capabilities:   v.Capabilities(),
	}

	if cfg.IncludeFiles {
		files, truncated, err := v.fileSummaries(ctx, cfg)
		if err != nil {
			return Report{}, err
		}
		rep.Files = files
		rep.FilesTruncated = truncated
	}

	// Read the anomalies last: the file listing is what provokes most of them.
	rep.Anomalies = v.Anomalies()
	rep.AnomalyTotal = v.AnomalyCount()
	return rep, nil
}

func (v *Volume) volumeSummary() VolumeSummary {
	hdr := v.header
	caps := v.Capabilities()

	out := VolumeSummary{
		Kind:          string(v.kind),
		Version:       hdr.Version,
		BaseOffset:    v.BaseOffset(),
		BlockSize:     hdr.BlockSize,
		TotalBlocks:   hdr.TotalBlocks,
		FreeBlocks:    hdr.FreeBlocks,
		TotalBytes:    uint64(hdr.TotalBlocks) * uint64(hdr.BlockSize),
		FreeBytes:     uint64(hdr.FreeBlocks) * uint64(hdr.BlockSize),
		FileCount:     hdr.FileCount,
		FolderCount:   hdr.FolderCount,
		NextCatalogID: hdr.NextCatalogID,
		Journaled:     caps.Journaled,
		Created:       headerTime(hdr.CreateTime),
		Modified:      headerTime(hdr.ModifyTime),
		Backup:        headerTime(hdr.BackupTime),
		Checked:       headerTime(hdr.CheckedTime),
	}

	if id, err := v.VolumeIdentifier(); err == nil {
		out.Identifier = id.String()
		out.UUID = id.UUID()
	}
	// A volume with no readable name still gets a report: the name is how a
	// reader recognises the volume, but it is not what the report is for, and
	// a damaged catalog is the case where the rest of these numbers matter
	// most.
	if name, err := v.VolumeName(); err == nil {
		out.Name = name
	}
	return out
}

func (v *Volume) fileSummaries(ctx context.Context, cfg ReportOptions) ([]FileSummary, bool, error) {
	var (
		out       []FileSummary
		truncated bool
	)

	err := v.WalkPathsContext(ctx, func(path string, rec CatalogRecord) error {
		system := isSystemFile(rec.Name)
		if system && !cfg.IncludeSystemFiles {
			return nil
		}
		if cfg.MaxFiles >= 0 && len(out) >= cfg.MaxFiles {
			truncated = true
			return errReportFull
		}
		// WalkPaths yields decoded records, not hydrated ones, and decmpfs
		// compression is recorded in an extended attribute rather than in the
		// catalog. Without this the report calls every compressed file
		// uncompressed and zero-length — a silent misstatement in the one
		// artefact meant to be quotable. It costs an attribute lookup only for
		// a file whose data fork is empty; hydrateCompressedRecord returns
		// immediately for anything else.
		out = append(out, fileSummary(path, v.hydrateCompressedRecord(rec), system))
		return nil
	})
	if err != nil && !errors.Is(err, errReportFull) {
		return nil, false, err
	}
	return out, truncated, nil
}

func fileSummary(path string, rec CatalogRecord, system bool) FileSummary {
	return FileSummary{
		Path:            path,
		CNID:            rec.CNID,
		ParentCNID:      rec.ParentCNID,
		Name:            rec.Name,
		Type:            rec.Type.String(),
		Identity:        rec.Identity().String(),
		Size:            rec.DataFork.LogicalSize,
		ResourceSize:    rec.RsrcFork.LogicalSize,
		Compressed:      rec.Compressed,
		CompressionType: rec.CompressionType,
		Link:            rec.Link.String(),
		LinkTarget:      rec.LinkTarget,
		Mode:            rec.Perms.FileMode,
		UID:             rec.Perms.OwnerID,
		GID:             rec.Perms.GroupID,
		System:          system,
		Times:           timeSummary(rec.Times),
		FinderInfo:      finderInfoHex(rec.FinderInfo),
	}
}

func timeSummary(t CatalogTimes) TimeSummary {
	return TimeSummary{
		Created:         catalogTime(t.Created),
		ContentModified: catalogTime(t.ContentModified),
		AttrModified:    catalogTime(t.AttrModified),
		Accessed:        catalogTime(t.Accessed),
		Backup:          catalogTime(t.Backup),
		Source:          t.Source.String(),
	}
}

// catalogTime encodes an absent catalog stamp as null. A zero time.Time means
// the field was unset on disk, and rendering it as year 1 would invent a date.
func catalogTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// headerTime encodes an absent volume-header date as null.
//
// Header dates cannot use IsZero: hfsTimeToUnix clamps everything at or below
// the epoch delta to exactly time.Unix(0, 0), so an unset field arrives as 1970
// rather than as the zero time. The cost of reading it as absent is that a
// volume genuinely created at the Unix epoch reports no creation date, which is
// documented on [Report].
func headerTime(t time.Time) *time.Time {
	if t.IsZero() || t.Equal(time.Unix(0, 0)) {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// finderInfoHex renders Finder information as hex, and as the empty string when
// there is none — 64 zeroes carry no more meaning than an absent field and make
// every listing harder to read.
func finderInfoHex(fi [32]byte) string {
	for _, b := range fi {
		if b != 0 {
			return hex.EncodeToString(fi[:])
		}
	}
	return ""
}
