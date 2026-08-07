package hfs

// Reserved catalog node IDs for the special files described by the volume
// header. Their forks are addressed like any other file's, so extent resolution
// needs their CNIDs to look up overflow records.
const (
	extentsFileCNID    = uint32(3)
	catalogFileCNID    = uint32(4)
	allocationFileCNID = uint32(6)
	startupFileCNID    = uint32(7)
	// attributesFileCNID is declared in attributes.go.
)

// forkExtents resolves the complete extent list for one of the volume's
// special files.
//
// The extents overflow file is special-cased because resolving it through the
// overflow tree would be circular. It cannot describe its own overflow — there
// would be nowhere to put those records — so the eight extents in the volume
// header are by definition its whole extent list. Every other special file may
// legitimately overflow and is resolved through the tree.
func (v *Volume) forkExtents(cnid uint32, fork ForkData) ([]ExtentDescriptor, error) {
	if cnid == extentsFileCNID {
		return compactExtents(fork.Extents[:]), nil
	}
	return v.resolveForkExtentsFromFork(cnid, fork, extentKeyTypeData)
}

// nodeReaderFor returns a function that reads B-tree node number n of a special
// file's fork into dst.
//
// Node offsets are computed within the fork's logical address space and mapped
// onto its extents, so a B-tree file split across several extents is read
// correctly. Addressing nodes from the first extent alone — as this package
// previously did for the catalog and extents trees — silently reads unrelated
// blocks once a tree grows past one extent, which is normal on any volume of
// size.
func (v *Volume) nodeReaderFor(cnid uint32, fork ForkData, nodeSize uint16) (func(uint32, []byte) error, error) {
	if nodeSize == 0 {
		return nil, &ParseError{Op: "node_reader", Offset: int64(cnid), Err: ErrInvalidBTreeNode}
	}

	exts, err := v.forkExtents(cnid, fork)
	if err != nil {
		return nil, err
	}
	if len(exts) == 0 {
		return nil, &ParseError{Op: "node_reader", Offset: int64(cnid), Err: ErrMissingExtent}
	}

	reader := v.reader
	blockSize := v.header.BlockSize
	base := v.baseOffset
	ns := int64(nodeSize)

	return func(num uint32, dst []byte) error {
		return readFromExtents(reader, exts, blockSize, base, int64(num)*ns, dst)
	}, nil
}

// catalogNodeReader and friends name the fork each tree lives in, so callers do
// not have to pair the right CNID with the right ForkData at every use.
func (v *Volume) catalogNodeReader(nodeSize uint16) (func(uint32, []byte) error, error) {
	return v.nodeReaderFor(catalogFileCNID, v.header.CatalogFile, nodeSize)
}

func (v *Volume) extentsNodeReader(nodeSize uint16) (func(uint32, []byte) error, error) {
	return v.nodeReaderFor(extentsFileCNID, v.header.ExtentsFile, nodeSize)
}

func (v *Volume) attributesNodeReader(nodeSize uint16) (func(uint32, []byte) error, error) {
	return v.nodeReaderFor(attributesFileCNID, v.header.AttributesFile, nodeSize)
}
