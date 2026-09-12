package hfs

// volume.go holds volume-header parsing and the geometry derived from it.
// The library's entry point, Open, lives in hfs.go.

import (
	"errors"
	"math"
	"time"
)

func parseHFSWrapperEmbeddedOffset(mdb []byte) (int64, bool) {
	if len(mdb) < volumeHeaderSize {
		return 0, false
	}
	if be16(mdb[volHdrSignature:volHdrSignature+2]) != signatureHFS {
		return 0, false
	}
	if be16(mdb[hfsMDBOffEmbedSigWord:hfsMDBOffEmbedSigWord+2]) != signatureHFSP {
		return 0, false
	}

	allocBlockSize := be32(mdb[hfsMDBOffBlockSize : hfsMDBOffBlockSize+4])
	allocBlockStart512 := be16(mdb[hfsMDBOffAlBlSt : hfsMDBOffAlBlSt+2])
	embedStartBlock := be16(mdb[hfsMDBOffEmbedExtent : hfsMDBOffEmbedExtent+2])
	embedBlockCount := be16(mdb[hfsMDBOffEmbedExtent+2 : hfsMDBOffEmbedExtent+4])

	if allocBlockSize == 0 || embedBlockCount == 0 {
		return 0, false
	}

	return int64(allocBlockStart512)*hfsSectorSize + int64(embedStartBlock)*int64(allocBlockSize), true
}

// parseHFSMasterDirectoryBlock decodes a classic HFS MDB. It returns the header,
// the byte offset of the allocation-block area, and the volume bitmap's start
// sector (drVBMSt), which classic HFS records directly rather than through a
// fork.
func parseHFSMasterDirectoryBlock(mdb []byte) (VolumeHeader, int64, uint16, error) {
	if len(mdb) < volumeHeaderSize {
		return VolumeHeader{}, 0, 0, &ParseError{Op: "parse_hfs_mdb", Offset: volumeHeaderOffset, Err: ErrShortRead}
	}
	if be16(mdb[volHdrSignature:volHdrSignature+2]) != signatureHFS {
		return VolumeHeader{}, 0, 0, &ParseError{Op: "parse_hfs_mdb", Offset: volumeHeaderOffset, Err: ErrInvalidSignature}
	}

	blockSize := be32(mdb[hfsMDBOffBlockSize : hfsMDBOffBlockSize+4])
	totalBlocks := uint32(be16(mdb[hfsMDBOffTotalBlocks : hfsMDBOffTotalBlocks+2]))
	if blockSize == 0 || totalBlocks == 0 {
		return VolumeHeader{}, 0, 0, &ParseError{Op: "parse_hfs_mdb", Offset: volumeHeaderOffset, Err: ErrCorrupt}
	}

	allocBlockStart512 := be16(mdb[hfsMDBOffAlBlSt : hfsMDBOffAlBlSt+2])
	dataBase := int64(allocBlockStart512) * hfsSectorSize

	hdr := VolumeHeader{
		Signature:      signatureHFS,
		Version:        0,
		Attributes:     uint32(be16(mdb[hfsMDBOffAttributes : hfsMDBOffAttributes+2])),
		CreateTime:     hfsTimeToUnix(be32(mdb[hfsMDBOffCreateTime : hfsMDBOffCreateTime+4])),
		ModifyTime:     hfsTimeToUnix(be32(mdb[hfsMDBOffModifyTime : hfsMDBOffModifyTime+4])),
		BackupTime:     hfsTimeToUnix(be32(mdb[hfsMDBOffBackupTime : hfsMDBOffBackupTime+4])),
		FileCount:      uint32(be16(mdb[hfsMDBOffFileCount : hfsMDBOffFileCount+2])),
		FolderCount:    uint32(be16(mdb[hfsMDBOffFolderCount : hfsMDBOffFolderCount+2])),
		BlockSize:      blockSize,
		TotalBlocks:    totalBlocks,
		FreeBlocks:     uint32(be16(mdb[hfsMDBOffFreeBlocks : hfsMDBOffFreeBlocks+2])),
		NextAllocation: uint32(be16(mdb[hfsMDBOffAllocPtr : hfsMDBOffAllocPtr+2])),
		NextCatalogID:  be32(mdb[hfsMDBOffNextCatalogID : hfsMDBOffNextCatalogID+4]),
		WriteCount:     uint32(be16(mdb[hfsMDBOffWriteCount : hfsMDBOffWriteCount+2])),
	}

	for i := range extentRecordCount {
		off := hfsMDBOffFinderInfo + i*4
		hdr.FinderInfo[i] = be32(mdb[off : off+4])
	}

	hdr.ExtentsFile = parseHFSForkData(be32(mdb[hfsMDBOffXTFlSize:hfsMDBOffXTFlSize+4]), mdb[hfsMDBOffXTExtRec:hfsMDBOffXTExtRec+12], blockSize)
	hdr.CatalogFile = parseHFSForkData(be32(mdb[hfsMDBOffCTFlSize:hfsMDBOffCTFlSize+4]), mdb[hfsMDBOffCTExtRec:hfsMDBOffCTExtRec+12], blockSize)

	return hdr, dataBase, be16(mdb[hfsMDBOffVBMStart : hfsMDBOffVBMStart+2]), nil
}

func parseHFSForkData(logicalSize uint32, extRec []byte, blockSize uint32) ForkData {
	fd := ForkData{LogicalSize: uint64(logicalSize)}
	if blockSize == 0 {
		return fd
	}
	fd.TotalBlocks = uint32((uint64(logicalSize) + uint64(blockSize) - 1) / uint64(blockSize))
	for i := 0; i < 3; i++ {
		base := i * 4
		if base+4 > len(extRec) {
			break
		}
		fd.Extents[i] = ExtentDescriptor{
			StartBlock: uint32(be16(extRec[base : base+2])),
			BlockCount: uint32(be16(extRec[base+2 : base+4])),
		}
	}
	return fd
}

func (v *Volume) diskOffset(rel int64) int64 {
	if v == nil {
		return rel
	}
	return v.baseOffset + rel
}

// BaseOffset returns the image byte offset that allocation block 0 maps to.
//
// Every block number this package reports — in an [ExtentDescriptor], from
// [Volume.WalkUnallocated], on a [DeletedRecord] — is an allocation block
// number, and the byte it begins at is
//
//	BaseOffset() + int64(block)*int64(Header().BlockSize)
//
// That formula holds on all three formats, which is the only reason a single
// accessor is meaningful. It is zero for a plain HFS+ or HFSX volume at the
// start of the reader. For an HFS wrapper carrying an embedded HFS+ volume it
// is where the embedded volume begins, because the embedded volume numbers its
// blocks from there. For classic HFS it is drAlBlSt*512, the start of the
// allocation-block area rather than the start of the volume, because classic
// HFS numbers allocation block 0 from there and not from sector 0 — a volume
// offset would be wrong by the MDB and the bitmap on every read.
//
// Prefer [Volume.BlockOffset] to repeating the arithmetic; it rejects the
// geometry that makes it overflow.
//
// Safe for concurrent use.
func (v *Volume) BaseOffset() int64 {
	if v == nil {
		return 0
	}
	return v.baseOffset
}

// BlockOffset returns the image byte offset of an allocation block.
//
// It reports [ErrCorrupt] when the volume declares a zero block size, and when
// the product would exceed the range of an int64 — a corrupt header can declare
// a block size of 4 GiB, and 2^32 blocks of it do not fit in the offset type an
// io.ReaderAt takes.
//
// Safe for concurrent use.
func (v *Volume) BlockOffset(block uint32) (int64, error) {
	if v == nil {
		return 0, &ParseError{Op: "block_offset", Offset: int64(block), Err: ErrCorrupt}
	}
	blockSize := uint64(v.header.BlockSize)
	if blockSize == 0 {
		return 0, &ParseError{Op: "block_offset", Offset: int64(block), Err: ErrCorrupt}
	}

	rel := uint64(block) * blockSize
	if rel > uint64(math.MaxInt64)-uint64(v.baseOffset) {
		return 0, &ParseError{Op: "block_offset", Offset: int64(block), Err: ErrCorrupt}
	}
	return v.baseOffset + int64(rel), nil
}

func parseVolumeHeader(buf []byte) (VolumeHeader, FileSystemKind, error) {
	if len(buf) < volumeHeaderSize {
		return VolumeHeader{}, "", &ParseError{Op: "parse_header", Offset: volumeHeaderOffset, Err: ErrShortRead}
	}

	sig := be16(buf[volHdrSignature : volHdrSignature+2])
	ver := be16(buf[volHdrVersion : volHdrVersion+2])

	kind, err := kindFromSignature(sig)
	if err != nil {
		return VolumeHeader{}, "", &ParseError{Op: "parse_signature", Offset: volumeHeaderOffset, Err: err}
	}
	if kind == KindHFS {
		return VolumeHeader{}, "", &ParseError{Op: "parse_signature", Offset: volumeHeaderOffset, Err: ErrUnsupportedFormat}
	}
	if err := validateVersion(kind, ver); err != nil {
		return VolumeHeader{}, "", &ParseError{Op: "parse_version", Offset: volumeHeaderOffset + 2, Err: err}
	}

	hdr := VolumeHeader{
		Signature:          sig,
		Version:            ver,
		Attributes:         be32(buf[volHdrAttributes : volHdrAttributes+4]),
		LastMountedVersion: be32(buf[volHdrLastMountedVersion : volHdrLastMountedVersion+4]),
		JournalInfoBlock:   be32(buf[volHdrJournalInfoBlock : volHdrJournalInfoBlock+4]),
		CreateTime:         hfsTimeToUnix(be32(buf[volHdrCreateDate : volHdrCreateDate+4])),
		ModifyTime:         hfsTimeToUnix(be32(buf[volHdrModifyDate : volHdrModifyDate+4])),
		BackupTime:         hfsTimeToUnix(be32(buf[volHdrBackupDate : volHdrBackupDate+4])),
		CheckedTime:        hfsTimeToUnix(be32(buf[volHdrCheckedDate : volHdrCheckedDate+4])),
		FileCount:          be32(buf[volHdrFileCount : volHdrFileCount+4]),
		FolderCount:        be32(buf[volHdrFolderCount : volHdrFolderCount+4]),
		BlockSize:          be32(buf[volHdrBlockSize : volHdrBlockSize+4]),
		TotalBlocks:        be32(buf[volHdrTotalBlocks : volHdrTotalBlocks+4]),
		FreeBlocks:         be32(buf[volHdrFreeBlocks : volHdrFreeBlocks+4]),
		NextAllocation:     be32(buf[volHdrNextAllocation : volHdrNextAllocation+4]),
		RsrcClumpSize:      be32(buf[volHdrRsrcClumpSize : volHdrRsrcClumpSize+4]),
		DataClumpSize:      be32(buf[volHdrDataClumpSize : volHdrDataClumpSize+4]),
		NextCatalogID:      be32(buf[volHdrNextCatalogID : volHdrNextCatalogID+4]),
		WriteCount:         be32(buf[volHdrWriteCount : volHdrWriteCount+4]),
		EncodingsBitmap:    be64(buf[volHdrEncodingsBitmap : volHdrEncodingsBitmap+8]),
	}

	off := volHdrFinderInfo
	for i := range extentRecordCount {
		hdr.FinderInfo[i] = be32(buf[off : off+4])
		off += 4
	}

	hdr.AllocationFile = parseForkData(buf[volHdrAllocationFile : volHdrAllocationFile+forkDataSize])
	hdr.ExtentsFile = parseForkData(buf[volHdrExtentsFile : volHdrExtentsFile+forkDataSize])
	hdr.CatalogFile = parseForkData(buf[volHdrCatalogFile : volHdrCatalogFile+forkDataSize])
	hdr.AttributesFile = parseForkData(buf[volHdrAttributesFile : volHdrAttributesFile+forkDataSize])
	hdr.StartupFile = parseForkData(buf[volHdrStartupFile : volHdrStartupFile+forkDataSize])

	if hdr.BlockSize == 0 || hdr.TotalBlocks == 0 {
		return VolumeHeader{}, "", &ParseError{Op: "validate_header", Offset: volumeHeaderOffset, Err: ErrCorrupt}
	}

	return hdr, kind, nil
}

func parseForkData(buf []byte) ForkData {
	var fd ForkData
	fd.LogicalSize = be64(buf[forkDataLogicalSize : forkDataLogicalSize+8])
	fd.ClumpSize = be32(buf[forkDataClumpSize : forkDataClumpSize+4])
	fd.TotalBlocks = be32(buf[forkDataTotalBlocks : forkDataTotalBlocks+4])
	for i := range extentRecordCount {
		base := forkDataExtents + i*extentDescriptorSize
		fd.Extents[i] = ExtentDescriptor{
			StartBlock: be32(buf[base : base+4]),
			BlockCount: be32(buf[base+4 : base+8]),
		}
	}
	return fd
}

func kindFromSignature(sig uint16) (FileSystemKind, error) {
	switch sig {
	case signatureHFS:
		return KindHFS, nil
	case signatureHFSP:
		return KindHFSP, nil
	case signatureHFSX:
		return KindHFSX, nil
	default:
		return "", ErrInvalidSignature
	}
}

func validateVersion(kind FileSystemKind, ver uint16) error {
	switch kind {
	case KindHFSP:
		if ver != versionHFSPlus {
			return ErrUnsupportedVer
		}
	case KindHFSX:
		if ver != versionHFSX {
			return ErrUnsupportedVer
		}
	default:
		return ErrUnsupportedFormat
	}
	return nil
}

// hfsTimeToUnix converts a raw HFS date for volume-level fields.
//
// Note that it clamps every value at or below the 1904 epoch delta to the Unix
// epoch, so an unset field and a genuine 1970 timestamp are indistinguishable
// in its output. That behaviour is preserved for VolumeHeader compatibility.
//
// Two further caveats apply to the volume dates it produces. Per the HFS+
// specification VolumeHeader.CreateTime is stored in *local* time, while
// ModifyTime, BackupTime and CheckedTime are GMT; on classic HFS every MDB date
// is local. The returned time.Time is labelled UTC in all cases, so CreateTime
// (and all classic-HFS volume dates) should be read as wall-clock values.
//
// Prefer hfsCatalogTime for catalog records: it distinguishes unset fields from
// real timestamps and preserves pre-1970 values. This function remains only
// because VolumeHeader's existing output must not change.
func hfsTimeToUnix(raw uint32) time.Time {
	if raw <= hfsEpochDeltaSeconds {
		return time.Unix(0, 0).UTC()
	}
	return time.Unix(int64(raw-hfsEpochDeltaSeconds), 0).UTC()
}

// hfsCatalogTime converts a raw HFS date from a catalog record.
//
// A raw value of 0 means the field was never set and yields the zero
// time.Time. Every other value is offset from the 1904 epoch; values below the
// Unix epoch produce negative Unix times rather than being clamped, because a
// pre-1970 date is real evidence and silently rewriting it to 1970 would be a
// fabrication.
func hfsCatalogTime(raw uint32) time.Time {
	if raw == 0 {
		return time.Time{}
	}
	return time.Unix(int64(raw)-int64(hfsEpochDeltaSeconds), 0).UTC()
}

func IsCorrupt(err error) bool {
	return errors.Is(err, ErrCorrupt) || errors.Is(err, ErrInvalidSignature) || errors.Is(err, ErrUnsupportedVer)
}

func (v *Volume) CatalogBTreeHeader() (BTreeHeaderRecord, error) {
	return v.readForkBTreeHeader(v.header.CatalogFile, "catalog")
}

func (v *Volume) ExtentsBTreeHeader() (BTreeHeaderRecord, error) {
	return v.readForkBTreeHeader(v.header.ExtentsFile, "extents")
}

func (v *Volume) readForkBTreeHeader(f ForkData, op string) (BTreeHeaderRecord, error) {
	if v == nil || v.reader == nil {
		return BTreeHeaderRecord{}, &ParseError{Op: op + "_btree_header", Offset: 0, Err: ErrCorrupt}
	}
	if v.header.BlockSize == 0 {
		return BTreeHeaderRecord{}, &ParseError{Op: op + "_btree_header", Offset: 0, Err: ErrCorrupt}
	}
	if f.Extents[0].BlockCount == 0 {
		return BTreeHeaderRecord{}, &ParseError{Op: op + "_btree_header", Offset: 0, Err: ErrMissingExtent}
	}

	treeStart := v.diskOffset(int64(f.Extents[0].StartBlock) * int64(v.header.BlockSize))
	buf := make([]byte, btreeNodeDescSize+btreeHeaderRecSize)
	if err := readAtExact(v.reader, treeStart, buf); err != nil {
		return BTreeHeaderRecord{}, err
	}

	nodeDesc, err := parseBTreeNodeDescriptor(buf[:btreeNodeDescSize])
	if err != nil {
		return BTreeHeaderRecord{}, err
	}
	if nodeDesc.Type != btreeNodeTypeHead {
		return BTreeHeaderRecord{}, &ParseError{Op: op + "_btree_header", Offset: treeStart, Err: ErrInvalidBTreeNode}
	}

	hdr, err := parseBTreeHeaderRecord(buf[btreeNodeDescSize:])
	if err != nil {
		return BTreeHeaderRecord{}, err
	}
	return hdr, nil
}
