package hfs

import (
	"sort"
)

type catalogLeafCallback func(key CatalogKey, payload []byte) error
type extentsLeafCallback func(key ExtentsKey, payload []byte) error

func (v *Volume) walkCatalogBTree(cb catalogLeafCallback) error {
	if v.kind == KindHFS {
		return v.walkCatalogLeafChain(cb)
	}

	hdr, err := v.CatalogBTreeHeader()
	if err != nil {
		return err
	}
	if hdr.NodeSize == 0 {
		return &ParseError{Op: "walk_catalog_btree", Offset: 0, Err: ErrInvalidBTreeNode}
	}

	treeStart := v.diskOffset(int64(v.header.CatalogFile.Extents[0].StartBlock) * int64(v.header.BlockSize))
	state := catalogWalkState{
		vol:      v,
		header:   hdr,
		treeBase: treeStart,
		visited:  make(map[uint32]struct{}),
	}
	return state.walkNode(hdr.RootNode, cb)
}

func (v *Volume) walkExtentsBTree(cb extentsLeafCallback) error {
	if v.kind == KindHFS {
		return v.walkExtentsLeafChain(cb)
	}

	hdr, err := v.ExtentsBTreeHeader()
	if err != nil {
		return err
	}
	if hdr.NodeSize == 0 {
		return &ParseError{Op: "walk_extents_btree", Offset: 0, Err: ErrInvalidBTreeNode}
	}

	treeStart := v.diskOffset(int64(v.header.ExtentsFile.Extents[0].StartBlock) * int64(v.header.BlockSize))
	state := extentsWalkState{
		vol:      v,
		header:   hdr,
		treeBase: treeStart,
		visited:  make(map[uint32]struct{}),
	}
	return state.walkNode(hdr.RootNode, cb)
}

type catalogWalkState struct {
	vol      *Volume
	header   BTreeHeaderRecord
	treeBase int64
	visited  map[uint32]struct{}
}

type extentsWalkState struct {
	vol      *Volume
	header   BTreeHeaderRecord
	treeBase int64
	visited  map[uint32]struct{}
}

func (s *catalogWalkState) walkNode(nodeNum uint32, cb catalogLeafCallback) error {
	if _, ok := s.visited[nodeNum]; ok {
		return nil
	}
	s.visited[nodeNum] = struct{}{}

	node, desc, err := s.readNode(nodeNum)
	if err != nil {
		return err
	}

	switch desc.Type {
	case btreeNodeTypeIdx:
		for _, rec := range extractNodeRecords(node, desc) {
			_, consumed, err := parseCatalogKeyForKind(s.vol.kind, rec)
			if err != nil {
				continue
			}
			if consumed+4 > len(rec) {
				continue
			}
			child := be32(rec[consumed : consumed+4])
			if err := s.walkNode(child, cb); err != nil {
				return err
			}
		}
		return nil
	case btreeNodeTypeLeaf:
		for _, rec := range extractNodeRecords(node, desc) {
			key, consumed, err := parseCatalogKeyForKind(s.vol.kind, rec)
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
		return nil
	default:
		return nil
	}
}

func (s *extentsWalkState) walkNode(nodeNum uint32, cb extentsLeafCallback) error {
	if _, ok := s.visited[nodeNum]; ok {
		return nil
	}
	s.visited[nodeNum] = struct{}{}

	node, desc, err := s.readNode(nodeNum)
	if err != nil {
		return err
	}

	switch desc.Type {
	case btreeNodeTypeIdx:
		for _, rec := range extractNodeRecords(node, desc) {
			_, consumed, err := parseExtentsKeyForKind(s.vol.kind, rec)
			if err != nil {
				continue
			}
			if consumed+4 > len(rec) {
				continue
			}
			child := be32(rec[consumed : consumed+4])
			if err := s.walkNode(child, cb); err != nil {
				return err
			}
		}
		return nil
	case btreeNodeTypeLeaf:
		for _, rec := range extractNodeRecords(node, desc) {
			key, consumed, err := parseExtentsKeyForKind(s.vol.kind, rec)
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
		return nil
	default:
		return nil
	}
}

// walkCatalogLeafChain iterates leaf nodes sequentially using ForwardLink
// without recursing through index nodes. This is O(leaf nodes) instead of
// O(all nodes) and is used by WalkCatalog for full sequential scans.
func (v *Volume) walkCatalogLeafChain(cb catalogLeafCallback) error {
	hdr, err := v.CatalogBTreeHeader()
	if err != nil {
		return err
	}
	if hdr.NodeSize == 0 {
		return &ParseError{Op: "walk_catalog_leaf_chain", Offset: 0, Err: ErrInvalidBTreeNode}
	}

	treeStart := v.diskOffset(int64(v.header.CatalogFile.Extents[0].StartBlock) * int64(v.header.BlockSize))
	state := catalogWalkState{
		vol:      v,
		header:   hdr,
		treeBase: treeStart,
		visited:  make(map[uint32]struct{}),
	}

	nodeNum := hdr.FirstLeafNode
	for nodeNum != 0 {
		if _, ok := state.visited[nodeNum]; ok {
			break // cycle guard
		}
		state.visited[nodeNum] = struct{}{}

		node, desc, err := state.readNode(nodeNum)
		if err != nil {
			return err
		}

		for _, rec := range extractNodeRecords(node, desc) {
			key, consumed, err := parseCatalogKeyForKind(v.kind, rec)
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

func (v *Volume) walkExtentsLeafChain(cb extentsLeafCallback) error {
	hdr, err := v.ExtentsBTreeHeader()
	if err != nil {
		return err
	}
	if hdr.NodeSize == 0 {
		return &ParseError{Op: "walk_extents_leaf_chain", Offset: 0, Err: ErrInvalidBTreeNode}
	}

	treeStart := v.diskOffset(int64(v.header.ExtentsFile.Extents[0].StartBlock) * int64(v.header.BlockSize))
	state := extentsWalkState{
		vol:      v,
		header:   hdr,
		treeBase: treeStart,
		visited:  make(map[uint32]struct{}),
	}

	nodeNum := hdr.FirstLeafNode
	for nodeNum != 0 {
		if _, ok := state.visited[nodeNum]; ok {
			break
		}
		state.visited[nodeNum] = struct{}{}

		node, desc, err := state.readNode(nodeNum)
		if err != nil {
			return err
		}

		for _, rec := range extractNodeRecords(node, desc) {
			key, consumed, err := parseExtentsKeyForKind(v.kind, rec)
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

func (s *catalogWalkState) readNode(nodeNum uint32) ([]byte, BTreeNodeDescriptor, error) {
	if nodeNum >= s.header.TotalNodes {
		return nil, BTreeNodeDescriptor{}, &ParseError{Op: "read_btree_node", Offset: int64(nodeNum), Err: ErrInvalidBTreeNode}
	}
	nodeSize := int(s.header.NodeSize)
	node := make([]byte, nodeSize)
	off := s.treeBase + int64(nodeNum)*int64(nodeSize)
	if err := readAtExact(s.vol.reader, off, node); err != nil {
		return nil, BTreeNodeDescriptor{}, err
	}
	desc, err := parseBTreeNodeDescriptor(node)
	if err != nil {
		return nil, BTreeNodeDescriptor{}, err
	}
	return node, desc, nil
}

func (s *extentsWalkState) readNode(nodeNum uint32) ([]byte, BTreeNodeDescriptor, error) {
	if nodeNum >= s.header.TotalNodes {
		return nil, BTreeNodeDescriptor{}, &ParseError{Op: "read_btree_node", Offset: int64(nodeNum), Err: ErrInvalidBTreeNode}
	}
	nodeSize := int(s.header.NodeSize)
	node := make([]byte, nodeSize)
	off := s.treeBase + int64(nodeNum)*int64(nodeSize)
	if err := readAtExact(s.vol.reader, off, node); err != nil {
		return nil, BTreeNodeDescriptor{}, err
	}
	desc, err := parseBTreeNodeDescriptor(node)
	if err != nil {
		return nil, BTreeNodeDescriptor{}, err
	}
	return node, desc, nil
}

func extractNodeRecords(node []byte, desc BTreeNodeDescriptor) [][]byte {
	offs, err := parseNodeRecordOffsets(node, desc.NumRecords)
	if err != nil {
		return nil
	}
	starts := make([]int, 0, len(offs))
	seen := make(map[int]struct{}, len(offs))
	for _, o := range offs {
		start := int(o)
		if start < btreeNodeDescSize || start >= len(node)-2 {
			continue
		}
		if _, ok := seen[start]; ok {
			continue
		}
		seen[start] = struct{}{}
		starts = append(starts, start)
	}
	sort.Ints(starts)
	if len(starts) == 0 {
		return nil
	}

	out := make([][]byte, 0, len(starts))
	for i, s := range starts {
		e := len(node)
		if i+1 < len(starts) {
			e = starts[i+1]
		}
		if e <= s || s < 0 || e > len(node) {
			continue
		}
		out = append(out, node[s:e])
	}
	return out
}
