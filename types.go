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
	DataFork      ForkData
	RsrcFork      ForkData
	Times         CatalogTimes
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

func (v *Volume) Kind() FileSystemKind { return v.kind }
func (v *Volume) Header() VolumeHeader { return v.header }

func (v *Volume) Close() error { return nil }
