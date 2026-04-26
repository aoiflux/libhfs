package hfs

import "time"

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

type Volume struct {
	reader ioReaderAt
	kind   FileSystemKind
	header VolumeHeader
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
