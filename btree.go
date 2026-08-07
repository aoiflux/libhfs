package hfs

import "fmt"

func parseBTreeNodeDescriptor(node []byte) (BTreeNodeDescriptor, error) {
	if len(node) < btreeNodeDescSize {
		return BTreeNodeDescriptor{}, &ParseError{Op: "parse_btree_node", Offset: 0, Err: ErrShortRead}
	}
	return BTreeNodeDescriptor{
		ForwardLink:  be32(node[nodeForwardLink : nodeForwardLink+4]),
		BackwardLink: be32(node[nodeBackwardLink : nodeBackwardLink+4]),
		Type:         int8(node[nodeType]),
		Height:       node[nodeHeight],
		NumRecords:   be16(node[nodeNumRecords : nodeNumRecords+2]),
	}, nil
}

func parseBTreeHeaderRecord(rec []byte) (BTreeHeaderRecord, error) {
	if len(rec) < btreeHeaderRecSize {
		return BTreeHeaderRecord{}, &ParseError{Op: "parse_btree_header", Offset: 0, Err: ErrShortRead}
	}

	h := BTreeHeaderRecord{
		Depth:         be16(rec[btHdrDepth : btHdrDepth+2]),
		RootNode:      be32(rec[btHdrRootNode : btHdrRootNode+4]),
		LeafRecords:   be32(rec[btHdrLeafRecords : btHdrLeafRecords+4]),
		FirstLeafNode: be32(rec[btHdrFirstLeafNode : btHdrFirstLeafNode+4]),
		LastLeafNode:  be32(rec[btHdrLastLeafNode : btHdrLastLeafNode+4]),
		NodeSize:      be16(rec[btHdrNodeSize : btHdrNodeSize+2]),
		MaxKeyLen:     be16(rec[btHdrMaxKeyLength : btHdrMaxKeyLength+2]),
		TotalNodes:    be32(rec[btHdrTotalNodes : btHdrTotalNodes+4]),
		FreeNodes:     be32(rec[btHdrFreeNodes : btHdrFreeNodes+4]),
		ClumpSize:     be32(rec[btHdrClumpSize : btHdrClumpSize+4]),
		Type:          rec[btHdrBTreeType],
		CompType:      rec[btHdrKeyCompareTyp],
		Attributes:    be32(rec[btHdrAttributes : btHdrAttributes+4]),
	}

	if h.NodeSize < minBTreeNodeSize || h.NodeSize > maxBTreeNodeSize {
		return BTreeHeaderRecord{}, &ParseError{Op: "parse_btree_header", Offset: 0, Err: ErrInvalidBTreeNode}
	}
	if h.TotalNodes == 0 {
		return BTreeHeaderRecord{}, &ParseError{Op: "parse_btree_header", Offset: 0, Err: ErrCorrupt}
	}

	return h, nil
}

func parseCatalogKey(raw []byte) (CatalogKey, int, error) {
	if len(raw) < catKeyMinSize {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := be16(raw[0:2])
	total := int(keyLen) + 2
	if total > len(raw) || total < catKeyMinSize {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	body := raw[2:total]
	nameChars := int(be16(body[catKeyNameLength : catKeyNameLength+2]))
	need := catKeyName + nameChars*utf16CodeUnitSize
	if need > len(body) {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	name := make([]uint16, nameChars)
	for i := 0; i < nameChars; i++ {
		base := catKeyName + i*utf16CodeUnitSize
		name[i] = be16(body[base : base+2])
	}

	return CatalogKey{
		KeyLength:  keyLen,
		ParentCNID: be32(body[catKeyParentID : catKeyParentID+4]),
		NameUTF16:  name,
	}, total, nil
}

func parseCatalogKeyHFS(raw []byte) (CatalogKey, int, error) {
	if len(raw) < hfsCatKeyMinSize {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := int(raw[0])
	total := keyLen + 1
	if total > len(raw) || total < hfsCatKeyMinSize {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	body := raw[1:total]
	parent := be32(body[hfsCatKeyParentID : hfsCatKeyParentID+4])
	nameLen := int(body[hfsCatKeyNameLength])
	if nameLen < 0 || nameLen > hfsCatKeyMaxName || hfsCatKeyName+nameLen > len(body) {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	nameBytes := make([]byte, nameLen)
	copy(nameBytes, body[hfsCatKeyName:hfsCatKeyName+nameLen])

	// NameUTF16 keeps the raw byte values so key ordering stays a pure function
	// of the bytes on disk. The displayed name is decoded separately, per
	// volume, by decodeHFSName.
	name := make([]uint16, 0, nameLen)
	for _, b := range nameBytes {
		name = append(name, uint16(b))
	}

	if total%2 != 0 {
		total++
	}
	if total > len(raw) {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	return CatalogKey{
		KeyLength:  uint16(keyLen),
		ParentCNID: parent,
		NameUTF16:  name,
		NameBytes:  nameBytes,
	}, total, nil
}

func parseExtentsKey(raw []byte) (ExtentsKey, int, error) {
	if len(raw) < extKeyMinSize {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := be16(raw[0:2])
	total := int(keyLen) + 2
	if total > len(raw) || total < extKeyMinSize {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	forkType := raw[extKeyForkType]
	if forkType != extentKeyTypeData && forkType != extentKeyTypeRsrc {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	return ExtentsKey{
		KeyLength:  keyLen,
		ForkType:   forkType,
		FileID:     be32(raw[extKeyFileID : extKeyFileID+4]),
		StartBlock: be32(raw[extKeyStartBlock : extKeyStartBlock+4]),
	}, total, nil
}

func parseExtentsKeyHFS(raw []byte) (ExtentsKey, int, error) {
	if len(raw) < hfsExtKeyMinSize {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := int(raw[0])
	total := keyLen + 1
	if total > len(raw) || total < hfsExtKeyMinSize {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	body := raw[1:total]
	forkType := body[hfsExtKeyForkType]
	if forkType != extentKeyTypeData && forkType != extentKeyTypeRsrc {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	if total%2 != 0 {
		total++
	}
	if total > len(raw) {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	return ExtentsKey{
		KeyLength:  uint16(keyLen),
		ForkType:   forkType,
		FileID:     be32(body[hfsExtKeyFileID : hfsExtKeyFileID+4]),
		StartBlock: uint32(be16(body[hfsExtKeyStartBlock : hfsExtKeyStartBlock+2])),
	}, total, nil
}

func parseCatalogKeyForKind(kind FileSystemKind, raw []byte) (CatalogKey, int, error) {
	if kind == KindHFS {
		return parseCatalogKeyHFS(raw)
	}
	return parseCatalogKey(raw)
}

func parseExtentsKeyForKind(kind FileSystemKind, raw []byte) (ExtentsKey, int, error) {
	if kind == KindHFS {
		return parseExtentsKeyHFS(raw)
	}
	return parseExtentsKey(raw)
}

func parseNodeRecordOffsets(node []byte, numRecords uint16) ([]uint16, error) {
	if len(node) < btreeNodeDescSize {
		return nil, &ParseError{Op: "parse_node_offsets", Offset: 0, Err: ErrShortRead}
	}

	count := int(numRecords) + 1 // the extra entry marks the start of free space
	needed := count * nodeRecordOffsetSize
	if len(node) < needed {
		return nil, &ParseError{Op: "parse_node_offsets", Offset: 0, Err: ErrInvalidBTreeNode}
	}

	offs := make([]uint16, count)
	nodeSize := len(node)
	for i := 0; i < count; i++ {
		base := nodeSize - nodeRecordOffsetSize*(i+1)
		if base < 0 || base+nodeRecordOffsetSize > nodeSize {
			return nil, &ParseError{Op: "parse_node_offsets", Offset: 0, Err: ErrInvalidBTreeNode}
		}
		v := be16(node[base : base+nodeRecordOffsetSize])
		if int(v) > nodeSize {
			return nil, &ParseError{Op: "parse_node_offsets", Offset: 0, Err: ErrInvalidBTreeNode}
		}
		offs[i] = v
	}
	return offs, nil
}

func (k CatalogKey) NameString() string {
	r := make([]rune, 0, len(k.NameUTF16))
	for _, u := range k.NameUTF16 {
		r = append(r, rune(u))
	}
	return string(r)
}

func (k ExtentsKey) String() string {
	fork := "data"
	if k.ForkType == extentKeyTypeRsrc {
		fork = "resource"
	}
	return fmt.Sprintf("cnid=%d fork=%s start=%d", k.FileID, fork, k.StartBlock)
}
