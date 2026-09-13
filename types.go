package hfs

import (
	"sync"
	"time"
)

type FileSystemKind string

const (
	KindHFS  FileSystemKind = "HFS"
	KindHFSP FileSystemKind = "HFS+"
	KindHFSX FileSystemKind = "HFSX"
)

type ExtentDescriptor struct {
	StartBlock uint32
	BlockCount uint32
}

type ForkData struct {
	LogicalSize uint64
	ClumpSize   uint32
	TotalBlocks uint32
	Extents     [8]ExtentDescriptor
}

type VolumeHeader struct {
	Signature          uint16
	Version            uint16
	Attributes         uint32
	LastMountedVersion uint32
	JournalInfoBlock   uint32
	CreateTime         time.Time
	ModifyTime         time.Time
	BackupTime         time.Time
	CheckedTime        time.Time
	FileCount          uint32
	FolderCount        uint32
	BlockSize          uint32
	TotalBlocks        uint32
	FreeBlocks         uint32
	NextAllocation     uint32
	RsrcClumpSize      uint32
	DataClumpSize      uint32
	NextCatalogID      uint32
	WriteCount         uint32
	EncodingsBitmap    uint64
	FinderInfo         [8]uint32
	AllocationFile     ForkData
	ExtentsFile        ForkData
	CatalogFile        ForkData
	AttributesFile     ForkData
	StartupFile        ForkData
}

// Volume is an open, read-only HFS, HFS+ or HFSX filesystem.
//
// A *Volume is safe for concurrent use by multiple goroutines. This requires
// that the io.ReaderAt it was opened with honours the standard contract that
// parallel ReadAt calls are permitted — *os.File, *bytes.Reader and
// *io.SectionReader all do.
//
// A *File returned by the Open*By* methods is NOT safe for concurrent use,
// because Read advances a per-handle offset. Give each goroutine its own
// handle, or use ReadAt, which does not touch that offset.
type Volume struct {
	reader ioReaderAt
	kind   FileSystemKind
	header VolumeHeader
	// baseOffset is the byte offset where the parsed HFS+ volume starts on disk.
	// It is zero for non-wrapper volumes.
	baseOffset int64

	// hfsVBMStart is drVBMSt from a classic HFS MDB: the start of the volume
	// bitmap, in 512-byte sectors from the volume start. Classic HFS has no
	// allocation *file*, so the bitmap's location cannot be derived from a
	// ForkData the way it can on HFS+. Zero on HFS+ and HFSX.
	hfsVBMStart uint16

	// mu guards every field below it. The fields above are written once during
	// Open and read-only thereafter, so they need no locking.
	mu           sync.RWMutex
	recCache     map[uint32]CatalogRecord
	recOrder     []uint32
	cacheMax     int
	nodeCache    map[nodeCacheKey][]byte
	nodeOrder    []nodeCacheKey
	nodeCacheMax int
	anomalies    []Anomaly
	anomalyTotal int
	textEncoding TextEncoding
	maxAlloc     int64
	carveWorkers int

	// privDirCNID caches the hard-link store's CNID, and privDirLooked records
	// that the search has been made — including when it found nothing, so a
	// volume without one does not rescan its root for every link.
	privDirCNID   uint32
	privDirFound  bool
	privDirLooked bool

	// codecs holds decmpfs decoders scoped to this volume. It carries its own
	// lock, so it is not guarded by mu.
	codecs codecRegistry
}

type BTreeNodeDescriptor struct {
	ForwardLink  uint32
	BackwardLink uint32
	Type         int8
	Height       uint8
	NumRecords   uint16
}

type BTreeHeaderRecord struct {
	Depth         uint16
	RootNode      uint32
	LeafRecords   uint32
	FirstLeafNode uint32
	LastLeafNode  uint32
	NodeSize      uint16
	MaxKeyLen     uint16
	TotalNodes    uint32
	FreeNodes     uint32
	ClumpSize     uint32
	Type          uint8
	CompType      uint8
	Attributes    uint32
}

type CatalogKey struct {
	KeyLength  uint16
	ParentCNID uint32
	NameUTF16  []uint16

	// NameBytes holds the undecoded name for classic HFS keys, whose names are
	// bytes in a Mac script encoding rather than UTF-16. It is nil for HFS+ and
	// HFSX. Decoding is deferred to the Volume so the choice of encoding can be
	// a per-volume setting rather than a parse-time guess.
	NameBytes []byte
}

type ExtentsKey struct {
	KeyLength  uint16
	ForkType   uint8
	FileID     uint32
	StartBlock uint32
}

type CatalogRecordType uint16

const (
	CatalogRecordFolder       CatalogRecordType = CatalogRecordType(catalogRecordFolder)
	CatalogRecordFile         CatalogRecordType = CatalogRecordType(catalogRecordFile)
	CatalogRecordFolderThread CatalogRecordType = CatalogRecordType(catalogRecordFolderThread)
	CatalogRecordFileThread   CatalogRecordType = CatalogRecordType(catalogRecordFileThread)
)

// String names the record type. An unrecognised value renders as "unknown"
// rather than as its number, because a record type this package does not know
// is a damaged record rather than a new kind of one.
func (t CatalogRecordType) String() string {
	switch t {
	case CatalogRecordFolder:
		return "folder"
	case CatalogRecordFile:
		return "file"
	case CatalogRecordFolderThread:
		return "folder thread"
	case CatalogRecordFileThread:
		return "file thread"
	default:
		return "unknown"
	}
}

// TimeSource describes how a set of catalog timestamps must be interpreted.
// Callers doing timeline work need this: HFS+ catalog dates are GMT, but
// classic HFS records wall-clock local time with no recorded UTC offset.
type TimeSource uint8

const (
	// TimeSourceUnknown means no timestamps were parsed for the record
	// (thread records carry none).
	TimeSourceUnknown TimeSource = iota

	// TimeSourceHFSPlusGMT marks HFS+/HFSX catalog dates: seconds since
	// 1904-01-01 GMT. Directly comparable across volumes.
	TimeSourceHFSPlusGMT

	// TimeSourceHFSLocal marks classic HFS dates: seconds since 1904-01-01
	// in whatever local time the writing Mac was set to, with no offset
	// stored anywhere on the volume. Treat these as wall-clock readings, not
	// as absolute instants; they are not comparable across time zones without
	// an examiner-supplied offset.
	TimeSourceHFSLocal
)

func (s TimeSource) String() string {
	switch s {
	case TimeSourceHFSPlusGMT:
		return "HFS+ GMT"
	case TimeSourceHFSLocal:
		return "HFS local"
	default:
		return "unknown"
	}
}

// CatalogTimes holds the on-disk MACB timestamp set for one catalog record.
//
// A zero time.Time means the field was unset on disk. It does not mean 1904
// and it does not mean 1970 — always test with IsZero before using a value.
// Classic HFS has no access or attribute-modification date, so those two
// fields are always zero when Source is TimeSourceHFSLocal.
type CatalogTimes struct {
	Created         time.Time // birth ("B")
	ContentModified time.Time // data last written ("M")
	AttrModified    time.Time // metadata last changed ("C")
	Accessed        time.Time // last read ("A"); may be disabled volume-wide
	Backup          time.Time // last backup stamp
	Source          TimeSource
}

// IsZero reports whether no timestamp in the set was present on disk.
func (t CatalogTimes) IsZero() bool {
	return t.Created.IsZero() && t.ContentModified.IsZero() &&
		t.AttrModified.IsZero() && t.Accessed.IsZero() && t.Backup.IsZero()
}

type CatalogRecord struct {
	Type          CatalogRecordType
	ParentCNID    uint32
	CNID          uint32
	ThreadCNID    uint32
	Name          string
	Valence       uint32
	LinkID        uint32
	FinderType    uint32
	FinderCreator uint32
	Compressed    bool

	// CompressionType is the decmpfs codec identifier when Compressed is true.
	// It is meaningful even when decompression is unavailable: a caller that
	// receives ErrUnsupportedCompression can report exactly which codec the
	// file needed.
	CompressionType uint32

	DataFork ForkData
	RsrcFork ForkData
	Times    CatalogTimes

	// Link classifies this record as a hard link, symlink or ordinary node.
	// OpenCNID resolves hard links before returning, so a record obtained that
	// way reports the target's content with the link's identity; use
	// OpenCNIDRaw to see the link record untouched.
	Link LinkKind

	// LinkTarget is the inode CNID a hard link points at, or 0.
	LinkTarget uint32

	// LinkCount is how many hard links share this inode, where known. It is 0
	// for records that are not hard-link inodes.
	LinkCount uint32

	// Perms holds POSIX ownership and mode. Zero on classic HFS, which has no
	// equivalent.
	Perms BSDInfo

	// FinderInfo is the record's 32 bytes of Finder metadata — userInfo
	// followed by finderInfo — exposed undecoded. The layout differs between
	// files and folders, and forensic callers generally want the bytes rather
	// than an interpretation of them. Zero on classic HFS, whose smaller Finder
	// fields are surfaced through FinderType and FinderCreator instead.
	FinderInfo [32]byte
}

// IsSymlink reports whether this record is a symbolic link.
func (r CatalogRecord) IsSymlink() bool { return r.Link == LinkSymbolic }

// IsHardLink reports whether this record is a hard link to a file or directory.
func (r CatalogRecord) IsHardLink() bool {
	return r.Link == LinkHardFile || r.Link == LinkHardDir
}

func (r CatalogRecord) IsDirectory() bool {
	return r.Type == CatalogRecordFolder
}

type DirEntry struct {
	Name        string
	CNID        uint32
	Type        CatalogRecordType
	IsDirectory bool
	IsSystem    bool // HFS+ system files (e.g., $BadBlockFile, .HFS+ Private Directory Data)
}

type ioReaderAt interface {
	ReadAt(p []byte, off int64) (n int, err error)
}

// Kind reports which of the three formats this volume is.
//
// A nil Volume reports the empty kind rather than panicking. That matters
// because the value a failed Open returns is a nil *Volume, and an examiner
// tool that logs the kind before checking the error should produce a useless
// line in a report rather than take the process down mid-acquisition. The same
// reasoning runs through every method here: see Volume.Close.
func (v *Volume) Kind() FileSystemKind {
	if v == nil {
		return ""
	}
	return v.kind
}

// Header returns the parsed volume header, or the zero header for a nil Volume.
//
// The zero header is distinguishable from any real one: Open rejects a volume
// whose BlockSize or TotalBlocks is zero, so a header with either field zero
// never comes from a volume that opened successfully.
func (v *Volume) Header() VolumeHeader {
	if v == nil {
		return VolumeHeader{}
	}
	return v.header
}

func (v *Volume) Close() error { return nil }
