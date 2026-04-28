package hfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"unicode/utf16"
)

const (
	attributesFileCNID       = uint32(8)
	attrRecordTypeInlineData = uint32(0x10)
	decmpfsTypeZlibAttr      = uint32(3)
	decmpfsTypeRawAttr       = uint32(9)
	decmpfsHeaderSize        = 16
	decmpfsAttrName          = "com.apple.decmpfs"
)

type attributesKey struct {
	FileID     uint32
	StartBlock uint32
	Name       string
}

func (v *Volume) AttributesBTreeHeader() (BTreeHeaderRecord, error) {
	return v.readForkBTreeHeader(v.header.AttributesFile, "attributes")
}

func parseAttributesKey(raw []byte) (attributesKey, int, error) {
	if len(raw) < 14 {
		return attributesKey{}, 0, &ParseError{Op: "parse_attributes_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := be16(raw[0:2])
	total := int(keyLen) + 2
	if total > len(raw) || total < 14 {
		return attributesKey{}, 0, &ParseError{Op: "parse_attributes_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	body := raw[2:total]
	nameChars := int(be16(body[10:12]))
	need := 12 + nameChars*2
	if need > len(body) {
		return attributesKey{}, 0, &ParseError{Op: "parse_attributes_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	nameUTF16 := make([]uint16, nameChars)
	for i := 0; i < nameChars; i++ {
		base := 12 + i*2
		nameUTF16[i] = be16(body[base : base+2])
	}

	return attributesKey{
		FileID:     be32(body[2:6]),
		StartBlock: be32(body[6:10]),
		Name:       string(utf16.Decode(nameUTF16)),
	}, total, nil
}

func readFromExtents(r io.ReaderAt, exts []ExtentDescriptor, blockSize uint32, baseOffset int64, off int64, dst []byte) error {
	if off < 0 {
		return &ParseError{Op: "read_from_extents", Offset: off, Err: ErrInvalidOffset}
	}
	if len(dst) == 0 {
		return nil
	}

	remain := len(dst)
	wrote := 0
	logicalBase := int64(0)
	curOff := off
	bSize := int64(blockSize)

	for _, ext := range exts {
		extBytes := int64(ext.BlockCount) * bSize
		if curOff >= logicalBase+extBytes {
			logicalBase += extBytes
			continue
		}

		inExt := curOff - logicalBase
		if inExt < 0 {
			inExt = 0
		}
		can := extBytes - inExt
		if can > int64(remain-wrote) {
			can = int64(remain - wrote)
		}
		physOff := baseOffset + int64(ext.StartBlock)*bSize + inExt
		chunk := dst[wrote : wrote+int(can)]
		if err := readAtExact(r, physOff, chunk); err != nil {
			return err
		}
		wrote += len(chunk)
		curOff += int64(len(chunk))
		logicalBase += extBytes
		if wrote == remain {
			return nil
		}
	}

	return &ParseError{Op: "read_from_extents", Offset: off, Err: ErrShortRead}
}

func (v *Volume) walkAttributesLeafChain(cb func(key attributesKey, payload []byte) error) error {
	hdr, err := v.AttributesBTreeHeader()
	if err != nil {
		return err
	}
	if hdr.NodeSize == 0 {
		return &ParseError{Op: "walk_attributes_leaf_chain", Offset: 0, Err: ErrInvalidBTreeNode}
	}

	exts, err := v.resolveForkExtentsFromFork(attributesFileCNID, v.header.AttributesFile, extentKeyTypeData)
	if err != nil {
		return err
	}

	visited := make(map[uint32]struct{})
	nodeNum := hdr.FirstLeafNode
	for nodeNum != 0 {
		if _, ok := visited[nodeNum]; ok {
			break
		}
		visited[nodeNum] = struct{}{}

		node := make([]byte, hdr.NodeSize)
		if err := readFromExtents(v.reader, exts, v.header.BlockSize, v.baseOffset, int64(nodeNum)*int64(hdr.NodeSize), node); err != nil {
			return err
		}
		desc, err := parseBTreeNodeDescriptor(node)
		if err != nil {
			return err
		}

		for _, rec := range extractNodeRecords(node, desc) {
			key, consumed, err := parseAttributesKey(rec)
			if err != nil {
				continue
			}
			if consumed > len(rec) {
				continue
			}
			if err := cb(key, rec[consumed:]); err != nil {
				return err
			}
		}

		nodeNum = desc.ForwardLink
	}
	return nil
}

func (v *Volume) readDecmpfsInline(cnid uint32) ([]byte, uint64, bool, error) {
	if v.header.AttributesFile.Extents[0].BlockCount == 0 {
		return nil, 0, false, nil
	}
	var data []byte
	var uncSize uint64
	found := false
	err := v.walkAttributesLeafChain(func(key attributesKey, payload []byte) error {
		if key.FileID != cnid || key.StartBlock != 0 || key.Name != decmpfsAttrName {
			return nil
		}
		if len(payload) < 16 {
			return nil
		}
		if be32(payload[0:4]) != attrRecordTypeInlineData {
			return nil
		}
		attrSize := int(be32(payload[12:16]))
		if attrSize < decmpfsHeaderSize || 16+attrSize > len(payload) {
			return nil
		}
		decoded, size, ok := decodeDecmpfsInline(payload[16 : 16+attrSize])
		if !ok {
			return nil
		}
		data = decoded
		uncSize = size
		found = true
		return errStopWalk
	})
	if errors.Is(err, ErrMissingExtent) {
		return nil, 0, false, nil
	}
	if err != nil && !errors.Is(err, errStopWalk) {
		return nil, 0, false, err
	}
	return data, uncSize, found, nil
}

func decodeDecmpfsInline(attr []byte) ([]byte, uint64, bool) {
	if len(attr) < decmpfsHeaderSize {
		return nil, 0, false
	}
	cmpType := binary.LittleEndian.Uint32(attr[4:8])
	uncSize := binary.LittleEndian.Uint64(attr[8:16])
	raw := attr[16:]

	switch cmpType {
	case decmpfsTypeRawAttr:
		if uint64(len(raw)) < uncSize {
			return nil, 0, false
		}
		return append([]byte(nil), raw[:uncSize]...), uncSize, true
	case decmpfsTypeZlibAttr:
		if len(raw) == 0 {
			return nil, 0, false
		}
		if raw[0] == 0x0f {
			if uint64(len(raw)-1) < uncSize {
				return nil, 0, false
			}
			return append([]byte(nil), raw[1:1+uncSize]...), uncSize, true
		}
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, 0, false
		}
		defer zr.Close()
		out, err := io.ReadAll(zr)
		if err != nil || uint64(len(out)) != uncSize {
			return nil, 0, false
		}
		return out, uncSize, true
	default:
		return nil, 0, false
	}
}

func (v *Volume) hydrateCompressedRecord(rec CatalogRecord) CatalogRecord {
	if rec.Type != CatalogRecordFile {
		return rec
	}
	if rec.DataFork.LogicalSize != 0 || rec.RsrcFork.LogicalSize != 0 {
		return rec
	}
	_, uncSize, ok, err := v.readDecmpfsInline(rec.CNID)
	if err != nil || !ok {
		return rec
	}
	rec.Compressed = true
	rec.DataFork.LogicalSize = uncSize
	return rec
}
