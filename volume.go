package hfs

import (
	"errors"
	"io"
	"time"
)

func Open(r io.ReaderAt) (*Volume, error) {
	if r == nil {
		return nil, &ParseError{Op: "open", Offset: 0, Err: ErrCorrupt}
	}

	buf := make([]byte, volumeHeaderSize)
	if err := readAtExact(r, volumeHeaderOffset, buf); err != nil {
		return nil, err
	}

	baseOffset := int64(0)
	if be16(buf[0:2]) == signatureHFS {
		embeddedOffset, ok := parseHFSWrapperEmbeddedOffset(buf)
		if !ok {
			hdr, hfsBase, err := parseHFSMasterDirectoryBlock(buf)
			if err != nil {
				return nil, err
			}
			return &Volume{reader: r, kind: KindHFS, header: hdr, baseOffset: hfsBase}, nil
		}
		if err := readAtExact(r, embeddedOffset+volumeHeaderOffset, buf); err != nil {
			return nil, err
		}
		baseOffset = embeddedOffset
	}

	hdr, kind, err := parseVolumeHeader(buf)
	if err != nil {
		return nil, err
	}

	return &Volume{reader: r, kind: kind, header: hdr, baseOffset: baseOffset}, nil
}

func parseHFSWrapperEmbeddedOffset(mdb []byte) (int64, bool) {
	if len(mdb) < volumeHeaderSize {
		return 0, false
	}
	if be16(mdb[0:2]) != signatureHFS {
		return 0, false
	}
	if be16(mdb[hfsMDBOffEmbedSigWord:hfsMDBOffEmbedSigWord+2]) != signatureHFSP {
		return 0, false
	}

	allocBlockSize := be32(mdb[hfsMDBOffBlockSize : hfsMDBOffBlockSize+4])
	allocBlockStart512 := be16(mdb[28:30])
	embedStartBlock := be16(mdb[hfsMDBOffEmbedExtent : hfsMDBOffEmbedExtent+2])
	embedBlockCount := be16(mdb[hfsMDBOffEmbedExtent+2 : hfsMDBOffEmbedExtent+4])

	if allocBlockSize == 0 || embedBlockCount == 0 {
		return 0, false
	}

	return int64(allocBlockStart512)*512 + int64(embedStartBlock)*int64(allocBlockSize), true
}

func parseHFSMasterDirectoryBlock(mdb []byte) (VolumeHeader, int64, error) {
	if len(mdb) < volumeHeaderSize {
		return VolumeHeader{}, 0, &ParseError{Op: "parse_hfs_mdb", Offset: volumeHeaderOffset, Err: ErrShortRead}
	}
	if be16(mdb[0:2]) != signatureHFS {
		return VolumeHeader{}, 0, &ParseError{Op: "parse_hfs_mdb", Offset: volumeHeaderOffset, Err: ErrInvalidSignature}
	}

	blockSize := be32(mdb[hfsMDBOffBlockSize : hfsMDBOffBlockSize+4])
	totalBlocks := uint32(be16(mdb[hfsMDBOffTotalBlocks : hfsMDBOffTotalBlocks+2]))
	if blockSize == 0 || totalBlocks == 0 {
		return VolumeHeader{}, 0, &ParseError{Op: "parse_hfs_mdb", Offset: volumeHeaderOffset, Err: ErrCorrupt}
	}

	allocBlockStart512 := be16(mdb[28:30])
	dataBase := int64(allocBlockStart512) * 512

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

	for i := 0; i < 8; i++ {
		off := hfsMDBOffFinderInfo + i*4
		hdr.FinderInfo[i] = be32(mdb[off : off+4])
	}

	hdr.ExtentsFile = parseHFSForkData(be32(mdb[hfsMDBOffXTFlSize:hfsMDBOffXTFlSize+4]), mdb[hfsMDBOffXTExtRec:hfsMDBOffXTExtRec+12], blockSize)
	hdr.CatalogFile = parseHFSForkData(be32(mdb[hfsMDBOffCTFlSize:hfsMDBOffCTFlSize+4]), mdb[hfsMDBOffCTExtRec:hfsMDBOffCTExtRec+12], blockSize)

	return hdr, dataBase, nil
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

func parseVolumeHeader(buf []byte) (VolumeHeader, FileSystemKind, error) {
	if len(buf) < volumeHeaderSize {
		return VolumeHeader{}, "", &ParseError{Op: "parse_header", Offset: volumeHeaderOffset, Err: ErrShortRead}
	}

	sig := be16(buf[0:2])
	ver := be16(buf[2:4])

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
		Attributes:         be32(buf[4:8]),
		LastMountedVersion: be32(buf[8:12]),
		JournalInfoBlock:   be32(buf[12:16]),
		CreateTime:         hfsTimeToUnix(be32(buf[16:20])),
		ModifyTime:         hfsTimeToUnix(be32(buf[20:24])),
		BackupTime:         hfsTimeToUnix(be32(buf[24:28])),
		CheckedTime:        hfsTimeToUnix(be32(buf[28:32])),
		FileCount:          be32(buf[32:36]),
		FolderCount:        be32(buf[36:40]),
		BlockSize:          be32(buf[40:44]),
		TotalBlocks:        be32(buf[44:48]),
		FreeBlocks:         be32(buf[48:52]),
		NextAllocation:     be32(buf[52:56]),
		RsrcClumpSize:      be32(buf[56:60]),
		DataClumpSize:      be32(buf[60:64]),
		NextCatalogID:      be32(buf[64:68]),
		WriteCount:         be32(buf[68:72]),
		EncodingsBitmap:    be64(buf[72:80]),
	}

	off := 80
	for i := 0; i < 8; i++ {
		hdr.FinderInfo[i] = be32(buf[off : off+4])
		off += 4
	}

	hdr.AllocationFile = parseForkData(buf[112:192])
	hdr.ExtentsFile = parseForkData(buf[192:272])
	hdr.CatalogFile = parseForkData(buf[272:352])
	hdr.AttributesFile = parseForkData(buf[352:432])
	hdr.StartupFile = parseForkData(buf[432:512])

	if hdr.BlockSize == 0 || hdr.TotalBlocks == 0 {
		return VolumeHeader{}, "", &ParseError{Op: "validate_header", Offset: volumeHeaderOffset, Err: ErrCorrupt}
	}

	return hdr, kind, nil
}

func parseForkData(buf []byte) ForkData {
	var fd ForkData
	fd.LogicalSize = be64(buf[0:8])
	fd.ClumpSize = be32(buf[8:12])
	fd.TotalBlocks = be32(buf[12:16])
	for i := 0; i < 8; i++ {
		base := 16 + i*8
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

func hfsTimeToUnix(raw uint32) time.Time {
	if raw <= hfsEpochDeltaSeconds {
		return time.Unix(0, 0).UTC()
	}
	return time.Unix(int64(raw-hfsEpochDeltaSeconds), 0).UTC()
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
