package hfs

import (
	"context"
	"sort"
)

// RecoverySource says where a recovered record was found. The sources differ in
// reliability, so this is part of the finding, not an implementation detail.
type RecoverySource uint8

const (
	// RecoveredFromNodeSlack means the record was found in the free space of a
	// live B-tree node — the region a record is deleted into, which HFS+ does
	// not zero.
	RecoveredFromNodeSlack RecoverySource = iota

	// RecoveredFromFreeNode means the record was found in a B-tree node marked
	// free in the tree's node map. Such nodes routinely retain an intact former
	// leaf.
	RecoveredFromFreeNode

	// RecoveredFromUnallocated means the record was carved from a block not
	// currently allocated to any file.
	RecoveredFromUnallocated
)

func (s RecoverySource) String() string {
	switch s {
	case RecoveredFromNodeSlack:
		return "node slack"
	case RecoveredFromFreeNode:
		return "free node"
	case RecoveredFromUnallocated:
		return "unallocated"
	}
	return "unknown"
}

// Confidence grades how much of a recovered record was independently
// corroborated.
//
// It is advisory metadata for triage and never a guarantee. A high-confidence
// record is one whose internal fields agree with the live filesystem; it is not
// a promise that the content is intact.
type Confidence uint8

const (
	// ConfidenceLow means the bytes parsed as a record and nothing more.
	ConfidenceLow Confidence = iota

	// ConfidenceMedium means the record's CNID is below the volume's next
	// unassigned CNID, or its parent resolves to a live folder — so it is
	// plausibly a record this volume really wrote.
	ConfidenceMedium

	// ConfidenceHigh means the parent resolves to a live folder, the fork
	// extents fall inside the volume, and those blocks are currently
	// unallocated — so the content it points at has not been handed to another
	// file.
	ConfidenceHigh
)

func (c Confidence) String() string {
	switch c {
	case ConfidenceMedium:
		return "medium"
	case ConfidenceHigh:
		return "high"
	}
	return "low"
}

// DeletedRecord is a catalog record recovered from space the filesystem no
// longer considers live.
type DeletedRecord struct {
	// Record is the decoded catalog record, as far as it could be decoded.
	// Name may be empty when only part of the record survived.
	Record CatalogRecord

	Source     RecoverySource
	Confidence Confidence

	// NodeNumber is the B-tree node the record was found in, for node-based
	// sources. It is 0 for unallocated carving.
	NodeNumber uint32

	// ByteOffset is the record's absolute offset in the image — the provenance
	// an examiner needs to go back to the bytes.
	ByteOffset int64

	// Overwritten reports that at least one block this record's data fork
	// points at is currently allocated to a live file.
	//
	// This is the single most important field here. A recovered record's extent
	// list is a stale pointer: when the blocks have been reused, reading them
	// returns another file's data, not the deleted one's.
	Overwritten bool

	// StaleCopy reports that an identical record — same CNID, parent and name —
	// is still live in the catalog.
	//
	// Such a record was not deleted. B-tree inserts shift records within a node,
	// leaving the previous bytes in the slack behind them, so a live file's
	// record routinely appears there too. These are excluded by default because
	// presenting one as a deleted file is a false positive of the worst kind:
	// it would tell an examiner a file was removed when it never was. Set
	// RecoveryOptions.IncludeStaleCopies to see them anyway — as evidence that
	// a record was rewritten, they have their own uses.
	StaleCopy bool
}

// RecoveryOptions selects which sources to scan. A nil *RecoveryOptions means
// the defaults: node slack and free nodes, but not unallocated carving.
type RecoveryOptions struct {
	// ScanNodeSlack scans the free space of live B-tree nodes. Cheap: those
	// nodes are read anyway.
	ScanNodeSlack bool

	// ScanFreeNodes scans B-tree nodes marked free. Moderate cost, high yield.
	ScanFreeNodes bool

	// ScanUnallocated carves catalog nodes out of unallocated blocks. This
	// finds records from a previous catalog file after the tree has grown and
	// moved, but reads a large part of the image, so it is off by default.
	ScanUnallocated bool

	// MinConfidence drops findings graded below this level. The zero value
	// keeps everything, which is the right default for forensic work: silently
	// discarding evidence is worse than reporting it with a grade attached.
	MinConfidence Confidence

	// IncludeStaleCopies reports records that are still live in the catalog,
	// flagged with DeletedRecord.StaleCopy. They are excluded by default — see
	// that field for why.
	IncludeStaleCopies bool
}

// DefaultRecoveryOptions returns the default scan selection.
func DefaultRecoveryOptions() *RecoveryOptions {
	return &RecoveryOptions{ScanNodeSlack: true, ScanFreeNodes: true}
}

func (o *RecoveryOptions) orDefaults() *RecoveryOptions {
	if o == nil {
		return DefaultRecoveryOptions()
	}
	return o
}

// WalkDeleted enumerates catalog records recoverable from space the filesystem
// no longer treats as live. Pass nil for the default sources.
//
// # What this can and cannot tell you
//
// Deleting a file on HFS+ removes its catalog record and frees its blocks. It
// does not erase anything. Recovery therefore depends entirely on whether that
// space has since been reused, which correlates with elapsed time and volume
// pressure — nothing this library can observe.
//
// Consequences worth stating plainly:
//
//   - A recovered record's extents are stale pointers. Check Overwritten
//     before trusting any content read through them.
//   - Partial records are normal. An empty Record.Name is not an error.
//   - False positives are expected from slack scanning, where arbitrary bytes
//     occasionally parse as a record. Confidence grades them; MinConfidence
//     filters them.
//   - Absence of a record is not evidence the file never existed.
//
// Records are deduplicated across sources by CNID, name and parent; when the
// same record is found more than once, the highest-confidence instance wins.
func (v *Volume) WalkDeleted(opts *RecoveryOptions, cb func(DeletedRecord) error) error {
	return v.WalkDeletedContext(context.Background(), opts, cb)
}

// WalkDeletedContext is [Volume.WalkDeleted] with cancellation.
//
// Unallocated carving reads the whole free area of a volume, which on a large
// image takes minutes; a caller that cannot wait needs a way out. Cancellation
// is cooperative and checked between blocks, so it takes effect promptly
// without abandoning a read in progress. The context's error is returned.
func (v *Volume) WalkDeletedContext(ctx context.Context, opts *RecoveryOptions, cb func(DeletedRecord) error) error {
	if cb == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o := opts.orDefaults()

	hdr, err := v.CatalogBTreeHeader()
	if err != nil {
		return err
	}
	nodeAt, err := v.catalogNodeReader(hdr.NodeSize)
	if err != nil {
		return err
	}

	// Recovered bytes are only evidence of deletion if the record is not also
	// live. Building the live set up front is one extra catalog pass and is
	// what separates a deleted file from a stale copy of a current one.
	live, err := v.liveRecordKeys()
	if err != nil {
		return err
	}

	seen := make(map[deletedKey]Confidence)
	emit := func(rec DeletedRecord) error {
		k := deletedKey{
			cnid:   rec.Record.CNID,
			parent: rec.Record.ParentCNID,
			name:   rec.Record.Name,
		}
		if _, isLive := live[k]; isLive {
			if !o.IncludeStaleCopies {
				return nil
			}
			rec.StaleCopy = true
		}
		if rec.Confidence < o.MinConfidence {
			return nil
		}
		if prev, dup := seen[k]; dup && prev >= rec.Confidence {
			return nil
		}
		seen[k] = rec.Confidence
		return cb(rec)
	}

	if o.ScanNodeSlack {
		if err := v.scanNodeSlack(hdr, nodeAt, emit); err != nil {
			return err
		}
	}
	if o.ScanFreeNodes {
		if err := v.scanFreeNodes(hdr, nodeAt, emit); err != nil {
			return err
		}
	}
	if o.ScanUnallocated {
		if err := v.scanUnallocatedNodes(ctx, hdr, emit); err != nil {
			return err
		}
	}
	return nil
}

type deletedKey struct {
	cnid   uint32
	parent uint32
	name   string
}

// liveRecordKeys returns the identity of every record the catalog currently
// lists, so recovery can tell a deleted record from a stale copy of a live one.
func (v *Volume) liveRecordKeys() (map[deletedKey]struct{}, error) {
	live := make(map[deletedKey]struct{}, 256)
	err := v.WalkCatalog(func(r CatalogRecord) error {
		if r.Type != CatalogRecordFile && r.Type != CatalogRecordFolder {
			return nil
		}
		live[deletedKey{cnid: r.CNID, parent: r.ParentCNID, name: r.Name}] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return live, nil
}

// RecoverDeleted collects WalkDeleted into a slice, ordered by confidence
// (highest first) then by CNID.
func (v *Volume) RecoverDeleted(opts *RecoveryOptions) ([]DeletedRecord, error) {
	return v.RecoverDeletedContext(context.Background(), opts)
}

// RecoverDeletedContext is [Volume.RecoverDeleted] with cancellation.
func (v *Volume) RecoverDeletedContext(ctx context.Context, opts *RecoveryOptions) ([]DeletedRecord, error) {
	var out []DeletedRecord
	err := v.WalkDeletedContext(ctx, opts, func(r DeletedRecord) error {
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Record.CNID < out[j].Record.CNID
	})
	return out, nil
}

// OpenDeleted returns a reader over a recovered record's data fork.
//
// It reads the blocks the record's extents point at as they exist now. When
// [DeletedRecord.Overwritten] is true those blocks belong to a live file and
// the bytes are that file's, not the deleted one's — the read still succeeds,
// because refusing it would deny the examiner data they may need, but the
// result must not be presented as recovered content.
func (v *Volume) OpenDeleted(rec DeletedRecord) (*File, error) {
	exts := compactExtents(rec.Record.DataFork.Extents[:])
	if len(exts) == 0 {
		return nil, &ParseError{Op: "open_deleted", Offset: int64(rec.Record.CNID), Err: ErrMissingExtent}
	}

	size := int64(rec.Record.DataFork.LogicalSize)
	if capacity := v.extentsByteCapacity(exts); size <= 0 || size > capacity {
		// A partially recovered record may carry a corrupt size. Fall back to
		// what the extents can actually hold rather than refusing outright.
		size = capacity
	}
	return &File{vol: v, rec: rec.Record, extents: exts, size: size}, nil
}

func (v *Volume) extentsByteCapacity(exts []ExtentDescriptor) int64 {
	var blocks int64
	for _, e := range exts {
		blocks += int64(e.BlockCount)
	}
	return blocks * int64(v.header.BlockSize)
}

// scanNodeSlack examines the free space of every live leaf node.
//
// Records are laid out from the start of a node and the offset array grows down
// from the end; the gap between them is where a deleted record's bytes remain.
func (v *Volume) scanNodeSlack(hdr BTreeHeaderRecord, nodeAt func(uint32, []byte) error, emit func(DeletedRecord) error) error {
	node := make([]byte, hdr.NodeSize)
	visited := make(map[uint32]struct{})

	for num := hdr.FirstLeafNode; num != 0; {
		if _, dup := visited[num]; dup {
			break
		}
		visited[num] = struct{}{}

		if err := nodeAt(num, node); err != nil {
			return nil // a node we cannot read simply yields nothing
		}
		desc, err := parseBTreeNodeDescriptor(node)
		if err != nil {
			break
		}

		slack, base := nodeSlackRegion(node, desc)
		if err := v.carveRecords(slack, num, base, RecoveredFromNodeSlack, emit); err != nil {
			return err
		}
		num = desc.ForwardLink
	}
	return nil
}

// nodeSlackRegion returns the unused span between the last live record and the
// offset array, plus its offset within the node.
func nodeSlackRegion(node []byte, desc BTreeNodeDescriptor) ([]byte, int) {
	offs, err := parseNodeRecordOffsets(node, desc.NumRecords)
	if err != nil {
		return nil, 0
	}
	n := int(desc.NumRecords)
	if n >= len(offs) {
		return nil, 0
	}
	start := int(offs[n]) // free space begins where the last record ends
	end := len(node) - 2*(n+1)
	if start < btreeNodeDescSize || end > len(node) || start >= end {
		return nil, 0
	}
	return node[start:end], start
}

// scanFreeNodes examines nodes the tree's map record marks as unused.
func (v *Volume) scanFreeNodes(hdr BTreeHeaderRecord, nodeAt func(uint32, []byte) error, emit func(DeletedRecord) error) error {
	freeMap, err := v.readNodeBitmap(hdr, nodeAt)
	if err != nil || len(freeMap) == 0 {
		return nil
	}

	node := make([]byte, hdr.NodeSize)
	for num := uint32(1); num < hdr.TotalNodes; num++ {
		byteIdx := int(num / 8)
		if byteIdx >= len(freeMap) {
			break
		}
		if freeMap[byteIdx]&(1<<(7-num%8)) != 0 {
			continue // in use
		}
		if err := nodeAt(num, node); err != nil {
			continue
		}
		desc, derr := parseBTreeNodeDescriptor(node)
		if derr != nil {
			continue
		}
		// A former leaf still describes itself as one.
		if desc.Type != btreeNodeTypeLeaf || desc.NumRecords == 0 {
			continue
		}
		for _, rec := range extractNodeRecords(node, desc) {
			if err := v.carveOneRecord(rec, num, 0, RecoveredFromFreeNode, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

// readNodeBitmap returns the B-tree's node allocation bitmap, held in the third
// record of the header node.
func (v *Volume) readNodeBitmap(hdr BTreeHeaderRecord, nodeAt func(uint32, []byte) error) ([]byte, error) {
	node := make([]byte, hdr.NodeSize)
	if err := nodeAt(0, node); err != nil {
		return nil, err
	}
	desc, err := parseBTreeNodeDescriptor(node)
	if err != nil {
		return nil, err
	}
	recs := extractNodeRecords(node, desc)
	if len(recs) < 3 {
		return nil, nil
	}
	return recs[2], nil
}

// scanUnallocatedNodes carves catalog leaf nodes out of blocks the volume no
// longer considers allocated.
//
// This is by far the most expensive source — it reads the whole free area, and
// measured at roughly 500× a full catalog walk on a 511 MB image, scaling
// linearly with volume size. It is therefore the one operation in this package
// that runs concurrently.
//
// Parallelism does not change what is found. Each task scans a disjoint span of
// blocks and returns its own findings; those are concatenated in block order,
// so the sequence of emitted records is identical at any worker count. See
// CONCURRENCY.md.
func (v *Volume) scanUnallocatedNodes(ctx context.Context, hdr BTreeHeaderRecord, emit func(DeletedRecord) error) error {
	if hdr.NodeSize == 0 || v.header.BlockSize == 0 {
		return nil
	}

	// Collecting the runs first makes the work partitionable and bounds the
	// scan: everything after this point operates on a fixed set of tasks.
	var runs []blockRange
	err := v.WalkUnallocated(func(start, count uint32) error {
		runs = append(runs, blockRange{start: start, count: count})
		return nil
	})
	if err != nil {
		return err
	}

	tasks := splitBlockRuns(runs, carveBlockBatch)
	if len(tasks) == 0 {
		return nil
	}

	v.mu.RLock()
	workers := v.carveWorkers
	v.mu.RUnlock()

	// Each task returns its own findings rather than calling emit, so the
	// callback stays on the caller's goroutine and needs no synchronisation of
	// its own.
	batches, err := parallelMap(ctx, workers, len(tasks),
		func(ctx context.Context, i int) ([]DeletedRecord, error) {
			return v.carveBlockRange(ctx, hdr, tasks[i])
		})
	if err != nil {
		return err
	}

	for _, batch := range batches {
		for _, rec := range batch {
			if err := emit(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// carveBlockRange scans one span of allocation blocks for anything that parses
// as a catalog leaf node, and returns what it finds.
//
// It allocates its own buffer so concurrent tasks share nothing but the
// read-only volume and the io.ReaderAt beneath it.
func (v *Volume) carveBlockRange(ctx context.Context, hdr BTreeHeaderRecord, span blockRange) ([]DeletedRecord, error) {
	nodeSize := int(hdr.NodeSize)
	blockSize := int64(v.header.BlockSize)
	buf := make([]byte, blockSize)

	var found []DeletedRecord
	collect := func(rec DeletedRecord) error {
		found = append(found, rec)
		return nil
	}

	for b := span.start; b < span.start+span.count; b++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		off := v.diskOffset(int64(b) * blockSize)
		if err := readAtExact(v.reader, off, buf); err != nil {
			// An unreadable block yields nothing; a bad sector must not abort
			// the scan of everything after it.
			continue
		}
		for pos := 0; pos+nodeSize <= len(buf); pos += nodeSize {
			candidate := buf[pos : pos+nodeSize]
			desc, err := parseBTreeNodeDescriptor(candidate)
			if err != nil || desc.Type != btreeNodeTypeLeaf || desc.NumRecords == 0 {
				continue
			}
			for _, rec := range extractNodeRecords(candidate, desc) {
				if err := v.carveOneRecord(rec, 0, off+int64(pos), RecoveredFromUnallocated, collect); err != nil {
					return nil, err
				}
			}
		}
	}
	return found, nil
}

// carveRecords scans a byte span for anything that parses as a catalog record.
//
// Deleted records do not announce themselves, so this walks every offset. The
// validity checks in carveOneRecord are what keep the false-positive rate
// tolerable.
func (v *Volume) carveRecords(span []byte, nodeNum uint32, baseOff int, source RecoverySource, emit func(DeletedRecord) error) error {
	if len(span) < 8 {
		return nil
	}
	// Keys are 2-byte aligned within a node.
	for off := 0; off+8 <= len(span); off += 2 {
		if err := v.carveOneRecord(span[off:], nodeNum, int64(baseOff+off), source, emit); err != nil {
			return err
		}
	}
	return nil
}

// carveOneRecord attempts to decode one candidate record and grade it.
func (v *Volume) carveOneRecord(raw []byte, nodeNum uint32, byteOff int64, source RecoverySource, emit func(DeletedRecord) error) error {
	key, consumed, err := parseCatalogKeyForKind(v.kind, raw)
	if err != nil || consumed > len(raw) {
		return nil
	}
	// A key with an implausible parent is noise, not a record.
	if key.ParentCNID == 0 || (v.header.NextCatalogID != 0 && key.ParentCNID > v.header.NextCatalogID) {
		return nil
	}

	rec, err := v.decodeCatalogRecord(key, raw[consumed:])
	if err != nil {
		return nil
	}
	// Thread records carry no content and are mostly noise when carved.
	if rec.Type != CatalogRecordFile && rec.Type != CatalogRecordFolder {
		return nil
	}
	if rec.CNID == 0 || (v.header.NextCatalogID != 0 && rec.CNID > v.header.NextCatalogID) {
		return nil
	}
	if rec.Name == "" && rec.DataFork.LogicalSize == 0 {
		return nil
	}

	out := DeletedRecord{
		Record:     rec,
		Source:     source,
		NodeNumber: nodeNum,
		ByteOffset: byteOff,
	}
	out.Confidence, out.Overwritten = v.gradeRecovered(rec)
	return emit(out)
}

// gradeRecovered assigns a confidence grade and reports whether the record's
// blocks have been reused.
func (v *Volume) gradeRecovered(rec CatalogRecord) (Confidence, bool) {
	conf := ConfidenceLow

	parentLive := false
	if parent, err := v.OpenCNID(rec.ParentCNID); err == nil && parent.IsDirectory() {
		parentLive = true
	}
	if parentLive || (v.header.NextCatalogID != 0 && rec.CNID < v.header.NextCatalogID) {
		conf = ConfidenceMedium
	}

	exts := compactExtents(rec.DataFork.Extents[:])
	if len(exts) == 0 {
		return conf, false
	}

	overwritten := false
	inBounds := true
	for _, e := range exts {
		if e.StartBlock >= v.header.TotalBlocks || e.StartBlock+e.BlockCount > v.header.TotalBlocks {
			inBounds = false
			break
		}
		for b := e.StartBlock; b < e.StartBlock+e.BlockCount; b++ {
			inUse, err := v.BlockAllocated(b)
			if err != nil {
				inBounds = false
				break
			}
			if inUse {
				overwritten = true
				break
			}
		}
		if overwritten || !inBounds {
			break
		}
	}

	if parentLive && inBounds && !overwritten {
		conf = ConfidenceHigh
	}
	return conf, overwritten
}
