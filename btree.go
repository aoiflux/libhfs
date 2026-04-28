package hfs

import "fmt"

func parseBTreeNodeDescriptor(node []byte) (BTreeNodeDescriptor, error) {
	if len(node) < btreeNodeDescSize {
		return BTreeNodeDescriptor{}, &ParseError{Op: "parse_btree_node", Offset: 0, Err: ErrShortRead}
	}
	return BTreeNodeDescriptor{
		ForwardLink:  be32(node[0:4]),
		BackwardLink: be32(node[4:8]),
		Type:         int8(node[8]),
		Height:       node[9],
		NumRecords:   be16(node[10:12]),
	}, nil
}

func parseBTreeHeaderRecord(rec []byte) (BTreeHeaderRecord, error) {
	if len(rec) < btreeHeaderRecSize {
		return BTreeHeaderRecord{}, &ParseError{Op: "parse_btree_header", Offset: 0, Err: ErrShortRead}
	}

	h := BTreeHeaderRecord{
		Depth:         be16(rec[0:2]),
		RootNode:      be32(rec[2:6]),
		LeafRecords:   be32(rec[6:10]),
		FirstLeafNode: be32(rec[10:14]),
		LastLeafNode:  be32(rec[14:18]),
		NodeSize:      be16(rec[18:20]),
		MaxKeyLen:     be16(rec[20:22]),
		TotalNodes:    be32(rec[22:26]),
		FreeNodes:     be32(rec[26:30]),
		ClumpSize:     be32(rec[32:36]),
		Type:          rec[36],
		CompType:      rec[37],
		Attributes:    be32(rec[38:42]),
	}

	if h.NodeSize < 512 || h.NodeSize > 32768 {
		return BTreeHeaderRecord{}, &ParseError{Op: "parse_btree_header", Offset: 0, Err: ErrInvalidBTreeNode}
	}
	if h.TotalNodes == 0 {
		return BTreeHeaderRecord{}, &ParseError{Op: "parse_btree_header", Offset: 0, Err: ErrCorrupt}
	}

	return h, nil
}

func parseCatalogKey(raw []byte) (CatalogKey, int, error) {
	if len(raw) < 8 {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := be16(raw[0:2])
	total := int(keyLen) + 2
	if total > len(raw) || total < 8 {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	body := raw[2:total]
	nameChars := int(be16(body[4:6]))
	need := 6 + nameChars*2
	if need > len(body) {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	name := make([]uint16, nameChars)
	for i := 0; i < nameChars; i++ {
		base := 6 + i*2
		name[i] = be16(body[base : base+2])
	}

	return CatalogKey{
		KeyLength:  keyLen,
		ParentCNID: be32(body[0:4]),
		NameUTF16:  name,
	}, total, nil
}

func parseCatalogKeyHFS(raw []byte) (CatalogKey, int, error) {
	if len(raw) < 7 {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := int(raw[0])
	total := keyLen + 1
	if total > len(raw) || total < 7 {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	body := raw[1:total]
	parent := be32(body[1:5])
	nameLen := int(body[5])
	if nameLen < 0 || nameLen > 31 || 6+nameLen > len(body) {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	name := make([]uint16, 0, nameLen)
	for _, b := range body[6 : 6+nameLen] {
		name = append(name, uint16(b))
	}

	if total%2 != 0 {
		total++
	}
	if total > len(raw) {
		return CatalogKey{}, 0, &ParseError{Op: "parse_catalog_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	return CatalogKey{KeyLength: uint16(keyLen), ParentCNID: parent, NameUTF16: name}, total, nil
}

func parseExtentsKey(raw []byte) (ExtentsKey, int, error) {
	if len(raw) < 12 {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := be16(raw[0:2])
	total := int(keyLen) + 2
	if total > len(raw) || total < 12 {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	forkType := raw[2]
	if forkType != extentKeyTypeData && forkType != extentKeyTypeRsrc {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	return ExtentsKey{
		KeyLength:  keyLen,
		ForkType:   forkType,
		FileID:     be32(raw[4:8]),
		StartBlock: be32(raw[8:12]),
	}, total, nil
}

func parseExtentsKeyHFS(raw []byte) (ExtentsKey, int, error) {
	if len(raw) < 8 {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	keyLen := int(raw[0])
	total := keyLen + 1
	if total > len(raw) || total < 8 {
		return ExtentsKey{}, 0, &ParseError{Op: "parse_extents_key_hfs", Offset: 0, Err: ErrInvalidBTreeKey}
	}

	body := raw[1:total]
	forkType := body[0]
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
		FileID:     be32(body[1:5]),
		StartBlock: uint32(be16(body[5:7])),
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

	count := int(numRecords) + 1
	needed := count * 2
	if len(node) < needed {
		return nil, &ParseError{Op: "parse_node_offsets", Offset: 0, Err: ErrInvalidBTreeNode}
	}

	offs := make([]uint16, count)
	nodeSize := len(node)
	for i := 0; i < count; i++ {
		base := nodeSize - 2*(i+1)
		if base < 0 || base+2 > nodeSize {
			return nil, &ParseError{Op: "parse_node_offsets", Offset: 0, Err: ErrInvalidBTreeNode}
		}
		v := be16(node[base : base+2])
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
