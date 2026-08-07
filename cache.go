package hfs

// B-tree identifiers, so nodes from different trees cannot collide in the
// node cache.
const (
	treeCatalog uint8 = iota
	treeExtents
	treeAttributes
)

type nodeCacheKey struct {
	tree uint8
	num  uint32
}

// SetNodeCacheSize bounds the B-tree node cache. A size of 0 disables it and
// drops anything already held. Negative values are ignored.
//
// Safe for concurrent use.
func (v *Volume) SetNodeCacheSize(n int) {
	if v == nil || n < 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	v.nodeCacheMax = n
	if n == 0 {
		v.nodeCache = nil
		v.nodeOrder = nil
		return
	}
	for len(v.nodeOrder) > n {
		v.evictOldestNodeLocked()
	}
}

// nodeCacheGet returns a cached node buffer.
//
// The returned slice is shared, not copied. Callers must treat it as read-only,
// which every parser in this package already does — they copy out the fields
// they retain rather than aliasing the node.
func (v *Volume) nodeCacheGet(k nodeCacheKey) ([]byte, bool) {
	if v == nil {
		return nil, false
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.nodeCache == nil {
		return nil, false
	}
	buf, ok := v.nodeCache[k]
	return buf, ok
}

func (v *Volume) nodeCachePut(k nodeCacheKey, buf []byte) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.nodeCacheMax == 0 {
		return
	}
	if v.nodeCache == nil {
		v.nodeCache = make(map[nodeCacheKey][]byte, min(v.nodeCacheMax, 32))
	}
	if _, exists := v.nodeCache[k]; exists {
		return
	}
	for len(v.nodeOrder) >= v.nodeCacheMax {
		v.evictOldestNodeLocked()
	}
	v.nodeCache[k] = buf
	v.nodeOrder = append(v.nodeOrder, k)
}

func (v *Volume) evictOldestNodeLocked() {
	if len(v.nodeOrder) == 0 {
		return
	}
	oldest := v.nodeOrder[0]
	v.nodeOrder = v.nodeOrder[1:]
	delete(v.nodeCache, oldest)
}

// Anomaly records a structural inconsistency encountered during parsing that
// did not stop the operation.
//
// Anomalies are forensically meaningful in their own right: a catalog whose
// records largely fail to decode is a finding, not merely an inconvenience,
// and without this a caller cannot distinguish a clean volume from a damaged
// one that happened to answer the questions asked of it.
type Anomaly struct {
	Op     string // the operation that observed it, e.g. "catalog_search"
	Offset int64  // context value; a CNID or byte offset depending on Op
	Detail string // human-readable description
}

// Anomalies returns the distinct anomalies observed on this volume so far, in
// the order they were first seen. Repeats of an already-recorded (Op, Detail)
// pair are counted but not appended.
//
// Safe for concurrent use.
func (v *Volume) Anomalies() []Anomaly {
	if v == nil {
		return nil
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]Anomaly, len(v.anomalies))
	copy(out, v.anomalies)
	return out
}

// AnomalyCount returns the total number of anomalies observed, including
// repeats and any beyond the tracking limit. It may exceed len(Anomalies()).
//
// Safe for concurrent use.
func (v *Volume) AnomalyCount() int {
	if v == nil {
		return 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.anomalyTotal
}

// noteDecodeFailure records a record that could not be decoded during a walk.
//
// These were previously discarded silently, which left a catalog with a large
// fraction of unreadable records indistinguishable from a clean one. The record
// is still skipped — a walk must not abort because one entry is damaged — but
// the caller can now see that it happened.
func (v *Volume) noteDecodeFailure(op string, parentCNID uint32) {
	v.noteAnomaly(op, int64(parentCNID), "catalog record failed to decode and was skipped")
}

// noteAnomaly records a structural inconsistency, deduplicating on (Op,
// Detail) so a fallback that fires once per file does not flood the list.
func (v *Volume) noteAnomaly(op string, offset int64, detail string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	v.anomalyTotal++
	if len(v.anomalies) >= maxTrackedAnomalies {
		return
	}
	for _, a := range v.anomalies {
		if a.Op == op && a.Detail == detail {
			return
		}
	}
	v.anomalies = append(v.anomalies, Anomaly{Op: op, Offset: offset, Detail: detail})
}

// SetCacheSize bounds the catalog record cache. A size of 0 disables caching
// and drops anything already held. Negative values are ignored.
//
// Safe for concurrent use, though calling it while other goroutines are
// reading simply means they may observe either the old or the new bound.
func (v *Volume) SetCacheSize(n int) {
	if v == nil || n < 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	v.cacheMax = n
	if n == 0 {
		v.recCache = nil
		v.recOrder = nil
		return
	}
	for len(v.recOrder) > n {
		v.evictOldestLocked()
	}
}

// cacheLookup returns a cached raw catalog record.
//
// Only records as they appear on disk are cached. Hard-link resolution and
// decmpfs hydration are applied afterwards by hydrateCatalogRecord, so the
// cached value stays a pure function of the volume bytes and cannot go stale.
func (v *Volume) cacheLookup(cnid uint32) (CatalogRecord, bool) {
	if v == nil {
		return CatalogRecord{}, false
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.recCache == nil {
		return CatalogRecord{}, false
	}
	rec, ok := v.recCache[cnid]
	return rec, ok
}

// cacheStore records a raw catalog record, evicting in insertion order once
// the bound is reached.
func (v *Volume) cacheStore(cnid uint32, rec CatalogRecord) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.cacheMax == 0 {
		return
	}
	if v.recCache == nil {
		v.recCache = make(map[uint32]CatalogRecord, min(v.cacheMax, 64))
	}
	if _, exists := v.recCache[cnid]; exists {
		v.recCache[cnid] = rec
		return
	}
	for len(v.recOrder) >= v.cacheMax {
		v.evictOldestLocked()
	}
	v.recCache[cnid] = rec
	v.recOrder = append(v.recOrder, cnid)
}

// evictOldestLocked drops the oldest inserted entry. The caller must hold the
// write lock.
//
// Insertion-order eviction rather than true LRU: the access pattern that
// matters here is a traversal sweeping through records once, where recency of
// use tracks recency of insertion closely enough that the bookkeeping a real
// LRU needs would not pay for itself.
func (v *Volume) evictOldestLocked() {
	if len(v.recOrder) == 0 {
		return
	}
	oldest := v.recOrder[0]
	v.recOrder = v.recOrder[1:]
	delete(v.recCache, oldest)
}
