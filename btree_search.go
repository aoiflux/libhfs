package libhfs

import (
	"errors"
)

// errSearchDegraded reports that keyed descent could not complete because the
// tree structure did not permit it — an unreadable node, an unparseable key, a
// cycle, or a depth blowout. It is never returned to callers: every search
// entry point falls back to an exhaustive linear walk when it sees this, so a
// damaged tree yields a slower answer rather than a wrong one.
//
// Partial corruption is the normal case on forensic images. A search that
// gives up is worse than a slow search that succeeds.
var errSearchDegraded = errors.New("libhfs: btree search degraded")

// callbackError distinguishes an error raised by a caller's callback from one
// raised by the search machinery. Callback errors must propagate; search
// errors trigger the linear fallback.
type callbackError struct{ err error }

func (e *callbackError) Error() string { return e.err.Error() }
func (e *callbackError) Unwrap() error { return e.err }

// keyOps adapts one B-tree's key format to the generic descent.
type keyOps[K any] struct {
	// parse extracts a key from a raw record, reporting how many bytes it
	// occupies so the caller can locate the payload that follows.
	parse func(raw []byte) (K, int, error)
	// compare orders two keys, returning <0, 0 or >0.
	compare func(a, b K) int
}

// btreeSearcher performs ordered descent over one B-tree.
type btreeSearcher[K any] struct {
	vol    *Volume
	tree   uint8
	header BTreeHeaderRecord
	ops    keyOps[K]
	// nodeAt reads node nodeNum into dst. It is supplied per tree because the
	// catalog and extents files are addressed from a flat base offset while the
	// attributes file is read through its resolved extent list.
	nodeAt func(nodeNum uint32, dst []byte) error
}

func (s *btreeSearcher[K]) readNode(nodeNum uint32) ([]byte, BTreeNodeDescriptor, error) {
	if nodeNum >= s.header.TotalNodes {
		return nil, BTreeNodeDescriptor{}, errSearchDegraded
	}

	key := nodeCacheKey{tree: s.tree, num: nodeNum}
	node, cached := s.vol.nodeCacheGet(key)
	if !cached {
		node = make([]byte, s.header.NodeSize)
		if err := s.nodeAt(nodeNum, node); err != nil {
			return nil, BTreeNodeDescriptor{}, errSearchDegraded
		}
		s.vol.nodeCachePut(key, node)
	}

	desc, err := parseBTreeNodeDescriptor(node)
	if err != nil {
		return nil, BTreeNodeDescriptor{}, errSearchDegraded
	}
	return node, desc, nil
}

// isEmpty reports whether the tree holds no records at all.
//
// A volume with no overflow extents has an empty extents tree, and one with no
// extended attributes has an empty attributes tree; both are allocated and
// formatted but carry no root node. That is ordinary, not corruption, so it
// must not be reported as a degraded tree — doing so both raised false
// anomalies and sent every lookup down the linear fallback.
func (s *btreeSearcher[K]) isEmpty() bool {
	return s.header.RootNode == 0 || s.header.LeafRecords == 0
}

// findLeaf descends from the root to the leaf node that would hold target,
// following the last index key that is <= target at each level.
func (s *btreeSearcher[K]) findLeaf(target K) (uint32, error) {
	node := s.header.RootNode
	visited := make(map[uint32]struct{})

	// A well-formed tree needs exactly Depth levels. The slack absorbs headers
	// that under-report depth without letting a malformed tree spin.
	maxDepth := int(s.header.Depth) + 4

	for range maxDepth + 1 {
		if _, seen := visited[node]; seen {
			return 0, errSearchDegraded
		}
		visited[node] = struct{}{}

		buf, desc, err := s.readNode(node)
		if err != nil {
			return 0, err
		}

		switch desc.Type {
		case btreeNodeTypeLeaf:
			return node, nil
		case btreeNodeTypeIdx:
			next, err := s.descendIndex(buf, desc, target)
			if err != nil {
				return 0, err
			}
			node = next
		default:
			return 0, errSearchDegraded
		}
	}
	return 0, errSearchDegraded
}

// descendIndex picks the child to follow from one index node.
func (s *btreeSearcher[K]) descendIndex(buf []byte, desc BTreeNodeDescriptor, target K) (uint32, error) {
	recs, err := orderedNodeRecords(buf, desc)
	if err != nil || len(recs) == 0 {
		return 0, errSearchDegraded
	}

	var next uint32
	have := false
	for _, rec := range recs {
		k, consumed, err := s.ops.parse(rec)
		if err != nil || consumed+4 > len(rec) {
			return 0, errSearchDegraded
		}
		child := be32(rec[consumed : consumed+4])

		if !have {
			// Leftmost child is the fallback when every key exceeds target.
			next, have = child, true
			if s.ops.compare(k, target) > 0 {
				break
			}
			continue
		}
		if s.ops.compare(k, target) > 0 {
			break
		}
		next = child
	}
	if !have {
		return 0, errSearchDegraded
	}
	return next, nil
}

// scanFrom descends to target and then walks leaf records in key order,
// following the leaf chain, starting at the first key >= target. cb reports
// whether to continue; returning false stops the scan cleanly.
func (s *btreeSearcher[K]) scanFrom(target K, cb func(K, []byte) (bool, error)) error {
	if s.isEmpty() {
		return nil
	}

	leaf, err := s.findLeaf(target)
	if err != nil {
		return err
	}

	visited := make(map[uint32]struct{})
	started := false
	firstLeaf := true

	for leaf != 0 {
		if _, seen := visited[leaf]; seen {
			return errSearchDegraded
		}
		visited[leaf] = struct{}{}

		buf, desc, err := s.readNode(leaf)
		if err != nil {
			return err
		}
		if desc.Type != btreeNodeTypeLeaf {
			return errSearchDegraded
		}
		recs, err := orderedNodeRecords(buf, desc)
		if err != nil {
			return errSearchDegraded
		}

		if firstLeaf {
			firstLeaf = false
			if err := s.checkLanding(leaf, recs, target); err != nil {
				return err
			}
		}

		for _, rec := range recs {
			k, consumed, err := s.ops.parse(rec)
			if err != nil || consumed > len(rec) {
				return errSearchDegraded
			}
			if !started {
				if s.ops.compare(k, target) < 0 {
					continue
				}
				started = true
			}
			cont, cbErr := cb(k, rec[consumed:])
			if cbErr != nil {
				return &callbackError{err: cbErr}
			}
			if !cont {
				return nil
			}
		}

		leaf = desc.ForwardLink
	}
	return nil
}

// checkLanding verifies that descent landed where a well-formed tree says it
// should, and reports errSearchDegraded when it did not.
//
// In a valid B-tree each index key equals the first key of the child it points
// at, so descent — which follows the last index key <= target — always lands on
// a leaf whose first key is <= target. The single exception is target sorting
// before every key in the tree, where descent falls back to the leftmost child
// and reaches the tree's first leaf.
//
// A landing leaf that violates both conditions proves the index keys disagree
// with the leaves they index. Without this check descent would quietly skip the
// records it overshot, under-reporting rather than failing — the worst outcome
// for a forensic read. Detecting it here converts that into a fallback to the
// linear walk plus a recorded anomaly.
func (s *btreeSearcher[K]) checkLanding(leaf uint32, recs [][]byte, target K) error {
	if len(recs) == 0 {
		return nil
	}
	k0, _, err := s.ops.parse(recs[0])
	if err != nil {
		return errSearchDegraded
	}
	if s.ops.compare(k0, target) > 0 && leaf != s.header.FirstLeafNode {
		return errSearchDegraded
	}
	return nil
}

// orderedNodeRecords returns exactly NumRecords records in key order.
//
// It differs from extractNodeRecords in two ways that matter for descent:
// records stay in their on-disk order rather than being sorted by offset, and
// the trailing free-space region is excluded rather than being handed back as
// an extra pseudo-record. A structurally invalid node is an error here instead
// of being silently trimmed, because descent must not follow a guess.
func orderedNodeRecords(node []byte, desc BTreeNodeDescriptor) ([][]byte, error) {
	offs, err := parseNodeRecordOffsets(node, desc.NumRecords)
	if err != nil {
		return nil, err
	}

	out := make([][]byte, 0, desc.NumRecords)
	for i := range int(desc.NumRecords) {
		start, end := int(offs[i]), int(offs[i+1])
		if start < btreeNodeDescSize || end > len(node) || end <= start {
			return nil, &ParseError{Op: "ordered_node_records", Offset: int64(start), Err: ErrInvalidBTreeNode}
		}
		out = append(out, node[start:end])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Key comparators
// ---------------------------------------------------------------------------

// compareCatalogKeys orders catalog keys by parent CNID, then by name.
//
// The name comparison is a plain UTF-16 code-unit comparison, NOT the
// case-folding collation the volume was written with. That is sufficient
// because every descent target this package issues carries an empty name, and
// an empty name sorts first under any comparator — so the parent CNID alone
// decides where descent lands. Exact-match-by-name would need real collation
// (TN1150 FastUnicodeCompare, binary order on HFSX case-sensitive volumes, or
// the case-insensitive Mac script comparison on classic HFS); findChild
// deliberately avoids needing it by scanning the child run instead.
//
// The classic HFS case is not hypothetical. On a volume written by hfsutils one
// directory held, in leaf order, "café.txt", "empty.txt", "renamed.txt",
// "Reports", "© 2026.txt" — "renamed.txt" ahead of "Reports", which only
// happens if 'r' and 'R' compare equal. Byte order would have put "Reports"
// first. Anything that starts matching names through this comparator would
// silently stop finding records on volumes like that one.
func compareCatalogKeys(a, b CatalogKey) int {
	if a.ParentCNID != b.ParentCNID {
		if a.ParentCNID < b.ParentCNID {
			return -1
		}
		return 1
	}
	return compareUTF16(a.NameUTF16, b.NameUTF16)
}

func compareUTF16(a, b []uint16) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// compareExtentsKeys orders extents-overflow keys by file ID, then fork type,
// then start block, per TN1150.
func compareExtentsKeys(a, b ExtentsKey) int {
	if a.FileID != b.FileID {
		if a.FileID < b.FileID {
			return -1
		}
		return 1
	}
	if a.ForkType != b.ForkType {
		if a.ForkType < b.ForkType {
			return -1
		}
		return 1
	}
	if a.StartBlock != b.StartBlock {
		if a.StartBlock < b.StartBlock {
			return -1
		}
		return 1
	}
	return 0
}

// compareAttributesKeys orders attribute keys by file ID, then name, then
// start block. As with compareCatalogKeys the name ordering only has to place
// the empty name first, which every descent target here relies on.
func compareAttributesKeys(a, b attributesKey) int {
	if a.FileID != b.FileID {
		if a.FileID < b.FileID {
			return -1
		}
		return 1
	}
	if a.Name != b.Name {
		if a.Name < b.Name {
			return -1
		}
		return 1
	}
	if a.StartBlock != b.StartBlock {
		if a.StartBlock < b.StartBlock {
			return -1
		}
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Per-tree searcher construction
// ---------------------------------------------------------------------------

// supportsKeyedSearch reports whether this volume can be searched by descent.
//
// Classic HFS qualifies for the same reason HFS+ does: every descent target
// this package issues carries an empty name, which sorts first under any
// comparator, so only the parent CNID decides where descent lands. Classic
// HFS's Mac-script collation therefore never has to be reproduced — see
// compareCatalogKeys.
func (v *Volume) supportsKeyedSearch() bool {
	return v != nil && v.kind != ""
}

func (v *Volume) catalogSearcher() (*btreeSearcher[CatalogKey], error) {
	hdr, err := v.CatalogBTreeHeader()
	if err != nil {
		return nil, err
	}
	if hdr.NodeSize == 0 {
		return nil, errSearchDegraded
	}
	nodeAt, err := v.catalogNodeReader(hdr.NodeSize)
	if err != nil {
		return nil, err
	}
	return &btreeSearcher[CatalogKey]{
		vol:    v,
		tree:   treeCatalog,
		header: hdr,
		ops: keyOps[CatalogKey]{
			// Classic HFS keys are length-prefixed bytes, HFS+ keys are
			// UTF-16 — the descent must parse whichever this volume uses.
			parse: func(raw []byte) (CatalogKey, int, error) {
				return parseCatalogKeyForKind(v.kind, raw)
			},
			compare: compareCatalogKeys,
		},
		nodeAt: nodeAt,
	}, nil
}

func (v *Volume) extentsSearcher() (*btreeSearcher[ExtentsKey], error) {
	hdr, err := v.ExtentsBTreeHeader()
	if err != nil {
		return nil, err
	}
	if hdr.NodeSize == 0 {
		return nil, errSearchDegraded
	}
	nodeAt, err := v.extentsNodeReader(hdr.NodeSize)
	if err != nil {
		return nil, err
	}
	return &btreeSearcher[ExtentsKey]{
		vol:    v,
		tree:   treeExtents,
		header: hdr,
		ops: keyOps[ExtentsKey]{
			parse: func(raw []byte) (ExtentsKey, int, error) {
				return parseExtentsKeyForKind(v.kind, raw)
			},
			compare: compareExtentsKeys,
		},
		nodeAt: nodeAt,
	}, nil
}

func (v *Volume) attributesSearcher() (*btreeSearcher[attributesKey], error) {
	hdr, err := v.AttributesBTreeHeader()
	if err != nil {
		return nil, err
	}
	if hdr.NodeSize == 0 {
		return nil, errSearchDegraded
	}
	nodeAt, err := v.attributesNodeReader(hdr.NodeSize)
	if err != nil {
		return nil, err
	}
	return &btreeSearcher[attributesKey]{
		vol:    v,
		tree:   treeAttributes,
		header: hdr,
		ops: keyOps[attributesKey]{
			parse:   parseAttributesKey,
			compare: compareAttributesKeys,
		},
		nodeAt: nodeAt,
	}, nil
}

// ---------------------------------------------------------------------------
// Search entry points, each with a linear fallback
// ---------------------------------------------------------------------------

// runFallback reports whether err means "descent failed, use the linear walk".
// Callback errors are returned to the caller unchanged; everything else is
// treated as degradation.
func unwrapSearchErr(err error) (cbErr error, degraded bool) {
	if err == nil {
		return nil, false
	}
	var ce *callbackError
	if errors.As(err, &ce) {
		return ce.err, false
	}
	return nil, true
}

// walkCatalogChildren invokes cb for every catalog leaf record whose key parent
// is parent, using keyed descent where possible.
func (v *Volume) walkCatalogChildren(parent uint32, cb catalogLeafCallback) error {
	if v.supportsKeyedSearch() {
		s, err := v.catalogSearcher()
		if err == nil {
			target := CatalogKey{ParentCNID: parent}
			searchErr := s.scanFrom(target, func(k CatalogKey, payload []byte) (bool, error) {
				if k.ParentCNID != parent {
					return false, nil // past the run; done
				}
				return true, cb(k, payload)
			})
			cbErr, degraded := unwrapSearchErr(searchErr)
			if !degraded {
				return cbErr
			}
			v.noteAnomaly("catalog_search", int64(parent),
				"keyed descent failed; fell back to a full catalog scan")
		}
	}

	return v.walkCatalogBTree(func(k CatalogKey, payload []byte) error {
		if k.ParentCNID != parent {
			return nil
		}
		return cb(k, payload)
	})
}

// walkExtentsForFork invokes cb for every extents-overflow record belonging to
// the given fork of the given file.
func (v *Volume) walkExtentsForFork(fileID uint32, forkType uint8, cb extentsLeafCallback) error {
	if v.supportsKeyedSearch() {
		s, err := v.extentsSearcher()
		if err == nil {
			target := ExtentsKey{FileID: fileID, ForkType: forkType}
			searchErr := s.scanFrom(target, func(k ExtentsKey, payload []byte) (bool, error) {
				if k.FileID != fileID {
					return false, nil
				}
				if k.ForkType != forkType {
					// Fork types for one file are contiguous; data (0x00)
					// precedes resource (0xFF), so keep scanning.
					return true, nil
				}
				return true, cb(k, payload)
			})
			cbErr, degraded := unwrapSearchErr(searchErr)
			if !degraded {
				return cbErr
			}
			v.noteAnomaly("extents_search", int64(fileID),
				"keyed descent failed; fell back to a full extents scan")
		}
	}

	return v.walkExtentsBTree(func(k ExtentsKey, payload []byte) error {
		if k.FileID != fileID || k.ForkType != forkType {
			return nil
		}
		return cb(k, payload)
	})
}

// walkAttributesForFile invokes cb for every attributes record belonging to
// the given file.
func (v *Volume) walkAttributesForFile(fileID uint32, cb func(attributesKey, []byte) error) error {
	if v.supportsKeyedSearch() {
		s, err := v.attributesSearcher()
		if err == nil {
			target := attributesKey{FileID: fileID}
			searchErr := s.scanFrom(target, func(k attributesKey, payload []byte) (bool, error) {
				if k.FileID != fileID {
					return false, nil
				}
				return true, cb(k, payload)
			})
			cbErr, degraded := unwrapSearchErr(searchErr)
			if !degraded {
				return cbErr
			}
			v.noteAnomaly("attributes_search", int64(fileID),
				"keyed descent failed; fell back to a full attributes scan")
		}
	}

	return v.walkAttributesLeafChain(func(k attributesKey, payload []byte) error {
		if k.FileID != fileID {
			return nil
		}
		return cb(k, payload)
	})
}
