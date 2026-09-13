package hfs

import (
	"context"
	"errors"
)

// walkPathMemoMax bounds the number of directory paths [Volume.WalkPaths] holds
// at once. It is a variable so that a test can lower it and prove that eviction
// changes nothing but the amount of work done.
//
// The memo only ever holds directories, and children of one parent are
// contiguous in catalog key order, so even a very small memo serves the common
// case. This bound exists to stop a volume with an enormous number of
// directories from growing it without limit, not to make the usual walk fit.
var walkPathMemoMax = 8192

// pathMemoEntry is a resolved path, or the error that resolving it produced.
//
// Failures are remembered as well as successes. On a damaged volume one broken
// directory has many children, and without a negative entry each of them pays a
// fresh upward resolution that has already degraded into an exhaustive catalog
// scan.
type pathMemoEntry struct {
	path string
	err  error
}

// pathMemo caches directory paths for the duration of one walk.
//
// Eviction is by insertion order rather than by use, on the same reasoning as
// evictOldestLocked in cache.go: the access pattern is a single sweep, so
// insertion recency tracks use recency closely enough. Evicting an entry costs
// extra lookups and never changes a result.
type pathMemo struct {
	entries map[uint32]pathMemoEntry
	order   []uint32
	max     int
}

func newPathMemo(max int) *pathMemo {
	if max < 1 {
		max = 1
	}
	return &pathMemo{
		entries: make(map[uint32]pathMemoEntry, 64),
		order:   make([]uint32, 0, 64),
		max:     max,
	}
}

func (m *pathMemo) get(cnid uint32) (pathMemoEntry, bool) {
	if m == nil {
		return pathMemoEntry{}, false
	}
	e, ok := m.entries[cnid]
	return e, ok
}

func (m *pathMemo) put(cnid uint32, e pathMemoEntry) {
	if m == nil {
		return
	}
	if _, exists := m.entries[cnid]; !exists {
		m.order = append(m.order, cnid)
	}
	m.entries[cnid] = e

	for len(m.order) > m.max {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.entries, oldest)
	}
}

// pathUp resolves a node's full path by climbing thread records to the root.
//
// memo may be nil, in which case nothing is cached and the walk is exactly the
// one PathForCNID has always done. When it is not nil, the climb stops at the
// first ancestor already known and every ancestor resolved on the way is
// recorded, so a full traversal costs one climb per directory rather than one
// per directory per level.
//
// The root's path is "/" here, but the root is memoised as "" so that a child
// can be formed by appending "/" and a name without a special case.
func (v *Volume) pathUp(cnid uint32, memo *pathMemo) (string, error) {
	if cnid == rootFolderCNID {
		return "/", nil
	}
	if e, ok := memo.get(cnid); ok {
		return e.path, e.err
	}

	var (
		names   []string
		chain   []uint32
		visited = map[uint32]struct{}{}
		base    string
		cur     = cnid
	)

	for cur != rootFolderCNID {
		if e, ok := memo.get(cur); ok {
			if e.err != nil {
				return v.failPath(cnid, chain, memo, e.err)
			}
			base = e.path
			break
		}
		if _, seen := visited[cur]; seen {
			return v.failPath(cnid, chain, memo,
				&ParseError{Op: "path_for_cnid", Offset: int64(cur), Err: ErrCorrupt})
		}
		visited[cur] = struct{}{}

		thr, err := v.findThreadRecord(cur)
		if err != nil {
			// Classic HFS does not require a thread record for a file. Inside
			// Macintosh: Files makes file threads optional — one is written
			// only when something asks for a file ID reference — while HFS+
			// makes them mandatory (TN1150). A volume written by System 7, or
			// by hfsutils today, carries a thread for every directory and none
			// for any file, so without this every by-CNID path lookup on a
			// classic volume fails for every file on it.
			//
			// The fallback belongs on the first hop and nowhere else: only the
			// starting node can be a file, because every node above one in a
			// path is a directory, and directory threads are required on all
			// three variants. Restricting it that way also bounds the cost,
			// since a missing thread higher up stays a fast failure rather
			// than a catalog scan per level.
			if cur != cnid || !errors.Is(err, ErrNotFound) {
				return v.failPath(cnid, chain, memo, err)
			}
			// PathForCNID resolves the record before it climbs, and
			// lookupCNIDRaw caches, so this is normally a cache hit rather
			// than a second scan.
			rec, lerr := v.lookupCNIDRaw(cur)
			if lerr != nil || rec.Name == "" {
				return v.failPath(cnid, chain, memo, err)
			}
			thr = CatalogRecord{Name: rec.Name, ParentCNID: rec.ParentCNID}
		}
		if thr.Name == "" {
			return v.failPath(cnid, chain, memo,
				&ParseError{Op: "path_for_cnid", Offset: int64(cur), Err: ErrCorrupt})
		}

		names = append(names, thr.Name)
		chain = append(chain, cur)
		cur = thr.ParentCNID
	}

	path := base
	for i := len(names) - 1; i >= 0; i-- {
		path += "/" + names[i]
		memo.put(chain[i], pathMemoEntry{path: path})
	}
	return path, nil
}

// failPath records the failure against every node the climb touched, so that
// the siblings and descendants of a broken directory do not each repeat it.
func (v *Volume) failPath(cnid uint32, chain []uint32, memo *pathMemo, err error) (string, error) {
	if memo != nil {
		memo.put(cnid, pathMemoEntry{err: err})
		for _, c := range chain {
			memo.put(c, pathMemoEntry{err: err})
		}
	}
	return "", err
}

// dirPath returns the prefix a child of this directory should be appended to,
// which is "" for the root so that a top-level child becomes "/name".
func (v *Volume) dirPath(cnid uint32, memo *pathMemo) (string, bool) {
	if cnid == rootFolderCNID {
		return "", true
	}
	p, err := v.pathUp(cnid, memo)
	if err != nil {
		return "", false
	}
	return p, true
}

// PathRecord pairs a catalog record with the path it occupies.
type PathRecord struct {
	Path   string
	Record CatalogRecord
}

// WalkPaths calls cb for every live file and folder on the volume, with the
// full path each one occupies.
//
// This differs from [Volume.WalkCatalog] in three ways that matter. Thread
// records are not emitted: a thread record is path information rather than a
// thing with a path, and passing one to the callback would hand it a record
// with no name, no timestamps and a CNID of zero — [Volume.WalkDirCNID] takes
// the same view. Records with an empty name are skipped for the same reason.
// The root folder is emitted once, as "/", whatever name the catalog stores for
// it; classic HFS volumes routinely store none.
//
// Records arrive in catalog B-tree key order — parent CNID ascending, then name
// — which is neither directory order nor parents before children. Do not build
// a tree by attaching each record to a parent already seen; use the path.
//
// An orphaned record, whose parent chain cannot be followed to the root because
// a thread record is missing or points into a cycle, is still emitted, with an
// empty path. Suppressing it would hide exactly the damage an examiner is
// looking for, and inventing a path for it would be a claim the volume does not
// support; [CatalogRecord.ParentCNID] says where the record believed it lived.
// The condition is recorded through [Volume.Anomalies].
//
// Hard links are not resolved, matching [Volume.WalkCatalog]: a link is
// reported at its own path with its own record. Call [Volume.OpenCNID] on the
// CNID to follow one.
//
// Paths are resolved from the walk itself wherever the catalog permits it and
// looked up only for directories the walk has not yet reached, so the cost and
// the memory both scale with the number of directories rather than the number
// of files.
func (v *Volume) WalkPaths(cb func(path string, rec CatalogRecord) error) error {
	if cb == nil {
		return nil
	}

	memo := newPathMemo(walkPathMemoMax)
	memo.put(rootFolderCNID, pathMemoEntry{path: ""})

	err := v.walkCatalogLeafChain(func(key CatalogKey, payload []byte) error {
		rec, err := v.decodeCatalogRecord(key, payload)
		if err != nil {
			v.noteDecodeFailure("catalog_decode", key.ParentCNID)
			return nil
		}
		if rec.Type != CatalogRecordFolder && rec.Type != CatalogRecordFile {
			return nil
		}
		// The root is checked before the empty-name filter: its path is "/"
		// whatever name the catalog stores for it, and classic HFS volumes
		// routinely store none.
		if rec.CNID == rootFolderCNID {
			return cb("/", rec)
		}
		if rec.Name == "" {
			return nil
		}

		base, ok := v.dirPath(rec.ParentCNID, memo)
		if !ok {
			v.noteAnomaly("walk_path", int64(rec.ParentCNID),
				"record's parent chain does not reach the root")
			return cb("", rec)
		}

		path := base + "/" + rec.Name
		if rec.IsDirectory() {
			memo.put(rec.CNID, pathMemoEntry{path: path})
		}
		return cb(path, rec)
	})
	return endWalk(err)
}

// WalkPathsContext is [Volume.WalkPaths] with cancellation.
//
// A full walk on a large image reads every leaf node and resolves a path per
// directory, so a caller that cannot wait needs a way out. The context is
// checked once per record; its error is returned.
func (v *Volume) WalkPathsContext(ctx context.Context, cb func(path string, rec CatalogRecord) error) error {
	if cb == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return v.WalkPaths(func(path string, rec CatalogRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return cb(path, rec)
	})
}

// PathRecords returns every live file and folder with its path, the
// slice-returning form of [Volume.WalkPaths].
//
// It holds the whole catalog in memory. Walk instead on a volume of any size.
func (v *Volume) PathRecords() ([]PathRecord, error) {
	var out []PathRecord
	err := v.WalkPaths(func(path string, rec CatalogRecord) error {
		out = append(out, PathRecord{Path: path, Record: rec})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
