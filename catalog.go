package hfs

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// decodeCatalogTimesHFSPlus reads the five HFS+ catalog dates. Both
// HFSPlusCatalogFolder and HFSPlusCatalogFile place them at the same offsets,
// so one decoder serves both. The caller must have already checked that
// payload is at least 88 bytes.
func decodeCatalogTimesHFSPlus(payload []byte) CatalogTimes {
	return CatalogTimes{
		Created:         hfsCatalogTime(be32(payload[catCreateDate : catCreateDate+4])),
		ContentModified: hfsCatalogTime(be32(payload[catContentModDte : catContentModDte+4])),
		AttrModified:    hfsCatalogTime(be32(payload[catAttrModDate : catAttrModDate+4])),
		Accessed:        hfsCatalogTime(be32(payload[catAccessDate : catAccessDate+4])),
		Backup:          hfsCatalogTime(be32(payload[catBackupDate : catBackupDate+4])),
		Source:          TimeSourceHFSPlusGMT,
	}
}

// decodeCatalogTimesHFSFile reads the three dates in a classic HFS CatFilRec:
// filCrDat, filMdDat and filBkDat. Classic HFS records no access or
// attribute-modification date. The caller must have already checked that
// payload is at least 56 bytes.
func decodeCatalogTimesHFSFile(payload []byte) CatalogTimes {
	return CatalogTimes{
		Created:         hfsCatalogTime(be32(payload[hfsFilCreateDate : hfsFilCreateDate+4])),
		ContentModified: hfsCatalogTime(be32(payload[hfsFilModifyDate : hfsFilModifyDate+4])),
		Backup:          hfsCatalogTime(be32(payload[hfsFilBackupDate : hfsFilBackupDate+4])),
		Source:          TimeSourceHFSLocal,
	}
}

// decodeCatalogTimesHFSDir reads the three dates in a classic HFS CatDirRec:
// dirCrDat, dirMdDat and dirBkDat. The caller must have already checked that
// payload is at least 22 bytes.
func decodeCatalogTimesHFSDir(payload []byte) CatalogTimes {
	return CatalogTimes{
		Created:         hfsCatalogTime(be32(payload[hfsDirCreateDate : hfsDirCreateDate+4])),
		ContentModified: hfsCatalogTime(be32(payload[hfsDirModifyDate : hfsDirModifyDate+4])),
		Backup:          hfsCatalogTime(be32(payload[hfsDirBackupDate : hfsDirBackupDate+4])),
		Source:          TimeSourceHFSLocal,
	}
}

func decodeCatalogRecord(key CatalogKey, payload []byte) (CatalogRecord, error) {
	if len(payload) < 2 {
		return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
	}

	recType := be16(payload[0:2])
	rec := CatalogRecord{
		Type:       CatalogRecordType(recType),
		ParentCNID: key.ParentCNID,
		Name:       key.NameString(),
	}

	switch recType {
	case catalogRecordFolder:
		if len(payload) < catFolderRecordSize {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
		}
		rec.Valence = be32(payload[catValence : catValence+4])
		rec.CNID = be32(payload[catNodeID : catNodeID+4])
		rec.Times = decodeCatalogTimesHFSPlus(payload)
		rec.Perms = parseBSDInfo(payload)
		rec.LinkID = rec.Perms.Special
		// For folders, userInfo is a FndrDirInfo whose first fields are window
		// bounds, not a type/creator pair. They are read into FinderType and
		// FinderCreator for backward compatibility, but the meaningful form for
		// a folder is the raw FinderInfo block.
		rec.FinderType = be32(payload[catFileType : catFileType+4])
		rec.FinderCreator = be32(payload[catFileCreator : catFileCreator+4])
		copy(rec.FinderInfo[:], payload[catFinderBlock:catFinderBlock+catFinderBlockSize])
		rec.Link = classifyLink(rec.FinderType, rec.FinderCreator, rec.Perms.FileMode, true)
		if rec.Link == LinkHardDir {
			rec.LinkTarget = rec.Perms.Special
		}
		return rec, nil
	case catalogRecordFile:
		if len(payload) < catFolderRecordSize {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
		}
		rec.CNID = be32(payload[catNodeID : catNodeID+4])
		rec.Times = decodeCatalogTimesHFSPlus(payload)
		rec.Perms = parseBSDInfo(payload)
		rec.LinkID = rec.Perms.Special
		rec.FinderType = be32(payload[catFileType : catFileType+4])
		rec.FinderCreator = be32(payload[catFileCreator : catFileCreator+4])
		copy(rec.FinderInfo[:], payload[catFinderBlock:catFinderBlock+catFinderBlockSize])
		rec.Link = classifyLink(rec.FinderType, rec.FinderCreator, rec.Perms.FileMode, false)
		if rec.Link == LinkHardFile {
			rec.LinkTarget = rec.Perms.Special
		}
		if len(payload) >= catRsrcFork {
			rec.DataFork = parseForkData(payload[catDataFork:catRsrcFork])
		}
		if len(payload) >= catFileRecordSize {
			rec.RsrcFork = parseForkData(payload[catRsrcFork:catFileRecordSize])
		}
		return rec, nil
	case catalogRecordFolderThread, catalogRecordFileThread:
		if len(payload) < threadHeaderSize {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
		}
		rec.CNID = 0
		rec.ThreadCNID = key.ParentCNID
		rec.ParentCNID = be32(payload[threadParentID : threadParentID+4])
		nameChars := int(be16(payload[threadNameLength : threadNameLength+2]))
		need := threadName + nameChars*utf16CodeUnitSize
		if need > len(payload) {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
		}
		u16 := make([]uint16, nameChars)
		for i := range nameChars {
			base := threadName + i*utf16CodeUnitSize
			u16[i] = be16(payload[base : base+utf16CodeUnitSize])
		}
		rec.Name = string(utf16.Decode(u16))
		return rec, nil
	default:
		return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
	}
}

func (v *Volume) decodeCatalogRecordHFS(key CatalogKey, payload []byte, blockSize uint32) (CatalogRecord, error) {
	if len(payload) < 1 {
		return CatalogRecord{}, &ParseError{Op: "decode_catalog_record_hfs", Offset: 0, Err: ErrCorrupt}
	}

	rec := CatalogRecord{ParentCNID: key.ParentCNID, Name: v.decodeHFSName(key.NameBytes)}

	// blocksFor converts a physical byte count to allocation blocks, rounding
	// up. Classic HFS records physical sizes in bytes, not blocks.
	blocksFor := func(physicalBytes uint32) uint32 {
		if blockSize == 0 {
			return 0
		}
		return uint32((uint64(physicalBytes) + uint64(blockSize) - 1) / uint64(blockSize))
	}

	switch payload[0] {
	case hfsRecordTypeFolder:
		rec.Type = CatalogRecordFolder
		if len(payload) < hfsDirMinSize {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record_hfs", Offset: 0, Err: ErrCorrupt}
		}
		rec.Valence = uint32(be16(payload[hfsDirValence : hfsDirValence+2]))
		rec.CNID = be32(payload[hfsDirDirID : hfsDirDirID+4])
		rec.Times = decodeCatalogTimesHFSDir(payload)
		return rec, nil

	case hfsRecordTypeFile:
		rec.Type = CatalogRecordFile
		if len(payload) < hfsFilMinSize {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record_hfs", Offset: 0, Err: ErrCorrupt}
		}
		rec.CNID = be32(payload[hfsFilFileNumber : hfsFilFileNumber+4])
		rec.Times = decodeCatalogTimesHFSFile(payload)
		rec.FinderType = be32(payload[hfsFilFdType : hfsFilFdType+4])
		rec.FinderCreator = be32(payload[hfsFilFdCreator : hfsFilFdCreator+4])

		rec.DataFork.LogicalSize = uint64(be32(payload[hfsFilLogicalSize : hfsFilLogicalSize+4]))
		rec.DataFork.TotalBlocks = blocksFor(be32(payload[hfsFilPhysSize : hfsFilPhysSize+4]))
		dataExtents, err := parseExtentsRecordHFS(payload[hfsFilExtentRec : hfsFilExtentRec+hfsExtentRecordSize])
		if err != nil {
			return CatalogRecord{}, err
		}
		copy(rec.DataFork.Extents[:], dataExtents)

		rec.RsrcFork.LogicalSize = uint64(be32(payload[hfsFilRLogicalLen : hfsFilRLogicalLen+4]))
		rec.RsrcFork.TotalBlocks = blocksFor(be32(payload[hfsFilRPhysLen : hfsFilRPhysLen+4]))
		rsrcExtents, err := parseExtentsRecordHFS(payload[hfsFilRExtentRec : hfsFilRExtentRec+hfsExtentRecordSize])
		if err != nil {
			return CatalogRecord{}, err
		}
		copy(rec.RsrcFork.Extents[:], rsrcExtents)
		return rec, nil
	case hfsRecordTypeFolderThread, hfsRecordTypeFileThread:
		if payload[0] == hfsRecordTypeFolderThread {
			rec.Type = CatalogRecordFolderThread
		} else {
			rec.Type = CatalogRecordFileThread
		}
		if len(payload) < hfsThrMinSize {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record_hfs", Offset: 0, Err: ErrCorrupt}
		}
		rec.ThreadCNID = key.ParentCNID
		rec.ParentCNID = be32(payload[hfsThrParID : hfsThrParID+4])
		nameLen := int(payload[hfsThrCName])
		nameAt := hfsThrCName + 1
		if nameAt+nameLen > len(payload) {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record_hfs", Offset: 0, Err: ErrCorrupt}
		}
		rec.Name = v.decodeHFSName(payload[nameAt : nameAt+nameLen])
		return rec, nil
	default:
		return CatalogRecord{}, &ParseError{Op: "decode_catalog_record_hfs", Offset: 0, Err: ErrCorrupt}
	}
}

func (v *Volume) decodeCatalogRecord(key CatalogKey, payload []byte) (CatalogRecord, error) {
	if v != nil && v.kind == KindHFS {
		return v.decodeCatalogRecordHFS(key, payload, v.header.BlockSize)
	}
	return decodeCatalogRecord(key, payload)
}

func (r CatalogRecord) hardLinkTargetCNID() uint32 {
	if r.Type != CatalogRecordFile {
		return 0
	}
	if r.Link != LinkHardFile {
		return 0
	}
	return r.LinkTarget
}

func (v *Volume) CatalogRecords() ([]CatalogRecord, error) {
	recs := make([]CatalogRecord, 0, 64)
	err := v.WalkCatalog(func(r CatalogRecord) error {
		recs = append(recs, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return recs, nil
}

// WalkCatalogContext is [Volume.WalkCatalog] with cancellation.
//
// A full catalog walk on a large image reads every leaf node, so a caller that
// cannot wait needs a way out. The context is checked once per record;
// its error is returned.
func (v *Volume) WalkCatalogContext(ctx context.Context, cb func(CatalogRecord) error) error {
	if cb == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return v.WalkCatalog(func(r CatalogRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return cb(r)
	})
}

func (v *Volume) WalkCatalog(cb func(CatalogRecord) error) error {
	if cb == nil {
		return nil
	}
	// Use the leaf-chain path for sequential full scans: faster than tree recursion.
	err := v.walkCatalogLeafChain(func(key CatalogKey, payload []byte) error {
		r, err := v.decodeCatalogRecord(key, payload)
		if err != nil {
			v.noteDecodeFailure("catalog_decode", key.ParentCNID)
			return nil
		}
		return cb(r)
	})
	return endWalk(err)
}

func (v *Volume) OpenCNID(cnid uint32) (CatalogRecord, error) {
	rec, err := v.lookupCNIDRaw(cnid)
	if err != nil {
		return CatalogRecord{}, err
	}
	return v.hydrateCatalogRecord(rec)
}

func (v *Volume) lookupCNIDRaw(cnid uint32) (CatalogRecord, error) {
	if v == nil {
		return CatalogRecord{}, &ParseError{Op: "open_cnid", Offset: int64(cnid), Err: ErrCorrupt}
	}
	if rec, ok := v.cacheLookup(cnid); ok {
		return rec, nil
	}

	// Preferred path: resolve the thread record to learn the parent, then scan
	// only that parent's children. Two keyed descents instead of a full scan.
	if v.supportsKeyedSearch() {
		if rec, err := v.lookupCNIDViaThread(cnid); err == nil {
			v.cacheStore(cnid, rec)
			return rec, nil
		}
	}

	rec, err := v.lookupCNIDLinear(cnid)
	if err != nil {
		return CatalogRecord{}, err
	}
	v.cacheStore(cnid, rec)
	return rec, nil
}

// lookupCNIDViaThread resolves a CNID through its thread record. Volumes
// written by macOS always carry thread records for both files and folders;
// when one is missing or damaged the caller falls back to a linear scan.
func (v *Volume) lookupCNIDViaThread(cnid uint32) (CatalogRecord, error) {
	thr, err := v.findThreadRecord(cnid)
	if err != nil {
		return CatalogRecord{}, err
	}

	var out CatalogRecord
	var found bool
	var lastCandidate CatalogRecord
	var lastCandidateFound bool

	err = v.walkCatalogChildren(thr.ParentCNID, func(key CatalogKey, payload []byte) error {
		r, err := v.decodeCatalogRecord(key, payload)
		if err != nil {
			v.noteDecodeFailure("catalog_decode", key.ParentCNID)
			return nil
		}
		if r.CNID != cnid {
			return nil
		}
		if r.Type != CatalogRecordFolder && r.Type != CatalogRecordFile {
			return nil
		}
		if v.isPlaceholderRecord(r) {
			if !lastCandidateFound {
				lastCandidate, lastCandidateFound = r, true
			}
			return nil
		}
		out = r
		found = true
		return ErrStopWalk
	})
	if err != nil && !errors.Is(err, ErrStopWalk) {
		return CatalogRecord{}, err
	}
	if found {
		return out, nil
	}
	if lastCandidateFound {
		return lastCandidate, nil
	}
	return CatalogRecord{}, ErrNotFound
}

// isPlaceholderRecord reports whether a file record looks like an empty
// placeholder that a real record with the same identity should win over.
//
// The heuristic keys off record contents rather than B-tree key order, so a
// genuinely empty file whose CNID also appears in a stale record can resolve
// unpredictably. It predates keyed descent and is preserved unchanged because
// altering it would change which record a duplicate CNID resolves to — a
// visible behaviour change that belongs in its own commit, not smuggled in
// alongside a performance rewrite. Re-deriving it from key order is the fix.
func (v *Volume) isPlaceholderRecord(r CatalogRecord) bool {
	return v.kind != KindHFS &&
		r.Type == CatalogRecordFile &&
		r.DataFork.LogicalSize == 0 &&
		r.DataFork.TotalBlocks == 0
}

func (v *Volume) lookupCNIDLinear(cnid uint32) (CatalogRecord, error) {
	var out CatalogRecord
	var found bool
	var lastCandidate CatalogRecord
	var lastCandidateFound bool

	err := v.walkCatalogBTree(func(key CatalogKey, payload []byte) error {
		r, err := v.decodeCatalogRecord(key, payload)
		if err != nil {
			v.noteDecodeFailure("catalog_decode", key.ParentCNID)
			return nil
		}
		if r.CNID == cnid {
			// Skip empty file records that appear to be placeholders (but not hard-links)
			if v.kind != KindHFS && r.Type == CatalogRecordFile && r.DataFork.LogicalSize == 0 && r.DataFork.TotalBlocks == 0 {
				// This might be a placeholder; keep looking for a non-empty record with the same CNID
				if !lastCandidateFound {
					lastCandidate = r
					lastCandidateFound = true
				}
				return nil // Continue searching
			}
			out = r
			found = true
			return ErrStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrStopWalk) {
		return CatalogRecord{}, err
	}
	if found {
		return out, nil
	}
	if lastCandidateFound {
		// Fall back to the placeholder we found if no real record exists
		return lastCandidate, nil
	}
	return CatalogRecord{}, ErrNotFound
}

func (v *Volume) resolveHardLinkRecord(rec CatalogRecord) (CatalogRecord, error) {
	seen := map[uint32]struct{}{}
	resolved := rec

	for {
		targetCNID := resolved.hardLinkTargetCNID()
		if targetCNID == 0 || targetCNID == resolved.CNID {
			return resolved, nil
		}
		if _, ok := seen[targetCNID]; ok {
			return CatalogRecord{}, &ParseError{Op: "resolve_hard_link", Offset: int64(targetCNID), Err: ErrCorrupt}
		}
		seen[targetCNID] = struct{}{}

		target, err := v.lookupHardLinkInode(targetCNID)
		if err != nil {
			return CatalogRecord{}, err
		}
		// The resolved record reports the target's content under the link's
		// identity. The link facts are carried across rather than discarded, so
		// a caller can still tell this was reached through a hard link and how
		// many links share the inode — on the inode, the BSD special field is
		// the link count.
		target.Name = rec.Name
		target.ParentCNID = rec.ParentCNID
		target.Link = resolved.Link
		// The inode's own CNID, which is normally the same number as the stub's
		// iNodeNum but is not guaranteed to be — see lookupHardLinkInode. The
		// raw iNodeNum stays available as Perms.Special and LinkID.
		target.LinkTarget = target.CNID
		target.LinkCount = target.Perms.Special
		resolved = target
	}
}

// lookupHardLinkInode finds the inode a hard-link stub names.
//
// HFS+ does not store the target's CNID in the stub. It stores an "indirect
// node number", and the inode itself lives in the volume's private metadata
// directory under the name "iNode<number>" — resolution is by name, not by
// CNID. Apple's hfs_makelink normally assigns that number from the file's own
// c_fileid, and renaming the file into the private directory preserves its
// CNID, so the two are usually equal:
//
//	indnodeno = cp->c_fileid;
//	MAKE_INODE_NAME(inodename, sizeof(inodename), indnodeno);
//
// Usually, but not always. When that name already exists the loop retries with
// a separate counter (`indnodeno = cur_link_id++`), and from then on the number
// in the stub is not the inode's CNID at all. Treating it as one then resolves
// to whatever unrelated record happens to hold that CNID, or to nothing.
//
// So the name lookup is authoritative and the CNID is the fallback, which also
// keeps volumes whose private directory is missing or damaged working exactly
// as they did before.
func (v *Volume) lookupHardLinkInode(iNodeNum uint32) (CatalogRecord, error) {
	if rec, ok := v.findInodeByName(iNodeNum); ok {
		return rec, nil
	}
	return v.lookupCNIDRaw(iNodeNum)
}

// findInodeByName looks for "iNode<n>" inside the private metadata directory.
func (v *Volume) findInodeByName(iNodeNum uint32) (CatalogRecord, bool) {
	parent, ok := v.privateDataDirCNID()
	if !ok {
		return CatalogRecord{}, false
	}

	want := "iNode" + strconv.FormatUint(uint64(iNodeNum), 10)
	var found CatalogRecord
	var ok2 bool
	err := v.walkCatalogChildren(parent, func(key CatalogKey, payload []byte) error {
		if key.NameString() != want {
			return nil
		}
		r, err := v.decodeCatalogRecord(key, payload)
		if err != nil || r.Type != CatalogRecordFile {
			return nil
		}
		found, ok2 = r, true
		return ErrStopWalk
	})
	if err != nil && !errors.Is(err, ErrStopWalk) {
		return CatalogRecord{}, false
	}
	return found, ok2
}

// privateDataDirCNID finds the hard-link store in the volume root.
//
// Its real name begins with four NUL bytes so that it cannot be typed, and
// isSystemFile keys off the same substring this does.
func (v *Volume) privateDataDirCNID() (uint32, bool) {
	// Cached because a volume with many hard links — a Time Machine backup is
	// the normal case — would otherwise rescan the root for every one of them.
	// The lock is dropped before walking, so at worst two callers race and both
	// do the search; holding it across walkCatalogChildren would deadlock
	// against the node cache.
	v.mu.RLock()
	looked, cnid, found := v.privDirLooked, v.privDirCNID, v.privDirFound
	v.mu.RUnlock()
	if looked {
		return cnid, found
	}

	cnid, found = v.findPrivateDataDir()

	v.mu.Lock()
	v.privDirCNID, v.privDirFound, v.privDirLooked = cnid, found, true
	v.mu.Unlock()
	return cnid, found
}

func (v *Volume) findPrivateDataDir() (uint32, bool) {
	var cnid uint32
	var ok bool
	err := v.walkCatalogChildren(rootFolderCNID, func(key CatalogKey, payload []byte) error {
		name := key.NameString()
		if !strings.Contains(name, privateDataDirNameSub) || strings.Contains(name, "Directory Data") {
			return nil
		}
		r, err := v.decodeCatalogRecord(key, payload)
		if err != nil || r.Type != CatalogRecordFolder {
			return nil
		}
		cnid, ok = r.CNID, true
		return ErrStopWalk
	})
	if err != nil && !errors.Is(err, ErrStopWalk) {
		return 0, false
	}
	return cnid, ok
}

func (v *Volume) hydrateCatalogRecord(rec CatalogRecord) (CatalogRecord, error) {
	resolved, err := v.resolveHardLinkRecord(rec)
	if err != nil {
		return CatalogRecord{}, err
	}
	return v.hydrateCompressedRecord(resolved), nil
}

// GetTimes returns the MACB timestamp set for a catalog node.
//
// Check CatalogTimes.Source before comparing values across volumes, and test
// individual fields with IsZero: a zero time means the field was unset on
// disk, not that the event happened at the epoch.
//
// For a hard link this reports the target inode's timestamps, matching the
// resolution OpenCNID performs.
func (v *Volume) GetTimes(cnid uint32) (CatalogTimes, error) {
	rec, err := v.OpenCNID(cnid)
	if err != nil {
		return CatalogTimes{}, err
	}
	return rec.Times, nil
}

// GetTimesByPath returns the MACB timestamp set for a path. See GetTimes for
// the caveats that apply to the returned values.
func (v *Volume) GetTimesByPath(path string) (CatalogTimes, error) {
	rec, err := v.OpenPath(path)
	if err != nil {
		return CatalogTimes{}, err
	}
	return rec.Times, nil
}

func (v *Volume) GetRootDirectory() (CatalogRecord, error) {
	return v.OpenCNID(rootFolderCNID)
}

func (v *Volume) OpenPath(path string) (CatalogRecord, error) {
	cmp, err := v.catalogNameComparer()
	if err != nil {
		return CatalogRecord{}, err
	}

	if path == "" || path == "/" {
		return v.GetRootDirectory()
	}
	parts := splitPath(path)
	if len(parts) == 0 {
		return v.GetRootDirectory()
	}

	cur, err := v.GetRootDirectory()
	if err != nil {
		return CatalogRecord{}, err
	}

	for _, p := range parts {
		next, err := v.findChild(cur.CNID, p, cmp)
		if err != nil {
			return CatalogRecord{}, err
		}
		cur = next
	}
	return cur, nil
}

func (v *Volume) findChild(parent uint32, name string, cmp func(a, b string) bool) (CatalogRecord, error) {
	var out CatalogRecord
	var found bool
	var lastCandidate CatalogRecord
	var lastCandidateFound bool

	// Scans only this parent's run of children rather than the whole catalog.
	// Names are matched with cmp instead of by keyed lookup, so the volume's
	// collation never has to be reproduced here — see compareCatalogKeys.
	err := v.walkCatalogChildren(parent, func(key CatalogKey, payload []byte) error {
		r, err := v.decodeCatalogRecord(key, payload)
		if err != nil {
			v.noteDecodeFailure("catalog_decode", key.ParentCNID)
			return nil
		}
		if r.ParentCNID != parent {
			return nil
		}
		if cmp(r.Name, name) {
			// Skip empty file records that appear to be placeholders
			if v.kind != KindHFS && r.Type == CatalogRecordFile && r.DataFork.LogicalSize == 0 && r.DataFork.TotalBlocks == 0 {
				// This might be a placeholder; keep looking for a non-empty record with the same name
				if !lastCandidateFound {
					lastCandidate = r
					lastCandidateFound = true
				}
				return nil // Continue searching
			}
			out = r
			found = true
			return ErrStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrStopWalk) {
		return CatalogRecord{}, err
	}
	if found {
		return v.hydrateCatalogRecord(out)
	}
	if lastCandidateFound {
		// Fall back to the placeholder if no real record exists
		return v.hydrateCatalogRecord(lastCandidate)
	}
	return CatalogRecord{}, ErrNotFound
}

func (v *Volume) catalogNameComparer() (func(a, b string) bool, error) {
	// Every by-path accessor asks for a comparer before it resolves anything,
	// so this is where the whole path family meets a nil receiver.
	if v == nil {
		return nil, &ParseError{Op: "open_path", Offset: 0, Err: ErrCorrupt}
	}
	if v.kind == KindHFS {
		return func(a, b string) bool {
			return strings.EqualFold(a, b)
		}, nil
	}

	h, err := v.CatalogBTreeHeader()
	if err != nil {
		return nil, err
	}
	if v.kind == KindHFSX && h.CompType == btreeCompTypeSensitive {
		return func(a, b string) bool {
			return a == b
		}, nil
	}
	// HFS+ (and HFSX case-insensitive) use Apple's FastUnicodeCompare table.
	return func(a, b string) bool {
		return hfsUnicodeEqual(a, b)
	}, nil
}

func (v *Volume) ReadDir(path string) ([]DirEntry, error) {
	rec, err := v.OpenPath(path)
	if err != nil {
		return nil, err
	}
	return v.ReadDirCNID(rec.CNID)
}

func (v *Volume) ReadDirCNID(cnid uint32) ([]DirEntry, error) {
	out := make([]DirEntry, 0, 16)
	err := v.WalkDirCNID(cnid, func(ent DirEntry) error {
		out = append(out, ent)
		return nil
	})
	if err != nil {
		return nil, err
	}
	seen := make(map[DirEntry]struct{}, len(out))
	dedup := out[:0]
	for _, ent := range out {
		if _, ok := seen[ent]; ok {
			continue
		}
		seen[ent] = struct{}{}
		dedup = append(dedup, ent)
	}
	out = dedup
	sort.Slice(out, func(i, j int) bool {
		if strings.EqualFold(out[i].Name, out[j].Name) {
			return out[i].CNID < out[j].CNID
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (v *Volume) WalkDir(path string, cb func(DirEntry) error) error {
	rec, err := v.OpenPath(path)
	if err != nil {
		return err
	}
	return v.WalkDirCNID(rec.CNID, cb)
}

func (v *Volume) WalkDirCNID(cnid uint32, cb func(DirEntry) error) error {
	rec, err := v.OpenCNID(cnid)
	if err != nil {
		return err
	}
	if !rec.IsDirectory() {
		return ErrNotDir
	}
	if cb == nil {
		return nil
	}

	err = v.walkCatalogChildren(cnid, func(key CatalogKey, payload []byte) error {
		r, err := v.decodeCatalogRecord(key, payload)
		if err != nil {
			v.noteDecodeFailure("catalog_decode", key.ParentCNID)
			return nil
		}
		if r.ParentCNID != cnid {
			return nil
		}
		if r.Type != CatalogRecordFolder && r.Type != CatalogRecordFile {
			return nil
		}
		if r.Name == "" {
			return nil
		}
		resolved, err := v.hydrateCatalogRecord(r)
		if err != nil {
			return err
		}
		return cb(DirEntry{
			Name:        r.Name,
			CNID:        resolved.CNID,
			Type:        resolved.Type,
			IsDirectory: resolved.IsDirectory(),
			IsSystem:    isSystemFile(r.Name),
		})
	})
	return endWalk(err)
}

// isSystemFile reports whether a catalog name is filesystem metadata rather
// than user data.
//
// The leading-NUL check matters most. Apple names the hard-link and directory-
// link stores "\x00\x00\x00\x00HFS+ Private Data" and
// "\x00\x00\x00\x00HFS+ Private Directory Data\r", using NULs and a trailing
// carriage return specifically so the names cannot be typed. Matching only on a
// "HFS+" or "." prefix misses them entirely, which left the private-data
// directory reported as ordinary user content — confirmed on the corpus image,
// where it appears in the root listing.
func isSystemFile(name string) bool {
	if name == "" {
		return false
	}
	if name[0] == 0x00 || name[0] == '$' {
		return true
	}
	if strings.Contains(name, privateDataDirNameSub) {
		return true
	}
	if strings.HasPrefix(name, ".HFS") {
		return true
	}
	// Journal files live in the root of a journaled volume.
	return name == ".journal" || name == ".journal_info_block"
}

func splitPath(p string) []string {
	parts := strings.Split(strings.TrimSpace(p), "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		out = append(out, part)
	}
	return out
}

// PathForCNID returns the full path of a catalog node, resolved by climbing
// thread records to the root.
//
// The record must exist: a CNID with no catalog record reports [ErrNotFound]
// rather than an empty path. A parent chain that loops reports [ErrCorrupt],
// which on a damaged volume is a finding rather than merely a failed lookup.
//
// Each call climbs the whole chain. To resolve paths for the entire catalog,
// use [Volume.WalkPaths], which shares the work across records instead of
// repeating it per record.
func (v *Volume) PathForCNID(cnid uint32) (string, error) {
	if cnid == rootFolderCNID {
		return "/", nil
	}

	if _, err := v.OpenCNID(cnid); err != nil {
		return "", err
	}

	return v.pathUp(cnid, nil)
}

func (v *Volume) findThreadRecord(targetCNID uint32) (CatalogRecord, error) {
	var out CatalogRecord
	found := false
	// A thread record is keyed on (ownCNID, ""), so it sits at the head of the
	// run of records whose key parent is the target CNID.
	err := v.walkCatalogChildren(targetCNID, func(key CatalogKey, payload []byte) error {
		r, err := v.decodeCatalogRecord(key, payload)
		if err != nil {
			v.noteDecodeFailure("catalog_decode", key.ParentCNID)
			return nil
		}
		if r.Type != CatalogRecordFolderThread && r.Type != CatalogRecordFileThread {
			return nil
		}
		if r.ThreadCNID != targetCNID {
			return nil
		}
		out = r
		found = true
		return ErrStopWalk
	})
	if err != nil && !errors.Is(err, ErrStopWalk) {
		return CatalogRecord{}, err
	}
	if !found {
		return CatalogRecord{}, ErrNotFound
	}
	return out, nil
}
