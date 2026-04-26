package hfs

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf16"
)

var errStopWalk = errors.New("hfs: stop walk")

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
		if len(payload) < 88 {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
		}
		rec.Valence = be32(payload[4:8])
		rec.CNID = be32(payload[8:12])
		rec.LinkID = be32(payload[44:48])
		rec.FinderType = be32(payload[48:52])
		rec.FinderCreator = be32(payload[52:56])
		return rec, nil
	case catalogRecordFile:
		if len(payload) < 88 {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
		}
		rec.CNID = be32(payload[8:12])
		rec.LinkID = be32(payload[44:48])
		rec.FinderType = be32(payload[48:52])
		rec.FinderCreator = be32(payload[52:56])
		if len(payload) >= 168 {
			rec.DataFork = parseForkData(payload[88:168])
		}
		if len(payload) >= 248 {
			rec.RsrcFork = parseForkData(payload[168:248])
		}
		return rec, nil
	case catalogRecordFolderThread, catalogRecordFileThread:
		if len(payload) < 10 {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
		}
		rec.CNID = 0
		rec.ThreadCNID = key.ParentCNID
		rec.ParentCNID = be32(payload[4:8])
		nameChars := int(be16(payload[8:10]))
		need := 10 + nameChars*2
		if need > len(payload) {
			return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
		}
		u16 := make([]uint16, nameChars)
		for i := 0; i < nameChars; i++ {
			base := 10 + i*2
			u16[i] = be16(payload[base : base+2])
		}
		rec.Name = string(utf16.Decode(u16))
		return rec, nil
	default:
		return CatalogRecord{}, &ParseError{Op: "decode_catalog_record", Offset: 0, Err: ErrCorrupt}
	}
}

func (r CatalogRecord) hardLinkTargetCNID() uint32 {
	if r.Type != CatalogRecordFile {
		return 0
	}
	if r.LinkID == 0 {
		return 0
	}
	if r.FinderType != hfsHardlinkFileType || r.FinderCreator != hfsHardlinkFileCreator {
		return 0
	}
	return r.LinkID
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

func (v *Volume) WalkCatalog(cb func(CatalogRecord) error) error {
	if cb == nil {
		return nil
	}
	// Use the leaf-chain path for sequential full scans: faster than tree recursion.
	err := v.walkCatalogLeafChain(func(key CatalogKey, payload []byte) error {
		r, err := decodeCatalogRecord(key, payload)
		if err != nil {
			return nil
		}
		return cb(r)
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return err
	}
	return nil
}

func (v *Volume) OpenCNID(cnid uint32) (CatalogRecord, error) {
	rec, err := v.lookupCNIDRaw(cnid)
	if err != nil {
		return CatalogRecord{}, err
	}
	return v.hydrateCatalogRecord(rec)
}

func (v *Volume) lookupCNIDRaw(cnid uint32) (CatalogRecord, error) {
	var out CatalogRecord
	var found bool
	var lastCandidate CatalogRecord
	var lastCandidateFound bool

	err := v.walkCatalogBTree(func(key CatalogKey, payload []byte) error {
		r, err := decodeCatalogRecord(key, payload)
		if err != nil {
			return nil
		}
		if r.CNID == cnid {
			// Skip empty file records that appear to be placeholders (but not hard-links)
			if r.Type == CatalogRecordFile && r.DataFork.LogicalSize == 0 && r.DataFork.TotalBlocks == 0 {
				// This might be a placeholder; keep looking for a non-empty record with the same CNID
				if !lastCandidateFound {
					lastCandidate = r
					lastCandidateFound = true
				}
				return nil // Continue searching
			}
			out = r
			found = true
			return errStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
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

		target, err := v.lookupCNIDRaw(targetCNID)
		if err != nil {
			return CatalogRecord{}, err
		}
		target.Name = rec.Name
		target.ParentCNID = rec.ParentCNID
		resolved = target
	}
}

func (v *Volume) hydrateCatalogRecord(rec CatalogRecord) (CatalogRecord, error) {
	resolved, err := v.resolveHardLinkRecord(rec)
	if err != nil {
		return CatalogRecord{}, err
	}
	return v.hydrateCompressedRecord(resolved), nil
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

	err := v.walkCatalogBTree(func(key CatalogKey, payload []byte) error {
		r, err := decodeCatalogRecord(key, payload)
		if err != nil {
			return nil
		}
		if r.ParentCNID != parent {
			return nil
		}
		if cmp(r.Name, name) {
			// Skip empty file records that appear to be placeholders
			if r.Type == CatalogRecordFile && r.DataFork.LogicalSize == 0 && r.DataFork.TotalBlocks == 0 {
				// This might be a placeholder; keep looking for a non-empty record with the same name
				if !lastCandidateFound {
					lastCandidate = r
					lastCandidateFound = true
				}
				return nil // Continue searching
			}
			out = r
			found = true
			return errStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
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

	err = v.walkCatalogBTree(func(key CatalogKey, payload []byte) error {
		r, err := decodeCatalogRecord(key, payload)
		if err != nil {
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
	if err != nil && !errors.Is(err, errStopWalk) {
		return err
	}
	return nil
}

func isSystemFile(name string) bool {
	if name == "" {
		return false
	}
	// HFS+ system files start with $ or .HFS
	return name[0] == '$' || strings.HasPrefix(name, ".HFS")
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

func (v *Volume) PathForCNID(cnid uint32) (string, error) {
	if cnid == rootFolderCNID {
		return "/", nil
	}

	if _, err := v.OpenCNID(cnid); err != nil {
		return "", err
	}

	parts := make([]string, 0, 8)
	visited := map[uint32]struct{}{}
	cur := cnid

	for cur != rootFolderCNID {
		if _, seen := visited[cur]; seen {
			return "", &ParseError{Op: "path_for_cnid", Offset: int64(cur), Err: ErrCorrupt}
		}
		visited[cur] = struct{}{}

		thr, err := v.findThreadRecord(cur)
		if err != nil {
			return "", err
		}
		if thr.Name == "" {
			return "", &ParseError{Op: "path_for_cnid", Offset: int64(cur), Err: ErrCorrupt}
		}
		parts = append(parts, thr.Name)
		cur = thr.ParentCNID
	}

	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return "/" + strings.Join(parts, "/"), nil
}

func (v *Volume) findThreadRecord(targetCNID uint32) (CatalogRecord, error) {
	var out CatalogRecord
	found := false
	err := v.walkCatalogBTree(func(key CatalogKey, payload []byte) error {
		r, err := decodeCatalogRecord(key, payload)
		if err != nil {
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
		return errStopWalk
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return CatalogRecord{}, err
	}
	if !found {
		return CatalogRecord{}, ErrNotFound
	}
	return out, nil
}
