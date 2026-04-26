package hfs

const (
	volumeHeaderOffset = 1024
	volumeHeaderSize   = 512
	btreeNodeDescSize  = 14
	btreeHeaderRecSize = 106

	signatureHFS  = 0x4244 // "BD"
	signatureHFSP = 0x482B // "H+"
	signatureHFSX = 0x4858 // "HX"

	versionHFSPlus = 0x0004
	versionHFSX    = 0x0005

	hfsEpochDeltaSeconds = uint32(2082844800)
)

const (
	btreeNodeTypeLeaf = int8(-1)
	btreeNodeTypeIdx  = int8(0)
	btreeNodeTypeHead = int8(1)
	btreeNodeTypeMap  = int8(2)
)

const (
	btreeCompTypeSensitive   = uint8(0xBC)
	btreeCompTypeInsensitive = uint8(0xC7)
)

const (
	extentKeyTypeData = uint8(0x00)
	extentKeyTypeRsrc = uint8(0xFF)
)

const (
	catalogRecordFolder       = uint16(0x0001)
	catalogRecordFile         = uint16(0x0002)
	catalogRecordFolderThread = uint16(0x0003)
	catalogRecordFileThread   = uint16(0x0004)
)

const (
	rootFolderCNID = uint32(2)
)

const (
	hfsHardlinkFileType    = uint32(0x686C6E6B) // "hlnk"
	hfsHardlinkFileCreator = uint32(0x6866732B) // "hfs+"
)
