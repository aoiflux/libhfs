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

	state, err := v.newCatalogWalkState()
	if err != nil {
		return err
	}
	return state.walkNode(state.header.RootNode, cb)
}

func (v *Volume) walkExtentsBTree(cb extentsLeafCallback) error {
	if v.kind == KindHFS {
		return v.walkExtentsLeafChain(cb)
	}

	state, err := v.newExtentsWalkState()
	if err != nil {
		return err
	}
	return state.walkNode(state.header.RootNode, cb)
}

// The walk states address nodes through nodeAt, which maps a node number onto
// the fork's extent list. Computing offsets from the first extent alone breaks
// once a tree spans more than one extent: reads run off the end of that extent
// into unrelated blocks and return plausible-looking wrong records rather than
// an error.
type catalogWalkState struct {
	vol     *Volume
	header  BTreeHeaderRecord
	nodeAt  func(uint32, []byte) error
	visited map[uint32]struct{}
}

type extentsWalkState struct {
	vol     *Volume
	header  BTreeHeaderRecord
	nodeAt  func(uint32, []byte) error
	visited map[uint32]struct{}
}

func (v *Volume) newCatalogWalkState() (*catalogWalkState, error) {
	hdr, err := v.CatalogBTreeHeader()
	if err != nil {
		return nil, err
	}
	if hdr.NodeSize == 0 {
		return nil, &ParseError{Op: "walk_catalog_btree", Offset: 0, Err: ErrInvalidBTreeNode}
	}
	nodeAt, err := v.catalogNodeReader(hdr.NodeSize)
	if err != nil {
		return nil, err
	}
	return &catalogWalkState{
		vol:     v,
		header:  hdr,
		nodeAt:  nodeAt,
		visited: make(map[uint32]struct{}),
	}, nil
}

func (v *Volume) newExtentsWalkState() (*extentsWalkState, error) {
	hdr, err := v.ExtentsBTreeHeader()
	if err != nil {
		return nil, err
	}
	if hdr.NodeSize == 0 {
		return nil, &ParseError{Op: "walk_extents_btree", Offset: 0, Err: ErrInvalidBTreeNode}
	}
	nodeAt, err := v.extentsNodeReader(hdr.NodeSize)
	if err != nil {
		return nil, err
	}
	return &extentsWalkState{
		vol:     v,
		header:  hdr,
		nodeAt:  nodeAt,
		visited: make(map[uint32]struct{}),
	}, nil
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
	state, err := v.newCatalogWalkState()
	if err != nil {
		return err
	}

	nodeNum := state.header.FirstLeafNode
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
	state, err := v.newExtentsWalkState()
	if err != nil {
		return err
	}

	nodeNum := state.header.FirstLeafNode
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
	node := make([]byte, s.header.NodeSize)
	if err := s.nodeAt(nodeNum, node); err != nil {
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
	node := make([]byte, s.header.NodeSize)
	if err := s.nodeAt(nodeNum, node); err != nil {
		return nil, BTreeNodeDescriptor{}, err
	}
	desc, err := parseBTreeNodeDescriptor(node)
	if err != nil {
		return nil, BTreeNodeDescriptor{}, err
	}
	return node, desc, nil
}

// extractNodeRecords returns the live records of a B-tree node.
//
// The offset array holds NumRecords+1 entries. The final entry marks the start
// of the node's free space, which is NOT a record — it is the region records
// are deleted into. HFS+ does not zero it, so it routinely still holds an
// intact copy of a deleted record's bytes. Returning it as a record made
// deleted files reappear in live directory listings and inflated the volume's
// file and folder counts; on the corpus image, 36 such regions decoded as
// valid catalog records.
//
// Those bytes are recoverable evidence and are the intended source for the
// node-slack recovery pass in deleted.go, but they must be surfaced
// deliberately through the deleted-record API rather than leaking into live
// results.
//
// The out-of-order and duplicate-offset tolerance below is kept: damaged nodes
// are normal on forensic images, and salvaging what parses beats discarding the
// node.
func extractNodeRecords(node []byte, desc BTreeNodeDescriptor) [][]byte {
	offs, err := parseNodeRecordOffsets(node, desc.NumRecords)
	if err != nil {
		return nil
	}

	// Records end where free space begins.
	limit := len(node)
	if n := int(desc.NumRecords); n < len(offs) {
		if fo := int(offs[n]); fo >= btreeNodeDescSize && fo <= len(node) {
			limit = fo
		}
	}

	starts := make([]int, 0, desc.NumRecords)
	seen := make(map[int]struct{}, desc.NumRecords)
	for i := range int(desc.NumRecords) {
		start := int(offs[i])
		if start < btreeNodeDescSize || start >= limit {
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
		e := limit
		if i+1 < len(starts) {
			e = starts[i+1]
		}
		if e <= s || e > len(node) {
			continue
		}
		out = append(out, node[s:e])
	}
	return out
}
