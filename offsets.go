package hfs

// On-disk structure offsets, grouped by the structure they describe.
//
// Every offset here is a fact about the format, not a tunable. They are named
// after the field names used in Apple's own headers (TN1150 for HFS+, Inside
// Macintosh: Files for classic HFS) so a reader can check them against the
// specification without decoding arithmetic.
//
// Sizes are byte counts. Offsets are from the start of the structure named in
// the group comment, not from the start of the record or the volume.

// HFSPlusForkData — the 80-byte fork descriptor embedded in volume headers,
// catalog records and attribute records.
const (
	forkDataLogicalSize = 0  // UInt64
	forkDataClumpSize   = 8  // UInt32
	forkDataTotalBlocks = 12 // UInt32
	forkDataExtents     = 16 // HFSPlusExtentRecord: 8 × (startBlock, blockCount)
	forkDataSize        = 80

	extentDescriptorSize  = 8 // UInt32 startBlock + UInt32 blockCount
	extentRecordCount     = 8 // extents per HFSPlusExtentRecord
	extentRecordSize      = extentDescriptorSize * extentRecordCount
	hfsExtentRecordCount  = 3 // classic HFS packs only three
	hfsExtentDescriptorSz = 4 // UInt16 startBlock + UInt16 blockCount
	hfsExtentRecordSize   = hfsExtentDescriptorSz * hfsExtentRecordCount
)

// HFSPlusVolumeHeader — at volumeHeaderOffset from the start of the volume.
const (
	volHdrSignature          = 0
	volHdrVersion            = 2
	volHdrAttributes         = 4
	volHdrLastMountedVersion = 8
	volHdrJournalInfoBlock   = 12
	volHdrCreateDate         = 16
	volHdrModifyDate         = 20
	volHdrBackupDate         = 24
	volHdrCheckedDate        = 28
	volHdrFileCount          = 32
	volHdrFolderCount        = 36
	volHdrBlockSize          = 40
	volHdrTotalBlocks        = 44
	volHdrFreeBlocks         = 48
	volHdrNextAllocation     = 52
	volHdrRsrcClumpSize      = 56
	volHdrDataClumpSize      = 60
	volHdrNextCatalogID      = 64
	volHdrWriteCount         = 68
	volHdrEncodingsBitmap    = 72
	volHdrFinderInfo         = 80
	volHdrFinderInfoWords    = 8
	volHdrAllocationFile     = 112
	volHdrExtentsFile        = 192
	volHdrCatalogFile        = 272
	volHdrAttributesFile     = 352
	volHdrStartupFile        = 432
)

// HFSPlusCatalogFolder and HFSPlusCatalogFile.
//
// The two records share their first 88 bytes, so the date, permission and
// Finder offsets below apply to both. Only the tail differs: a folder ends at
// 88, while a file continues with two fork descriptors.
const (
	catRecordType    = 0
	catFlags         = 2
	catValence       = 4 // folders only; reserved on files
	catNodeID        = 8
	catCreateDate    = 12
	catContentModDte = 16
	catAttrModDate   = 20
	catAccessDate    = 24
	catBackupDate    = 28
	catPermissions   = 32 // HFSPlusBSDInfo
	catUserInfo      = 48 // FndrFileInfo or FndrDirInfo
	catFinderInfo    = 64 // FndrOpaqueInfo
	catTextEncoding  = 80

	// catFinderBlock spans userInfo and finderInfo together, which is how this
	// package exposes them: their interpretation differs between files and
	// folders, so the bytes are surfaced undecoded.
	catFinderBlock     = catUserInfo
	catFinderBlockSize = 32

	// Within FndrFileInfo, valid for file records only.
	catFileType    = catUserInfo + 0
	catFileCreator = catUserInfo + 4

	catFolderRecordSize = 88
	catDataFork         = 88
	catRsrcFork         = catDataFork + forkDataSize
	catFileRecordSize   = catRsrcFork + forkDataSize // 248
)

// HFSPlusBSDInfo — 16 bytes at catPermissions.
const (
	bsdOwnerID    = 0
	bsdGroupID    = 4
	bsdAdminFlags = 8
	bsdOwnerFlags = 9
	bsdFileMode   = 10
	bsdSpecial    = 14 // union: iNodeNum | linkCount | rawDevice
	bsdInfoSize   = 16
)

// HFSPlusCatalogThread — the record that maps a CNID back to its parent and
// name.
const (
	threadRecordType = 0
	threadParentID   = 4
	threadNameLength = 8
	threadName       = 10
	threadHeaderSize = threadName
)

// HFSPlusCatalogKey.
const (
	catKeyLength     = 0 // UInt16, counts everything after itself
	catKeyParentID   = 0 // within the key body
	catKeyNameLength = 4 // within the key body
	catKeyName       = 6 // within the key body
	catKeyBodyMin    = catKeyName
	catKeyMinSize    = 8 // keyLength word plus a zero-length name
)

// HFSPlusExtentKey.
const (
	extKeyLength     = 0 // UInt16
	extKeyForkType   = 2 // UInt8, then one pad byte
	extKeyFileID     = 4 // UInt32
	extKeyStartBlock = 8 // UInt32
	extKeyMinSize    = 12
)

// HFSPlusAttrKey.
const (
	attrKeyPad        = 0 // within the key body
	attrKeyFileID     = 2
	attrKeyStartBlock = 6
	attrKeyNameLength = 10
	attrKeyName       = 12
	attrKeyBodyMin    = attrKeyName
	attrKeyMinSize    = 14 // keyLength word plus a zero-length name
)

// BTNodeDescriptor — the 14-byte header every B-tree node begins with.
const (
	nodeForwardLink  = 0
	nodeBackwardLink = 4
	nodeType         = 8
	nodeHeight       = 9
	nodeNumRecords   = 10
	nodeReserved     = 12

	// nodeRecordOffsetSize is the width of one entry in the offset array that
	// grows down from the end of a node.
	nodeRecordOffsetSize = 2

	// nodeChildPointerSize is the node number appended to every index record.
	nodeChildPointerSize = 4
)

// BTHeaderRec — the first record of a B-tree's header node.
const (
	btHdrDepth         = 0
	btHdrRootNode      = 2
	btHdrLeafRecords   = 6
	btHdrFirstLeafNode = 10
	btHdrLastLeafNode  = 14
	btHdrNodeSize      = 18
	btHdrMaxKeyLength  = 20
	btHdrTotalNodes    = 22
	btHdrFreeNodes     = 26
	btHdrClumpSize     = 32
	btHdrBTreeType     = 36
	btHdrKeyCompareTyp = 37
	btHdrAttributes    = 38

	// btHdrMapRecordIndex is where the node allocation bitmap lives in the
	// header node — the third record, after the header record and a reserved
	// one.
	btHdrMapRecordIndex = 2
	btHdrMapRecordCount = 3 // records a header node must hold for the map to exist
)

// Classic HFS CatDirRec.
const (
	hfsDirFlags      = 2
	hfsDirValence    = 4
	hfsDirDirID      = 6
	hfsDirCreateDate = 10
	hfsDirModifyDate = 14
	hfsDirBackupDate = 18
	hfsDirUserInfo   = 22
	hfsDirFinderInfo = 38
	hfsDirRecordSize = 70

	// hfsDirMinSize covers the record through dirBkDat, which is as far as this
	// package reads.
	hfsDirMinSize = 22
)

// Classic HFS CatFilRec.
const (
	hfsFilFlags       = 2
	hfsFilType        = 3
	hfsFilUserWords   = 4 // FInfo: fdType then fdCreator
	hfsFilFdType      = hfsFilUserWords + 0
	hfsFilFdCreator   = hfsFilUserWords + 4
	hfsFilFileNumber  = 20
	hfsFilStartBlock  = 24
	hfsFilLogicalSize = 26
	hfsFilPhysSize    = 30
	hfsFilRStartBlock = 34
	hfsFilRLogicalLen = 36
	hfsFilRPhysLen    = 40
	hfsFilCreateDate  = 44
	hfsFilModifyDate  = 48
	hfsFilBackupDate  = 52
	hfsFilFinderInfo  = 56
	hfsFilClumpSize   = 72
	hfsFilExtentRec   = 74
	hfsFilRExtentRec  = 86
	hfsFilRecordSize  = 102

	// hfsFilMinSize covers the record through filRExtRec.
	hfsFilMinSize = 98
)

// Classic HFS catalog key (CatKeyRec) and extents key.
const (
	hfsCatKeyReserved   = 0 // within the key body
	hfsCatKeyParentID   = 1
	hfsCatKeyNameLength = 5
	hfsCatKeyName       = 6
	hfsCatKeyMinSize    = 7
	hfsCatKeyMaxName    = 31 // Str31

	hfsExtKeyForkType   = 0 // within the key body
	hfsExtKeyFileID     = 1
	hfsExtKeyStartBlock = 5
	hfsExtKeyMinSize    = 8
)

// Classic HFS record type tags, in the first byte of a catalog record.
const (
	hfsRecordTypeFolder       = 0x01
	hfsRecordTypeFile         = 0x02
	hfsRecordTypeFolderThread = 0x03
	hfsRecordTypeFileThread   = 0x04
)

// decmpfs attribute value layout.
const (
	decmpfsMagicOffset      = 0
	decmpfsMagicSize        = 4
	decmpfsTypeOffset       = 4
	decmpfsUncompressedSize = 8
	// decmpfsHeaderSize is declared in attributes.go alongside the attribute
	// record constants it is used with.
)

// decmpfs resource-fork chunk table.
const (
	// resourceHeaderSize is the fixed resource-fork header preceding the data.
	resourceHeaderSize = 0x100
	// resourceDataOffsetField is the word holding the offset of the resource
	// data, at the very start of that header.
	resourceDataOffsetField = 0
	resourceDataOffsetSize  = 4
	// chunkTableLengthSize is the big-endian total length preceding the table.
	chunkTableLengthSize = 4
	// chunkCountSize is the little-endian chunk count that begins the table,
	// and the origin every chunk offset in the table is relative to.
	chunkCountSize = 4
	// chunkEntrySize is one (offset, length) pair, both little-endian.
	chunkEntrySize = 8
)

// Structural limits used to sanity-check values read from a volume.
const (
	// minBTreeNodeSize and maxBTreeNodeSize bound a plausible node size. HFS+
	// node sizes are powers of two in this range; anything outside it means the
	// header record is not what it claims to be.
	minBTreeNodeSize = 512
	maxBTreeNodeSize = 32768

	// hfsSectorSize is the 512-byte sector classic HFS counts several of its
	// offsets in, including drVBMSt and drAlBlSt.
	hfsSectorSize = 512
)

// Primitive widths used throughout the on-disk structures.
const (
	// utf16CodeUnitSize is the width of one UTF-16 code unit. HFS+ name
	// lengths are counted in code units, not bytes, so name spans are always a
	// multiple of this.
	utf16CodeUnitSize = 2
)

// Bit and byte arithmetic used by the allocation bitmap.
const (
	bitsPerByte = 8
	// bitmapHighBitFirst reflects that block N is bit (7 - N%8) of byte N/8:
	// the most significant bit of each byte describes the lowest block.
	bitmapMSBFirst = bitsPerByte - 1
)
