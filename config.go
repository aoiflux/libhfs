package libhfs

import (
	"io"
	"runtime"
)

// Tunable defaults. Every value a caller might reasonably want to change lives
// here rather than being written into the code that uses it.
const (
	// DefaultCacheSize is the number of catalog records a Volume retains.
	// Path reconstruction and directory-then-stat traversal both revisit the
	// same records repeatedly, so a small cache removes most repeat lookups.
	DefaultCacheSize = 4096

	// DefaultNodeCacheSize is the number of B-tree nodes a Volume retains.
	// Every keyed descent re-reads the same root index node, so without this a
	// multi-component path lookup pays for it once per component.
	DefaultNodeCacheSize = 128

	// DefaultMaxAlloc bounds any single buffer sized from an on-disk field.
	// Sizes read from a volume are attacker-controlled in the sense that
	// matters: a corrupt image can declare a fork of 2^60 bytes, and
	// allocating that ends the process with an OOM kill rather than an error.
	DefaultMaxAlloc = int64(1) << 30 // 1 GiB

	// DefaultCarveWorkers is the worker count used for unallocated-space
	// carving when Config.CarveWorkers is left at zero.
	//
	// It is 1 — sequential — because that is what measurement showed to be
	// fastest. Carving reads contiguous blocks, which the operating system
	// prefetches aggressively; splitting that into several concurrent readers
	// turns one sequential stream into N interleaved ones and defeats the
	// readahead. On a 511 MB image over a local disk, one worker took ~470 ms
	// while four took ~615 ms, and larger per-task batches did not recover the
	// difference.
	//
	// The worker pool remains available because that result is a property of
	// the storage, not of the algorithm: a reader with high per-request latency
	// and deep queueing — network-backed, or NVMe with many outstanding
	// requests — can benefit. Set Config.CarveWorkers explicitly to opt in, and
	// measure on the storage you actually use.
	DefaultCarveWorkers = 1

	// MaxCarveWorkers caps the worker count derived from GOMAXPROCS by
	// [AutoCarveWorkers]. It does not cap an explicitly configured value.
	MaxCarveWorkers = 16

	// MaxAllocUnlimited removes the allocation cap when set on
	// [Config.MaxAlloc]. It is a sentinel rather than a bare zero so that
	// disabling a memory-safety guard cannot happen by leaving a field unset.
	MaxAllocUnlimited = int64(-1)

	// AutoCarveWorkers requests a worker count derived from GOMAXPROCS,
	// capped at MaxCarveWorkers. Pass it to Config.CarveWorkers or
	// [Volume.SetCarveWorkers] to scale with the machine rather than taking the
	// measured sequential default.
	AutoCarveWorkers = -1

	// maxTrackedAnomalies bounds the anomaly list so a thoroughly corrupt image
	// cannot exhaust memory through reporting alone. AnomalyCount keeps
	// counting past the limit.
	maxTrackedAnomalies = 1000

	// carveBlockBatch is how many allocation blocks one carve task covers.
	// Larger batches amortise task overhead; smaller ones spread work more
	// evenly across workers and make cancellation more responsive.
	carveBlockBatch = 64

	// bitmapChunkBytes is how much of the allocation bitmap is read at a time.
	bitmapChunkBytes = 64 << 10
)

// Config holds the tunable behaviour of a Volume.
//
// The zero Config is valid and equivalent to [DefaultConfig]: every field's zero
// value means "use the default", so a caller can set one field without knowing
// about the others.
//
// Config is copied into the Volume at open time. Changing a Config afterwards
// has no effect; use the Set* methods on Volume instead.
type Config struct {
	// CacheSize bounds the catalog record cache, in records. Negative means
	// unlimited is not offered — use a large value. A caller that wants
	// caching off entirely should call Volume.SetCacheSize(0) after opening,
	// since 0 here means "use the default".
	CacheSize int

	// NodeCacheSize bounds the B-tree node cache, in nodes.
	NodeCacheSize int

	// MaxAlloc caps any single buffer sized from an on-disk field. Reads that
	// would exceed it return ErrSizeLimit.
	//
	// Zero means [DefaultMaxAlloc], following the zero-value convention of this
	// struct. To remove the cap entirely, use [MaxAllocUnlimited] — an explicit
	// sentinel rather than a bare 0, so that lifting a memory-safety guard is
	// always a deliberate act and never the result of a forgotten field.
	MaxAlloc int64

	// TextEncoding selects how classic HFS filename bytes are decoded. It has
	// no effect on HFS+ or HFSX, which store names as UTF-16.
	TextEncoding TextEncoding

	// CarveWorkers sets the degree of parallelism for unallocated-space
	// carving, the only operation in this package that can run concurrently.
	//
	// Zero takes [DefaultCarveWorkers], which is sequential because that
	// measured fastest on local storage. [AutoCarveWorkers] scales with
	// GOMAXPROCS. Any positive value is used as given.
	//
	// Results are identical at every worker count, so this is purely a
	// throughput knob: each task scans a disjoint span of blocks and its
	// findings are merged in block order.
	CarveWorkers int

	// DisableCache turns off both caches. Distinct from CacheSize: zero sizes
	// mean "default", while this means "none", which a Config literal can
	// express without knowing the defaults.
	DisableCache bool

	// BaseOffset is the byte of the reader at which the volume begins.
	//
	// Set it when the reader is a whole disk image and the volume lives in a
	// partition part-way through it. Every offset the package then reports —
	// [ByteRange.DiskOffset], a [DeletedRecord.ByteOffset] carved from
	// unallocated space, [VolumeSummary.BaseOffset] — stays absolute against
	// that image. The alternative, wrapping the partition in an
	// [io.SectionReader], also reads the volume correctly but makes every
	// reported offset partition-relative, and nothing at the point of use says
	// so; intersecting those against whole-image offsets yields a confident
	// wrong answer rather than an error.
	//
	// It ADDS to the offset [Open] derives, and does not replace it. For an
	// image holding a partition at byte N that contains an HFS wrapper whose
	// embedded HFS+ volume starts W bytes into the wrapper, set this to N — the
	// wrapper's start, which is all a caller can see from outside — and
	// [Volume.BaseOffset] reports N+W. Assuming it replaces rather than composes
	// produces offsets wrong by exactly W, which is small enough to look
	// plausible.
	//
	// Note that this and [Volume.BaseOffset] are deliberately named alike
	// across this library's siblings but are NOT the same quantity: this is
	// where the volume begins, that is where its allocation block 0 begins. On
	// classic HFS they differ by the MDB and the bitmap, so on a volume opened
	// with BaseOffset N, Config().BaseOffset is N while Volume.BaseOffset() is
	// N+drAlBlSt*512.
	//
	// Zero means the volume begins at the start of the reader, which is what
	// [Open] does, so leaving it unset changes nothing. A negative value is
	// rejected at open with [ErrInvalidOffset]; unlike the other fields here a
	// negative value is not clamped, because a sign error in an offset is a bug
	// in the caller and reading the wrong part of an image is the outcome this
	// field exists to prevent.
	BaseOffset int64
}

// DefaultConfig returns the configuration [Open] uses.
func DefaultConfig() Config {
	return Config{
		CacheSize:     DefaultCacheSize,
		NodeCacheSize: DefaultNodeCacheSize,
		MaxAlloc:      DefaultMaxAlloc,
		TextEncoding:  TextEncodingMacRoman,
		CarveWorkers:  DefaultCarveWorkers,
	}
}

// normalise fills zero fields with their defaults and clamps the rest, so the
// rest of the package can read Config without repeating the checks.
func (c Config) normalise() Config {
	out := c
	if out.CacheSize <= 0 {
		out.CacheSize = DefaultCacheSize
	}
	if out.NodeCacheSize <= 0 {
		out.NodeCacheSize = DefaultNodeCacheSize
	}
	switch {
	case out.MaxAlloc == MaxAllocUnlimited:
		out.MaxAlloc = 0 // 0 is "no limit" internally
	case out.MaxAlloc <= 0:
		out.MaxAlloc = DefaultMaxAlloc
	}
	if out.DisableCache {
		out.CacheSize = 0
		out.NodeCacheSize = 0
	}
	out.CarveWorkers = resolveWorkerCount(out.CarveWorkers)
	return out
}

// resolveWorkerCount turns a configured worker count into a concrete one.
//
// Zero means "unset" and takes the measured default. [AutoCarveWorkers] derives
// from GOMAXPROCS, capped, for callers who want to scale with the machine. Any
// other value is honoured as given, including counts above the cap: the cap
// guards the derived value, not the caller's judgement about their own storage.
func resolveWorkerCount(configured int) int {
	switch {
	case configured == 0:
		return DefaultCarveWorkers
	case configured > 0:
		return configured
	}

	// AutoCarveWorkers, or any other negative value.
	n := runtime.GOMAXPROCS(0)
	if n > MaxCarveWorkers {
		n = MaxCarveWorkers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// Config returns the volume's current effective configuration.
//
// Safe for concurrent use. The returned value is a snapshot: mutating it does
// not affect the volume.
func (v *Volume) Config() Config {
	if v == nil {
		return DefaultConfig()
	}
	// volumeStart belongs to the write-once group above mu and needs no lock;
	// it is read inside the critical section only to keep the literal in one
	// piece. It is deliberately absent from applyConfig: every unsynchronised
	// read of baseOffset would race a post-open setter, so there is none.
	v.mu.RLock()
	defer v.mu.RUnlock()
	return Config{
		CacheSize:     v.cacheMax,
		NodeCacheSize: v.nodeCacheMax,
		MaxAlloc:      v.maxAlloc,
		TextEncoding:  v.textEncoding,
		CarveWorkers:  v.carveWorkers,
		BaseOffset:    v.volumeStart,
	}
}

// SetCarveWorkers sets the degree of parallelism used for unallocated-space
// carving. One forces sequential execution and starts no goroutines at all;
// zero restores [DefaultCarveWorkers]; [AutoCarveWorkers] scales with
// GOMAXPROCS.
//
// Results do not depend on this value, so it is safe to tune purely for
// throughput. Note that the default is sequential because concurrency measured
// slower on local storage — carving reads contiguous blocks that the operating
// system prefetches, and concurrent readers defeat that prefetching. Raise it
// only for a reader with high per-request latency and deep queueing, and
// measure before doing so.
//
// Safe for concurrent use.
func (v *Volume) SetCarveWorkers(n int) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.carveWorkers = resolveWorkerCount(n)
}

// OpenWithConfig is [Open] with explicit configuration.
//
// The zero Config is valid and matches Open's behaviour, so this is only needed
// to change a default. See [Config] for the individual fields.
//
// Unlike the other fields, [Config.BaseOffset] is honoured during the open
// rather than applied to the volume afterwards — it decides where the header is
// read from, so it cannot be a post-open adjustment.
func OpenWithConfig(r io.ReaderAt, cfg Config) (*Volume, error) {
	norm := cfg.normalise()
	vol, err := openAt(r, norm.BaseOffset)
	if err != nil {
		return nil, err
	}
	vol.applyConfig(norm)
	return vol, nil
}

// applyConfig installs a normalised configuration on a freshly opened volume.
func (v *Volume) applyConfig(cfg Config) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cacheMax = cfg.CacheSize
	v.nodeCacheMax = cfg.NodeCacheSize
	v.maxAlloc = cfg.MaxAlloc
	v.textEncoding = cfg.TextEncoding
	v.carveWorkers = cfg.CarveWorkers
}
